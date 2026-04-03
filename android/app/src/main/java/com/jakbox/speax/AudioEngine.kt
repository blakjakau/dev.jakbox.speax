package com.jakbox.speax

import android.annotation.SuppressLint
import android.media.AudioFormat
import android.media.AudioManager
import android.media.AudioTrack
import android.util.Log
import kotlin.math.sqrt
import java.util.concurrent.LinkedBlockingQueue
import android.os.Process

class AudioEngine(
    private val onSpeechFinalized: (ByteArray, Long) -> Unit,
    private val onVolumeChange: (Float) -> Unit = {},
    private val onSpeechStart: () -> Unit = {},
    private val onAiDataChange: (Float, List<Float>) -> Unit = { _, _ -> },
    private val onBufferProgress: (Float) -> Unit = {},
    private val onPlaybackComplete: () -> Unit = {},
    private val onTextPlayed: (String) -> Unit = {},
    private val onStreamingChunk: (ByteArray, Long, Byte) -> Unit = { _, _, _ -> }
) {

    private val sampleRate = 16000
    private val channelConfig = AudioFormat.CHANNEL_IN_MONO
    private val audioFormat = AudioFormat.ENCODING_PCM_16BIT

    private var audioTrack: AudioTrack? = null
    private var playbackThread: Thread? = null
    private var progressThread: Thread? = null
    private val audioQueue = LinkedBlockingQueue<Triple<ByteArray, Int, String?>>()
    @Volatile private var isPausedLocally = false
    var isMicMuted = false
        set(value) {
            Log.d("AudioEngine", "Switching Mic Mute: $field -> $value")
            field = value
            nativeAudioEngine?.setMuted(value)
        }
    var micProfile: String = "standard"
        set(value) {
            Log.d("AudioEngine", "Switching Mic Profile: $field -> $value")
            field = value
            nativeAudioEngine?.setProfile(value)
        }
    var averageSpeechRms = 600.0 // Baseline adaptive RMS tracker
    private var totalAiFrames = 0
    private var totalWrittenFrames = 0
    // Stores (StartFrame, RMS, Bands)
    private val audioDataQueue = java.util.concurrent.ConcurrentLinkedQueue<Triple<Int, Float, List<Float>>>()
    private val textSyncQueue = java.util.concurrent.ConcurrentLinkedQueue<Pair<Int, String>>()
    private var currentSeqID = 0L

    var noiseThreshold = 255.0
        set(value) {
            Log.d("AudioEngine", "Switching Noise Threshold: $field -> $value")
            field = value
            nativeAudioEngine?.setThreshold(value)
        }
    var isPlaybackActive = false
        set(value) {
            Log.d("AudioEngine", "Switching Playback Active: $field -> $value")
            field = value
            nativeAudioEngine?.setPlaybackActive(value)
        }
    
    var isRecording = false
        private set

    private var nativeAudioEngine: NativeAudioEngine? = null

    init {
        nativeAudioEngine = NativeAudioEngine(
            onSpeechStart = {
                if (!isMicMuted) {
                    Log.d("AudioEngine", "Speech detected! Suspending playback instantly.")
                    suspendPlayback()
                    onSpeechStart()
                }
            },
            onStreamingChunk = { buffer, size, type ->
                if (!isMicMuted) {
                    val byteArray = ByteArray(size)
                    buffer.get(byteArray)
                    onStreamingChunk(byteArray, currentSeqID, type)
                }
            },
            onSpeechEnd = { buffer, size ->
                if (!isMicMuted) {
                    val byteArray = ByteArray(size)
                    buffer.get(byteArray)
                    Log.d("AudioEngine", "Native VAD: End of speech, sending final ${byteArray.size} bytes")
                    onStreamingChunk(byteArray, currentSeqID, 0x02.toByte()) // 0x02: END
                }
            },
            onVolumeChange = { rms ->
                onVolumeChange(if (isMicMuted) 0f else rms)
            }
        )
    }

    @SuppressLint("MissingPermission")
    fun startRecording(force: Boolean = false) {
        if (isRecording && !force) {
            Log.d("AudioEngine", "startRecording: Already recording, skipping re-init.")
            return
        }
        
        Log.d("AudioEngine", "Starting recording (force=$force) with profile=$micProfile, threshold=$noiseThreshold, muted=$isMicMuted")
        
        // Use a background thread for everything that follows, as AAudio open/start 
        // can be slow and we involve Thread.sleep for retries!
        Thread {
            // Safety: Ensure any previous session is stopped first
            isRecording = false // Reset while we transition 
            nativeAudioEngine?.stop()
            
            // Give the OS a moment to fully release the hardware resource
            try {
                Thread.sleep(150)
            } catch (e: InterruptedException) {
                // ignore
            }
            
            nativeAudioEngine?.setProfile(micProfile)
            nativeAudioEngine?.setThreshold(noiseThreshold)
            nativeAudioEngine?.setMuted(isMicMuted) // Apply current mute state
            nativeAudioEngine?.setPlaybackActive(isPlaybackActive) // Apply current playback state
            currentSeqID = System.currentTimeMillis()
            
            // Retry logic: opening the mic can fail if the audio system is busy or transitioning modes
            var attempts = 3
            while (attempts > 0) {
                val result = nativeAudioEngine?.start() ?: -1
                if (result == 0) {
                    Log.d("AudioEngine", "Native audio engine started successfully.")
                    isRecording = true
                    break
                }
                
                attempts--
                Log.w("AudioEngine", "Failed to start native audio engine: $result. Attempts remaining: $attempts")
                if (attempts > 0) {
                    try {
                        // Increased delay slightly to give more room for system transitions
                        Thread.sleep(800)
                    } catch (e: InterruptedException) {
                        break
                    }
                } else {
                    Log.e("AudioEngine", "Giving up on starting native audio engine after 3 attempts.")
                }
            }
        }.start()
    }

    fun stopRecording() {
        Log.d("AudioEngine", "Stopping recording (current profile=$micProfile)")
        isRecording = false
        // Stop on a background thread too, just to be safe and consistent 
        Thread {
            nativeAudioEngine?.stop()
        }.start()
    }

    fun forceEndStreaming() {
        nativeAudioEngine?.forceEndStreaming()
    }

    fun release() {
        isRecording = false
        nativeAudioEngine?.release()
        nativeAudioEngine = null
    }

    fun playAudioChunk(pcmData: ByteArray, pcmSampleRate: Int = 22050, associatedText: String? = null) {
        // Calculate frames from bytes (16-bit Mono = 2 bytes per frame)
        // We subtract 44 bytes if it's a WAV header so we don't count metadata as audio
        val isWav = pcmData.size >= 44 && pcmData[0] == 'R'.code.toByte() && pcmData[1] == 'I'.code.toByte()
        val audioBytes = if (isWav) pcmData.size - 44 else pcmData.size
        totalAiFrames += audioBytes / 2
        
        audioQueue.put(Triple(pcmData, pcmSampleRate, associatedText)) // Instantly queues and returns, freeing the WebSocket thread!

        if (playbackThread == null || playbackThread?.isAlive != true) {
            playbackThread = Thread {
                Process.setThreadPriority(Process.THREAD_PRIORITY_URGENT_AUDIO)
                while (!Thread.currentThread().isInterrupted) {
                    try {
                        val chunk = audioQueue.take()
                        playChunkInternal(chunk.first, chunk.second, chunk.third)
                    } catch (e: InterruptedException) {
                        break
                    }
                }
            }
            playbackThread?.start()
        }

        if (progressThread == null || progressThread?.isAlive != true) {
            progressThread = Thread {
                while (!Thread.currentThread().isInterrupted) {
                    val track = audioTrack
                    if (track != null && track.playState == AudioTrack.PLAYSTATE_PLAYING && !isPausedLocally) {
                        val currentFrame = track.playbackHeadPosition
                        // Draining logic: 1.0 (100% remaining) down to 0.0
                        val progress = if (totalAiFrames > 0) (1f - (currentFrame.toFloat() / totalAiFrames.toFloat())).coerceIn(0f, 1f) else 0f
                        onBufferProgress(progress)

                        // Pull synced data from queue based on hardware playback head!
                        var targetRms = -1f
                        var targetBands = listOf<Float>()
                        var poppedAny = false
                        while (audioDataQueue.isNotEmpty() && audioDataQueue.peek()!!.first <= currentFrame) {
                            val popped = audioDataQueue.poll()!!
                            if (popped.second > targetRms) {
                                targetRms = popped.second // Find max transient in this 16ms window
                                targetBands = popped.third
                            }
                            poppedAny = true
                        }
                        if (poppedAny) {
                            onAiDataChange(targetRms, targetBands)
                        } 

                        // 3. Hardware Text Reveal: Sync text to the exact playback frame
                        while (textSyncQueue.isNotEmpty() && textSyncQueue.peek()!!.first <= currentFrame) {
                            val text = textSyncQueue.poll()!!.second
                            onTextPlayed(text)
                        }

                        if (audioDataQueue.isEmpty() && currentFrame >= totalWrittenFrames) {
                            if (totalAiFrames > 0) { 
                                // Hardware Drain: Wait for the buffer to actually reach the speaker
                                try {
                                    Thread.sleep(250)
                                } catch (e: InterruptedException) { break }
                                
                                totalAiFrames = 0 
                                onAiDataChange(0f, listOf(0f,0f,0f,0f,0f)) 
                                onPlaybackComplete()
                            }
                        }
                    }
                    try {
                        Thread.sleep(16) // ~60fps UI polling
                    } catch (e: InterruptedException) {
                        break
                    }
                }
            }
            progressThread?.start()
        }
    }

    private fun playChunkInternal(pcmData: ByteArray, chunkSampleRate: Int, associatedText: String?) {
        if (associatedText != null) {
            textSyncQueue.add(Pair(totalWrittenFrames, associatedText))
        }

        // Check if the data has a standard 44-byte RIFF/WAVE header
        val isWav = pcmData.size >= 44 && pcmData[0] == 'R'.code.toByte() && pcmData[1] == 'I'.code.toByte()
        
        var trackSampleRate = chunkSampleRate
        
        if (isWav) {
            // Extract Sample Rate from WAV header (bytes 24-27, Little Endian)
            trackSampleRate = (pcmData[24].toInt() and 0xFF) or
                    ((pcmData[25].toInt() and 0xFF) shl 8) or
                    ((pcmData[26].toInt() and 0xFF) shl 16) or
                    ((pcmData[27].toInt() and 0xFF) shl 24)
        }

        // If the sample rate changed, or the track doesn't exist, (re)build it
        if (audioTrack == null || audioTrack?.sampleRate != trackSampleRate) {
            audioTrack?.release()
            
            val minBufferSize = AudioTrack.getMinBufferSize(trackSampleRate, AudioFormat.CHANNEL_OUT_MONO, audioFormat)
            
            audioTrack = AudioTrack.Builder()
                .setAudioAttributes(
                    android.media.AudioAttributes.Builder()
                        .setUsage(android.media.AudioAttributes.USAGE_VOICE_COMMUNICATION)
                        .setContentType(android.media.AudioAttributes.CONTENT_TYPE_SPEECH)
                        .build()
                )
                .setAudioFormat(
                    AudioFormat.Builder()
                        .setEncoding(audioFormat)
                        .setSampleRate(trackSampleRate)
                        .setChannelMask(AudioFormat.CHANNEL_OUT_MONO)
                        .build()
                )
                .setBufferSizeInBytes(minBufferSize * 4) // Generous buffer for gapless TTS
                .setTransferMode(AudioTrack.MODE_STREAM)
                .build()
        }

        var offset = if (isWav) 44 else 0
        // Write in exactly 512-byte blocks (16ms) to feed the Jetpack Compose visualizer at exactly 60fps!
        val chunkSize = 512 
        while (offset < pcmData.size) {
            // The Guillotine: If an abort was triggered, instantly exit this chunk to prevent "ghost audio"
            if (Thread.currentThread().isInterrupted) break

            // If VAD tripped, lock the thread here so we don't throw away data!
            if (isPausedLocally) {
                try {
                    Thread.sleep(20)
                } catch (e: InterruptedException) {
                    Thread.currentThread().interrupt()
                    break
                }
                continue
            }

            if (audioTrack?.playState != AudioTrack.PLAYSTATE_PLAYING) {
                audioTrack?.play()
            }

            var bytesToWrite = minOf(chunkSize, pcmData.size - offset)
            if (bytesToWrite % 2 != 0) bytesToWrite-- // Force even boundary for 16-bit PCM shorts

            // Calculate AI RMS for the pulsing visualizer
            var sum = 0.0
            for (i in 0 until bytesToWrite step 2) {
                val low = pcmData[offset + i].toInt() and 0xFF
                val high = pcmData[offset + i + 1].toInt() shl 8
                val sample = (low or high).toShort()
                sum += sample.toDouble() * sample.toDouble()
            }

            val written = audioTrack?.write(pcmData, offset, bytesToWrite) ?: 0
            if (written > 0) {
                val chunkFrames = written / 2
                val rms = if (chunkFrames > 0) sqrt(sum / chunkFrames).toFloat() else 0f
                val bands = FftHelper.calculateBands(pcmData, offset, bytesToWrite)
                audioDataQueue.add(Triple(totalWrittenFrames, rms, bands)) // Queue data to its exact playback frame
                offset += written
                totalWrittenFrames += chunkFrames
            } else if (written < 0) {
                break // Native audio error, drop chunk
            }
        }
    }

    fun suspendPlayback() {
        isPausedLocally = true
        audioTrack?.pause()
    }

    fun resumePlayback() {
        isPausedLocally = false
        // The playChunkInternal loop will automatically wake up and call .play()
    }

    fun prepareForNewResponse() {
        Log.d("AudioEngine", "Preparing for new AI response session. Resetting frames.")
        totalAiFrames = 0
        totalWrittenFrames = 0
        audioDataQueue.clear()
        textSyncQueue.clear()
        audioTrack?.flush()
        val track = audioTrack
        if (track != null && track.playState == AudioTrack.PLAYSTATE_PLAYING) {
            // Already initialized, don't kill it but we need to reset head tracking if possible
            // AudioTrack head doesn't reset easily without stop/start, but we'll manage with totalAiFrames as a latch
        }
    }

    fun abortPlayback() {
        isPausedLocally = false
        audioQueue.clear() // Drop all pending TTS chunks
        // KILL the playback thread so it instantly drops the CURRENT chunk!
        playbackThread?.interrupt()
        playbackThread = null
        audioTrack?.pause()
        audioTrack?.flush()
        prepareForNewResponse()
        
        onBufferProgress(0f)
        onAiDataChange(0f, listOf(0f, 0f, 0f, 0f, 0f))
        progressThread?.interrupt()
        progressThread = null
    }
}
