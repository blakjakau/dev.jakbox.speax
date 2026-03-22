package com.jakbox.speax

import java.nio.ByteBuffer

class NativeAudioEngine(
    private val onSpeechStart: () -> Unit,
    private val onStreamingChunk: (ByteBuffer, Int, Byte) -> Unit,
    private val onSpeechEnd: (ByteBuffer, Int) -> Unit,
    private val onVolumeChange: (Float) -> Unit
) {
    companion object {
        init {
            System.loadLibrary("native-audio")
        }
    }

    private var nativePtr: Long = 0

    init {
        nativePtr = create()
    }

    private external fun create(): Long
    private external fun destroy(ptr: Long)

    external fun startRecording(ptr: Long): Int
    external fun stopRecording(ptr: Long)
    external fun forceEndStreaming(ptr: Long)
    external fun setMicProfile(ptr: Long, profile: String)
    external fun setThreshold(ptr: Long, threshold: Double)
    external fun setMuted(ptr: Long, muted: Boolean)

    // Bridge for startRecording without passing ptr every time
    fun start() = startRecording(nativePtr)
    fun stop() = stopRecording(nativePtr)
    fun forceEndStreaming() = forceEndStreaming(nativePtr)
    fun setProfile(profile: String) = setMicProfile(nativePtr, profile)
    fun setThreshold(threshold: Double) = setThreshold(nativePtr, threshold)
    fun setMuted(muted: Boolean) = setMuted(nativePtr, muted)

    // This will be called by native code
    private fun handleSpeechStart() {
        onSpeechStart()
    }

    private fun handleStreamingChunk(buffer: ByteBuffer, size: Int, type: Byte) {
        onStreamingChunk(buffer, size, type)
    }

    private fun handleSpeechEnd(buffer: ByteBuffer, size: Int) {
        onSpeechEnd(buffer, size)
    }

    private fun handleVolumeChange(rms: Float) {
        onVolumeChange(rms)
    }

    fun release() {
        if (nativePtr != 0L) {
            destroy(nativePtr)
            nativePtr = 0
        }
    }
}
