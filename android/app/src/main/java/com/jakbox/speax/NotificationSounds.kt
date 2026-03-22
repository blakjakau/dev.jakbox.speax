package com.jakbox.speax

import android.media.AudioFormat
import android.media.AudioManager
import android.media.AudioTrack
import kotlin.math.PI
import kotlin.math.sin

class NotificationSounds {
    private val sampleRate = 22050
    private val audioFormat = AudioFormat.ENCODING_PCM_16BIT
    private val channelConfig = AudioFormat.CHANNEL_OUT_MONO

    private val connectTone: ByteArray by lazy { generateTwoTone(440.0, 554.37, 0.1) } // A4 to C#5 (Major Third)
    private val disconnectTone: ByteArray by lazy { generateTwoTone(554.37, 440.0, 0.1) } // C#5 to A4 (Major Third)

    private fun generateTwoTone(freq1: Double, freq2: Double, durationPerTone: Double): ByteArray {
        val numSamples1 = (durationPerTone * sampleRate).toInt()
        val numSamples2 = (durationPerTone * sampleRate).toInt()
        val totalSamples = numSamples1 + numSamples2
        val generatedSnd = ByteArray(2 * totalSamples)
        
        val fadeSamples = (0.01 * sampleRate).toInt() // 10ms fade

        // Generate first tone
        for (i in 0 until numSamples1) {
            val sample = sin(2.0 * PI * i / (sampleRate / freq1))
            
            // Apply linear fade-in/out
            val fade = when {
                i < fadeSamples -> i.toDouble() / fadeSamples
                i > numSamples1 - fadeSamples -> (numSamples1 - i).toDouble() / fadeSamples
                else -> 1.0
            }
            
            val pcm = (sample * 8192 * fade).toInt().toShort() 
            generatedSnd[2 * i] = (pcm.toInt() and 0x00FF).toByte()
            generatedSnd[2 * i + 1] = ((pcm.toInt() and 0xFF00) shr 8).toByte()
        }

        // Generate second tone
        for (i in 0 until numSamples2) {
            val sample = sin(2.0 * PI * i / (sampleRate / freq2))
            
            val fade = when {
                i < fadeSamples -> i.toDouble() / fadeSamples
                i > numSamples2 - fadeSamples -> (numSamples2 - i).toDouble() / fadeSamples
                else -> 1.0
            }
            
            val pcm = (sample * 8192 * fade).toInt().toShort()
            val offset = numSamples1
            generatedSnd[2 * (offset + i)] = (pcm.toInt() and 0x00FF).toByte()
            generatedSnd[2 * (offset + i) + 1] = ((pcm.toInt() and 0xFF00) shr 8).toByte()
        }

        return generatedSnd
    }

    fun playConnect() {
        playTone(connectTone)
    }

    fun playDisconnect() {
        playTone(disconnectTone)
    }

    private fun playTone(tone: ByteArray) {
        val minBufferSize = AudioTrack.getMinBufferSize(sampleRate, channelConfig, audioFormat)
        val audioTrack = AudioTrack.Builder()
            .setAudioAttributes(
                android.media.AudioAttributes.Builder()
                    .setUsage(android.media.AudioAttributes.USAGE_VOICE_COMMUNICATION)
                    .setContentType(android.media.AudioAttributes.CONTENT_TYPE_SONIFICATION)
                    .build()
            )
            .setAudioFormat(
                AudioFormat.Builder()
                    .setEncoding(audioFormat)
                    .setSampleRate(sampleRate)
                    .setChannelMask(channelConfig)
                    .build()
            )
            .setBufferSizeInBytes(tone.size.coerceAtLeast(minBufferSize))
            .setTransferMode(AudioTrack.MODE_STATIC)
            .build()

        audioTrack.write(tone, 0, tone.size)
        audioTrack.play()
        
        // Cleanup after playback
        Thread {
            try {
                // Wait for the duration of the tone + small buffer
                Thread.sleep((tone.size / (sampleRate * 2) * 1000L) + 500)
                audioTrack.stop()
                audioTrack.release()
            } catch (e: Exception) {
                e.printStackTrace()
            }
        }.start()
    }
}
