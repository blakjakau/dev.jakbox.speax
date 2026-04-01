# Piper TTS Service API Documentation

High-performance, low-latency, gapless Text-to-Speech (TTS) service using the Piper engine. This service is designed for real-time applications requiring sentence-level streaming and synchronization.

- **Default Port**: `4410`
- **Audio Format**: Raw 16-bit Signed PCM, Mono, Little-Endian.
- **Sample Rate**: Dynamic (determined by the loaded `.onnx` model).

---

## 🚀 Core Endpoints

### 1. `GET /health`
Returns runtime metrics and engine heartbeat.
- **Response**: `200 OK`
```json
{
  "status": "ok",
  "uptime_seconds": 1240.5,
  "memory_usage_bytes": 15728640,
  "goroutines": 8,
  "cpu_usage_percent": 0.0
}
```

### 2. `GET /status`
Returns information about the active model and the internal hot-RAM cache.
- **Response**: `200 OK`
```json
{
  "active_model": "alan-medium.onnx",
  "cached_models": [
    { "name": "alan-medium.onnx", "size_bytes": 63201430, "last_used": "..." }
  ],
  "total_cache_size_estimate": 63201430
}
```

### 3. `POST /models`
Switches the active voice model. If the model is not in cache, it will be loaded from the `models/` directory.
- **Request Body**:
```json
{ "model": "maddy-medium.onnx" }
```

### 4. `POST /tts`
Standard HTTP synthesis. Blocks until the entire utterance is generated and returns a WAV file.
- **Request Body**:
```json
{ "text": "Hello world", "annotated": false }
```
- **Response**: `audio/wav` binary stream.

---

## 🌊 WebSocket Streaming Protocol (`/stream`)
The WebSocket endpoint is designed for maximum throughput and real-time UI synchronization.

### Connection Parameters
The client sends a JSON payload to initiate synthesis:
```json
{
  "text": "The quick brown fox. It jumps over the dog.",
  "annotated": true
}
```

### Server Packet Sequence
The server interleaves JSON metadata with raw Binary audio chunks.

1.  **`{"type": "start", "sampleRate": 22050}`**: (JSON) Sent first. Configures the client's audio clock.
2.  **`{"type": "text", "text": "The quick brown fox."}`**: (JSON) Sent before the binary chunks for this sentence.
3.  **`[Raw PCM Binary]`**: (Binary) Multiple packets containing the audio for the preceding text.
4.  **`{"type": "text", "text": "It jumps over the dog."}`**: (JSON) Next sentence metadata.
5.  **`[Raw PCM Binary]`**: (Binary) Audio for the second sentence.
6.  **`{"type": "end"}`**: (JSON) Sent once the entire request is flushed and finished.

> [!TIP]
> Use **Annotated Mode** (`annotated: true`) to implement real-time text highlighting. Store the `text` metadata in a queue and sync it with your audio playback head.

---

## 💻 Code Samples

### JavaScript (Browser)
```javascript
const socket = new WebSocket('ws://localhost:8079/stream');
socket.binaryType = 'arraybuffer';

socket.onmessage = (event) => {
    if (typeof event.data === 'string') {
        const meta = JSON.parse(event.data);
        if (meta.type === 'start') {
            console.log("Setting sample rate to:", meta.sampleRate);
        } else if (meta.type === 'text') {
            console.log("Next sentence coming:", meta.text);
        }
    } else {
        // Handle raw PCM binary data
        playBuffer(event.data);
    }
};

socket.onopen = () => {
    socket.send(JSON.stringify({ text: "Hello from Piper!", annotated: true }));
};
```

### Go (API Consumer)
```go
import "net/http"
import "bytes"
import "encoding/json"

func Synthesize(text string) {
    payload, _ := json.Marshal(map[string]string{"text": text})
    resp, err := http.Post("http://localhost:4410/tts", "application/json", bytes.NewBuffer(payload))
    if err == nil {
        // resp.Body is a standard WAV file
        saveToFile(resp.Body)
    }
}
```

### Kotlin (Android / OkHttp)
```kotlin
val request = Request.Builder().url("ws://localhost:4410/stream").build()
val client = OkHttpClient()

val listener = object : WebSocketListener() {
    override fun onMessage(webSocket: WebSocket, text: String) {
        val meta = JSONObject(text)
        if (meta.optString("type") == "start") {
             val sr = meta.getInt("sampleRate")
             // Init AudioTrack with sr
        }
    }

    override fun onMessage(webSocket: WebSocket, bytes: ByteString) {
        val pcmData = bytes.toByteArray()
        // Feed to AudioTrack.write()
    }
}
client.newWebSocket(request, listener)
```

---

## 🛠️ Deployment & Build
To run the service with its native dependencies:
```bash
# Set LD_LIBRARY_PATH to the local lib folder containing piper and onnxruntime libs
LD_LIBRARY_PATH=./lib ./piper-service -port 4410
```

**Resource Requirements**:
- ~200MB RAM base (plus ~60-150MB per cached model).
- CPU usage scales with synthesis speed; roughly 1 core per active stream.
