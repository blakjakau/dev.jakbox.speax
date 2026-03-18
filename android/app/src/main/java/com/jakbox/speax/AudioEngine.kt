package com.jakbox.speax

import android.annotation.SuppressLint
import android.media.AudioFormat
import android.media.AudioManager
import android.media.AudioRecord
import android.media.AudioTrack
import android.media.MediaRecorder
import android.media.audiofx.AcousticEchoCanceler
import android.media.audiofx.AutomaticGainControl
import android.media.audiofx.NoiseSuppressor
import android.util.Log
import kotlin.math.sqrt
import java.util.concurrent.LinkedBlockingQueue
import android.os.Process

class AudioEngine(
    private val onSpeechFinalized: (ByteArray, Long) -> Unit,
    private val onVolumeChange: (Float) -> Unit = {},
    private val onSpeechStart: () -> Unit = {},
    private val onAiVolumeChange: (Float) -> Unit = {},
    private val onBufferProgress: (Float) -> Unit = {},
    private val onPlaybackComplete: () -> Unit = {}
) {

    private val sampleRate = 16000
    private val channelConfig = AudioFormat.CHANNEL_IN_MONO
    private val audioFormat = AudioFormat.ENCODING_PCM_16BIT
    private val bufferSize = AudioRecord.getMinBufferSize(sampleRate, channelConfig, audioFormat)

    private var audioRecord: AudioRecord? = null
    private var audioTrack: AudioTrack? = null
    private var recordingThread: Thread? = null
    private var playbackThread: Thread? = null
    private var noiseSuppressor: NoiseSuppressor? = null
    private var echoCanceler: AcousticEchoCanceler? = null
    private var autoGainControl: AutomaticGainControl? = null
    private var progressThread: Thread? = null
    private val audioQueue = LinkedBlockingQueue<Pair<ByteArray, Int>>()
    @Volatile private var isPausedLocally = false
    var isMicMuted = false
    var micProfile: String = "standard"
    var averageSpeechRms = 600.0 // Baseline adaptive RMS tracker
    private var totalAiFrames = 0
    private var totalWrittenFrames = 0
    private val rmsQueue = java.util.concurrent.ConcurrentLinkedQueue<Pair<Int, Float>>()
    private var speechStartTime = 0L

    // VAD Constants
    // 500.0 matches the 0.015 float threshold from the PWA (0.015 * 32768 ≈ 491.5)
    var noiseThreshold = 300.0 
    private val SILENCE_FRAMES_LIMIT = 65 // 65 frames * 32ms = ~2s silence before finalizing
    private val PRE_ROLL_FRAMES = 16 // 16 frames * 32ms = ~0.5s to catch soft leading consonants
    private val MIN_CHUNKS = 16 // 16 frames * 1024 bytes = ~16,384 bytes minimum for Whisper
    private val FRAME_SIZE = 512 // 512 shorts (1024 bytes) = 32ms audio chunks for silky smooth UI
    private val GAIN_MULTIPLIER = 1.0f // Software boost to counteract VOICE_COMMUNICATION AGC

    @SuppressLint("MissingPermission")
    fun startRecording() {
        if (recordingThread != null) return

        audioRecord = AudioRecord(
            MediaRecorder.AudioSource.VOICE_COMMUNICATION,
            sampleRate,
            channelConfig,
            audioFormat,
            bufferSize
        )

        // Explicitly attach hardware filtering to match PWA capabilities
        val sessionId = audioRecord?.audioSessionId ?: 0
        if (sessionId != 0 && micProfile == "heavy") {
            if (NoiseSuppressor.isAvailable()) {
                noiseSuppressor = NoiseSuppressor.create(sessionId)
                noiseSuppressor?.enabled = true
            }
            if (AcousticEchoCanceler.isAvailable()) {
                echoCanceler = AcousticEchoCanceler.create(sessionId)
                echoCanceler?.enabled = true
            }
            if (AutomaticGainControl.isAvailable()) {
                autoGainControl = AutomaticGainControl.create(sessionId)
                autoGainControl?.enabled = true
            }
        }

        audioRecord?.startRecording()

        recordingThread = Thread {
            Process.setThreadPriority(Process.THREAD_PRIORITY_URGENT_AUDIO)
            val buffer = ShortArray(FRAME_SIZE)
            var isSpeaking = false
            var silenceFrames = 0
            val audioChunks = mutableListOf<ShortArray>()
            val preRollBuffer = mutableListOf<ShortArray>()

            while (!Thread.currentThread().isInterrupted) {
                // READ_BLOCKING ensures we always get exactly 4096 samples per loop, keeping time logic accurate
                val readResult = audioRecord?.read(buffer, 0, FRAME_SIZE, AudioRecord.READ_BLOCKING) ?: 0
                if (readResult > 0) {
                    
                    val isAiPlaying = audioTrack?.playState == AudioTrack.PLAYSTATE_PLAYING || audioQueue.isNotEmpty()
                    var targetDuckMultiplier = 1.0f
                    if (isAiPlaying) {
                        if (micProfile == "mute_playback") {
                            targetDuckMultiplier = 0.0f
                        } else if (micProfile == "adaptive") {
                            targetDuckMultiplier = Math.max(0.01f, Math.min(0.5f, (averageSpeechRms.toFloat() / 600.0f) * 0.1f))
                        } else {
                            targetDuckMultiplier = 0.1f
                        }
                    }

                    // Apply Digital Gain Boost
                    for (i in 0 until readResult) {
                        var sample = (buffer[i] * GAIN_MULTIPLIER * targetDuckMultiplier).toInt()
                        // Hard clip to prevent integer overflow distortion
                        if (sample > Short.MAX_VALUE) sample = Short.MAX_VALUE.toInt()
                        if (sample < Short.MIN_VALUE) sample = Short.MIN_VALUE.toInt()
                        buffer[i] = sample.toShort()
                    }

                    // Calculate RMS
                    var sum = 0.0
                    for (i in 0 until readResult) {
                        sum += buffer[i] * buffer[i]
                    }
                    val rms = sqrt(sum / readResult)

                    // Pipe volume back to UI for the visualizer
                    onVolumeChange(if (isMicMuted) 0f else rms.toFloat())

                    val currentThreshold = if (micProfile == "adaptive") Math.max(100.0, averageSpeechRms * 0.3) else noiseThreshold

                    if (isMicMuted) {
                        if (isSpeaking) {
                            isSpeaking = false
                            silenceFrames = 0
                            if (audioChunks.size >= MIN_CHUNKS) {
                                var sumSquares = 0.0
                                var totalSamples = 0
                                // Flatten short arrays to byte array for WebSocket
                                val byteBuffer = java.nio.ByteBuffer.allocate(audioChunks.sumOf { it.size } * 2)
                                byteBuffer.order(java.nio.ByteOrder.LITTLE_ENDIAN)
                                for (chunk in audioChunks) {
                                    for (sample in chunk) {
                                        byteBuffer.putShort(sample)
                                        sumSquares += sample.toDouble() * sample.toDouble()
                                        totalSamples++
                                    }
                                }
                                if (totalSamples > 0) {
                                    val chunkRms = sqrt(sumSquares / totalSamples)
                                    averageSpeechRms = (0.8 * averageSpeechRms) + (0.2 * chunkRms)
                                }
                                Log.d("AudioEngine", "Speech finalized due to mute, sending ${byteBuffer.capacity()} bytes")
                                onSpeechFinalized(byteBuffer.array(), speechStartTime)
                            }
                            audioChunks.clear()
                        }
                        preRollBuffer.add(buffer.copyOf(readResult))
                        if (preRollBuffer.size > PRE_ROLL_FRAMES) preRollBuffer.removeAt(0)
                        continue
                    }

                    if (rms > currentThreshold) {
                        if (!isSpeaking) {
                            Log.d("AudioEngine", "Speech detected! Suspending playback instantly.")
                            isSpeaking = true
                            speechStartTime = System.currentTimeMillis()
                            suspendPlayback() // Match PWA: Instant pause on interruption
                            onSpeechStart()
                            audioChunks.addAll(preRollBuffer) // Prepend pre-roll
                            preRollBuffer.clear() // Clear it so we don't accidentally duplicate on rapid VAD toggles
                        }
                        silenceFrames = 0
                        audioChunks.add(buffer.copyOf(readResult))
                    } else if (isSpeaking) {
                        audioChunks.add(buffer.copyOf(readResult)) // Keep trailing silence
                        silenceFrames++
                        if (silenceFrames >= SILENCE_FRAMES_LIMIT) {
                            isSpeaking = false
                            silenceFrames = 0
                            
                            if (audioChunks.size >= MIN_CHUNKS) {
                                var sumSquares = 0.0
                                var totalSamples = 0
                                // Flatten short arrays to byte array for WebSocket
                                val byteBuffer = java.nio.ByteBuffer.allocate(audioChunks.sumOf { it.size } * 2)
                                byteBuffer.order(java.nio.ByteOrder.LITTLE_ENDIAN)
                                for (chunk in audioChunks) {
                                    for (sample in chunk) {
                                        byteBuffer.putShort(sample)
                                        sumSquares += sample.toDouble() * sample.toDouble()
                                        totalSamples++
                                    }
                                }
                                if (totalSamples > 0) {
                                    val chunkRms = sqrt(sumSquares / totalSamples)
                                    averageSpeechRms = (0.8 * averageSpeechRms) + (0.2 * chunkRms)
                                }
                                Log.d("AudioEngine", "Speech finalized, sending ${byteBuffer.capacity()} bytes")
                                onSpeechFinalized(byteBuffer.array(), speechStartTime)
                            } else {
                                Log.d("AudioEngine", "Speech discarded (too short)")
                            }
                            audioChunks.clear()
                        }
                    } else {
                        // Not speaking, maintain rolling pre-roll
                        preRollBuffer.add(buffer.copyOf(readResult))
                        if (preRollBuffer.size > PRE_ROLL_FRAMES) {
                            preRollBuffer.removeAt(0)
                        }
                    }
                }
            }
        }
        recordingThread?.start()
    }

    fun stopRecording() {
        recordingThread?.interrupt()
        recordingThread = null
        audioRecord?.stop()
        audioRecord?.release()
        audioRecord = null

        noiseSuppressor?.release()
        noiseSuppressor = null
        
        echoCanceler?.release()
        echoCanceler = null
        
        autoGainControl?.release()
        autoGainControl = null
    }

    fun playAudioChunk(pcmData: ByteArray, pcmSampleRate: Int = 22050) {
        // Calculate frames from bytes (16-bit Mono = 2 bytes per frame)
        // We subtract 44 bytes if it's a WAV header so we don't count metadata as audio
        val isWav = pcmData.size >= 44 && pcmData[0] == 'R'.code.toByte() && pcmData[1] == 'I'.code.toByte()
        val audioBytes = if (isWav) pcmData.size - 44 else pcmData.size
        totalAiFrames += audioBytes / 2
        
        audioQueue.put(Pair(pcmData, pcmSampleRate)) // Instantly queues and returns, freeing the WebSocket thread!

        if (playbackThread == null || playbackThread?.isAlive != true) {
            playbackThread = Thread {
                Process.setThreadPriority(Process.THREAD_PRIORITY_URGENT_AUDIO)
                while (!Thread.currentThread().isInterrupted) {
                    try {
                        val chunk = audioQueue.take()
                        playChunkInternal(chunk.first, chunk.second)
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

                        // Pull synced RMS from queue based on hardware playback head!
                        var targetRms = -1f
                        var poppedAny = false
                        while (rmsQueue.isNotEmpty() && rmsQueue.peek()!!.first <= currentFrame) {
                            val popped = rmsQueue.poll()?.second ?: 0f
                            if (popped > targetRms) targetRms = popped // Find max transient in this 16ms window
                            poppedAny = true
                        }
                        if (poppedAny) {
                            onAiVolumeChange(targetRms)
                        } else if (rmsQueue.isEmpty() && currentFrame >= totalWrittenFrames) {
                            if (totalAiFrames > 0) { // Only fire if we actually played something
                                totalAiFrames = 0 // Resetting this acts as a latch so we only fire once per playback queue exhaustion
                                onAiVolumeChange(0f) // Audio finished, drop to 0
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

    private fun playChunkInternal(pcmData: ByteArray, chunkSampleRate: Int) {
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
            totalWrittenFrames = 0
            rmsQueue.clear()
            
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
                rmsQueue.add(Pair(totalWrittenFrames, rms)) // Queue RMS to its exact playback frame
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

    fun abortPlayback() {
        isPausedLocally = false
        audioQueue.clear() // Drop all pending TTS chunks
        // KILL the playback thread so it instantly drops the CURRENT chunk!
        playbackThread?.interrupt()
        playbackThread = null
        audioTrack?.pause()
        audioTrack?.flush()
        totalAiFrames = 0
        totalWrittenFrames = 0
        rmsQueue.clear()
        onBufferProgress(0f)
        onAiVolumeChange(0f)
        progressThread?.interrupt()
        progressThread = null
        // We don't release here so we can reuse the track for gapless streaming
    }
}
