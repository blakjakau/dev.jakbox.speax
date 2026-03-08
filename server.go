package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	//"net/url"

	"github.com/gorilla/websocket"
	"os/exec"
	"strings"
	"sync"
)

const (
	whisperURL    = "http://localhost:8081/inference"
	piperBin      = "./piper/piper"                     // Path to the piper executable
	piperModel    = "./piper/en_GB-cori-medium.onnx" // Path to the voice model
	//en_GB-cori-medium.onnx
	//en_GB-southern_english_female-low.onnx
	sampleRate    = 16000
	ollamaURL     = "http://localhost:11434/api/generate"
	ollamaChatURL = "http://localhost:11434/api/chat"
	ollamaModel   = "gemma3n:e2b" // Change this if you pulled a different model!
	
	// gemma3n:e2b
	// gemma3:1b-it-qat
	// gemma3:4b-it-qat
)

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ClientSession struct {
	History []ChatMessage
	Archive []ChatMessage
	Summary string
	Mutex   sync.Mutex
}

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

	session := &ClientSession{}
	var activeCancel context.CancelFunc

	for {
		messageType, p, err := ws.ReadMessage()
		if err != nil {
			return
		}

		// Handle incoming text (TTS Request)
		if messageType == websocket.TextMessage {
			text := string(p)

			if text == "[INTERRUPT]" {
				if activeCancel != nil {
					activeCancel()
					activeCancel = nil
				}
				continue
			}

			go func(t string) {
				audioBytes, err := queryTTS(t)
				if err != nil {
					log.Println("TTS error:", err)
					return
				}
				ws.WriteMessage(websocket.BinaryMessage, audioBytes)
			}(text)
		}

		// Handle incoming audio (STT Request)
		if messageType == websocket.BinaryMessage {
			// Minimum byte length check (~0.5 seconds of 16-bit PCM is 16000 bytes)
			if len(p) < 16000 {
				continue
			}

			if activeCancel != nil {
				activeCancel()
			}
			ctx, cancel := context.WithCancel(context.Background())
			activeCancel = cancel

			// Process the complete phrase sent by the client
			go func(audio []byte, reqCtx context.Context) {
				text, err := queryWhisper(audio)
				if err != nil {
					log.Println("Whisper error:", err)
					return
				}
				
				// Clean up Whisper's special tokens and hallucinations
				text = strings.ReplaceAll(text, "[BLANK_AUDIO]", "")
				text = strings.TrimSpace(text)

				if text != "" {
					// 1. Send the user's transcription to the browser
					if err := ws.WriteMessage(websocket.TextMessage, []byte(text)); err != nil {
						log.Println("Write error:", err)
					}

					// 2. Stream Ollama and TTS responses to the browser
					ws.WriteMessage(websocket.TextMessage, []byte("[AI_START]"))
					if err := streamOllamaAndTTS(reqCtx, text, ws, session); err != nil {
						log.Println("Ollama stream error:", err)
					}
					ws.WriteMessage(websocket.TextMessage, []byte("[AI_END]"))
				}
			}(p, ctx)
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

func streamOllamaAndTTS(ctx context.Context, prompt string, ws *websocket.Conn, session *ClientSession) error {
	session.Mutex.Lock()
	sysContent := "You are 'Alex' a highly capable, voice-based AI assistant. Your output is sent directly to a Text-to-Speech engine. You MUST strictly follow these rules: 1. NEVER use emojis. 3. Keep responses conversational, concise, and easy to listen to."
	
	if session.Summary != "" {
		sysContent += "\n\nContext from earlier in the conversation: " + session.Summary
	}

	messages := []ChatMessage{
		{Role: "system", Content: sysContent},
	}
	messages = append(messages, session.History...)
	messages = append(messages, ChatMessage{Role: "user", Content: prompt})
	session.Mutex.Unlock()

	payload := map[string]interface{}{
		"model":    ollamaModel,
		"messages": messages,
		"stream":   true,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", ollamaChatURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	var sentence strings.Builder
	var fullResponse strings.Builder

	for {
		select {
		case <-ctx.Done():
			return ctx.Err() // Abort processing if interrupted
		default:
		}

		var result struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Done bool `json:"done"`
		}
		if err := decoder.Decode(&result); err != nil {
			break
		}

		content := result.Message.Content

		// Stream text token directly to UI
		ws.WriteMessage(websocket.TextMessage, []byte(content))

		// Buffer token for TTS processing
		sentence.WriteString(content)
		fullResponse.WriteString(content)
		
		// Trigger TTS on paragraph breaks (newlines), or if it's done.
		// Fallback: Also trigger on sentence boundaries IF the paragraph is getting extremely long (>250 chars) to prevent huge latency spikes.
		isNewline := strings.Contains(content, "\n")
		isLongSentenceBoundary := sentence.Len() > 250 && strings.ContainsAny(content, ".!?")

		if isNewline || isLongSentenceBoundary || result.Done {
			cleanChunk := strings.TrimSpace(sentence.String())
			if len(cleanChunk) > 0 {
				// Strip markdown punctuation so Piper doesn't read it aloud
				ttsText := cleanChunk
				ttsText = strings.ReplaceAll(ttsText, ":**", ".")
				ttsText = strings.ReplaceAll(ttsText, "*", "")
				ttsText = strings.ReplaceAll(ttsText, "#", "")
				ttsText = strings.ReplaceAll(ttsText, "_", "")
				ttsText = strings.ReplaceAll(ttsText, "`", "")
				
				if audioBytes, err := queryTTS(ttsText); err == nil {
					ws.WriteMessage(websocket.BinaryMessage, audioBytes)
				}
			}
			sentence.Reset()
		}

		if result.Done {
			break
		}
	}

	// If not interrupted, save to history and trigger background summary if needed
	if ctx.Err() == nil {
		session.Mutex.Lock()
		session.History = append(session.History, ChatMessage{Role: "user", Content: prompt})
		session.History = append(session.History, ChatMessage{Role: "assistant", Content: fullResponse.String()})

		if len(session.History) > 14 { // 7 interactions
			toSummarize := make([]ChatMessage, 6) // Slice off oldest 3 interactions
			copy(toSummarize, session.History[:6])

			session.Archive = append(session.Archive, toSummarize...)
			session.History = session.History[6:]

			go generateSummaryAsync(toSummarize, session)
		}
		session.Mutex.Unlock()
	}

	return nil
}

func generateSummaryAsync(messages []ChatMessage, session *ClientSession) {
	var transcript strings.Builder
	for _, msg := range messages {
		transcript.WriteString(fmt.Sprintf("%s: %s\n", msg.Role, msg.Content))
	}

	session.Mutex.Lock()
	prevSummary := session.Summary
	session.Mutex.Unlock()

	prompt := "Summarize the following conversation concisely. "
	if prevSummary != "" {
		prompt += "Incorporate this previous summary context: " + prevSummary + "\n\n"
	}
	prompt += "New conversation to add to summary:\n" + transcript.String() + "\n\nProvide ONLY the concise summary text."

	payload := map[string]interface{}{
		"model":  ollamaModel,
		"prompt": prompt,
		"stream": false, // Synchronous request for the background worker
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(ollamaURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Println("Summary error:", err)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil {
		session.Mutex.Lock()
		session.Summary = strings.TrimSpace(result.Response)
		log.Println("--- Context Memory Summarized for client ---")
		session.Mutex.Unlock()
	}
}

func queryTTS(text string) ([]byte, error) {
	// Execute piper binary: -f - tells it to output the WAV file directly to standard output
	cmd := exec.Command(piperBin, "--model", piperModel, "-f", "-")
	
	// Feed our text into Piper's standard input
	cmd.Stdin = strings.NewReader(text)
	
	// Capture the raw WAV audio from Piper's standard output
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("piper execution failed: %v, stderr: %s", err, stderr.String())
	}

	return out.Bytes(), nil
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
