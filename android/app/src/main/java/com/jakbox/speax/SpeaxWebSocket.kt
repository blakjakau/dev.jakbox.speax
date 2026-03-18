package com.jakbox.speax

import android.util.Log
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.GlobalScope
import kotlinx.coroutines.launch
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okio.ByteString
import okio.ByteString.Companion.toByteString
import kotlin.math.min

class SpeaxWebSocket(
    private val url: String,
    private val cookie: String, // e.g. speax_session=12345
    private val onTextReceived: (String) -> Unit,
    private val onAudioReceived: (ByteArray) -> Unit,
    private val onClosed: () -> Unit
) {
    private var webSocket: WebSocket? = null
    private val client = OkHttpClient()

    fun connect() {
        val request = Request.Builder()
            .url(url)
            .addHeader("Cookie", cookie)
            .build()

        webSocket = client.newWebSocket(request, object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                Log.d("SpeaxWS", "Connected!")
                webSocket.send("[REQUEST_SYNC]")
            }

            override fun onMessage(webSocket: WebSocket, text: String) {
                onTextReceived(text)
            }

            override fun onMessage(webSocket: WebSocket, bytes: ByteString) {
                // This is our Piper TTS streaming in!
                onAudioReceived(bytes.toByteArray())
            }

            override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                Log.d("SpeaxWS", "Closing: $reason")
                onClosed()
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                Log.e("SpeaxWS", "Error: ${t.message}")
                onClosed()
            }
        })
    }

    fun sendText(text: String) {
        GlobalScope.launch(Dispatchers.IO) {
            webSocket?.send(text)
        }
    }

    fun sendAudio(pcmData: ByteArray, startTime: Long) {
        GlobalScope.launch(Dispatchers.IO) {
            val timestampBytes = java.nio.ByteBuffer.allocate(8)
                .order(java.nio.ByteOrder.BIG_ENDIAN)
                .putLong(startTime)
                .array()
            val combined = timestampBytes + pcmData
            webSocket?.send(combined.toByteString())
        }
    }

    fun disconnect() {
        webSocket?.close(1000, "User disconnected")
        webSocket = null
    }
}
