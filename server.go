package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"

	"github.com/gorilla/websocket"
	"strings"
)

const (
	whisperURL    = "http://localhost:8081/inference"
	sampleRate    = 16000
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer ws.Close()

	fmt.Println("Client connected!")

	for {
		messageType, p, err := ws.ReadMessage()
		if err != nil {
			return
		}

		if messageType == websocket.BinaryMessage {
			// Minimum byte length check (~0.5 seconds of 16-bit PCM is 16000 bytes)
			if len(p) < 16000 {
				continue
			}

			// Process the complete phrase sent by the client
			go func(audio []byte) {
				text, err := queryWhisper(audio)
				if err != nil {
					log.Println("Whisper error:", err)
					return
				}
				
				// Clean up Whisper's special tokens and hallucinations
				text = strings.ReplaceAll(text, "[BLANK_AUDIO]", "")
				text = strings.TrimSpace(text)

				if text != "" {
					if err := ws.WriteMessage(websocket.TextMessage, []byte(text)); err != nil {
						log.Println("Write error:", err)
					}
				}
			}(p)
		}
	}
}

func addWavHeader(pcmData []byte) []byte {
	buf := new(bytes.Buffer)
	// RIFF header
	buf.Write([]byte("RIFF"))
	binary.Write(buf, binary.LittleEndian, uint32(36+len(pcmData)))
	buf.Write([]byte("WAVE"))
	// fmt chunk
	buf.Write([]byte("fmt "))
	binary.Write(buf, binary.LittleEndian, uint32(16))
	binary.Write(buf, binary.LittleEndian, uint16(1))      // AudioFormat: PCM
	binary.Write(buf, binary.LittleEndian, uint16(1))      // NumChannels: Mono
	binary.Write(buf, binary.LittleEndian, uint32(16000))  // SampleRate: 16kHz
	binary.Write(buf, binary.LittleEndian, uint32(32000))  // ByteRate: SampleRate * NumChannels * BitsPerSample/8
	binary.Write(buf, binary.LittleEndian, uint16(2))      // BlockAlign: NumChannels * BitsPerSample/8
	binary.Write(buf, binary.LittleEndian, uint16(16))     // BitsPerSample: 16
	// data chunk
	buf.Write([]byte("data"))
	binary.Write(buf, binary.LittleEndian, uint32(len(pcmData)))
	buf.Write(pcmData)
	return buf.Bytes()
}

func queryWhisper(audioData []byte) (string, error) {
	if len(audioData) == 0 {
		return "", fmt.Errorf("empty audio data")
	}

	wavData := addWavHeader(audioData)
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="input.wav"`)
	h.Set("Content-Type", "audio/wav")
	part, _ := writer.CreatePart(h)
	part.Write(wavData)
	writer.Close()

	resp, err := http.Post(whisperURL, writer.FormDataContentType(), body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Text, nil
}

func main() {
	http.HandleFunc("/ws", handleConnections)
	http.Handle("/", http.FileServer(http.Dir("./public")))

	fmt.Println("Server started on :3000")
	err := http.ListenAndServe(":3000", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
