package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"os"
	"path/filepath"
	"time"
	//"net/url"

	"github.com/gorilla/websocket"
	"os/exec"
	"strings"
	"sync"
)

const (
	// whisperURL    = "http://192.168.1.21:8081/inference"
	whisperURL    = "http://localhost:8081/inference"

	piperBin      = "./piper/piper"                     // Path to the piper executable
	defaultVoice  = "en_GB-alba-tiny.onnx"            // Default fallback
	sampleRate    = 16000
	
	ollamaURL     = "http://localhost:11434/api/generate"
	ollamaChatURL = "http://localhost:11434/api/chat"
	// ollamaModel   = "gemma3n:e2b" // Change this if you pulled a different model!

	// ollamaURL     = "http://192.168.1.21:11434/api/generate"
	// ollamaChatURL = "http://192.168.1.21:11434/api/chat"
	ollamaModel   = "gemma3:1b-it-qat" //""llama3.2:1b" // Change this if you pulled a different model!
	
	// llama3.2:1b
	// llama3.2:3b
	// gemma3:1b-it-qat
	// gemma3:4b-it-qat
	// mistral-nemo:12b
	// gemma3n:e2b
	// gemma3:270m
)

var (
	googleClientID     string
	googleClientSecret string
)

func init() {
	data, err := os.ReadFile("google-client-secret.json")
	if err == nil {
		var creds struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			Web          *struct {
				ClientID     string `json:"client_id"`
				ClientSecret string `json:"client_secret"`
			} `json:"web"`
		}
		if err := json.Unmarshal(data, &creds); err == nil {
			if creds.Web != nil {
				googleClientID, googleClientSecret = creds.Web.ClientID, creds.Web.ClientSecret
			} else {
				googleClientID, googleClientSecret = creds.ClientID, creds.ClientSecret
			}
			log.Println("Loaded Google OAuth credentials from google-client-secret.json")
		}
	}

	if googleClientID == "" {
		googleClientID = os.Getenv("GOOGLE_CLIENT_ID")
		googleClientSecret = os.Getenv("GOOGLE_CLIENT_SECRET")
		if googleClientID != "" && googleClientSecret != "" {
			log.Println("Loaded Google OAuth credentials from environment variables")
		}		
	}

	if googleClientID == "" || googleClientSecret == "" {
		log.Fatal("FATAL: Missing Google OAuth credentials. Provide google-client-secret.json or launch using:\n\nGOOGLE_CLIENT_ID=\"your_id\" GOOGLE_CLIENT_SECRET=\"your_secret\" go run server.go\n")
	}
}

var (
	activeSessions      = make(map[string]*ClientSession)
	activeSessionsMutex sync.Mutex
)

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Thread struct {
	ID      string        `json:"id"`
	Name    string        `json:"name"`
	History []ChatMessage `json:"history"`
	Archive []ChatMessage `json:"archive"`
	Summary string        `json:"summary"`
}

type ClientSession struct {
	ClientID string        `json:"-"`
	
	Threads        map[string]*Thread `json:"threads"`
	ActiveThreadID string             `json:"activeThreadId"`
	
	// Legacy fields (kept for automatic migration on load)
	History  []ChatMessage `json:"history,omitempty"`
	Archive  []ChatMessage `json:"archive,omitempty"`
	Summary  string        `json:"summary,omitempty"`

	UserName string        `json:"userName"`
	GoogleName string      `json:"googleName"`
	UserBio  string        `json:"userBio"`
	Provider string        `json:"provider"`
	APIKey   string        `json:"apiKey"`
	Model    string        `json:"model"`
	Voice    string        `json:"voice"`
	Mutex    sync.Mutex    `json:"-"`
	ClientStorage bool     `json:"clientStorage"`
	ClientTts    bool      `json:"clientTts"`
	ConnMutex sync.Mutex   `json:"-"` // Dedicated lock for WebSocket writes
	TokenUsage map[string]int64 `json:"tokenUsage"`
	Conns map[*websocket.Conn]bool `json:"-"`
}

func (s *ClientSession) ActiveThread() *Thread {
	if s.ActiveThreadID == "" {
		for id := range s.Threads {
			s.ActiveThreadID = id
			break
		}
	}
	if t, ok := s.Threads[s.ActiveThreadID]; ok {
		return t
	}
	// Failsafe fallback
	t := &Thread{ID: "default", Name: "General Chat"}
	if s.Threads == nil {
		s.Threads = make(map[string]*Thread)
	}
	s.Threads["default"] = t
	s.ActiveThreadID = "default"
	return t
}

func safeWrite(ws *websocket.Conn, session *ClientSession, msgType int, data []byte) error {
	if ws == nil { return nil }
	session.ConnMutex.Lock()
	defer session.ConnMutex.Unlock()
	return ws.WriteMessage(msgType, data)
}

func sendOrBroadcastText(ws *websocket.Conn, session *ClientSession, data []byte) {
	session.ConnMutex.Lock()
	defer session.ConnMutex.Unlock()
	if ws != nil {
		ws.WriteMessage(websocket.TextMessage, data)
	} else {
		for conn := range session.Conns {
			conn.WriteMessage(websocket.TextMessage, data)
		}
	}
}

func getContextPath(clientID string) string {
	safeID := strings.ReplaceAll(clientID, "/", "")
	safeID = strings.ReplaceAll(safeID, "\\", "")
	safeID = strings.ReplaceAll(safeID, "..", "")
	return filepath.Join(".", "context", safeID+".json")
}

func loadSession(clientID string) *ClientSession {
	session := &ClientSession{ClientID: clientID, Threads: make(map[string]*Thread)}
	data, err := os.ReadFile(getContextPath(clientID))
	if err == nil {
		json.Unmarshal(data, session)
	}
	
	// Seamless migration of legacy flat structure into Thread structure
	if len(session.Threads) == 0 {
		t := &Thread{
			ID: "default", Name: "General Chat",
			History: session.History, Archive: session.Archive, Summary: session.Summary,
		}
		session.Threads["default"] = t
		session.ActiveThreadID = "default"
		session.History = nil
		session.Archive = nil
		session.Summary = ""
	}
	return session
}

func getOrCreateSession(clientID string) *ClientSession {
	activeSessionsMutex.Lock()
	defer activeSessionsMutex.Unlock()
	if s, exists := activeSessions[clientID]; exists {
		return s
	}
	s := loadSession(clientID)
	if s.Conns == nil {
		s.Conns = make(map[*websocket.Conn]bool)
	}
	activeSessions[clientID] = s
	return s
}

func saveSession(session *ClientSession) {
	session.Mutex.Lock()
	defer session.Mutex.Unlock()
	if session.ClientStorage {
		os.Remove(getContextPath(session.ClientID))
		return // Ephemeral mode: do not save to disk
	}
	if data, err := json.MarshalIndent(session, "", "  "); err == nil {
		os.MkdirAll(filepath.Join(".", "context"), 0755)
		os.WriteFile(getContextPath(session.ClientID), data, 0644)
	}
}

func trackTokens(session *ClientSession, key string, tokens int64) {
	if tokens <= 0 { return }
	session.Mutex.Lock()
	defer session.Mutex.Unlock()
	if session.TokenUsage == nil {
		session.TokenUsage = make(map[string]int64)
	}
	if key == "" { key = "default" }
	session.TokenUsage[key] += tokens
}

func sendHistory(ws *websocket.Conn, session *ClientSession) {
	session.Mutex.Lock()
	t := session.ActiveThread()
	payload := map[string]interface{}{
		"history": t.History,
		"archive": t.Archive,
	}
	historyJSON, err := json.Marshal(payload)
	session.Mutex.Unlock()
	if err == nil {
		sendOrBroadcastText(ws, session, []byte("[HISTORY]"+string(historyJSON)))
	}
}

func sendSummary(ws *websocket.Conn, session *ClientSession) {
	session.Mutex.Lock()
	t := session.ActiveThread()
	summary := t.Summary
	archiveTurns := len(t.Archive) / 2

	// Rough token estimate: 1 token ~= 4 chars
	chars := len(summary)
	for _, msg := range t.History {
		chars += len(msg.Content)
	}
	estTokens := chars / 4

	maxTokens := 8192 // Default Ollama visual scale
	if session.Provider == "gemini" {
		maxTokens = 1000000 // Gemini 1.5 Flash scale
	}

	payload, err := json.Marshal(map[string]interface{}{
		"text":            summary,
		"archiveTurns":    archiveTurns,
		"maxArchiveTurns": 100, // 200 messages / 2
		"estTokens":       estTokens,
		"maxTokens":       maxTokens,
	})
	session.Mutex.Unlock()
	if err == nil {
		sendOrBroadcastText(ws, session, []byte("[SUMMARY]"+string(payload)))
	}
}

func sendSettings(ws *websocket.Conn, session *ClientSession) {
	session.Mutex.Lock()
	settingsJSON, err := json.Marshal(map[string]interface{}{
		"userName": session.UserName,
		"googleName": session.GoogleName,
		"userBio":  session.UserBio,
		"provider": session.Provider,
		"apiKey":   session.APIKey,
		"model":    session.Model,
		"voice":    session.Voice,
		"clientStorage": session.ClientStorage,
		"clientTts": session.ClientTts,
		"tokenUsage": session.TokenUsage,
	})
	session.Mutex.Unlock()
	if err == nil {
		sendOrBroadcastText(ws, session, []byte("[SETTINGS_SYNC]"+string(settingsJSON)))
	}
}

func sendThreads(ws *websocket.Conn, session *ClientSession) {
	session.Mutex.Lock()
	type threadInfo struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	list := make([]threadInfo, 0) // Explicitly initialize so it marshals to [] instead of null
	for _, t := range session.Threads {
		list = append(list, threadInfo{ID: t.ID, Name: t.Name})
	}
	payload := map[string]interface{}{"activeId": session.ActiveThreadID, "threads": list}
	data, err := json.Marshal(payload)
	session.Mutex.Unlock()
	if err == nil {
		sendOrBroadcastText(ws, session, []byte("[THREADS_SYNC]"+string(data)))
	}
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

	cookie, err := r.Cookie("speax_session")
	if err != nil || cookie.Value == "" {
		log.Println("Unauthorized WS connection attempt")
		return
	}
	clientID := cookie.Value
	clientType := r.URL.Query().Get("client")
	if clientType == "" {
		clientType = "unknown"
	}
	deviceName, _ := url.QueryUnescape(r.URL.Query().Get("device"))
	if deviceName == "" {
		deviceName = "device"
	}
	fmt.Printf("Client connected: %s (%s on %s)\n", clientID, clientType, deviceName)

	session := getOrCreateSession(clientID)

	session.ConnMutex.Lock()
	session.Conns[ws] = true
	session.ConnMutex.Unlock()

	// Ephemerally store the Google name from the cookie for this session only.
	// This allows the LLM prompt to use it as a fallback if no custom name is set.
	googleNameCookie, err := r.Cookie("speax_google_name")
	if err == nil {
		googleName, _ := url.QueryUnescape(googleNameCookie.Value)
		session.GoogleName = googleName
	}

	var activeCancel context.CancelFunc

	session.Mutex.Lock()
	connectMsg := fmt.Sprintf("[System Note: User connected at %s]", time.Now().Format("Monday, January 2, 2006, 15:04 MST"))
	session.ActiveThread().History = append(session.ActiveThread().History, ChatMessage{Role: "system", Content: connectMsg})
	session.Mutex.Unlock()
	saveSession(session)

	defer func() {
		session.ConnMutex.Lock()
		delete(session.Conns, ws)
		session.ConnMutex.Unlock()

		session.Mutex.Lock()
		t := session.ActiveThread()
		lastIdx := len(t.History) - 1
		if lastIdx >= 0 && strings.HasPrefix(t.History[lastIdx].Content, "[System Note: User connected at") {
			// No actual turns were generated, so expunge the connection timestamp
			t.History = t.History[:lastIdx]
		} else {
			disconnectMsg := fmt.Sprintf("[System Note: User disconnected at %s]", time.Now().Format("Monday, January 2, 2006, 15:04 MST"))
			t.History = append(t.History, ChatMessage{Role: "system", Content: disconnectMsg})
		}
		session.Mutex.Unlock()
		saveSession(session)
		ws.Close()
	}()

	for {
		messageType, p, err := ws.ReadMessage()
		if err != nil {
			return
		}

		// Handle incoming text (TTS Request)
		if messageType == websocket.TextMessage {
			text := string(p)

			if text == "[REQUEST_SYNC]" {
				sendThreads(ws, session)
				sendSettings(ws, session)
				sendHistory(ws, session)
				sendSummary(ws, session)
				continue
			}

			if text == "[REQUEST_FULL_EXPORT]" {
				session.Mutex.Lock()
				payload := map[string]interface{}{
					"activeId": session.ActiveThreadID,
					"threads":  session.Threads,
				}
				data, err := json.Marshal(payload)
				session.Mutex.Unlock()
				if err == nil {
					safeWrite(ws, session, websocket.TextMessage, []byte("[FULL_EXPORT]"+string(data)))
				}
				continue
			}

			if text == "[INTERRUPT]" {
				if activeCancel != nil {
					activeCancel()
					activeCancel = nil
				}
				continue
			}

			if text == "[CLEAR_HISTORY]" {
				session.Mutex.Lock()
				t := session.ActiveThread()
				t.History = []ChatMessage{}
				t.Archive = []ChatMessage{}
				t.Summary = ""
				session.Mutex.Unlock()
				saveSession(session)
				sendHistory(ws, session)
				sendSummary(ws, session)
				continue
			}

			if text == "[REBUILD_SUMMARY]" {
				go rebuildSummaryAsync(session)
				continue
			}

			if strings.HasPrefix(text, "[NEW_THREAD]:") {
				name := strings.TrimPrefix(text, "[NEW_THREAD]:")
				if name == "" { name = "New Thread" }
				id := fmt.Sprintf("%d", time.Now().UnixNano())
				session.Mutex.Lock()
				session.Threads[id] = &Thread{ID: id, Name: name}
				session.ActiveThreadID = id
				session.Mutex.Unlock()
				saveSession(session)
				sendThreads(nil, session)
				sendHistory(nil, session)
				sendSummary(nil, session)
				continue
			}

			if strings.HasPrefix(text, "[SWITCH_THREAD]:") {
				id := strings.TrimPrefix(text, "[SWITCH_THREAD]:")
				session.Mutex.Lock()
				changed := false
				if _, exists := session.Threads[id]; exists && session.ActiveThreadID != id {
					session.ActiveThreadID = id
					changed = true
				}
				session.Mutex.Unlock()
				if changed {
					saveSession(session)
					sendThreads(nil, session)
					sendHistory(nil, session)
					sendSummary(nil, session)
				}
				continue
			}

			if strings.HasPrefix(text, "[RENAME_THREAD]:") {
				name := strings.TrimPrefix(text, "[RENAME_THREAD]:")
				session.Mutex.Lock()
				if t := session.ActiveThread(); t != nil {
					t.Name = name
				}
				session.Mutex.Unlock()
				saveSession(session)
				sendThreads(nil, session)
				continue
			}

			if strings.HasPrefix(text, "[DELETE_THREAD]:") {
				id := strings.TrimPrefix(text, "[DELETE_THREAD]:")
				session.Mutex.Lock()
				delete(session.Threads, id)
				if session.ActiveThreadID == id || len(session.Threads) == 0 {
					session.ActiveThreadID = ""
					session.ActiveThread() // Forces default fallback logic
				}
				session.Mutex.Unlock()
				saveSession(session)
				sendThreads(nil, session)
				sendHistory(nil, session)
				sendSummary(nil, session)
				continue
			}

			if strings.HasPrefix(text, "[RESTORE_CLIENT_THREADS]") {
				var payload struct {
					ActiveId string             `json:"activeId"`
					Threads  map[string]*Thread `json:"threads"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(text, "[RESTORE_CLIENT_THREADS]")), &payload); err == nil && len(payload.Threads) > 0 {
					session.Mutex.Lock()
					session.ActiveThreadID = payload.ActiveId
					session.Threads = payload.Threads
					session.Mutex.Unlock()
					sendThreads(nil, session)
					sendHistory(nil, session)
					sendSummary(nil, session)
				} else {
					sendThreads(nil, session)
					sendHistory(nil, session)
					sendSummary(nil, session)
				}
				continue
			}

			if strings.HasPrefix(text, "[SETTINGS]") {
				var settings struct {
					UserName string `json:"userName"`
					GoogleName string `json:"googleName"`
					UserBio  string `json:"userBio"`
					Provider string `json:"provider"`
					APIKey   string `json:"apiKey"`
					Model    string `json:"model"`
					Voice    string `json:"voice"`
					ClientStorage bool `json:"clientStorage"`
					ClientTts    bool `json:"clientTts"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(text, "[SETTINGS]")), &settings); err == nil {
					session.Mutex.Lock()
					changed := session.UserName != settings.UserName ||
						session.GoogleName != settings.GoogleName ||
						session.UserBio != settings.UserBio ||
						session.Provider != settings.Provider ||
						session.APIKey != settings.APIKey ||
						session.Model != settings.Model ||
						session.Voice != settings.Voice ||
						session.ClientStorage != settings.ClientStorage ||
						session.ClientTts != settings.ClientTts
					if changed {
						session.UserName = settings.UserName
						session.UserBio = settings.UserBio
						session.Provider = settings.Provider
						session.APIKey = settings.APIKey
						session.Model = settings.Model
						session.Voice = settings.Voice
						session.GoogleName = settings.GoogleName
						session.ClientStorage = settings.ClientStorage
						session.ClientTts = settings.ClientTts
					}
					session.Mutex.Unlock()
					if changed {
						log.Printf("Updated settings for client %s: User=%s, Provider=%s, Model=%s, Voice=%s", session.ClientID, session.UserName, session.Provider, session.Model, session.Voice)
						saveSession(session)
						sendSettings(nil, session)
					}
				}
				continue
			}

			if strings.HasPrefix(text, "[TEXT_PROMPT]:") {
				prompt := strings.TrimSpace(strings.TrimPrefix(text, "[TEXT_PROMPT]:"))
				if prompt != "" {
					go func(p string) {
						session.Mutex.Lock()
						if activeCancel != nil {
							activeCancel()
						}
						ctx, cancel := context.WithCancel(context.Background())
						activeCancel = cancel
						session.Mutex.Unlock()

						// Echo it back to client so it appears in chat immediately
						sendOrBroadcastText(nil, session, []byte(p))
						sendOrBroadcastText(nil, session, []byte("[AI_START]"))
						if err := streamLLMAndTTS(ctx, p, ws, session); err != nil {
							log.Println("LLM stream error:", err)
						}
						log.Println("[LLM] Stream complete (Text Prompt).")
						sendOrBroadcastText(nil, session, []byte("[AI_END]"))
						sendHistory(nil, session)
					}(prompt)
				}
				continue
			}

			if strings.HasPrefix(text, "[DELETE_MSG]:") {
				idx, err := strconv.Atoi(strings.TrimPrefix(text, "[DELETE_MSG]:"))
				if err == nil {
					session.Mutex.Lock()
					t := session.ActiveThread()
					archiveLen := len(t.Archive)
					isArchiveDelete := false
					
					if idx >= 0 && idx < archiveLen {
						// Delete the target message AND the one immediately after it (if it exists)
						endIdx := idx + 2
						if endIdx > len(t.Archive) { endIdx = len(t.Archive) }
						t.Archive = append(t.Archive[:idx], t.Archive[endIdx:]...)
						isArchiveDelete = true
					} else if idx >= archiveLen && idx < archiveLen+len(t.History) {
						hIdx := idx - archiveLen
						// Delete the target message AND the one immediately after it
						endIdx := hIdx + 2
						if endIdx > len(t.History) { endIdx = len(t.History) }
						t.History = append(t.History[:hIdx], t.History[endIdx:]...)
					}
					session.Mutex.Unlock()
					saveSession(session)
					sendHistory(nil, session) // re-sync UI
					if isArchiveDelete {
						go rebuildSummaryAsync(session)
					}
				}
				continue
			}

			go func(t string) {
				session.Mutex.Lock()
				v := session.Voice
				session.Mutex.Unlock()
				if v == "" { v = defaultVoice }
				audioBytes, err := queryTTS(t, v)
				if err != nil {
					log.Println("TTS error:", err)
					return
				}
				safeWrite(ws, session, websocket.BinaryMessage, audioBytes)
			}(text)
		}

		// Handle incoming audio (STT Request)
		if messageType == websocket.BinaryMessage {
			// Minimum byte length check (~0.5 seconds of 16-bit PCM is 16000 bytes)
			if len(p) < 16000 {
				log.Printf("[STT] Ignored short audio chunk: %d bytes", len(p))
				continue
			}

			// Process the complete phrase sent by the client
			go func(audio []byte) {
				text, err := queryWhisper(audio)
				if err != nil {
					log.Println("Whisper error:", err)
					return
				}
				log.Printf("[STT] Whisper Transcribed: '%s'", text)
				
				text = strings.TrimSpace(strings.ReplaceAll(text, "[BLANK_AUDIO]", ""))

				// Filter out Whisper artifacts like [ "..." ] or ( music )
				isNonResponse := (strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]")) ||
					(strings.HasPrefix(text, "(") && strings.HasSuffix(text, ")"))
				
				// Aggressively suppress common hallucinated "polite" closers that trigger mid-conversation
				cleanerText := strings.ToLower(strings.Trim(text, ".,! "))
				isHallucination := cleanerText == "thank you" || 
								   cleanerText == "thank you." || 
								   cleanerText == "thank you.\nthank you." || 
								   cleanerText == "thank you.\nthank you" || 
								   cleanerText == "thank you. thank you" || 
								   cleanerText == "bye" || 
								   cleanerText == "bye." || 
								   cleanerText == "goodbye" || 
								   cleanerText == "goodbye."

				if isHallucination { isNonResponse = true }

				if isNonResponse {
					clientText := strings.Replace(text, "[", "(", 1)
					clientText = strings.Replace(clientText, "]", ")", 1)
					log.Printf("[STT] Filtered Whisper artifact, sending to client: %s", clientText)
					// Inform client of the filtered non-response, which some clients use
					// as a signal to resume paused TTS. This does NOT go to the LLM.
					safeWrite(ws, session, websocket.TextMessage, []byte(clientText))
				} else if text != "" {
					session.Mutex.Lock()
					if activeCancel != nil {
						activeCancel()
					}
					ctx, cancel := context.WithCancel(context.Background())
					activeCancel = cancel
					session.Mutex.Unlock()

					sendOrBroadcastText(nil, session, []byte(text))

					sendOrBroadcastText(nil, session, []byte("[AI_START]"))
					if err := streamLLMAndTTS(ctx, text, ws, session); err != nil {
						log.Println("Ollama stream error:", err)
					}
					log.Println("[LLM] Stream complete.")
					sendOrBroadcastText(nil, session, []byte("[AI_END]"))
					sendHistory(nil, session) // Sync finalized history back to client
				} else {
					safeWrite(ws, session, websocket.TextMessage, []byte("[IGNORED]"))
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

func extractVoiceName(filename string) string {
	base := strings.TrimSuffix(filename, ".onnx")
	parts := strings.Split(base, "-")
	if len(parts) >= 3 {
		return strings.Join(parts[1:len(parts)-1], "-") // Grabs everything between lang and quality
	} else if len(parts) == 2 {
		return parts[1]
	}
	return base
}

func streamLLMAndTTS(ctx context.Context, prompt string, ws *websocket.Conn, session *ClientSession) error {
	session.Mutex.Lock()
	provider := session.Provider
	apiKey := session.APIKey
	session.Mutex.Unlock()

	log.Printf("[LLM] Routing request to Provider: '%s', Model: '%s'", provider, session.Model)
	if provider == "gemini" && apiKey != "" {
		return streamGeminiAndTTS(ctx, prompt, ws, session, apiKey)
	}
	return streamOllamaAndTTS(ctx, prompt, ws, session)
}

func streamGeminiAndTTS(ctx context.Context, prompt string, ws *websocket.Conn, session *ClientSession, apiKey string) error {
	session.Mutex.Lock()
	model := session.Model
	userName := session.UserName
	googleName := session.GoogleName
	userBio := session.UserBio
	voice := session.Voice
	if model == "" { model = "gemini-1.5-flash" }
	if voice == "" { voice = defaultVoice }
	voiceName := extractVoiceName(voice)
	sysContent := fmt.Sprintf("You are 'Alyx', a highly capable, voice-based AI programming assistant. "+
		"\n\n You are currently thinking using Google Gemini: %s"+
		"\n\nYour output is sent directly to a Text-to-Speech engine. "+
		"\n\n You are speaking using the voice model: %s  "+
		"\n\nYou MUST strictly follow these rules: 1. NEVER use emojis. 2. NEVER use markdown formatting like asterisks. 3. Keep responses conversational, concise, and easy to listen to. Avoid long verbose responses unless explicitly requested", model, voiceName)
	
	if userName != "" {
		sysContent += fmt.Sprintf("\n\nThe user's name is: %s.", userName)
	}
	if userBio != "" {
		sysContent += fmt.Sprintf("\nUser Bio: %s", userBio)
	}
	
	if userName == "" && session.GoogleName != "" {
		sysContent += fmt.Sprintf("\n\nThe user's name is: %s.", session.GoogleName)
	}

	currentTime := time.Now().Format("Monday, January 2, 2006, 15:04 MST")
	log.Printf("System time injected (Gemini): %s", currentTime)
	sysContent += fmt.Sprintf("\n\nThe current date and time is: %s.", currentTime)
	

	t := session.ActiveThread()
	log.Printf("\n--- [GEMINI SYSTEM PROMPT] ---\n%s\n------------------------------ [SUMMARY FOLLOWS]\n", sysContent)
	if t.Summary != "" {
		sysContent += "\n\nContext from earlier in the conversation: " + t.Summary
	}

	
	historySnapshot := make([]ChatMessage, len(t.History))
	copy(historySnapshot, t.History)

	type Part struct{ Text string `json:"text"` }
	type Content struct {
		Role  string `json:"role"`
		Parts []Part `json:"parts"`
	}

	var contents []Content
	for _, msg := range historySnapshot {
		role := "user"
		if msg.Role == "assistant" { role = "model" }
		contents = append(contents, Content{Role: role, Parts: []Part{{Text: msg.Content}}})
	}
	contents = append(contents, Content{Role: "user", Parts: []Part{{Text: prompt}}})
	session.Mutex.Unlock()

	effectiveUserName := userName
	if effectiveUserName == "" {
		effectiveUserName = googleName
	}

	// Prepend system instruction as a dedicated turn for better model compatibility
	systemContent := Content{Role: "user", Parts: []Part{{Text: sysContent + "\n\n(System instructions processed.)"}}}
	contents = append([]Content{systemContent}, contents...)

	payload := map[string]interface{}{
		"contents":          contents,
		"safetySettings": []map[string]string{
			{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "BLOCK_NONE"},
			{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "BLOCK_NONE"},
			{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "threshold": "BLOCK_NONE"},
			{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "BLOCK_NONE"},
		},
	}
	body, _ := json.Marshal(payload)

	reqURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:streamGenerateContent?alt=sse&key=%s", model, apiKey)
	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[Gemini] Failed to create request: %v", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[Gemini] HTTP request failed: %v", err)
		return err
	}
	defer resp.Body.Close()
	log.Printf("[Gemini] HTTP Response Status: %s", resp.Status)

	ttsChan := make(chan string, 50)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for text := range ttsChan {
			if ctx.Err() != nil { return }
			
			session.Mutex.Lock()
			isClientTts := session.ClientTts
			session.Mutex.Unlock()

			if isClientTts {
				safeWrite(ws, session, websocket.TextMessage, []byte("[TTS_CHUNK]"+text))
			} else {
				audioBytes, err := queryTTS(text, voice)
				if err != nil {
					log.Println("Gemini TTS Worker Error:", err)
				} else if ctx.Err() == nil {
					safeWrite(ws, session, websocket.BinaryMessage, audioBytes)
				}
			}
		}
	}()

	var sentence strings.Builder
	var fullResponse strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	var sessionTokens int64

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "" { continue }

			var result struct {
				Candidates []struct {
					Content struct {
						Parts []struct{ Text string `json:"text"` } `json:"parts"`
					} `json:"content"`
				} `json:"candidates"`
				UsageMetadata *struct {
					TotalTokenCount int64 `json:"totalTokenCount"`
				} `json:"usageMetadata"`
			}
			if err := json.Unmarshal([]byte(data), &result); err == nil {
				if result.UsageMetadata != nil {
					sessionTokens = result.UsageMetadata.TotalTokenCount
				}
				if len(result.Candidates) > 0 && len(result.Candidates[0].Content.Parts) > 0 {
				content := result.Candidates[0].Content.Parts[0].Text
				sendOrBroadcastText(nil, session, []byte(content))
				sentence.WriteString(content)
				fullResponse.WriteString(content)

				currentStr := sentence.String()
				flushIdx := -1
				if pIdx := strings.Index(currentStr, "\n"); pIdx != -1 {
					flushIdx = pIdx
				} else if len(currentStr) > 30 {
					for i := 30; i < len(currentStr); i++ {
						if currentStr[i] == '.' || currentStr[i] == '!' || currentStr[i] == '?' || currentStr[i] == ':' {
							// Lookahead to ensure it's a real boundary (followed by space or EOF)
							if i+1 == len(currentStr) || currentStr[i+1] == ' ' {
								flushIdx = i
								break
							}
						}
					}
				}

				if flushIdx != -1 {
					chunkToSend := currentStr[:flushIdx+1]
					remainder := currentStr[flushIdx+1:]
					
					ttsText := strings.TrimSpace(chunkToSend)
					if len(ttsText) > 0 {
						ttsText = strings.ReplaceAll(ttsText, "**", "")
						ttsText = strings.ReplaceAll(ttsText, "*", "")
						ttsText = strings.ReplaceAll(ttsText, "#", "")
						ttsText = strings.ReplaceAll(ttsText, "_", "")
						ttsText = strings.ReplaceAll(ttsText, "`", "")
						if effectiveUserName != "" {
							ttsText = strings.ReplaceAll(ttsText, ", "+effectiveUserName, " "+effectiveUserName)
						}
						select {
						case ttsChan <- ttsText:
						case <-ctx.Done():
						}
					}
					sentence.Reset()
					sentence.WriteString(remainder) // Keep the rest for the next chunk
				}
			}
			}
		}
	}

	if cleanChunk := strings.TrimSpace(sentence.String()); len(cleanChunk) > 0 {
		ttsText := cleanChunk
		ttsText = strings.ReplaceAll(ttsText, "**", "")
		ttsText = strings.ReplaceAll(ttsText, "*", "")
		ttsText = strings.ReplaceAll(ttsText, "#", "")
		ttsText = strings.ReplaceAll(ttsText, "_", "")
		ttsText = strings.ReplaceAll(ttsText, "`", "")
		if effectiveUserName != "" {
			ttsText = strings.ReplaceAll(ttsText, ", "+effectiveUserName, " "+effectiveUserName)
		}
		select {
		case ttsChan <- ttsText:
		case <-ctx.Done():
		}
	}

	close(ttsChan)
	wg.Wait()

	if ctx.Err() == nil {
		session.Mutex.Lock()
		t := session.ActiveThread()
		t.History = append(t.History, ChatMessage{Role: "user", Content: prompt})
		t.History = append(t.History, ChatMessage{Role: "assistant", Content: fullResponse.String()})
		if len(t.History) > 30 { // Keep ~15 turns active for the LLM context
			toSummarize := make([]ChatMessage, 10) // Summarize oldest 5 turns
			copy(toSummarize, t.History[:10])
			t.Archive = append(t.Archive, toSummarize...)
			t.History = t.History[10:]
			
			if len(t.Archive) > 200 { // Cap the visual UI history to 100 turns
				t.Archive = t.Archive[len(t.Archive)-200:]
			}
			go generateSummaryAsync(toSummarize, t.ID, session)
		}
		session.Mutex.Unlock()

		if sessionTokens > 0 {
			trackTokens(session, apiKey, sessionTokens)
		}
		saveSession(session)
	}
	return nil
}

func streamOllamaAndTTS(ctx context.Context, prompt string, ws *websocket.Conn, session *ClientSession) error {
	session.Mutex.Lock()
	model := session.Model
	userName := session.UserName
	googleName := session.GoogleName
	userBio := session.UserBio
	voice := session.Voice
	if model == "" { model = ollamaModel }
	if voice == "" { voice = defaultVoice }
	voiceName := extractVoiceName(voice)
	sysContent := fmt.Sprintf("You are 'Alyx', a highly capable, voice-based AI programming assistant. "+
		"You are built on a low-latency architecture: a Vanilla JS + WebAudio frontend, a Go WebSocket backend, Whisper STT for hearing, Ollama for thinking, and Piper TTS for speaking. "+
		"You are currently thinking using the %s model and speaking using the %s voice model. "+
		"Your output is sent directly to a Text-to-Speech engine. "+
		"You MUST strictly follow these rules: 1. NEVER use emojis. 2. NEVER use markdown formatting like asterisks. 3. Keep responses conversational, concise, and easy to listen to. Avoid long verbose responses unless explicitly requested",
		model, voiceName)

	if userName != "" {
		sysContent += fmt.Sprintf("\n\nThe user's name is: %s.", userName)
	}
	if userBio != "" {
		sysContent += fmt.Sprintf("\nUser Bio: %s", userBio)
	}

	if userName == "" && session.GoogleName != "" {
		sysContent += fmt.Sprintf("\n\nThe user's name is: %s.", session.GoogleName)
	}

	currentTime := time.Now().Format("Monday, January 2, 2006, 15:04 MST")
	log.Printf("System time injected (Ollama): %s", currentTime)
	sysContent += fmt.Sprintf("\n\nThe current date and time is: %s.", currentTime)

	t := session.ActiveThread()
	if t.Summary != "" {
		sysContent += "\n\nContext from earlier in the conversation: " + t.Summary
	}
	
	log.Printf("\n--- [OLLAMA SYSTEM PROMPT] ---\n%s\n------------------------------\n", sysContent)

	messages := []ChatMessage{
		{Role: "system", Content: sysContent},
	}
	historySnapshot := make([]ChatMessage, len(t.History))
	copy(historySnapshot, t.History)
	messages = append(messages, historySnapshot...)
	messages = append(messages, ChatMessage{Role: "user", Content: prompt})
	session.Mutex.Unlock()

	effectiveUserName := userName
	if effectiveUserName == "" {
		effectiveUserName = googleName
	}

	payload := map[string]interface{}{
		"model":    model,
		"messages": messages,
		"stream":   true,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", ollamaChatURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[Ollama] Failed to create request: %v", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[Ollama] HTTP request failed: %v", err)
		return err
	}
	defer resp.Body.Close()
	log.Printf("[Ollama] HTTP Response Status: %s", resp.Status)

	decoder := json.NewDecoder(resp.Body)
	var sentence strings.Builder
	var fullResponse strings.Builder

	// Setup an asynchronous TTS Worker so Ollama tokens aren't blocked by audio generation
	ttsChan := make(chan string, 50)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for text := range ttsChan {
			if ctx.Err() != nil {
				return // Abort if interrupted
			}
			
			session.Mutex.Lock()
			isClientTts := session.ClientTts
			session.Mutex.Unlock()

			if isClientTts {
				safeWrite(ws, session, websocket.TextMessage, []byte("[TTS_CHUNK]"+text))
			} else {
				audioBytes, err := queryTTS(text, voice)
				if err != nil {
					log.Println("Ollama TTS Worker Error:", err)
				} else if ctx.Err() == nil {
					safeWrite(ws, session, websocket.BinaryMessage, audioBytes)
				}
			}
		}
	}()

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
			PromptEvalCount int64 `json:"prompt_eval_count"`
			EvalCount       int64 `json:"eval_count"`
		}
		if err := decoder.Decode(&result); err != nil {
			break
		}

		content := result.Message.Content

		// Stream text token directly to UI
		sendOrBroadcastText(nil, session, []byte(content))

		// Buffer token for TTS processing
		sentence.WriteString(content)
		fullResponse.WriteString(content)
		
		currentStr := sentence.String()
		flushIdx := -1
		if pIdx := strings.Index(currentStr, "\n"); pIdx != -1 {
			flushIdx = pIdx
		} else if len(currentStr) > 30 {
			for i := 30; i < len(currentStr); i++ {
				if currentStr[i] == '.' || currentStr[i] == '!' || currentStr[i] == '?' || currentStr[i] == ':' {
					if i+1 == len(currentStr) || currentStr[i+1] == ' ' {
						flushIdx = i
						break
					}
				}
			}
		}

		if flushIdx != -1 {
			chunkToSend := currentStr[:flushIdx+1]
			remainder := currentStr[flushIdx+1:]
			
			ttsText := strings.TrimSpace(chunkToSend)
			if len(ttsText) > 0 {
				ttsText = strings.ReplaceAll(ttsText, "**", "")
				ttsText = strings.ReplaceAll(ttsText, "*", "")
				ttsText = strings.ReplaceAll(ttsText, "#", "")
				ttsText = strings.ReplaceAll(ttsText, "_", "")
				ttsText = strings.ReplaceAll(ttsText, "`", "")
				if effectiveUserName != "" {
					ttsText = strings.ReplaceAll(ttsText, ", "+effectiveUserName, " "+effectiveUserName)
				}
				select {
				case ttsChan <- ttsText:
				case <-ctx.Done():
				}
			}
			sentence.Reset()
			sentence.WriteString(remainder) // Keep the rest for the next chunk
		}

		if result.Done {
			trackTokens(session, "ollama", result.PromptEvalCount+result.EvalCount)
			break
		}
	}

	// Flush any final text remaining in the buffer when the stream ends
	if cleanChunk := strings.TrimSpace(sentence.String()); len(cleanChunk) > 0 {
		ttsText := cleanChunk
		ttsText = strings.ReplaceAll(ttsText, "**", "")
		ttsText = strings.ReplaceAll(ttsText, "*", "")
		ttsText = strings.ReplaceAll(ttsText, "#", "")
		ttsText = strings.ReplaceAll(ttsText, "_", "")
		ttsText = strings.ReplaceAll(ttsText, "`", "")
		if effectiveUserName != "" {
			ttsText = strings.ReplaceAll(ttsText, ", "+effectiveUserName, " "+effectiveUserName)
		}
		select {
		case ttsChan <- ttsText:
		case <-ctx.Done():
		}
	}

	// Close the channel and wait for the TTS worker to finish its last chunk
	close(ttsChan)
	wg.Wait()

	// If not interrupted, save to history and trigger background summary if needed
	if ctx.Err() == nil {
		session.Mutex.Lock()
		t := session.ActiveThread()
		t.History = append(t.History, ChatMessage{Role: "user", Content: prompt})
		t.History = append(t.History, ChatMessage{Role: "assistant", Content: fullResponse.String()})

		if len(t.History) > 30 { // Keep ~15 turns active for the LLM context
			toSummarize := make([]ChatMessage, 10) // Summarize oldest 5 turns
			copy(toSummarize, t.History[:10])

			t.Archive = append(t.Archive, toSummarize...)
			t.History = t.History[10:]

			if len(t.Archive) > 200 { // Cap the visual UI history to 100 turns
				t.Archive = t.Archive[len(t.Archive)-200:]
			}
			go generateSummaryAsync(toSummarize, t.ID, session)
		}
		session.Mutex.Unlock()
		saveSession(session)
	}

	return nil
}

func rebuildSummaryAsync(session *ClientSession) {
	session.Mutex.Lock()
	t := session.ActiveThread()
	threadID := t.ID
	archiveCopy := make([]ChatMessage, len(t.Archive))
	copy(archiveCopy, t.Archive)
	t.Summary = "" // Clear existing summary to trigger full rebuild
	session.Mutex.Unlock()
	generateSummaryAsync(archiveCopy, threadID, session)
}

func generateSummaryAsync(messages []ChatMessage, threadID string, session *ClientSession) {
	var transcript strings.Builder
	for _, msg := range messages {
		transcript.WriteString(fmt.Sprintf("%s: %s\n", msg.Role, msg.Content))
	}

	session.Mutex.Lock()
	t, exists := session.Threads[threadID]
	if !exists { session.Mutex.Unlock(); return }
	prevSummary := t.Summary
	provider := session.Provider
	apiKey := session.APIKey
	model := session.Model
	session.Mutex.Unlock()

	prompt := "You are generating a highly condensed context memory for yourself. Extract only essential facts, user preferences, and technical decisions from the following conversation. Disregard pleasantries and conversational filler. Optimize strictly for brevity and token density, as this will be injected into your system prompt for future interactions.\n\n"
	if prevSummary != "" {
		prompt += "Previous Context Memory:\n" + prevSummary + "\n\n"
	}
	prompt += "New conversation to merge:\n" + transcript.String() + "\n\nOutput ONLY the new condensed memory."

	var newSummary string
	localSuccess := false

	// 1. Always attempt local summarization first to save tokens (Fast fail after 10s)
	localPayload := map[string]interface{}{"model": ollamaModel, "prompt": prompt, "stream": false}
	localBody, _ := json.Marshal(localPayload)
	
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(ollamaURL, "application/json", bytes.NewReader(localBody))
	if err != nil {
		log.Printf("Local Ollama summary failed (Network/Timeout): %v", err)
	} else {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var result struct {
				Response        string `json:"response"`
				PromptEvalCount int64  `json:"prompt_eval_count"`
				EvalCount       int64  `json:"eval_count"`
			}
			if json.NewDecoder(resp.Body).Decode(&result) == nil {
				newSummary = strings.TrimSpace(result.Response)
				localSuccess = true
				trackTokens(session, "ollama", result.PromptEvalCount+result.EvalCount)
				log.Println("--- Context Memory Summarized locally via Ollama (Saved API Tokens!) ---")
			}
		} else {
			buf := new(bytes.Buffer)
			buf.ReadFrom(resp.Body)
			log.Printf("Local Ollama summary failed (HTTP %d): %s", resp.StatusCode, buf.String())
		}
	}

	// 2. Fallback to Gemini if Ollama is offline or failed
	if !localSuccess && provider == "gemini" && apiKey != "" {
		type Part struct{ Text string `json:"text"` }
		payload := map[string]interface{}{
			"contents": []map[string]interface{}{
				{"role": "user", "parts": []Part{{Text: prompt}}},
			},
		}
		body, _ := json.Marshal(payload)
		if model == "" { model = "gemini-1.5-flash" }
		reqURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)
		if resp, err := http.Post(reqURL, "application/json", bytes.NewReader(body)); err == nil {
			defer resp.Body.Close()
			var result struct {
				Candidates []struct {
					Content struct {
						Parts []struct{ Text string `json:"text"` } `json:"parts"`
					} `json:"content"`
				} `json:"candidates"`
				UsageMetadata *struct {
					TotalTokenCount int64 `json:"totalTokenCount"`
				} `json:"usageMetadata"`
			}
			if json.NewDecoder(resp.Body).Decode(&result) == nil && len(result.Candidates) > 0 && len(result.Candidates[0].Content.Parts) > 0 {
				newSummary = strings.TrimSpace(result.Candidates[0].Content.Parts[0].Text)
				if result.UsageMetadata != nil {
					trackTokens(session, apiKey, result.UsageMetadata.TotalTokenCount)
				}
				log.Println("--- Context Memory Summarized via Gemini API (Local fallback failed) ---")
			}
		}
	}

	if newSummary != "" {
		session.Mutex.Lock()
		if t, ok := session.Threads[threadID]; ok {
			t.Summary = newSummary
		}
		isActive := session.ActiveThreadID == threadID
		session.Mutex.Unlock()
		saveSession(session)
		if isActive {
			sendSummary(nil, session)
		}
	}
}

func queryTTS(text string, voiceFile string) ([]byte, error) {
	log.Printf("[TTS] Generating audio for text: '%s' with voice: %s", text, voiceFile)
	modelPath := filepath.Join(".", "piper", voiceFile)
	// Execute piper binary: -f - tells it to output the WAV file directly to standard output
	cmd := exec.Command(piperBin, "--model", modelPath, "-f", "-")
	
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

	audioBytes := out.Bytes()
	log.Printf("[TTS] Successfully generated %d bytes of audio", len(audioBytes))
	return audioBytes, nil
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 16)
	rand.Read(b)
	stateBase := base64.URLEncoding.EncodeToString(b)
	
	// Embed the client type into the state parameter
	clientType := r.URL.Query().Get("client")
	state := stateBase + "|" + clientType
	
	http.SetCookie(w, &http.Cookie{Name: "oauthstate", Value: stateBase, Path: "/", MaxAge: 3600, HttpOnly: true})

	baseURL := os.Getenv("PUBLIC_URL")
	if baseURL == "" {
		scheme := "https"
		if r.Header.Get("X-Forwarded-Proto") == "http" {
			scheme = "http"
		}
		baseURL = scheme + "://" + r.Host
	}
	redirectURI := baseURL + "/auth/callback"
	log.Printf("[Login] Host: %s, X-Forwarded-Proto: '%s', Generated Redirect: %s", r.Host, r.Header.Get("X-Forwarded-Proto"), redirectURI)

	url := fmt.Sprintf("https://accounts.google.com/o/oauth2/v2/auth?client_id=%s&redirect_uri=%s&response_type=code&scope=openid profile email&state=%s", googleClientID, redirectURI, state)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func handleCallback(w http.ResponseWriter, r *http.Request) {
	stateParam := r.FormValue("state")
	parts := strings.SplitN(stateParam, "|", 2)
	stateBase := parts[0]
	clientType := ""
	if len(parts) > 1 {
		clientType = parts[1]
	}

	stateCookie, err := r.Cookie("oauthstate")
	if err != nil || stateBase != stateCookie.Value {
		http.Error(w, "Invalid state", http.StatusBadRequest)
		return
	}

	data := url.Values{}
	data.Set("client_id", googleClientID)
	data.Set("client_secret", googleClientSecret)
	data.Set("code", r.FormValue("code"))
	data.Set("grant_type", "authorization_code")
	
	baseURL := os.Getenv("PUBLIC_URL")
	if baseURL == "" {
		scheme := "https"
		if r.Header.Get("X-Forwarded-Proto") == "http" {
			scheme = "http"
		}
		baseURL = scheme + "://" + r.Host
	}
	redirectURI := baseURL + "/auth/callback"
	log.Printf("[Callback] Host: %s, X-Forwarded-Proto: '%s', Generated Redirect: %s", r.Host, r.Header.Get("X-Forwarded-Proto"), redirectURI)
	data.Set("redirect_uri", redirectURI)

	resp, err := http.PostForm("https://oauth2.googleapis.com/token", data)
	if err != nil {
		http.Error(w, "Token exchange failed", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var tokenRes struct{ AccessToken string `json:"access_token"` }
	json.NewDecoder(resp.Body).Decode(&tokenRes)

	req, _ := http.NewRequest("GET", "https://www.googleapis.com/oauth2/v3/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tokenRes.AccessToken)
	userResp, _ := http.DefaultClient.Do(req)
	defer userResp.Body.Close()
	
	rawBytes, _ := io.ReadAll(userResp.Body)
	log.Printf("\n--- [GOOGLE OAUTH RAW RESPONSE] ---\n%s\n-----------------------------------\n", string(rawBytes))
	userResp.Body = io.NopCloser(bytes.NewBuffer(rawBytes))

	var userRes struct {
		Sub       string `json:"sub"` // v3 user ID
		ID        string `json:"id"`  // v2 user ID (fallback)
		Picture   string `json:"picture"`
		GivenName string `json:"given_name"`
	}
	json.NewDecoder(bytes.NewReader(rawBytes)).Decode(&userRes)

	userID := userRes.Sub
	if userID == "" {
		userID = userRes.ID
	} // Prioritize v3, fallback to v2

	if userID == "" {
		http.Error(w, "Failed to retrieve user ID from Google", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{Name: "speax_session", Value: userID, Path: "/", MaxAge: 86400 * 30, HttpOnly: false})
	http.SetCookie(w, &http.Cookie{Name: "speax_avatar", Value: url.QueryEscape(userRes.Picture), Path: "/", MaxAge: 86400 * 30, HttpOnly: false})
	http.SetCookie(w, &http.Cookie{Name: "speax_google_name", Value: url.QueryEscape(userRes.GivenName), Path: "/", MaxAge: 86400 * 30, HttpOnly: false})
	
	// If native Android app, deep link back with the session ID
	if clientType == "android" {
		http.Redirect(w, r, "speax://callback?session="+userID+"&name="+url.QueryEscape(userRes.GivenName), http.StatusTemporaryRedirect)
		return
	}
	
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	apiKey := r.URL.Query().Get("apiKey")

	type ModelData struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var out []ModelData

	if provider == "gemini" && apiKey != "" {
		reqURL := "https://generativelanguage.googleapis.com/v1beta/models?key=" + apiKey
		if resp, err := http.Get(reqURL); err == nil {
			defer resp.Body.Close()
			var res struct {
				Models []struct {
					Name                       string   `json:"name"`
					DisplayName                string   `json:"displayName"`
					SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
				} `json:"models"`
			}
			json.NewDecoder(resp.Body).Decode(&res)
			for _, m := range res.Models {
				for _, method := range m.SupportedGenerationMethods {
					if method == "generateContent" {
						displayName := m.DisplayName
						isFlash := strings.Contains(displayName, "Flash")
						isGemma := strings.Contains(displayName, "Gemma")
						isGemini2 := strings.Contains(displayName, "Gemini 2")
						if (isFlash || isGemma) && !isGemini2 {
							out = append(out, ModelData{ID: strings.TrimPrefix(m.Name, "models/"), Name: displayName})
						}
						break
					}
				}
			}
			// Sort: Gemini models to the top
			var geminiModels, otherModels []ModelData
			for _, m := range out {
				if strings.Contains(m.Name, "Gemini") {
					geminiModels = append(geminiModels, m)
				} else {
					otherModels = append(otherModels, m)
				}
			}
			out = append(geminiModels, otherModels...)
		}
	} else if provider == "ollama" {
		if resp, err := http.Get("http://localhost:11434/api/tags"); err == nil {
			defer resp.Body.Close()
			var res struct{ Models []struct{ Name string `json:"name"` } `json:"models"` }
			json.NewDecoder(resp.Body).Decode(&res)
			for _, m := range res.Models { out = append(out, ModelData{ID: m.Name, Name: m.Name}) }
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func handleVoices(w http.ResponseWriter, r *http.Request) {
	files, err := os.ReadDir("./piper")
	var out []string
	if err == nil {
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".onnx") {
				out = append(out, f.Name())
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func main() {
	http.HandleFunc("/auth/login", handleLogin)
	http.HandleFunc("/auth/callback", handleCallback)
	http.HandleFunc("/api/models", handleModels)
	http.HandleFunc("/api/voices", handleVoices)
	http.HandleFunc("/ws", handleConnections)
	http.Handle("/", http.FileServer(http.Dir("./public")))

	fmt.Println("Server started on :3000")
	err := http.ListenAndServe(":3000", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
