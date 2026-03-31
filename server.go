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
	"os"
	"path/filepath"
	"strconv"
	"time"

	//"net/url"

	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"github.com/gorilla/websocket"
	"speaks.jakbox.dev/tts"
)

func generateID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

type FallbackLLMConfig struct {
	URL    string   `json:"URL"`
	Models []string `json:"Models"`
}

type Config struct {
	WhisperURLs          []string                  `json:"WhisperURLs"`
	PiperBin             string                    `json:"PiperBin"`
	DefaultVoice         string                    `json:"DefaultVoice"`
	SampleRate           int                       `json:"SampleRate"`
	OllamaURLs           []string                  `json:"OllamaURLs"`
	OllamaChatURL        []string                  `json:"OllamaChatURL"`
	OllamaModel          string                    `json:"OllamaModel"`
	WakeWords            []string                  `json:"WakeWords"`
	PassiveWindowSeconds int                       `json:"PassiveWindowSeconds"`
	MaxArchiveTurns      int                       `json:"MaxArchiveTurns"`
	MaxTokensGemini      int                       `json:"MaxTokensGemini"`
	MaxTokensOllama      int                       `json:"MaxTokensOllama"`
	SystemPromptGemini   string                    `json:"SystemPromptGemini"`
	SystemPromptOllama   string                    `json:"SystemPromptOllama"`
	ToolSystemPrompt     string                    `json:"ToolSystemPrompt"`
	Admins               []string                  `json:"Admins"`
	ModelLimits          map[string]ModelRateLimit `json:"ModelLimits"`
	DefaultLimit         ModelRateLimit            `json:"DefaultLimit"`
	FallbackLLM          FallbackLLMConfig         `json:"FallbackLLM"`
	GeminiModels         []string                  `json:"GeminiModels"`
	OllamaModels         []string                  `json:"OllamaModels"`
}

type ModelRateLimit struct {
	RPM int `json:"RPM"` // Requests Per Minute
	TPM int `json:"TPM"` // Tokens Per Minute
	RPD int `json:"RPD"` // Requests Per Day
}

func (c *Config) Validate() error {
	if len(c.WhisperURLs) == 0 {
		return fmt.Errorf("WhisperURLs cannot be empty")
	}
	if len(c.OllamaURLs) == 0 {
		return fmt.Errorf("OllamaURLs cannot be empty")
	}
	if len(c.OllamaChatURL) == 0 {
		return fmt.Errorf("OllamaChatURL cannot be empty")
	}
	if c.PiperBin == "" {
		return fmt.Errorf("PiperBin cannot be empty")
	}
	return nil
}

var (
	config      Config
	configMutex sync.RWMutex
)

var (
	whisperIndex    uint32
	ollamaIndex     uint32
	ollamaChatIndex uint32
)

var (
	perfMetrics   *PerformanceMetricsStore
	perfMetricsMu sync.Mutex
)

var (
	ollamaToolsSupportCache   = make(map[string]bool)
	ollamaToolsSupportCacheMu sync.Mutex
)

func ollamaModelSupportsTools(model string) bool {
	low := strings.ToLower(model)
	// Explicitly block models that are known to struggle with native tool calling in this implementation
	if strings.Contains(low, "gemma") || strings.Contains(low, "llama") {
		return false
	}

	ollamaToolsSupportCacheMu.Lock()
	if v, ok := ollamaToolsSupportCache[model]; ok {
		ollamaToolsSupportCacheMu.Unlock()
		return v
	}
	ollamaToolsSupportCacheMu.Unlock()

	configMutex.RLock()
	chatURLs := config.OllamaChatURL
	configMutex.RUnlock()

	if len(chatURLs) == 0 {
		return false
	}

	showURL := strings.Replace(chatURLs[0], "/chat", "/show", 1)
	payload := map[string]string{"model": model}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(showURL, "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusOK {
		log.Printf("[Ollama] Failed to check tool support for %s: %v", model, err)
		return false
	}
	defer resp.Body.Close()

	var result struct {
		Capabilities []string `json:"capabilities"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	supported := false
	for _, cap := range result.Capabilities {
		if cap == "tools" {
			supported = true
			break
		}
	}

	log.Printf("[Ollama] Model %s tool support: %v (capabilities: %v)", model, supported, result.Capabilities)

	ollamaToolsSupportCacheMu.Lock()
	ollamaToolsSupportCache[model] = supported
	ollamaToolsSupportCacheMu.Unlock()

	return supported
}

// ---- Performance Metrics -----------------------------------------------

// ModelMetricsSample is a single recorded LLM call (kept in rolling window).
type ModelMetricsSample struct {
	Timestamp      time.Time `json:"ts"`
	PromptTokens   int64     `json:"promptTokens"`
	ResponseTokens int64     `json:"responseTokens"`
	TotalTokens    int64     `json:"totalTokens"`
	LatencyMs      int64     `json:"latencyMs"`
	Complexity     float64   `json:"complexity"` // tokens/sec throughput
}

type TokenUsageSample struct {
	Timestamp time.Time `json:"ts"`
	Count     int64     `json:"count"`
}

type ModelUsage struct {
	RequestTimes []time.Time        `json:"requestTimes"`
	TokenSamples []TokenUsageSample `json:"tokenSamples"`
}

// ModelMetricsAgg accumulates statistics for one provider+model pair.
type ModelMetricsAgg struct {
	Provider          string               `json:"provider"`
	Model             string               `json:"model"`
	CallCount         int64                `json:"callCount"`
	TotalPromptTokens int64                `json:"totalPromptTokens"`
	TotalRespTokens   int64                `json:"totalResponseTokens"`
	TotalTokens       int64                `json:"totalTokens"`
	TotalLatencyMs    int64                `json:"totalLatencyMs"`
	AvgLatencyMs      float64              `json:"avgLatencyMs"`
	AvgComplexity     float64              `json:"avgComplexity"`
	PeakLatencyMs     int64                `json:"peakLatencyMs"`
	MinLatencyMs      int64                `json:"minLatencyMs"` // -1 = unset
	LastUpdated       time.Time            `json:"lastUpdated"`
	RecentSamples     []ModelMetricsSample `json:"recentSamples"` // capped at 50
}

// PerformanceMetricsStore is the top-level JSON stored at context/performance-metrics.json.
type PerformanceMetricsStore struct {
	// Keyed as "provider/model", e.g. "gemini/gemini-1.5-flash"
	Models map[string]*ModelMetricsAgg `json:"models"`
}

const perfMetricsPath = "context/performance-metrics.json"
const perfMetricsMaxSamples = 50

func loadPerformanceMetrics() *PerformanceMetricsStore {
	store := &PerformanceMetricsStore{
		Models: make(map[string]*ModelMetricsAgg),
	}
	data, err := os.ReadFile(perfMetricsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[Metrics] Could not read %s: %v", perfMetricsPath, err)
		}
		return store
	}
	if err := json.Unmarshal(data, store); err != nil {
		log.Printf("[Metrics] Could not parse %s: %v (starting fresh)", perfMetricsPath, err)
		store.Models = make(map[string]*ModelMetricsAgg)
	}
	// Ensure MinLatencyMs sentinel is set for entries that have no samples yet
	for _, agg := range store.Models {
		if agg.MinLatencyMs == 0 && agg.CallCount == 0 {
			agg.MinLatencyMs = -1
		}
	}
	log.Printf("[Metrics] Loaded performance metrics (%d model entries) from %s", len(store.Models), perfMetricsPath)
	return store
}

func savePerformanceMetrics() {
	// Caller must hold perfMetricsMu
	data, err := json.MarshalIndent(perfMetrics, "", "  ")
	if err != nil {
		log.Printf("[Metrics] Failed to serialise performance metrics: %v", err)
		return
	}
	if err := os.WriteFile(perfMetricsPath, data, 0644); err != nil {
		log.Printf("[Metrics] Failed to write %s: %v", perfMetricsPath, err)
	}
}

// recordLLMCall records a completed LLM call into the shared performance store.
// promptTokens and responseTokens may both be 0 for providers that only report totals;
// pass the total in both if a split is unavailable (e.g. Gemini totalTokenCount fallback).
func recordLLMCall(provider, model string, promptTokens, responseTokens, latencyMs int64) {
	if latencyMs <= 0 {
		return
	}

	totalTokens := promptTokens + responseTokens
	var complexity float64
	if latencyMs > 0 {
		complexity = float64(totalTokens) / (float64(latencyMs) / 1000.0)
	}

	sample := ModelMetricsSample{
		Timestamp:      time.Now(),
		PromptTokens:   promptTokens,
		ResponseTokens: responseTokens,
		TotalTokens:    totalTokens,
		LatencyMs:      latencyMs,
		Complexity:     complexity,
	}

	perfMetricsMu.Lock()
	defer perfMetricsMu.Unlock()

	key := provider + "/" + model
	agg, ok := perfMetrics.Models[key]
	if !ok {
		agg = &ModelMetricsAgg{
			Provider:     provider,
			Model:        model,
			MinLatencyMs: -1,
		}
		perfMetrics.Models[key] = agg
	}

	agg.CallCount++
	agg.TotalPromptTokens += promptTokens
	agg.TotalRespTokens += responseTokens
	agg.TotalTokens += totalTokens
	agg.TotalLatencyMs += latencyMs
	agg.AvgLatencyMs = float64(agg.TotalLatencyMs) / float64(agg.CallCount)
	// Rolling avg complexity
	agg.AvgComplexity = agg.AvgComplexity + (complexity-agg.AvgComplexity)/float64(agg.CallCount)
	if latencyMs > agg.PeakLatencyMs {
		agg.PeakLatencyMs = latencyMs
	}
	if agg.MinLatencyMs == -1 || latencyMs < agg.MinLatencyMs {
		agg.MinLatencyMs = latencyMs
	}
	agg.LastUpdated = sample.Timestamp

	// Rolling window — keep last N samples
	agg.RecentSamples = append(agg.RecentSamples, sample)
	if len(agg.RecentSamples) > perfMetricsMaxSamples {
		agg.RecentSamples = agg.RecentSamples[len(agg.RecentSamples)-perfMetricsMaxSamples:]
	}

	log.Printf("[Metrics] %s | prompt=%d resp=%d total=%d latency=%dms complexity=%.1f tok/s",
		key, promptTokens, responseTokens, totalTokens, latencyMs, complexity)

	savePerformanceMetrics()
}

// ---- End Performance Metrics --------------------------------------------

type WhisperNode struct {
	URL                string
	Zombie             bool
	LastResponseTime   time.Duration // Last successful response time
	LastExecutionTime  time.Duration // Last raw request latency (any attempt)
	RollingCutoffRatio float64       // Normalized moving average of: (ExecutionTime / TimeoutThreshold)
	FailureCount       int
	TotalRequests      int
	TotalFailures      int
}

var (
	whisperNodes      []*WhisperNode
	whisperNodesMutex sync.RWMutex
)

var ErrNoHealthyNodes = fmt.Errorf("all Whisper nodes are unhealthy")

func getNextURL(urls []string, index *uint32) string {
	if len(urls) == 0 {
		return ""
	}
	newIdx := atomic.AddUint32(index, 1)
	return urls[int(newIdx-1)%len(urls)]
}

func getHealthyWhisperNode() (*WhisperNode, error) {
	whisperNodesMutex.RLock()
	defer whisperNodesMutex.RUnlock()

	numNodes := len(whisperNodes)
	if numNodes == 0 {
		return nil, ErrNoHealthyNodes
	}

	for i := 0; i < numNodes; i++ {
		idx := atomic.AddUint32(&whisperIndex, 1) - 1
		node := whisperNodes[idx%uint32(numNodes)]
		if !node.Zombie {
			return node, nil
		}
	}

	return nil, ErrNoHealthyNodes
}

func reloadConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var newConfig Config
	if err := json.Unmarshal(data, &newConfig); err != nil {
		return err
	}
	if err := newConfig.Validate(); err != nil {
		return err
	}

	configMutex.Lock()
	config = newConfig
	configMutex.Unlock()

	// Initialize Whisper nodes
	whisperNodesMutex.Lock()
	existingNodes := make(map[string]*WhisperNode)
	for _, node := range whisperNodes {
		existingNodes[node.URL] = node
	}
	var newNodes []*WhisperNode
	for _, url := range newConfig.WhisperURLs {
		if node, exists := existingNodes[url]; exists {
			newNodes = append(newNodes, node)
		} else {
			newNodes = append(newNodes, &WhisperNode{URL: url, Zombie: false})
		}
	}
	whisperNodes = newNodes
	whisperNodesMutex.Unlock()
	return nil
}

func watchConfig(path string) {
	initialStat, err := os.Stat(path)
	if err != nil {
		log.Printf("Error stating config file: %v", err)
		return
	}

	lastModTime := initialStat.ModTime()
	ticker := time.NewTicker(2 * time.Second)
	go func() {
		for range ticker.C {
			stat, err := os.Stat(path)
			if err != nil {
				continue
			}
			if stat.ModTime().After(lastModTime) {
				lastModTime = stat.ModTime()
				log.Println("Config file change detected, reloading...")
				if err := reloadConfig(path); err != nil {
					log.Printf("FAILED to reload config: %v (Update ignored)", err)
				} else {
					log.Println("Config successfully reloaded")
				}
			}
		}
	}()
}

var (
	googleClientID     string
	googleClientSecret string
)

func init() {
	// Load server.config
	if err := reloadConfig("server.config"); err != nil {
		log.Fatal("FATAL: Could not load server.config: ", err)
	}
	log.Println("Loaded server settings from server.config")
	watchConfig("server.config")

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

	loadPersonas("personas.json")
	watchPersonas("personas.json")

	perfMetrics = loadPerformanceMetrics()
}

type PersonaTheme struct {
	Primary    string `json:"primary"`
	Secondary  string `json:"secondary"`
	Tertiary   string `json:"tertiary"`
	Background string `json:"background"`
	Panel      string `json:"panel"`
}

type Persona struct {
	Name                  string       `json:"name"`
	NameMutations         string       `json:"name_mutations"`
	PhoneticPronunciation string       `json:"phonetic_pronunciation"`
	Tone                  string       `json:"tone"`
	AddressStyle          string       `json:"address_style"`
	Focus                 string       `json:"focus"`
	InteractionStyle      string       `json:"interaction_style"`
	Constraints           string       `json:"constraints"`
	VoiceFile             string       `json:"voice_file"`
	VoiceNoiseScale      float64      `json:"voice_noise_scale,omitempty"`
	VoiceLengthScale     float64      `json:"voice_length_scale,omitempty"`
	VoiceNoiseW          float64      `json:"voice_noise_w,omitempty"`
	Theme                 PersonaTheme `json:"theme"`
}

var (
	personas      map[string]Persona
	personasMutex sync.RWMutex
)

func loadPersonas(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var newPersonas map[string]Persona
	if err := json.Unmarshal(data, &newPersonas); err != nil {
		return err
	}

	personasMutex.Lock()
	personas = newPersonas
	personasMutex.Unlock()
	log.Printf("Loaded %d personas from %s", len(personas), path)
	return nil
}

func watchPersonas(path string) {
	initialStat, err := os.Stat(path)
	if err != nil {
		log.Printf("Error stating personas file: %v", err)
		return
	}

	lastModTime := initialStat.ModTime()
	ticker := time.NewTicker(2 * time.Second)
	go func() {
		for range ticker.C {
			stat, err := os.Stat(path)
			if err != nil {
				continue
			}
			if stat.ModTime().After(lastModTime) {
				lastModTime = stat.ModTime()
				log.Println("Personas file change detected, reloading...")
				if err := loadPersonas(path); err != nil {
					log.Printf("FAILED to reload personas: %v (Update ignored)", err)
				} else {
					log.Println("Personas successfully reloaded, updating active sessions...")
					activeSessionsMutex.Lock()
					for _, session := range activeSessions {
						session.Mutex.Lock()
						if session.Voice != "" {
							vName := strings.ToLower(extractVoiceName(session.Voice))
							personasMutex.RLock()
							if p, ok := personas[vName]; ok {
								if session.Theme != p.Theme {
									session.Theme = p.Theme
									log.Printf("Updated theme for session %s (voice: %s)", session.ClientID, vName)
								}
							}
							personasMutex.RUnlock()
						}
						session.Mutex.Unlock()
						sendSettings(nil, session) // Broadcast updated settings (including theme)
					}
					activeSessionsMutex.Unlock()
				}
			}
		}
	}()
}

var (
	activeSessions      = make(map[string]*ClientSession)
	activeSessionsMutex sync.Mutex
)

type NativeToolCall struct {
	ID   string                 `json:"id,omitempty"`
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

type NativeToolResult struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Result string `json:"result"`
}

type ChatMessage struct {
	Role       string            `json:"role"`
	Content    string            `json:"content"`
	Timestamp  time.Time         `json:"timestamp"`
	ToolCall   *NativeToolCall   `json:"tool_call,omitempty"`
	ToolResult *NativeToolResult `json:"tool_result,omitempty"`
}

type Thread struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	History     []ChatMessage `json:"history"`
	Archive     []ChatMessage `json:"archive"`
	Summary     string        `json:"summary"`
	ToolHistory []ChatMessage `json:"-"` // Stored separately in system_[name].json
	CreatedAt   time.Time     `json:"createdAt"`
	UpdatedAt   time.Time     `json:"updatedAt"`
}



type ToolAction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Schema      map[string]interface{} `json:"schema"`
}

type Tool struct {
	Name    string          `json:"name"`
	Actions []ToolAction    `json:"actions"`
	Conn    *websocket.Conn `json:"-"`
}

type ConnMeta struct {
	ClientType string
	Device     string
}

type ClientSession struct {
	ClientID string `json:"-"`

	Threads        map[string]*Thread `json:"threads"`
	ActiveThreadID string             `json:"activeThreadId"`
	SystemLog      []ChatMessage      `json:"systemLog"`
	IsAssistant    bool               `json:"isAssistant"`


	// Legacy fields (kept for automatic migration on load)
	History []ChatMessage `json:"history,omitempty"`
	Archive []ChatMessage `json:"archive,omitempty"`
	Summary string        `json:"summary,omitempty"`

	UserName      string                       `json:"userName"`
	GoogleName    string                       `json:"googleName"`
	UserBio       string                       `json:"userBio"`
	Provider      string                       `json:"provider"`
	APIKey        string                       `json:"apiKey"`
	Model         string                       `json:"model"`
	Voice         string                       `json:"voice"`
	Theme         PersonaTheme                 `json:"theme"`
	Mutex         sync.Mutex                   `json:"-"`
	ClientStorage bool                         `json:"clientStorage"`
	ClientTts     bool                         `json:"clientTts"`
	ConnMutex     sync.Mutex                   `json:"-"` // Dedicated lock for WebSocket writes
	TokenUsage    map[string]int64             `json:"tokenUsage"`
	Conns         map[*websocket.Conn]ConnMeta `json:"-"`
	Tools         map[string]*Tool             `json:"-"` // Map tool name to connected tool
	// Streaming Audio State
	StreamingBuffer    []byte                 `json:"-"`
	StreamingStartTime time.Time              `json:"-"`
	ActiveSeqID        int64                  `json:"-"`
	BufferMutex        sync.Mutex             `json:"-"`
	TurnMutex          sync.Mutex             `json:"-"` // Serializes AI responses/turns
	ActiveCancel       context.CancelFunc     `json:"-"` // Global interrupt control
	ToolDebounceTimer  *time.Timer            `json:"-"` // Debounces AI response after tool results
	PassiveAssistant   bool                   `json:"passiveAssistant"`
	LastActiveTime     time.Time              `json:"-"`
	LastActiveConn     *websocket.Conn        `json:"-"` // Tracks the last client to send input (text/audio)
	ModelUsage               map[string]*ModelUsage `json:"modelUsage"` // Per-model usage stats (pruned)
	FallbackOriginalProvider string                 `json:"fallbackOriginalProvider"`
	FallbackOriginalModel    string                 `json:"fallbackOriginalModel"`
	PassiveBlockUntil        time.Time              `json:"-"`
}

func IsAdminID(id string) bool {
	configMutex.RLock()
	defer configMutex.RUnlock()
	for _, adminID := range config.Admins {
		if id == adminID {
			return true
		}
	}
	return false
}

func (s *ClientSession) IsAdmin() bool {
	return IsAdminID(s.ClientID)
}

func (s *ClientSession) isToolConn(ws *websocket.Conn) bool {
	if ws == nil {
		return false
	}
	s.ConnMutex.Lock()
	defer s.ConnMutex.Unlock()
	if meta, exists := s.Conns[ws]; exists {
		return meta.ClientType == "tool"
	}
	return false
}

// appendMessage categorizes and adds a message to the appropriate log with a timestamp.
func (s *ClientSession) appendMessage(role, content string, thread *Thread) {
	msg := ChatMessage{
		Role:      role,
		Content:   content,
		Timestamp: time.Now(),
	}

	if role == "system" {
		if strings.HasPrefix(content, "[TOOL_RESULT") {
			thread.ToolHistory = append(thread.ToolHistory, msg)
			thread.UpdatedAt = msg.Timestamp
			if len(thread.ToolHistory) > 50 {
				thread.ToolHistory = thread.ToolHistory[len(thread.ToolHistory)-50:]
			}
			return

		}
		s.SystemLog = append(s.SystemLog, msg)
		if len(s.SystemLog) > 100 {
			s.SystemLog = s.SystemLog[len(s.SystemLog)-100:]
		}
		return
	}

	thread.History = append(thread.History, msg)
	thread.UpdatedAt = msg.Timestamp
}


func (s *ClientSession) appendNativeToolCall(nativeName string, args map[string]interface{}, thread *Thread) string {
	id := generateID()
	msg := ChatMessage{
		Role:      "assistant",
		Content:   fmt.Sprintf("[NATIVE_TOOL_CALL: %s]", nativeName),
		Timestamp: time.Now(),
		ToolCall: &NativeToolCall{
			ID:   id,
			Name: nativeName,
			Args: args,
		},
	}
	thread.History = append(thread.History, msg)
	return id
}

func (s *ClientSession) appendNativeToolResult(nativeName string, result string, thread *Thread, id string) {
	msg := ChatMessage{
		Role:      "tool",
		Content:   result,
		Timestamp: time.Now(),
		ToolResult: &NativeToolResult{
			ID:     id,
			Name:   nativeName,
			Result: result,
		},
	}
	thread.ToolHistory = append(thread.ToolHistory, msg)
	if len(thread.ToolHistory) > 50 {
		thread.ToolHistory = thread.ToolHistory[len(thread.ToolHistory)-50:]
	}
}

// getLLMContext prepares a chronologically interleaved history for the LLM.
// supportsTools controls whether ToolHistory is merged in.
func (s *ClientSession) getLLMContext(thread *Thread, supportsTools bool) []ChatMessage {
	var ctxMsgs []ChatMessage

	isAssistantThread := strings.HasPrefix(thread.ID, "assistant/")

	// 1. Collect last 20 from SystemLog (skip for assistant threads to keep them clean)
	if !isAssistantThread {
		startIdx := len(s.SystemLog) - 20
		if startIdx < 0 {
			startIdx = 0
		}
		ctxMsgs = append(ctxMsgs, s.SystemLog[startIdx:]...)
	}


	// 2. Map results by ID for deterministic matching.
	resultsByID := make(map[string]ChatMessage)
	for _, rm := range thread.ToolHistory {
		if rm.ToolResult != nil && rm.ToolResult.ID != "" {
			resultsByID[rm.ToolResult.ID] = rm
		}
	}

	// 3. Collect from History and match tool pairs.
	// We'll first identity all potential pairs.
	type toolPair struct {
		call   ChatMessage
		result ChatMessage
	}
	var pairs []toolPair
	var historyWithoutTools []ChatMessage

	for _, m := range thread.History {
		if m.ToolCall != nil || m.ToolResult != nil {
			if supportsTools && m.ToolCall != nil {
				if res, ok := resultsByID[m.ToolCall.ID]; ok {
					pairs = append(pairs, toolPair{call: m, result: res})
				}
			}
			// If tools are not supported, we include the message as plain text
			// (stripping the structured fields) to preserve conversational context.
			if !supportsTools && m.ToolCall != nil {
				m.ToolCall = nil
				historyWithoutTools = append(historyWithoutTools, m)
			}
			// We always skip the direct append and the ToolResult turns (which are handled via pairing)
			continue
		}
		// Otherwise keep as regular history
		historyWithoutTools = append(historyWithoutTools, m)
	}

	// 4. Filter to last 3 pairs
	if len(pairs) > 3 {
		pairs = pairs[len(pairs)-3:]
	}

	// 5. Build final context messages
	ctxMsgs = append(ctxMsgs, historyWithoutTools...)
	for _, p := range pairs {
		// Re-stamp results to follow calls by 1ms for stable sorting
		p.result.Timestamp = p.call.Timestamp.Add(1 * time.Millisecond)
		ctxMsgs = append(ctxMsgs, p.call, p.result)
	}

	// 6. Sort chronologically (stable sort)
	sort.SliceStable(ctxMsgs, func(i, j int) bool {
		if ctxMsgs[i].Timestamp.Equal(ctxMsgs[j].Timestamp) {
			if ctxMsgs[i].ToolCall != nil && ctxMsgs[j].ToolResult != nil {
				return true
			}
			if ctxMsgs[i].ToolResult != nil && ctxMsgs[j].ToolCall != nil {
				return false
			}
		}
		return ctxMsgs[i].Timestamp.Before(ctxMsgs[j].Timestamp)
	})

	// 7. Ensure role alternation and that we start with 'user'.
	// Gemini is extremely strict about the first role being 'user' and roles alternating.
	var finalized []ChatMessage
	var systemBuffer []ChatMessage
	var inToolSequence bool

	for _, m := range ctxMsgs {
		isSystem := m.Role == "system" || (m.Role == "user" && strings.HasPrefix(m.Content, "[System Note]"))

		if isSystem {
			if inToolSequence {
				systemBuffer = append(systemBuffer, m)
			} else {
				finalized = append(finalized, m)
			}
			continue
		}

		if m.ToolCall != nil || m.ToolResult != nil {
			inToolSequence = true
		} else {
			if len(systemBuffer) > 0 {
				finalized = append(finalized, systemBuffer...)
				systemBuffer = nil
			}
			inToolSequence = false
		}
		finalized = append(finalized, m)
	}

	if len(systemBuffer) > 0 {
		finalized = append(finalized, systemBuffer...)
	}

	// TRIMMING/FIXUP: Ensure the first turn is 'user'
	firstValidIdx := -1
	for i, m := range finalized {
		if m.Role == "user" {
			firstValidIdx = i
			break
		}
	}
	if firstValidIdx == -1 {
		// No user turns at all? Prepend a dummy one.
		finalized = append([]ChatMessage{{Role: "user", Content: "[System: Session started]", Timestamp: time.Now().Add(-1 * time.Hour)}}, finalized...)
	} else if firstValidIdx > 0 {
		// Drop everything before the first user turn (e.g. dangling model/function turns)
		finalized = finalized[firstValidIdx:]
	}

	return finalized
}

func normalisePersonaName(session *ClientSession, content string) string {
	session.Mutex.Lock()
	v := session.Voice
	session.Mutex.Unlock()

	vName := strings.ToLower(extractVoiceName(v))
	personasMutex.RLock()
	persona, personaOk := personas[vName]
	personasMutex.RUnlock()

	if personaOk && persona.NameMutations != "" {
		mutations := strings.Fields(persona.NameMutations)
		oldContent := content
		for _, m := range mutations {
			if m == "" {
				continue
			}
			re := regexp.MustCompile("(?i)\\b" + regexp.QuoteMeta(m) + "\\b")
			if re.MatchString(content) {
				newContent := re.ReplaceAllString(content, persona.Name)
				log.Printf("[STT] Match found! Mutation '%s' replaced. '%s' -> '%s'", m, content, newContent)
				content = newContent
			}
		}
		if content != oldContent {
			log.Printf("[STT] Prompt normalised: '%s' -> '%s'", oldContent, content)
		}
	}
	return content
}

func shouldProcessPrompt(session *ClientSession, prompt string, baseTime time.Time) bool {
	if !session.PassiveAssistant {
		return true // Always attentive
	}

	if baseTime.IsZero() {
		baseTime = time.Now()
	}

	lowerPrompt := strings.ToLower(prompt)
	configMutex.RLock()
	wakeWords := config.WakeWords
	passiveWindowSeconds := config.PassiveWindowSeconds
	configMutex.RUnlock()

	// Check persona-specific wake words (Name and Mutations)
	session.Mutex.Lock()
	v := session.Voice
	session.Mutex.Unlock()
	vName := strings.ToLower(extractVoiceName(v))
	personasMutex.RLock()
	p, hasP := personas[vName]
	personasMutex.RUnlock()

	if hasP {
		pWords := []string{strings.ToLower(p.Name)}
		if p.NameMutations != "" {
			pWords = append(pWords, strings.Fields(strings.ToLower(p.NameMutations))...)
		}
		for _, word := range pWords {
			if word != "" && strings.Contains(lowerPrompt, word) {
				session.Mutex.Lock()
				session.LastActiveTime = time.Now()
				session.Mutex.Unlock()
				log.Printf("[RUMBLE] Persona wake word detected at %v: '%s' (Persona: %s)", baseTime.Format("15:04:05.000"), word, p.Name)
				return true
			}
		}
	}

	for _, word := range wakeWords {
		if strings.Contains(lowerPrompt, word) {
			session.Mutex.Lock()
			session.LastActiveTime = time.Now()
			session.Mutex.Unlock()
			log.Printf("[RUMBLE] Wake word detected at %v: '%s'", baseTime.Format("15:04:05.000"), word)
			return true
		}
	}

	session.Mutex.Lock()
	recentlyActive := baseTime.Sub(session.LastActiveTime) < time.Duration(passiveWindowSeconds)*time.Second
	if recentlyActive {
		session.LastActiveTime = time.Now() // Reset the window from the moment of processing
	}
	session.Mutex.Unlock()

	if recentlyActive {
		log.Printf("[RUMBLE] Processed prompt within 60s active window (Start: %v).", baseTime.Format("15:04:05.000"))
		return true
	}

	log.Printf("[RUMBLE] Ignored prompt in Passive Mode (Start: %v): '%s'", baseTime.Format("15:04:05.000"), prompt)
	return false
}

// filterWhisperText determines if a transcription should be ignored as an artifact/hallucination
func filterWhisperText(text string) (string, bool) {
	text = strings.TrimSpace(text)
	text = strings.ReplaceAll(text, "[BLANK_AUDIO]", "")
	// Also remove common Whisper noise patterns
	text = strings.ReplaceAll(text, "[Audio]", "")
	text = strings.ReplaceAll(text, "(silence)", "")
	text = strings.TrimSpace(text)

	if text == "" || text == "." || text == "..." {
		return "", true
	}

	lower := strings.ToLower(text)
	// Remove common punctuation for hallucination check
	cleaner := strings.Trim(lower, ".,!? ")

	// Common Whisper artifacts/hallucinations
	artifacts := []string{
		"thank you",
		"thank you.",
		"thank you for watching",
		"thanks for watching",
		"bye",
		"goodbye",
		"you",
		"please like and subscribe",
	}

	for _, a := range artifacts {
		if cleaner == a {
			return "", true
		}
	}

	// Annotations like [BEEPING], (music), [AUDIO_OUT], etc.
	// Check if it's entirely wrapped in brackets or parentheses
	if (strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]")) ||
		(strings.HasPrefix(text, "(") && strings.HasSuffix(text, ")")) {
		// Specific system instructions we want to handle separately
		lowerText := strings.ToLower(text)
		if lowerText == "[pause]" || lowerText == "[resume]" || lowerText == "[request_sync]" {
			return text, false // Pass through for handleSystemTranscription
		}
		return "", true
	}

	// Handle the very specific leading space " thank you" case
	if strings.Contains(lower, "thank you") && len(text) < 15 {
		return "", true
	}

	return text, false
}

// handleSystemTranscription checks for special system instructions and records them as system turns
func handleSystemTranscription(session *ClientSession, text string, ws *websocket.Conn) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "[pause]" || lower == "[resume]" || lower == "[request_sync]" || lower == "[whisper_status]" {
		log.Printf("[STT] Intercepted system instruction: %s", lower)
		session.Mutex.Lock()
		t := session.ActiveThread()
		session.appendMessage("system", text, t)
		session.Mutex.Unlock()
		saveSession(session)
		sendHistory(nil, session)

		if lower == "[request_sync]" {
			sendThreads(ws, session)
			sendSettings(ws, session)
			sendHistory(ws, session)
			sendSummary(ws, session)
		} else if lower == "[whisper_status]" {
			broadcastWhisperStatus(session)
		}
		return true
	}
	return false
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
	if ws == nil {
		return nil
	}
	session.ConnMutex.Lock()
	defer session.ConnMutex.Unlock()
	return ws.WriteMessage(msgType, data)
}

func sendOrBroadcastText(ws *websocket.Conn, session *ClientSession, data []byte) {
	session.ConnMutex.Lock()
	defer session.ConnMutex.Unlock()
	if ws != nil && session.Conns[ws].ClientType != "tool" {
		ws.WriteMessage(websocket.TextMessage, data)
	} else {
		for conn, meta := range session.Conns {
			if meta.ClientType != "tool" {
				conn.WriteMessage(websocket.TextMessage, data)
			}
		}
	}
}

func targetWebClients(session *ClientSession, data []byte) {
	session.ConnMutex.Lock()
	defer session.ConnMutex.Unlock()
	for conn, meta := range session.Conns {
		if meta.ClientType != "tool" {
			conn.WriteMessage(websocket.TextMessage, data)
		}
	}
}

func broadcastWhisperStatus(session *ClientSession) {
	whisperNodesMutex.RLock()
	nodes := whisperNodes
	data, err := json.Marshal(nodes)
	whisperNodesMutex.RUnlock()
	if err == nil {
		sendOrBroadcastText(nil, session, []byte("[WHISPER_STATUS]"+string(data)))
	}
}

// getFirstUIConn retrieves an active WebSocket connection for a standard UI client
func getFirstUIConn(session *ClientSession) *websocket.Conn {
	session.ConnMutex.Lock()
	defer session.ConnMutex.Unlock()
	for conn, meta := range session.Conns {
		if meta.ClientType != "tool" {
			return conn
		}
	}
	return nil
}

// getLastActiveUIConn retrieves the specifically last active UI connection, if still alive
func getLastActiveUIConn(session *ClientSession) *websocket.Conn {
	session.ConnMutex.Lock()
	defer session.ConnMutex.Unlock()
	if session.LastActiveConn != nil {
		if meta, exists := session.Conns[session.LastActiveConn]; exists {
			if meta.ClientType != "tool" {
				return session.LastActiveConn
			}
		}
	}
	// Fallback to any alive UI connection
	for conn, meta := range session.Conns {
		if meta.ClientType != "tool" {
			log.Printf("[Routing] Using alternate connection for %s: %s", session.ClientID, meta.Device)
			return conn
		}
	}
	log.Printf("[Routing] No UI connection found for %s", session.ClientID)
	return nil
}

// targetToolClient sends a message to exactly one registered tool
func targetToolClient(session *ClientSession, toolName string, data []byte) error {
	session.Mutex.Lock()
	tool, exists := session.Tools[toolName]
	session.Mutex.Unlock()

	if !exists || tool.Conn == nil {
		return fmt.Errorf("tool '%s' is not connected", toolName)
	}

	session.ConnMutex.Lock()
	defer session.ConnMutex.Unlock()
	return tool.Conn.WriteMessage(websocket.TextMessage, data)
}

func getContextDir(clientID string) string {
	safeID := strings.ReplaceAll(clientID, "/", "")
	safeID = strings.ReplaceAll(safeID, "\\", "")
	safeID = strings.ReplaceAll(safeID, "..", "")
	return filepath.Join(".", "context", safeID)
}

func getConfigPath(clientID string) string {
	return filepath.Join(getContextDir(clientID), "config.json")
}

func getThreadPath(clientID string, sanitizedName string, shortID string) string {
	if strings.HasPrefix(shortID, "assistant/") {
		subDir := filepath.Join(getContextDir(clientID), "assistant")
		os.MkdirAll(subDir, 0755)
		// Strip assistant/ prefix for file naming
		actualID := strings.TrimPrefix(shortID, "assistant/")
		return filepath.Join(subDir, fmt.Sprintf("chat-%s-%s.json", sanitizedName, actualID))
	}
	return filepath.Join(getContextDir(clientID), fmt.Sprintf("chat-%s-%s.json", sanitizedName, shortID))
}

func generateThreadTopic(prompt string) string {
	configMutex.RLock()
	fbURL := config.FallbackLLM.URL
	fbModels := config.FallbackLLM.Models
	configMutex.RUnlock()

	if fbURL == "" || len(fbModels) == 0 {
		return "Assistant Chat"
	}

	topicPrompt := fmt.Sprintf("Analyze this user request and provide a concise 3-4 word topic title for a chat thread. Provide ONLY the title text, no quotes, no periods, no preamble. User request: %s", prompt)

	payload := map[string]interface{}{
		"model":  fbModels[0],
		"prompt": topicPrompt,
		"stream": false,
	}

	isChat := strings.Contains(fbURL, "/chat")
	if isChat {
		payload = map[string]interface{}{
			"model": fbModels[0],
			"messages": []map[string]string{
				{"role": "user", "content": topicPrompt},
			},
			"stream": false,
		}
	}

	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(fbURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[Assistant] Failed to call FallbackLLM for topic generation: %v", err)
		return "Assistant Chat"
	}
	defer resp.Body.Close()

	var res struct {
		Response string `json:"response"`
		Message  struct {
			Content string `json:"content"`
		} `json:"message"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		log.Printf("[Assistant] Failed to parse topic generation response: %v", err)
		return "Assistant Chat"
	}

	topic := res.Response
	if isChat {
		topic = res.Message.Content
	}

	topic = strings.TrimSpace(topic)
	topic = strings.Trim(topic, "\"`.*")
	if topic == "" {
		topic = "Assistant Chat"
	}
	if len(topic) > 50 {
		topic = topic[:47] + "..."
	}
	return topic
}


func sanitizeTitle(title string) string {
	reg, _ := regexp.Compile("[^a-zA-Z0-9]+")
	sanitized := reg.ReplaceAllString(title, "")
	if sanitized == "" {
		sanitized = "untitled"
	}
	return sanitized
}

func loadSession(clientID string) *ClientSession {
	session := &ClientSession{ClientID: clientID, Threads: make(map[string]*Thread)}

	// Load config
	configData, err := os.ReadFile(getConfigPath(clientID))
	if err == nil {
		json.Unmarshal(configData, session)
	}
	if session.Threads == nil {
		session.Threads = make(map[string]*Thread)
	}

	// Load SystemLog
	eventLogData, err := os.ReadFile(filepath.Join(getContextDir(clientID), "event-log.json"))
	if err == nil {
		var systemLog []ChatMessage
		if err := json.Unmarshal(eventLogData, &systemLog); err == nil {
			session.SystemLog = systemLog
		}
	}

	// Load threads
	ctxDir := getContextDir(clientID)
	loadFromDir := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") ||
				!strings.HasPrefix(entry.Name(), "chat-") {
				continue
			}
			threadData, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err == nil {
				var t Thread
				if err := json.Unmarshal(threadData, &t); err == nil && t.ID != "" {
					// Try to load per-thread tool history from matching system-... file
					sysFileName := "system-" + strings.TrimPrefix(entry.Name(), "chat-")
					sysData, err := os.ReadFile(filepath.Join(dir, sysFileName))
					if err == nil {
						var toolHistory []ChatMessage
						if json.Unmarshal(sysData, &toolHistory) == nil {
							t.ToolHistory = toolHistory
						}
					}
					if t.ToolHistory == nil {
						t.ToolHistory = []ChatMessage{}
					}
					session.Threads[t.ID] = &t
				}
			}
		}
	}
	loadFromDir(ctxDir)
	loadFromDir(filepath.Join(ctxDir, "assistant"))


	// Seamless migration of legacy flat structure into Thread structure
	if len(session.Threads) == 0 {
		t := &Thread{
			ID: "default", Name: "General Chat",
			History: session.History, Archive: session.Archive, Summary: session.Summary,
			ToolHistory: []ChatMessage{},
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}


		session.Threads["default"] = t
		session.ActiveThreadID = "default"
		session.History = nil
		session.Archive = nil
		session.Summary = ""
	}

	// Initialize new fields for existing threads
	for _, t := range session.Threads {
		if t.ToolHistory == nil {
			t.ToolHistory = []ChatMessage{}
		}
	}
	if session.SystemLog == nil {
		session.SystemLog = []ChatMessage{}
	}

	// Initialize theme if missing
	if (session.Theme == PersonaTheme{}) && session.Voice != "" {
		vName := strings.ToLower(extractVoiceName(session.Voice))
		personasMutex.RLock()
		if p, ok := personas[vName]; ok {
			session.Theme = p.Theme
		}
		personasMutex.RUnlock()
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
		s.Conns = make(map[*websocket.Conn]ConnMeta)
	}
	if s.Tools == nil {
		s.Tools = make(map[string]*Tool)
	}
	activeSessions[clientID] = s
	return s
}

func getMemoryPath(clientID string) string {
	return filepath.Join(getContextDir(clientID), "memory.json")
}

func getForgottenMemoryPath(clientID string) string {
	return filepath.Join(getContextDir(clientID), "forgotten.json")
}

func loadLongTermMemory(clientID string) map[string]string {
	memoryPath := getMemoryPath(clientID)
	// Fallback for migration
	safeID := strings.ReplaceAll(clientID, "/", "")
	safeID = strings.ReplaceAll(safeID, "\\", "")
	safeID = strings.ReplaceAll(safeID, "..", "")
	oldPath := filepath.Join(".", "context", safeID+"-alyx.json")

	data, err := os.ReadFile(memoryPath)
	if err != nil {
		data, err = os.ReadFile(oldPath)
		if err != nil {
			return make(map[string]string)
		}
	}

	var memMap map[string]string
	if err := json.Unmarshal(data, &memMap); err != nil {
		// Try legacy single-string format
		var oldStruct struct {
			Memory string `json:"memory"`
		}
		if err := json.Unmarshal(data, &oldStruct); err == nil && oldStruct.Memory != "" {
			memMap = map[string]string{"legacy_memory": oldStruct.Memory}
		} else {
			return make(map[string]string)
		}
	}
	return memMap
}

func saveLongTermMemory(clientID string, key string, content string) error {
	memMap := loadLongTermMemory(clientID)
	memMap[key] = content
	data, err := json.MarshalIndent(memMap, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Join(".", "context"), 0755)

	// Migration cleanup: remove old file if it exists
	safeID := strings.ReplaceAll(clientID, "/", "")
	safeID = strings.ReplaceAll(safeID, "\\", "")
	safeID = strings.ReplaceAll(safeID, "..", "")
	oldPath := filepath.Join(".", "context", safeID+"-alyx.json")
	os.Remove(oldPath)

	return os.WriteFile(getMemoryPath(clientID), data, 0644)
}

func deleteLongTermMemory(clientID string, key string) error {
	memMap := loadLongTermMemory(clientID)
	content, exists := memMap[key]
	if !exists {
		return fmt.Errorf("memory key '%s' not found", key)
	}

	delete(memMap, key)

	// Save updated memory
	data, _ := json.MarshalIndent(memMap, "", "  ")
	os.WriteFile(getMemoryPath(clientID), data, 0644)

	// Append to forgotten.json
	forgottenPath := getForgottenMemoryPath(clientID)
	var forgotten map[string][]map[string]string
	fData, err := os.ReadFile(forgottenPath)
	if err == nil {
		json.Unmarshal(fData, &forgotten)
	}
	if forgotten == nil {
		forgotten = make(map[string][]map[string]string)
	}

	entry := map[string]string{
		"content":      content,
		"forgotten_at": time.Now().Format(time.RFC3339),
	}
	forgotten[key] = append(forgotten[key], entry)

	fOut, _ := json.MarshalIndent(forgotten, "", "  ")
	return os.WriteFile(forgottenPath, fOut, 0644)
}

func saveSession(session *ClientSession) {
	session.Mutex.Lock()
	defer session.Mutex.Unlock()
	if session.ClientStorage {
		os.RemoveAll(getContextDir(session.ClientID))
		return // Ephemeral mode: do not save to disk
	}

	ctxDir := getContextDir(session.ClientID)
	os.MkdirAll(ctxDir, 0755)

	// Save active threads
	activeFiles := map[string]bool{
		"config.json":    true,
		"memory.json":    true,
		"forgotten.json": true,
		"event-log.json": true,
	}

	for threadID, t := range session.Threads {
		sanitized := sanitizeTitle(t.Name)
		shortID := threadID
		if len(shortID) > 6 && !strings.HasPrefix(shortID, "assistant/") && shortID != "default" {
			shortID = shortID[:6]
		}

		chatPath := getThreadPath(session.ClientID, sanitized, shortID)
		chatFileName := filepath.Base(chatPath)

		// Track relative path for cleanup logic
		relPath := chatFileName
		if strings.HasPrefix(shortID, "assistant/") {
			relPath = filepath.Join("assistant", chatFileName)
		}
		activeFiles[relPath] = true

		// Save tool history separately; omit field when serializing chat
		originalToolHistory := t.ToolHistory
		t.ToolHistory = nil
		if data, err := json.MarshalIndent(t, "", "  "); err == nil {
			os.WriteFile(chatPath, data, 0644)
		}
		t.ToolHistory = originalToolHistory

		// Handle system history file
		sysPath := strings.Replace(chatPath, "chat-", "system-", 1)
		sysFileName := filepath.Base(sysPath)
		sysRelPath := sysFileName
		if strings.HasPrefix(shortID, "assistant/") {
			sysRelPath = filepath.Join("assistant", sysFileName)
		}

		if len(originalToolHistory) > 0 {
			activeFiles[sysRelPath] = true
			if sysData, err := json.MarshalIndent(originalToolHistory, "", "  "); err == nil {
				os.WriteFile(sysPath, sysData, 0644)
			}
		} else {
			os.Remove(sysPath)
		}
	}

	// Temporarily hide Threads and SystemLog to marshal only config cleanly
	originalThreads := session.Threads
	originalSystemLog := session.SystemLog
	session.Threads = nil
	session.SystemLog = nil
	if data, err := json.MarshalIndent(session, "", "  "); err == nil {
		os.WriteFile(getConfigPath(session.ClientID), data, 0644)
	}

	if len(originalSystemLog) > 0 {
		if eventData, err := json.MarshalIndent(originalSystemLog, "", "  "); err == nil {
			os.WriteFile(filepath.Join(ctxDir, "event-log.json"), eventData, 0644)
		}
	} else {
		os.Remove(filepath.Join(ctxDir, "event-log.json"))
	}

	session.Threads = originalThreads
	session.SystemLog = originalSystemLog

	// Cleanup old thread files (including in assistant/ subfolder)
	cleanupDir := func(dir string, prefix string) {
		if entries, err := os.ReadDir(dir); err == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				fullRel := filepath.Join(prefix, entry.Name())
				if !activeFiles[fullRel] && strings.HasSuffix(entry.Name(), ".json") {
					os.Remove(filepath.Join(dir, entry.Name()))
				}
			}
		}
	}
	cleanupDir(ctxDir, "")
	cleanupDir(filepath.Join(ctxDir, "assistant"), "assistant")
}


func trackTokens(session *ClientSession, key string, tokens int64) {
	if tokens <= 0 {
		return
	}
	session.Mutex.Lock()
	defer session.Mutex.Unlock()
	if session.TokenUsage == nil {
		session.TokenUsage = make(map[string]int64)
	}
	if key == "" {
		key = "default"
	}
	session.TokenUsage[key] += tokens

	// Windowed tracking for TPM
	if session.ModelUsage == nil {
		session.ModelUsage = make(map[string]*ModelUsage)
	}
	usage, ok := session.ModelUsage[key]
	if !ok {
		usage = &ModelUsage{}
		session.ModelUsage[key] = usage
	}
	usage.TokenSamples = append(usage.TokenSamples, TokenUsageSample{
		Timestamp: time.Now(),
		Count:     tokens,
	})

	// Prune old samples
	oneMinuteAgo := time.Now().Add(-time.Minute)
	var validSamples []TokenUsageSample
	for _, ts := range usage.TokenSamples {
		if ts.Timestamp.After(oneMinuteAgo) {
			validSamples = append(validSamples, ts)
		}
	}
	usage.TokenSamples = validSamples
}

func (s *ClientSession) isFallbackModel(model string) bool {
	configMutex.RLock()
	defer configMutex.RUnlock()
	for _, m := range config.FallbackLLM.Models {
		if m == model || strings.HasPrefix(model, m+":") {
			return true
		}
	}
	return false
}

func getMidnightPTBound(now time.Time) time.Time {
	// 7am UTC is midnight PT (PDT) as specifically requested
	boundary := time.Date(now.Year(), now.Month(), now.Day(), 7, 0, 0, 0, time.UTC)
	if now.Before(boundary) {
		boundary = boundary.AddDate(0, 0, -1)
	}
	return boundary
}

func (s *ClientSession) checkRateLimitUnsafe(model string) (string, error) {
	configMutex.RLock()
	limits, ok := config.ModelLimits[model]
	if !ok {
		limits = config.DefaultLimit
	}
	configMutex.RUnlock()

	// If no limits defined at all, allow
	if limits.RPM <= 0 && limits.TPM <= 0 && limits.RPD <= 0 {
		return "", nil
	}

	// Fallback models are exempt from checking other models' limits
	// But in this context, we check if the SPECIFIED model is a fallback.
	if s.isFallbackModel(model) {
		return "", nil
	}

	s.Mutex.Lock()
	defer s.Mutex.Unlock()

	usage, ok := s.ModelUsage[model]
	if !ok {
		return "", nil
	}

	now := time.Now()
	oneMinuteAgo := now.Add(-time.Minute)
	midnightPT := getMidnightPTBound(now)

	var warning string

	// Check RPD
	if limits.RPD > 0 {
		count := 0
		for _, t := range usage.RequestTimes {
			if t.After(midnightPT) {
				count++
			}
		}
		if count >= limits.RPD {
			return "", fmt.Errorf("Daily request limit reached for model %s (%d/%d)", model, count, limits.RPD)
		}
		if float64(count)/float64(limits.RPD) >= 0.9 {
			warning = fmt.Sprintf("You are approaching the daily request limit for %s (%d/%d)", model, count, limits.RPD)
		}
	}

	// Check RPM
	rpmCount := 0
	for i := len(usage.RequestTimes) - 1; i >= 0; i-- {
		if usage.RequestTimes[i].After(oneMinuteAgo) {
			rpmCount++
		} else {
			break
		}
	}
	if limits.RPM > 0 {
		if rpmCount >= limits.RPM {
			return "", fmt.Errorf("Per-minute request limit reached for model %s (%d/%d)", model, rpmCount, limits.RPM)
		}
		if float64(rpmCount)/float64(limits.RPM) >= 0.9 && warning == "" {
			warning = fmt.Sprintf("You are approaching the per-minute request limit for %s (%d/%d)", model, rpmCount, limits.RPM)
		}
	}

	// Check TPM
	var tpmCount int64
	for _, ts := range usage.TokenSamples {
		if ts.Timestamp.After(oneMinuteAgo) {
			tpmCount += ts.Count
		}
	}
	if limits.TPM > 0 {
		if tpmCount >= int64(limits.TPM) {
			return "", fmt.Errorf("Per-minute token limit reached for model %s (%d/%d tokens used)", model, tpmCount, limits.TPM)
		}
		if float64(tpmCount)/float64(limits.TPM) >= 0.9 && warning == "" {
			warning = fmt.Sprintf("You are approaching the per-minute token limit for %s (%d/%d used)", model, tpmCount, limits.TPM)
		}
	}

	return warning, nil
}

func (s *ClientSession) checkRateLimit(model string) (string, error) {
	configMutex.RLock()
	limits, ok := config.ModelLimits[model]
	if !ok {
		limits = config.DefaultLimit
	}
	configMutex.RUnlock()

	// If no limits defined at all, allow
	if limits.RPM <= 0 && limits.TPM <= 0 && limits.RPD <= 0 {
		return "", nil
	}

	// Fallback models are exempt
	if s.isFallbackModel(model) {
		return "", nil
	}

	s.Mutex.Lock()
	defer s.Mutex.Unlock()

	if s.ModelUsage == nil {
		s.ModelUsage = make(map[string]*ModelUsage)
	}
	usage, ok := s.ModelUsage[model]
	if !ok {
		usage = &ModelUsage{}
		s.ModelUsage[model] = usage
	}

	now := time.Now()
	oneMinuteAgo := now.Add(-time.Minute)
	oneDayAgo := now.Add(-24 * time.Hour)
	midnightPT := getMidnightPTBound(now)

	// Prune old request times
	var validRequests []time.Time
	for _, t := range usage.RequestTimes {
		if t.After(oneDayAgo) {
			validRequests = append(validRequests, t)
		}
	}
	usage.RequestTimes = validRequests

	// Prune old token samples
	var validTokens []TokenUsageSample
	for _, ts := range usage.TokenSamples {
		if ts.Timestamp.After(oneMinuteAgo) {
			validTokens = append(validTokens, ts)
		}
	}
	usage.TokenSamples = validTokens

	var warning string

	// Check RPD
	if limits.RPD > 0 {
		rpdCount := 0
		for _, t := range usage.RequestTimes {
			if t.After(midnightPT) {
				rpdCount++
			}
		}
		if rpdCount >= limits.RPD {
			return "", fmt.Errorf("Daily request limit reached for model %s (%d/%d)", model, rpdCount, limits.RPD)
		}
		if float64(rpdCount)/float64(limits.RPD) >= 0.9 {
			warning = fmt.Sprintf("You are approaching the daily request limit for %s (%d/%d)", model, rpdCount, limits.RPD)
		}
	}

	// Check RPM
	rpmCount := 0
	for i := len(usage.RequestTimes) - 1; i >= 0; i-- {
		if usage.RequestTimes[i].After(oneMinuteAgo) {
			rpmCount++
		} else {
			break
		}
	}
	if limits.RPM > 0 {
		if rpmCount >= limits.RPM {
			return "", fmt.Errorf("Per-minute request limit reached for model %s (%d/%d)", model, rpmCount, limits.RPM)
		}
		if float64(rpmCount)/float64(limits.RPM) >= 0.9 && warning == "" {
			warning = fmt.Sprintf("You are approaching the per-minute request limit for %s (%d/%d)", model, rpmCount, limits.RPM)
		}
	}

	// Check TPM
	var tpmCount int64
	for _, ts := range usage.TokenSamples {
		tpmCount += ts.Count
	}
	if limits.TPM > 0 {
		if tpmCount >= int64(limits.TPM) {
			return "", fmt.Errorf("Per-minute token limit reached for model %s (%d/%d tokens used)", model, tpmCount, limits.TPM)
		}
		if float64(tpmCount)/float64(limits.TPM) >= 0.9 && warning == "" {
			warning = fmt.Sprintf("You are approaching the per-minute token limit for %s (%d/%d used)", model, tpmCount, limits.TPM)
		}
	}

	// All checks passed, record the request start time
	usage.RequestTimes = append(usage.RequestTimes, now)
	return warning, nil
}

func (s *ClientSession) getRateLimitUsageUnsafe(model string) (rpm, rpd int, tpm int64, limits ModelRateLimit, hasUsage bool) {
	configMutex.RLock()
	limits, ok := config.ModelLimits[model]
	if !ok {
		limits = config.DefaultLimit
	}
	configMutex.RUnlock()

	usage, ok := s.ModelUsage[model]
	if !ok {
		return 0, 0, 0, limits, false
	}

	now := time.Now()
	oneMinuteAgo := now.Add(-time.Minute)

	// RPM (requests in last minute)
	for i := len(usage.RequestTimes) - 1; i >= 0; i-- {
		if usage.RequestTimes[i].After(oneMinuteAgo) {
			rpm++
		} else {
			break
		}
	}

	// TPM (tokens in last minute)
	for _, ts := range usage.TokenSamples {
		if ts.Timestamp.After(oneMinuteAgo) {
			tpm += ts.Count
		}
	}

	// RPD (requests since midnight PT)
	midnightPT := getMidnightPTBound(now)
	for _, t := range usage.RequestTimes {
		if t.After(midnightPT) {
			rpd++
		}
	}

	return rpm, rpd, tpm, limits, true
}

func (s *ClientSession) getRateLimitStatus(model string) string {
	rpm, rpd, tpm, limits, _ := s.getRateLimitUsageUnsafe(model)

	// If no limits defined at all, return empty
	if limits.RPM <= 0 && limits.TPM <= 0 && limits.RPD <= 0 {
		return ""
	}

	return fmt.Sprintf("\n\n### Model Usage & Limits (%s)\n- Requests Per Minute: %d/%d\n- Tokens Per Minute: %d/%d used in last 60s\n- Requests Per Day: %d/%d since midnight PT",
		model, rpm, limits.RPM, tpm, limits.TPM, rpd, limits.RPD)
}

func (s *ClientSession) getRateLimitLogString(model string) string {
	s.Mutex.Lock()
	rpm, rpd, tpm, limits, _ := s.getRateLimitUsageUnsafe(model)
	s.Mutex.Unlock()

	return fmt.Sprintf("[%s] RPM %d/%d - TPM %d/%d - RPD %d/%d",
		model, rpm, limits.RPM, tpm, limits.TPM, rpd, limits.RPD)
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

	configMutex.RLock()
	maxArchiveTurns := config.MaxArchiveTurns
	configMutex.RUnlock()

	payload, err := json.Marshal(map[string]interface{}{
		"text":            summary,
		"archiveTurns":    archiveTurns,
		"maxArchiveTurns": maxArchiveTurns, // 100 messages / 2
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
		"userName":         session.UserName,
		"googleName":       session.GoogleName,
		"userBio":          session.UserBio,
		"provider":         session.Provider,
		"apiKey":           session.APIKey,
		"model":            session.Model,
		"voice":            session.Voice,
		"theme":            session.Theme,
		"clientStorage":    session.ClientStorage,
		"clientTts":        session.ClientTts,
		"tokenUsage":       session.TokenUsage,
		"passiveAssistant": session.PassiveAssistant,
	})
	session.Mutex.Unlock()
	if err == nil {
		sendOrBroadcastText(ws, session, []byte("[SETTINGS_SYNC]"+string(settingsJSON)))
	}
}

func sendThreads(ws *websocket.Conn, session *ClientSession) {
	session.Mutex.Lock()
	type threadInfo struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		CreatedAt time.Time `json:"createdAt"`
		UpdatedAt time.Time `json:"updatedAt"`
	}
	list := make([]threadInfo, 0) // Explicitly initialize so it marshals to [] instead of null
	for _, t := range session.Threads {
		list = append(list, threadInfo{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt})
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

	if clientType != "tool" {
		cacheModelsAsync(session.APIKey)
	}

	session.ConnMutex.Lock()
	session.Conns[ws] = ConnMeta{ClientType: clientType, Device: deviceName}
	session.ConnMutex.Unlock()

	// Ephemerally store the Google name from the cookie for this session only.
	// This allows the LLM prompt to use it as a fallback if no custom name is set.
	googleNameCookie, err := r.Cookie("speax_google_name")
	if err == nil {
		googleName, _ := url.QueryUnescape(googleNameCookie.Value)
		session.GoogleName = googleName
	}

	if clientType != "tool" {
		session.Mutex.Lock()
		connectMsg := fmt.Sprintf("[System Note: User connected at %s]", time.Now().Format("Monday, January 2, 2006, 15:04 MST"))
		session.appendMessage("system", connectMsg, session.ActiveThread())
		session.Mutex.Unlock()
		saveSession(session)
	}

	defer func() {
		session.ConnMutex.Lock()
		delete(session.Conns, ws)
		session.ConnMutex.Unlock()

		if clientType == "tool" {
			session.Mutex.Lock()
			// Find and remove this specific tool connection
			for name, tool := range session.Tools {
				if tool.Conn == ws {
					delete(session.Tools, name)
					log.Printf("Tool client disconnected: %s", name)
					break
				}
			}
			session.Mutex.Unlock()
		} else {
			fmt.Printf("Client disconnected: %s (%s on %s)\n", clientID, clientType, deviceName)
			session.Mutex.Lock()
			t := session.ActiveThread()
			lastIdx := len(t.History) - 1
			if lastIdx >= 0 && strings.HasPrefix(t.History[lastIdx].Content, "[System Note: User connected at") {
				// No actual turns were generated, so expunge the connection timestamp
				t.History = t.History[:lastIdx]
			} else {
				disconnectMsg := fmt.Sprintf("[System Note: User disconnected at %s]", time.Now().Format("Monday, January 2, 2006, 15:04 MST"))
				session.appendMessage("system", disconnectMsg, t)
			}
			session.Mutex.Unlock()
			saveSession(session)
		}
		ws.Close()
	}()

	for {
		messageType, p, err := ws.ReadMessage()
		if err != nil {
			return
		}

		// Update LastActiveConn whenever we receive a message that isn't a heart-beat or sync request
		if clientType != "tool" {
			session.Mutex.Lock()
			session.LastActiveConn = ws
			session.Mutex.Unlock()
		}

		// Handle incoming text (TTS Request)
		if messageType == websocket.TextMessage {
			text := string(p)

			if text == "[CANCEL]" {
				session.BufferMutex.Lock()
				session.StreamingBuffer = nil
				session.ActiveSeqID = 0
				session.BufferMutex.Unlock()
				log.Printf("[CANCEL] Session %s cancelled recording.", session.ClientID)
				continue
			}

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

			if strings.HasPrefix(text, "[TOOL_REGISTER]") {
				payloadPart := strings.TrimPrefix(text, "[TOOL_REGISTER]")
				var reg struct {
					ToolName string       `json:"toolName"`
					Actions  []ToolAction `json:"actions"`
				}
				if err := json.Unmarshal([]byte(payloadPart), &reg); err == nil {
					session.Mutex.Lock()
					session.Tools[reg.ToolName] = &Tool{
						Name:    reg.ToolName,
						Actions: reg.Actions,
						Conn:    ws,
					}
					session.Mutex.Unlock()
					log.Printf("Tool client registered: %s with %d actions", reg.ToolName, len(reg.Actions))
				}
				continue
			}

			if strings.HasPrefix(text, "[TOOL_EVENT]") {
				payloadPart := strings.TrimPrefix(text, "[TOOL_EVENT]")
				var event struct {
					ToolName string `json:"toolName"`
					Message  string `json:"message"`
				}
				if err := json.Unmarshal([]byte(payloadPart), &event); err == nil {
					log.Printf("Received tool event from %s: %s", event.ToolName, event.Message)

					session.Mutex.Lock()
					t := session.ActiveThread()
					session.appendMessage("system", fmt.Sprintf("[Tool Event (%s)]: %s", event.ToolName, event.Message), t)
					session.Mutex.Unlock()
					saveSession(session)

					// Auto-trigger the AI to handle the event (DEBOUNCED)
					session.Mutex.Lock()
					if session.ToolDebounceTimer != nil {
						session.ToolDebounceTimer.Stop()
					}
					session.ToolDebounceTimer = time.AfterFunc(2*time.Second, func() {
						log.Printf("[LLM] Tool event trigger: AI auto-resume for %s.", session.ClientID)
						ctx, cancel := context.WithCancel(context.Background())
						session.Mutex.Lock()
						session.ActiveCancel = cancel
						session.Mutex.Unlock()

						if err := streamLLMAndTTS(ctx, "[SYSTEM: Tool event received. Respond to user if necessary.]", ws, session); err != nil {
							log.Println("LLM auto-resume error:", err)
						}
					})
					session.Mutex.Unlock()
				}
				continue
			}

			if strings.HasPrefix(text, "[TOOL_RESULT]") {
				payloadPart := strings.TrimPrefix(text, "[TOOL_RESULT]")
				var result struct {
					ExecutionId string      `json:"executionId"`
					ToolName    string      `json:"toolName"`
					ActionName  string      `json:"actionName"`
					Status      string      `json:"status"`
					Data        interface{} `json:"data"`
					Error       interface{} `json:"error"`
				}

				if err := json.Unmarshal([]byte(payloadPart), &result); err == nil {
					log.Printf("Received tool result for %s: %s", result.ExecutionId, result.Status)

					// Send the completion event to standard UI web clients
					eventBytes, _ := json.Marshal(result)
					targetWebClients(session, []byte("[TOOL_UI_EVENT]"+string(eventBytes)))

					// We inject this silently into the history as a system message so the LLM knows what happened
					session.Mutex.Lock()
					t := session.ActiveThread()
					resultDetails := make(map[string]interface{})
					if result.Status == "error" {
						resultDetails["error"] = result.Error
					} else {
						resultDetails["data"] = result.Data
					}
					resultJson, _ := json.Marshal(resultDetails)
					nativeName := fmt.Sprintf("%s_%s", result.ToolName, result.ActionName)
					session.appendNativeToolResult(nativeName, string(resultJson), t, result.ExecutionId)
					session.Mutex.Unlock()
					saveSession(session)

					// Auto-trigger the AI to analyze the result and respond (DEBOUNCED)
					session.Mutex.Lock()
					if session.ToolDebounceTimer != nil {
						session.ToolDebounceTimer.Stop()
					}

					session.ToolDebounceTimer = time.AfterFunc(2*time.Second, func() {
						log.Printf("[LLM] Debounce timer expired for %s, triggering auto-resume.", session.ClientID)

						ctx, cancel := context.WithCancel(context.Background())

						session.Mutex.Lock()
						session.ActiveCancel = cancel
						session.Mutex.Unlock()

						// Passing nil for ws triggers a targeted response to the last active UI client
						if err := streamLLMAndTTS(ctx, "[SYSTEM: The tool execution has completed and the results are recorded in the system log above. Please analyze them and respond to the user.]", nil, session); err != nil {
							log.Println("LLM auto-resume stream error:", err)
						}
						log.Println("[LLM] Auto-resume Stream complete.")
					})
					session.Mutex.Unlock()
				}
				continue
			}

			if text == "[INTERRUPT]" {
				session.Mutex.Lock()
				if session.ActiveCancel != nil {
					session.ActiveCancel()
					session.ActiveCancel = nil
				}
				session.Mutex.Unlock()
				continue
			}

			if text == "[CLEAR_HISTORY]" {
				session.Mutex.Lock()
				t := session.ActiveThread()
				t.History = []ChatMessage{}
				t.Archive = []ChatMessage{}
				t.Summary = ""
				t.ToolHistory = []ChatMessage{}
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
				if name == "" {
					name = "New Thread"
				}
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

			if strings.HasPrefix(text, "[SET_ASSISTANT:") {
				isAssistant := strings.Contains(text, "true")
				session.Mutex.Lock()
				wasAssistant := session.IsAssistant
				session.IsAssistant = isAssistant

				// If we are starting a NEW assistant session, switch away from any previous assistant thread
				// so that streamLLMAndTTS will correctly trigger a new topic-labeled thread for this session.
				if isAssistant && !wasAssistant {
					if strings.HasPrefix(session.ActiveThreadID, "assistant/") {
						session.ActiveThreadID = "default"
						log.Printf("[Assistant] Resetting active thread to default for new assistant session.")
					}
				}
				session.Mutex.Unlock()
				log.Printf("[Assistant] Session IsAssistant sticky flag set to: %v", isAssistant)
				continue
			}



			if strings.HasPrefix(text, "[SETTINGS]") {

				var settings struct {
					UserName         string `json:"userName"`
					GoogleName       string `json:"googleName"`
					UserBio          string `json:"userBio"`
					Provider         string `json:"provider"`
					APIKey           string `json:"apiKey"`
					Model            string `json:"model"`
					Voice            string `json:"voice"`
					ClientStorage    bool   `json:"clientStorage"`
					ClientTts        bool   `json:"clientTts"`
					PassiveAssistant bool   `json:"passiveAssistant"`
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
						session.ClientTts != settings.ClientTts ||
						session.PassiveAssistant != settings.PassiveAssistant
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
						session.PassiveAssistant = settings.PassiveAssistant

						// Clear fallback state if model/provider changed manually
						session.FallbackOriginalModel = ""
						session.FallbackOriginalProvider = ""

						// Update theme if voice changed
						vName := strings.ToLower(extractVoiceName(session.Voice))
						personasMutex.RLock()
						if p, ok := personas[vName]; ok {
							session.Theme = p.Theme
						} else {
							session.Theme = PersonaTheme{} // Reset if no persona
						}
						personasMutex.RUnlock()
					}
					session.Mutex.Unlock()
					if changed {
						log.Printf("Updated settings for client %s: User=%s, Provider=%s, Model=%s, Voice=%s, Passive=%v", session.ClientID, session.UserName, session.Provider, session.Model, session.Voice, session.PassiveAssistant)
						saveSession(session)
						sendSettings(nil, session)
					}
				}
				continue
			}

			if strings.HasPrefix(text, "[TYPED_PROMPT") || strings.HasPrefix(text, "[TEXT_PROMPT") {
				closeIdx := strings.Index(text, "]")
				if closeIdx == -1 {
					goto skipPrompt
				}

				isTyped := strings.HasPrefix(text, "[TYPED_PROMPT")
				tagContent := text[1:closeIdx]
				prompt := strings.TrimSpace(text[closeIdx+1:])
				if strings.HasPrefix(prompt, ":") {
					prompt = strings.TrimSpace(prompt[1:])
				}

				if prompt != "" {
					baseTime := time.Now()
					content := prompt

					// Extract timestamp from tag if present: [TYPED_PROMPT:123456789]
					if strings.Contains(tagContent, ":") {
						parts := strings.SplitN(tagContent, ":", 2)
						tsStr := strings.TrimSpace(parts[1])
						if ts, err := strconv.ParseInt(tsStr, 10, 64); err == nil {
							baseTime = time.Unix(0, ts*int64(time.Millisecond))
						}
					}

					// Also support legacy [TEXT_PROMPT:TIMESTAMP]:Content if someone still uses it
					if strings.HasPrefix(content, "[") {
						if cId := strings.Index(content, "]:"); cId != -1 {
							tsStr := strings.Trim(content[1:cId], " ")
							if ts, err := strconv.ParseInt(tsStr, 10, 64); err == nil {
								baseTime = time.Unix(0, ts*int64(time.Millisecond))
								content = strings.TrimSpace(content[cId+2:])
							}
						}
					}

					isAssistant := strings.Contains(tagContent, "ASSISTANT")

					content = normalisePersonaName(session, content)
					shouldProcess := isTyped || shouldProcessPrompt(session, content, baseTime)

					if shouldProcess {
						if isTyped {
							session.Mutex.Lock()
							session.LastActiveTime = time.Now()
							session.Mutex.Unlock()
							log.Printf("[RUMBLE] Manual typed prompt received. Forcing Passive Assistant into Active mode.")
						}

						if isAssistant {
							session.Mutex.Lock()
							if !session.IsAssistant {
								log.Printf("[Assistant] ASSISTANT flag detected in prompt tag. Enabling session sticky flag.")
								session.IsAssistant = true
							}
							session.Mutex.Unlock()
						}



						go func(p string, bt time.Time) {
							session.Mutex.Lock()
							if session.ActiveCancel != nil {
								session.ActiveCancel()
							}
							ctx, cancel := context.WithCancel(context.Background())
							session.ActiveCancel = cancel
							session.Mutex.Unlock()

							sendOrBroadcastText(nil, session, []byte("[CHAT]:"+p))

							if handleSystemTranscription(session, p, ws) {
								return
							}

							log.Printf("[LLM] Processing text prompt: '%s' (isTyped=%v, Start=%v)", p, isTyped, bt.Format("15:04:05.000"))
							if err := streamLLMAndTTS(ctx, p, ws, session); err != nil {
								log.Println("LLM stream error:", err)
							}
						}(content, baseTime)
					}
				}
				continue
			}

		skipPrompt:

			if text == "[PLAYBACK_COMPLETE]" {
				session.Mutex.Lock()
				if time.Now().After(session.PassiveBlockUntil) {
					session.LastActiveTime = time.Now()
					log.Printf("[RUMBLE] Passive Assistant timeout reset triggered by audio playback complete.")
				} else {
					log.Printf("[RUMBLE] Passive Assistant timeout reset SUPPRESSED by PassiveBlockUntil (%.1fs remaining).", time.Until(session.PassiveBlockUntil).Seconds())
				}
				session.Mutex.Unlock()
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
						if endIdx > len(t.Archive) {
							endIdx = len(t.Archive)
						}
						t.Archive = append(t.Archive[:idx], t.Archive[endIdx:]...)
						isArchiveDelete = true
					} else if idx >= archiveLen && idx < archiveLen+len(t.History) {
						hIdx := idx - archiveLen
						// Delete the target message AND the one immediately after it
						endIdx := hIdx + 2
						if endIdx > len(t.History) {
							endIdx = len(t.History)
						}
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
				log.Printf("[TTS] Catch-all TTS triggered for message: '%s'", t)
				session.Mutex.Lock()
				v := session.Voice
				session.Mutex.Unlock()
				if v == "" {
					configMutex.RLock()
					v = config.DefaultVoice
					configMutex.RUnlock()
				}
				audioBytes, err := queryTTS(t, v, session.UserName, "", "") // System prompts usually don't need phonetic names here unless specific
				if err != nil {
					log.Println("TTS error:", err)
					return
				}
				safeWrite(ws, session, websocket.BinaryMessage, audioBytes)
			}(text)
		}

		// Handle incoming audio (STT Request)
		if messageType == websocket.BinaryMessage {
			// Hybrid-Protocol Gateway: Check for Streaming Magic Byte 0xFF
			if len(p) > 9 && p[0] == 0xFF {
				handleStreamingAudio(ws, session, p)
				continue
			}

			if clientType != "tool" {
				session.Mutex.Lock()
				session.LastActiveConn = ws
				session.Mutex.Unlock()
			}
			// Minimum byte length check (~0.5 seconds of 16-bit PCM is 16000 bytes)
			// Now with 8-byte timestamp prefix
			if len(p) < 16008 {
				log.Printf("[STT] Ignored short audio chunk: %d bytes (min 16008 with timestamp)", len(p))
				continue
			}

			// Extract 8-byte Big Endian timestamp (milliseconds since epoch)
			startTimeMs := int64(binary.BigEndian.Uint64(p[:8]))
			audioData := p[8:]
			baseTime := time.Unix(0, startTimeMs*int64(time.Millisecond))

			// Process the complete phrase sent by the client
			session.Mutex.Lock()
			isAssAtStart := session.IsAssistant
			session.Mutex.Unlock()

			go func(audio []byte, bt time.Time, isA bool) {
				text, err := queryWhisper(audio, session)

				if err != nil {
					if err == ErrNoHealthyNodes {
						session.Mutex.Lock()
						t := session.ActiveThread()
						failMsg := "[System: All STT nodes are currently offline or unhealthy. The user's most recent audio could not be transcribed. Please inform the user of this service interruption and offer to help via text instead.]"
						session.appendMessage("system", failMsg, t)
						session.Mutex.Unlock()

						ctx, cancel := context.WithCancel(context.Background())
						session.Mutex.Lock()
						session.ActiveCancel = cancel
						session.Mutex.Unlock()

						if err := streamLLMAndTTS(ctx, "[SYSTEM_STT_FAILURE]", ws, session); err != nil {
							log.Println("LLM stt failure stream error:", err)
						}
					}
					log.Println("Whisper error:", err)
					return
				}
				log.Printf("[STT] Whisper Transcribed: '%s' (Start: %v)", text, bt.Format("15:04:05.000"))

				filteredText, isArtifact := filterWhisperText(text)
				if isArtifact {
					log.Printf("[STT] Suppressed Whisper artifact: '%s'", text)
					safeWrite(ws, session, websocket.TextMessage, []byte("[IGNORED]"))
					return
				}
				text = filteredText
				if handleSystemTranscription(session, text, ws) {
					return
				}

				if shouldProcessPrompt(session, text, bt) {
					session.Mutex.Lock()
					if session.ActiveCancel != nil {
						session.ActiveCancel()
					}
					ctx, cancel := context.WithCancel(context.Background())
					session.ActiveCancel = cancel
					session.Mutex.Unlock()

					sendOrBroadcastText(nil, session, []byte("[CHAT]:"+text))

					if isA {
						text += " :ASSISTANT"
					}
					if err := streamLLMAndTTS(ctx, text, ws, session); err != nil {

						log.Println("LLM stream error:", err)
					}
					log.Println("[LLM] Stream complete.")
				} else {
					safeWrite(ws, session, websocket.TextMessage, []byte("[IGNORED]"))
				}
			}(audioData, baseTime, isAssAtStart)
		}
	}
}

func handleStreamingAudio(ws *websocket.Conn, session *ClientSession, p []byte) {
	// Protocol: [0xFF][TYPE][SEQ_ID (8 bytes)][DATA]
	packetType := p[1]
	seqID := binary.BigEndian.Uint64(p[2:10])
	audioData := p[10:]

	session.BufferMutex.Lock()
	defer session.BufferMutex.Unlock()

	// Reset if sequence changes (prevents interleaving/orphaned buffers)
	if session.ActiveSeqID != int64(seqID) {
		session.ActiveSeqID = int64(seqID)
		session.StreamingBuffer = nil // Reset on new sequence
		session.StreamingStartTime = time.Now()
	}

	if packetType == 0x01 { // STREAM
		session.StreamingBuffer = append(session.StreamingBuffer, audioData...)
		log.Printf("[STT-Stream] Appended %d bytes to sequence %d", len(audioData), seqID)
	} else if packetType == 0x02 { // END
		fullBuffer := session.StreamingBuffer
		session.StreamingBuffer = nil
		session.ActiveSeqID = 0

		go processStreamingWhisper(ws, session, fullBuffer)
	}
}

func processStreamingWhisper(ws *websocket.Conn, session *ClientSession, audio []byte) {
	text, err := queryWhisper(audio, session)
	if err != nil {
		if err == ErrNoHealthyNodes {
			session.Mutex.Lock()
			t := session.ActiveThread()
			failMsg := "[System: All STT nodes are currently offline or unhealthy. The user's most recent audio could not be transcribed. Please inform the user of this service interruption and offer to help via text instead.]"
			session.appendMessage("system", failMsg, t)
			session.Mutex.Unlock()

			ctx, cancel := context.WithCancel(context.Background())
			session.Mutex.Lock()
			session.ActiveCancel = cancel
			session.Mutex.Unlock()

			if err := streamLLMAndTTS(ctx, "[SYSTEM_STT_FAILURE]", ws, session); err != nil {
				log.Println("LLM stt failure stream error:", err)
			}
		}
		log.Println("Whisper error:", err)
		return
	}
	log.Printf("[STT-Stream] Final Whisper Transcribed: '%s'", text)

	filteredText, isArtifact := filterWhisperText(text)
	if isArtifact {
		log.Printf("[STT-Stream] Suppressed Whisper artifact: '%s'", text)
		safeWrite(ws, session, websocket.TextMessage, []byte("[IGNORED]"))
		return
	}
	text = filteredText
	text = normalisePersonaName(session, text)
	if handleSystemTranscription(session, text, ws) {
		return
	}

	if text != "" && shouldProcessPrompt(session, text, session.StreamingStartTime) {
		session.Mutex.Lock()
		if session.ActiveCancel != nil {
			session.ActiveCancel()
		}
		ctx, cancel := context.WithCancel(context.Background())
		session.ActiveCancel = cancel
		session.Mutex.Unlock()

		sendOrBroadcastText(nil, session, []byte("[CHAT]:"+text))

		if err := streamLLMAndTTS(ctx, text, ws, session); err != nil {
			log.Println("LLM stream error:", err)
		}
		log.Println("[LLM-Stream] Stream complete.")
	} else {
		safeWrite(ws, session, websocket.TextMessage, []byte("[IGNORED]"))
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
	binary.Write(buf, binary.LittleEndian, uint16(1)) // AudioFormat: PCM
	binary.Write(buf, binary.LittleEndian, uint16(1)) // NumChannels: Mono
	configMutex.RLock()
	sampleRate := config.SampleRate
	configMutex.RUnlock()

	binary.Write(buf, binary.LittleEndian, uint32(sampleRate))   // SampleRate: e.g. 16kHz
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate*2)) // ByteRate: SampleRate * NumChannels * BitsPerSample/8
	binary.Write(buf, binary.LittleEndian, uint16(2))            // BlockAlign: NumChannels * BitsPerSample/8
	binary.Write(buf, binary.LittleEndian, uint16(16))           // BitsPerSample: 16
	// data chunk
	buf.Write([]byte("data"))
	binary.Write(buf, binary.LittleEndian, uint32(len(pcmData)))
	buf.Write(pcmData)
	return buf.Bytes()
}

func queryWhisper(audioData []byte, session *ClientSession) (string, error) {
	if len(audioData) == 0 {
		return "", fmt.Errorf("empty audio data")
	}

	durationSecs := float64(len(audioData)) / float64(config.SampleRate*2)
	timeoutSecs := (durationSecs * 0.25) + 2.5
	timeoutDuration := time.Duration(timeoutSecs * float64(time.Second))

	wavData := addWavHeader(audioData)

	for attempt := 0; attempt < 3; attempt++ {
		node, err := getHealthyWhisperNode()
		if err != nil {
			return "", err
		}

		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="file"; filename="input.wav"`)
		h.Set("Content-Type", "audio/wav")
		part, _ := writer.CreatePart(h)
		part.Write(wavData)
		writer.Close()

		log.Printf("[STT] Sending audio to node: %s (Attempt %d)", node.URL, attempt+1)
		start := time.Now()
		resp, err := http.Post(node.URL, writer.FormDataContentType(), body)
		duration := time.Since(start)

		// Update raw metrics for every request attempt
		node.LastExecutionTime = duration
		ratio := duration.Seconds() / timeoutDuration.Seconds()
		const alpha = 0.2
		if node.RollingCutoffRatio == 0 {
			node.RollingCutoffRatio = ratio
		} else {
			node.RollingCutoffRatio = (ratio * alpha) + (node.RollingCutoffRatio * (1.0 - alpha))
		}
		broadcastWhisperStatus(session)

		node.TotalRequests++
		if err != nil || resp.StatusCode != http.StatusOK || duration > timeoutDuration {
			node.FailureCount++
			node.TotalFailures++

			statusStr := "N/A"
			if resp != nil {
				statusStr = resp.Status
			}

			if node.FailureCount >= 5 {
				node.Zombie = true
				log.Printf("[STT] Node flagged: %s (Duration: %v, Cutoff Ratio: %.2f, Status: %s, Err: %v).", node.URL, duration, ratio, statusStr, err)

				// Notify the user via a system message in the thread
				session.Mutex.Lock()
				flagMsg := fmt.Sprintf("[System Note: Whisper node %s flagged as unhealthy/slow (%v). Routing to fallback.]", node.URL, duration.Truncate(time.Millisecond))
				session.appendMessage("system", flagMsg, session.ActiveThread())
				session.Mutex.Unlock()
				saveSession(session)
				sendHistory(nil, session) // Sync history to all clients
			} else {
				log.Printf("[STT] Node attempt failed: %s (Duration: %v, Cutoff Ratio: %.2f, Status: %s, Err: %v). Failures: %d/5", node.URL, duration, ratio, statusStr, err, node.FailureCount)
			}

			if resp != nil {
				resp.Body.Close()
			}
			continue // Retry with another node
		}
		node.Zombie = false
		node.LastResponseTime = duration
		node.FailureCount = 0
		broadcastWhisperStatus(session)
		log.Printf("[STT] Node response: %s (Duration: %v, Cutoff Ratio: %.2f)", node.URL, duration.Truncate(time.Millisecond), ratio)

		defer resp.Body.Close()
		var result struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return "", err
		}
		return result.Text, nil
	}

	return "", fmt.Errorf("transcription failed after multiple attempts")
}

// buildToolSystemPrompt generates the JSON schema block for connected tools to inject into the LLM prompt.
func getInternalTools() []Tool {
	return []Tool{
		{
			Name: "LongTermMemory",
			Actions: []ToolAction{
				{
					Name:        "save",
					Description: "Save a piece of info to long-term memory for future recall. Use for facts, preferences, or important life events mentioned by the user.",
					Schema: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"key":     map[string]interface{}{"type": "string", "description": "Short mnemonic key (e.g. 'user_birthday')"},
							"content": map[string]interface{}{"type": "string", "description": "The information to remember"},
						},
						"required": []string{"key", "content"},
					},
				},
				{
					Name:        "delete",
					Description: "Forget a piece of info from long-term memory. Use when information is corrected, updated, or no longer relevant.",
					Schema: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"key": map[string]interface{}{"type": "string", "description": "The exact key to forget"},
						},
						"required": []string{"key"},
					},
				},
			},
		},
		{
			Name: "Assistant",
			Actions: []ToolAction{
				{
					Name:        "sleep",
					Description: "Enter passive mode immediately and stop listening for 15 seconds unless a wake word is used. Use this when the user says goodbye or indicates the conversation is over.",
					Schema: map[string]interface{}{
						"type":       "object",
						"properties": map[string]interface{}{},
					},
				},
			},
		},
	}
}

func getNativeToolsGemini(session *ClientSession) []interface{} {
	var functions []interface{}

	allTools := getInternalTools()
	session.Mutex.Lock()
	for _, t := range session.Tools {
		allTools = append(allTools, *t)
	}
	session.Mutex.Unlock()

	for _, tool := range allTools {
		for _, action := range tool.Actions {
			// Map ToolName.ActionName to ToolName_ActionName
			nativeName := fmt.Sprintf("%s_%s", tool.Name, action.Name)

			functions = append(functions, map[string]interface{}{
				"name":        nativeName,
				"description": action.Description,
				"parameters":  action.Schema,
			})
		}
	}

	if len(functions) == 0 {
		return nil
	}

	return []interface{}{
		map[string]interface{}{
			"function_declarations": functions,
		},
	}
}

func getNativeToolsOllama(session *ClientSession) []interface{} {
	var tools []interface{}

	allTools := getInternalTools()
	session.Mutex.Lock()
	for _, t := range session.Tools {
		allTools = append(allTools, *t)
	}
	session.Mutex.Unlock()

	for _, tool := range allTools {
		for _, action := range tool.Actions {
			nativeName := fmt.Sprintf("%s_%s", tool.Name, action.Name)

			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        nativeName,
					"description": action.Description,
					"parameters":  action.Schema,
				},
			})
		}
	}

	return tools
}

func executeNativeToolCall(session *ClientSession, nativeName string, args map[string]interface{}, ws *websocket.Conn) {
	parts := strings.SplitN(nativeName, "_", 2)
	if len(parts) < 2 {
		log.Printf("Invalid native tool name: %s", nativeName)
		return
	}

	toolName := parts[0]
	actionName := parts[1]
	executionId := fmt.Sprintf("req-%d", time.Now().UnixNano()%1000000)

	log.Printf("Executing native tool call: %s.%s (ID: %s) | Args: %v", toolName, actionName, executionId, args)

	// Log last 2 turns of conversation context for debugging tool call loops
	session.Mutex.Lock()
	t := session.ActiveThread()
	history := t.History
	session.Mutex.Unlock()
	start := len(history) - 2
	if start < 0 {
		start = 0
	}
	log.Printf("[TOOL_DEBUG] Last %d history entries leading to tool call:", len(history[start:]))
	for i, m := range history[start:] {
		label := fmt.Sprintf("  [%d] role=%s", i+1, m.Role)
		if m.ToolCall != nil {
			label += fmt.Sprintf(" (ToolCall: %s)", m.ToolCall.Name)
		} else if m.ToolResult != nil {
			label += fmt.Sprintf(" (ToolResult for: %s)", m.ToolResult.Name)
		} else {
			content := m.Content
			if len(content) > 200 {
				content = content[:200] + "..."
			}
			label += fmt.Sprintf(" content=%q", content)
		}
		log.Print(label)
	}

	if toolName == "Assistant" && actionName == "sleep" {
		session.Mutex.Lock()
		session.PassiveAssistant = true
		session.PassiveBlockUntil = time.Now().Add(15 * time.Second)
		// Set last active time to long ago to ensure immediate passive window cutoff
		session.LastActiveTime = time.Now().Add(-1 * time.Hour)

		id := session.appendNativeToolCall(nativeName, args, session.ActiveThread())
		result := "{\"status\": \"success\", \"message\": \"Assistant is now in passive mode for 15 seconds.\"}"
		session.appendNativeToolResult(nativeName, result, session.ActiveThread(), id)
		session.Mutex.Unlock()

		log.Printf("[RUMBLE] Assistant_sleep tool executed. PassiveAssistant=true, BlockUntil=%v", session.PassiveBlockUntil)
		saveSession(session)
		sendSettings(nil, session)
		return // DO NOT trigger ToolDebounceTimer (suppress LLM)
	}

	if toolName == "LongTermMemory" {
		var err error
		var statusMsg string

		if actionName == "save" {
			key, _ := args["key"].(string)
			content, _ := args["content"].(string)
			err = saveLongTermMemory(session.ClientID, key, content)
			statusMsg = "Memory saved successfully."
		} else if actionName == "delete" {
			key, _ := args["key"].(string)
			err = deleteLongTermMemory(session.ClientID, key)
			statusMsg = "Memory deleted successfully."
		}

		session.Mutex.Lock()
		id := session.appendNativeToolCall(nativeName, args, session.ActiveThread())
		var result string
		if err != nil {
			result = fmt.Sprintf("{\"error\": \"%v\"}", err)
		} else {
			result = fmt.Sprintf("{\"status\": \"success\", \"message\": \"%s\"}", statusMsg)
		}
		session.appendNativeToolResult(nativeName, result, session.ActiveThread(), id)
		session.Mutex.Unlock()

		// Trigger auto-resume
		session.Mutex.Lock()
		if session.ToolDebounceTimer != nil {
			session.ToolDebounceTimer.Stop()
		}
		session.ToolDebounceTimer = time.AfterFunc(1500*time.Millisecond, func() {
			log.Printf("[LLM] Native Tool auto-resume triggered.")
			ctx, cancel := context.WithCancel(context.Background())
			session.Mutex.Lock()
			session.ActiveCancel = cancel
			session.Mutex.Unlock()

			if err := streamLLMAndTTS(ctx, "[SYSTEM: Tool execution completed. Results recorded. Respond to user.]", ws, session); err != nil {
				log.Println("LLM auto-resume error:", err)
			}
		})
		session.Mutex.Unlock()
		return
	}

	session.Mutex.Lock()
	id := session.appendNativeToolCall(nativeName, args, session.ActiveThread())
	session.Mutex.Unlock()

	toolCall := struct {
		ToolName    string      `json:"toolName"`
		ActionName  string      `json:"actionName"`
		ExecutionId string      `json:"executionId"`
		Params      interface{} `json:"params"`
	}{
		ToolName:    toolName,
		ActionName:  actionName,
		ExecutionId: id,
		Params:      args,
	}

	executePayload, _ := json.Marshal(toolCall)
	err := targetToolClient(session, toolName, []byte("[TOOL_EXECUTE]"+string(executePayload)))
	if err != nil {
		log.Printf("Failed to route native tool call: %v", err)
		session.Mutex.Lock()
		errMsg := "{\"error\": \"Client disconnected or not found\"}"
		session.appendNativeToolResult(nativeName, errMsg, session.ActiveThread(), id)
		session.Mutex.Unlock()
	}
}

func extractVoiceName(filename string) string {
	base := strings.TrimSuffix(filename, ".onnx")

	// Strip known technical suffixes first
	re := regexp.MustCompile(`-(qint8|int8|fp16|low|medium|high|standard)$`)
	base = re.ReplaceAllString(base, "")

	parts := strings.Split(base, "-")
	if len(parts) >= 2 {
		// If it looks like lang-name (e.g. en_GB-alba), return the name
		// Check if first part looks like a language code (e.g. en_GB, en_US)
		langRe := regexp.MustCompile(`^[a-z]{2}_[A-Z]{2}$`)
		if langRe.MatchString(parts[0]) {
			return strings.Join(parts[1:], "-")
		}
	}
	// Otherwise return the remaining base
	return base
}

func getOllamaModelsInternal(isAdmin bool) []ModelData {
	var out []ModelData

	configMutex.RLock()
	chatURLs := config.OllamaChatURL
	inclusionList := config.OllamaModels
	configMutex.RUnlock()

	if len(chatURLs) == 0 {
		return out
	}

	client := &http.Client{Timeout: 5 * time.Second}

	for _, baseURL := range chatURLs {
		var tagsURL string
		parsedURL, parseErr := url.Parse(baseURL)
		if parseErr == nil && parsedURL.Scheme != "" && parsedURL.Host != "" {
			tagsURL = parsedURL.Scheme + "://" + parsedURL.Host + "/api/tags"
		} else {
			// Fallback: simple string replacement if URL parsing fails
			tagsURL = strings.Replace(baseURL, "/chat", "/tags", 1)
		}

		resp, err := client.Get(tagsURL)
		if err != nil {
			log.Printf("[Ollama] Failed to check tags at %s: %v", tagsURL, err)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("[Ollama] Tags request to %s returned status %s", tagsURL, resp.Status)
			continue
		}

		var res struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			log.Printf("[Ollama] Failed to parse Ollama tags from %s: %v", tagsURL, err)
			continue
		}

		for _, m := range res.Models {
			if !isAdmin {
				if m.Name == "gemma3:270m" {
					continue // Internal model — not exposed to regular users
				}

				if len(inclusionList) > 0 {
					found := false
					for _, allowed := range inclusionList {
						// Relaxed match: allow "llama3" to match "llama3:latest"
						if m.Name == allowed || strings.HasPrefix(m.Name, allowed+":") {
							found = true
							break
						}
					}
					if !found {
						continue
					}
				}
			}

			// Deduplicate models across multiple servers
			exists := false
			for _, existing := range out {
				if existing.ID == m.Name {
					exists = true
					break
				}
			}
			if !exists {
				out = append(out, ModelData{ID: m.Name, Name: m.Name})
			}
		}
	}
	return out
}

func selectFallbackModel() (string, string) {
	configMutex.RLock()
	fbURL := config.FallbackLLM.URL
	fbModels := config.FallbackLLM.Models
	configMutex.RUnlock()

	if fbURL == "" || len(fbModels) == 0 {
		return "", ""
	}

	// Strip any existing API path (e.g. /api/chat) so we always hit the root /api/tags endpoint
	parsedURL, parseErr := url.Parse(fbURL)
	var tagsURL string
	if parseErr == nil {
		tagsURL = parsedURL.Scheme + "://" + parsedURL.Host + "/api/tags"
	} else {
		// Fallback: best-effort strip known API suffixes
		base := strings.TrimSuffix(strings.TrimSuffix(fbURL, "/"), "/api/chat")
		base = strings.TrimSuffix(base, "/api/generate")
		tagsURL = strings.TrimSuffix(base, "/") + "/api/tags"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(tagsURL)
	if err != nil {
		log.Printf("[Fallback] Failed to check Ollama tags at %s: %v", tagsURL, err)
		return "", ""
	}
	defer resp.Body.Close()

	var res struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		log.Printf("[Fallback] Failed to parse Ollama tags: %v", err)
		return "", ""
	}

	available := make(map[string]bool)
	for _, m := range res.Models {
		available[m.Name] = true
	}

	for _, preferred := range fbModels {
		if available[preferred] {
			return "ollama", preferred
		}
	}

	// If no exact match, try partial match (e.g. "llama3" matching "llama3:latest")
	for _, preferred := range fbModels {
		for _, m := range res.Models {
			if strings.HasPrefix(m.Name, preferred+":") || m.Name == preferred {
				return "ollama", m.Name
			}
		}
	}

	return "", ""
}

func getGeminiModelsInternal(apiKey string, isAdmin bool) []ModelData {
	var out []ModelData
	if apiKey == "" {
		return out
	}
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

					if isAdmin {
						// Admins see everything
						out = append(out, ModelData{ID: strings.TrimPrefix(m.Name, "models/"), Name: displayName})
						break
					}

					// Standard user filtering based on inclusion list in config
					configMutex.RLock()
					inclusionList := config.GeminiModels
					configMutex.RUnlock()

					if len(inclusionList) > 0 {
						found := false
						id := strings.TrimPrefix(m.Name, "models/")
						for _, allowed := range inclusionList {
							if id == allowed {
								found = true
								break
							}
						}
						if found {
							out = append(out, ModelData{ID: id, Name: displayName})
						}
					} else {
						// Legacy fallback filtering if no inclusion list defined
						isFlash := strings.Contains(displayName, "Flash")
						isGemma := strings.Contains(displayName, "Gemma")
						isGemini2 := strings.Contains(displayName, "Gemini 2")
						if (isFlash || isGemma) && !isGemini2 {
							out = append(out, ModelData{ID: strings.TrimPrefix(m.Name, "models/"), Name: displayName})
						}
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
	return out
}

func getSystemStatusPrompt(session *ClientSession, provider string) string {
	var sb strings.Builder

	// 1. Whisper STT Nodes
	whisperNodesMutex.RLock()
	if len(whisperNodes) > 0 {
		sb.WriteString("\n\n### system status: Whisper STT Nodes\n")
		for _, node := range whisperNodes {
			status := "Healthy"
			if node.Zombie {
				status = "Zombie / Slow"
			}
			sb.WriteString(fmt.Sprintf("- %s: %s (last latency: %v, rolling cutoff ratio: %.2f, failures: %d/%d)\n",
				node.URL, status, node.LastExecutionTime.Truncate(time.Millisecond), node.RollingCutoffRatio, node.FailureCount, node.TotalFailures))
		}
	}
	whisperNodesMutex.RUnlock()

	// 2. Connected Users Monitor
	activeSessionsMutex.Lock()
	if len(activeSessions) > 0 {
		sb.WriteString("\n### system status: Connected Users Monitor\n")
		sb.WriteString(fmt.Sprintf("Active Sessions: %d\n", len(activeSessions)))
		for id, sess := range activeSessions {
			currentUser := ""
			if id == session.ClientID {
				currentUser = " (YOU)"
			}

			name := "Unknown"
			threadName := "(Initial Thread)"

			// Note: The caller (prepareSystemPrompt's wrapper) already holds the lock for 'session'.
			// We must not lock it again to avoid deadlocks. For other sessions, we lock normally.
			if id == session.ClientID {
				name = sess.UserName
				if name == "" {
					name = sess.GoogleName
				}
				if name == "" {
					name = id
				}
				if t, ok := sess.Threads[sess.ActiveThreadID]; ok {
					threadName = t.Name
				}
			} else {
				sess.Mutex.Lock()
				name = sess.UserName
				if name == "" {
					name = sess.GoogleName
				}
				if name == "" {
					name = id
				}
				if t, ok := sess.Threads[sess.ActiveThreadID]; ok {
					threadName = t.Name
				}
				sess.Mutex.Unlock()
			}

			sb.WriteString(fmt.Sprintf("- %s%s (Active Thread: %s)\n", name, currentUser, threadName))
		}
	}
	activeSessionsMutex.Unlock()

	return sb.String()
}

func prepareSystemPrompt(session *ClientSession, model, voiceName, provider string) string {
	configMutex.RLock()
	var basePrompt string
	if provider == "gemini" {
		basePrompt = config.SystemPromptGemini
	} else {
		basePrompt = config.SystemPromptOllama
	}
	configMutex.RUnlock()

	// 1. Get persona
	personaName := strings.ToLower(extractVoiceName(voiceName))
	personasMutex.RLock()
	p, hasPersona := personas[personaName]
	personasMutex.RUnlock()

	effectiveAssistantName := strings.Title(extractVoiceName(voiceName))
	if hasPersona && p.Name != "" {
		effectiveAssistantName = p.Name
	}

	// 2. Format base with model and effective name
	sysContent := fmt.Sprintf(basePrompt, model, effectiveAssistantName)
	sysContent = strings.ReplaceAll(sysContent, "Alyx", effectiveAssistantName)

	// 3. Persona section
	if hasPersona {
		sysContent += fmt.Sprintf("\n\n### Your Persona\n- Name: %s", effectiveAssistantName)
		if p.NameMutations != "" {
			sysContent += fmt.Sprintf("\n- Alternative Transcript Spellings: %s", p.NameMutations)
		}
		sysContent += fmt.Sprintf("\n- Tone: %s\n- Address Style: %s\n- Focus: %s\n- Interaction Style: %s\n- Constraints: %s",
			p.Tone, p.AddressStyle, p.Focus, p.InteractionStyle, p.Constraints)
	}

	// 3. User details
	roleStr := "user"
	if session.IsAdmin() {
		roleStr = "admin"
	}
	sysContent += fmt.Sprintf("\n\n### User Information\n- user_system_role: %s", roleStr)

	if session.UserName != "" {
		sysContent += fmt.Sprintf("\n- name: %s", session.UserName)
	} else if session.GoogleName != "" {
		sysContent += fmt.Sprintf("\n- name: %s", session.GoogleName)
	}
	if session.UserBio != "" {
		sysContent += fmt.Sprintf("\n- bio: %s", session.UserBio)
	}

	// 4. Metadata
	currentTime := time.Now().Format("Monday, January 2, 2006, 15:04 MST")
	sysContent += fmt.Sprintf("\n\nThe current date and time is: %s.", currentTime)
	if session.IsAdmin() {
		sysContent += getSystemStatusPrompt(session, provider)
	}

	// 5. Long-term Memory
	if memoryMap := loadLongTermMemory(session.ClientID); len(memoryMap) > 0 {
		sysContent += "\n### Long-term Memory\n"
		for k, v := range memoryMap {
			sysContent += fmt.Sprintf("- %s: %s\n", k, v)
		}
	}

	// 5b. Tool use directive — inject when tools are registered AND the model supports them
	// NOTE: caller may hold session.Mutex, so we access session.Tools directly (consistent with rest of function)
	toolsSupported := true
	if provider == "ollama" {
		toolsSupported = ollamaModelSupportsTools(model)
	} else if provider == "gemini" {
		toolsSupported = strings.Contains(strings.ToLower(model), "gemini")
	}

	if len(session.Tools) > 0 && toolsSupported {
		configMutex.RLock()
		toolDirective := config.ToolSystemPrompt
		configMutex.RUnlock()
		if toolDirective != "" {
			sysContent += "\n\n" + toolDirective
		}
	}

	// 6. Thread Summary
	t := session.ActiveThread()
	if t.Summary != "" {
		sysContent += "\n\nContext from earlier in the conversation: " + t.Summary
	}

	// 7. Rate Limit Status (Inject at bottom for recency/visibility)
	sysContent += session.getRateLimitStatus(model)

	return sysContent
}

func saveCalculatedSystemPrompt(session *ClientSession, content string) {
	promptPath := filepath.Join(getContextDir(session.ClientID), "calculated-system-prompt.md")
	os.WriteFile(promptPath, []byte(content), 0644)
}

func saveCalculatedContentWindow(session *ClientSession, body []byte) {
	// 1. Root-level for easy debugging as requested
	os.WriteFile("last-request.json", body, 0644)

	// 2. Per-client for long-term reference
	windowPath := filepath.Join(getContextDir(session.ClientID), "calculated-content-window.json")
	os.WriteFile(windowPath, body, 0644)
}

func prepareLLMHistory(msgs []ChatMessage, provider string) []ChatMessage {
	var finalMsgs []ChatMessage
	var systemNoteBuffer []string

	for _, m := range msgs {
		role := m.Role
		content := m.Content

		isSystemNote := role == "system" && m.ToolResult == nil
		// Coerce system into user messages with prefixes if no result field
		if isSystemNote {
			role = "user"
			if !strings.HasPrefix(content, "[System Note:") && !strings.HasPrefix(content, "[TOOL_RESULT") {
				content = "[System Note]: " + content
			}
		}

		if provider == "gemini" {
			if role == "assistant" {
				role = "model"
			} else if role == "tool" || m.ToolResult != nil {
				role = "function"
			}

			// Gemini specific: function response MUST follow function call immediately.
			// System notes (coerced to user) must not interject if we are in a tool sequence.
			if isSystemNote && len(finalMsgs) > 0 {
				lastRole := finalMsgs[len(finalMsgs)-1].Role
				// If last was model with call OR another function result, keep buffering
				if lastRole == "function" || (lastRole == "model" && finalMsgs[len(finalMsgs)-1].ToolCall != nil) {
					systemNoteBuffer = append(systemNoteBuffer, content)
					continue
				}
			}
		}

		// Tool calls/results should NOT be merged with previous text blocks
		isToolRelated := m.ToolCall != nil || m.ToolResult != nil

		// Merge consecutive identical roles (Strictly required by Gemini, good practice elsewhere)
		if len(finalMsgs) > 0 && finalMsgs[len(finalMsgs)-1].Role == role && !isToolRelated && finalMsgs[len(finalMsgs)-1].ToolCall == nil && finalMsgs[len(finalMsgs)-1].ToolResult == nil {
			finalMsgs[len(finalMsgs)-1].Content += "\n\n" + content
		} else {
			// If we were buffering system notes and now we have a non-interjecting message,
			// or if this is a function result (which must follow the call),
			// decide where to put the buffer.
			if len(systemNoteBuffer) > 0 && role != "function" {
				// Flush buffer as a user message before this one
				notes := strings.Join(systemNoteBuffer, "\n\n")
				if role == "user" {
					content = notes + "\n\n" + content
				} else {
					finalMsgs = append(finalMsgs, ChatMessage{Role: "user", Content: notes, Timestamp: time.Now()})
				}
				systemNoteBuffer = nil
			}

			finalMsgs = append(finalMsgs, ChatMessage{
				Role:       role,
				Content:    content,
				Timestamp:  m.Timestamp,
				ToolCall:   m.ToolCall,
				ToolResult: m.ToolResult,
			})

			// If we just added a tool result and have buffered notes, flush them AFTER the result turn
			if len(systemNoteBuffer) > 0 && role == "function" {
				// We need to wait for all parts of the function response if there are multiple?
				// Actually, Gemini allows multiple function results in one turn, but they are separate messages in our ToolHistory.
				// For now, we'll flush after the first result we see that isn't followed by another result?
				// Simple approach: flush after EVERY function result if the NEXT message isn't also a function result.
				// But we are in a loop.
			}
		}
	}

	// Flush any remaining buffered notes
	if len(systemNoteBuffer) > 0 {
		finalMsgs = append(finalMsgs, ChatMessage{Role: "user", Content: strings.Join(systemNoteBuffer, "\n\n"), Timestamp: time.Now()})
	}

	return finalMsgs
}

func streamLLMAndTTS(ctx context.Context, prompt string, ws *websocket.Conn, session *ClientSession) error {
	session.TurnMutex.Lock()
	defer session.TurnMutex.Unlock()

	// Ensure we are in an assistant thread if session.IsAssistant is true
	session.Mutex.Lock()
	isAss := session.IsAssistant || strings.Contains(prompt, ":ASSISTANT")
	t := session.ActiveThread()

	needsSwitch := isAss && !strings.HasPrefix(t.ID, "assistant/")
	if isAss {
		log.Printf("[Assistant] streamLLMAndTTS: IsAssistant=true active_thread=%s needs_switch=%v", t.ID, needsSwitch)
	}
	session.Mutex.Unlock()

	if needsSwitch {
		log.Printf("[Assistant] Assistant mode active but current thread is not an assistant thread. Generating topic for new thread...")
		topic := generateThreadTopic(prompt)
		shouldSync := false
		session.Mutex.Lock()
		// Re-check after generating topic (which might have taken time)
		t = session.ActiveThread()
		if session.IsAssistant && !strings.HasPrefix(t.ID, "assistant/") {
			newID := "assistant/chat_" + generateID()
			log.Printf("[Assistant] Creating new assistant thread: %s (Topic: %s)", newID, topic)
			session.Threads[newID] = &Thread{
				ID:        newID,
				Name:      topic,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			session.ActiveThreadID = newID
			shouldSync = true
		}
		session.Mutex.Unlock()
		if shouldSync {
			sendThreads(nil, session)
		}

	}



	return streamLLMAndTTSInternal(ctx, prompt, ws, session, false)
}


func streamLLMAndTTSInternal(ctx context.Context, prompt string, ws *websocket.Conn, session *ClientSession, isFallback bool) error {
	// Per-client per-model rate limiting
	session.Mutex.Lock()
	model := session.Model
	origModel := session.FallbackOriginalModel
	origProvider := session.FallbackOriginalProvider
	session.Mutex.Unlock()

	// 1. Recovery Logic: If we are currently on a fallback, check if original model is now available
	// Skip recovery if we are currently in a recursive fallback turn to avoid immediate loops.
	if !isFallback && origModel != "" {
		_, err := session.checkRateLimitUnsafe(origModel)
		if err == nil {
			log.Printf("[LLM] Rate limits for %s have reset. Restoring original model for client %s.", origModel, session.ClientID)
			session.Mutex.Lock()
			session.Model = origModel
			session.Provider = origProvider
			session.FallbackOriginalModel = ""
			session.FallbackOriginalProvider = ""
			t := session.ActiveThread()
			session.appendMessage("system", fmt.Sprintf("[System Note: Rate limits for %s have reset. Restoring your preferred model.]", origModel), t)
			session.Mutex.Unlock()
			model = origModel // Continue with restored model
			saveSession(session)
		} else {
			log.Printf("[LLM] Original model %s is still rate-limited: %v", origModel, err)
		}
	}

	// 2. Rate Limit Check
	warning, err := session.checkRateLimit(model)
	if err != nil {
		log.Printf("[LLM] Rate limit exceeded for client %s model %s: %v", session.ClientID, model, err)

		// 3. Fallback Logic: Try to find a fallback if current model is limited
		fallbackProvider, fallbackModel := selectFallbackModel()
		if fallbackProvider != "" && fallbackModel != model {
			session.Mutex.Lock()
			// Remember original if not already in fallback state
			if session.FallbackOriginalModel == "" {
				session.FallbackOriginalModel = model
				session.FallbackOriginalProvider = session.Provider
			}
			session.Model = fallbackModel
			session.Provider = fallbackProvider
			t := session.ActiveThread()
			msg := fmt.Sprintf("[Rate Limit hit for %s]: %v. Switching to fallback %s %s until limits reset.", model, err, fallbackProvider, fallbackModel)
			session.appendMessage("system", msg, t)
			session.Mutex.Unlock()
			log.Printf("[LLM] %s", msg)

			// Process session saving and audio notification asynchronously to reduce latency
			// for starting the fallback LLM request.
			go func(s *ClientSession, m string, conn *websocket.Conn) {
				saveSession(s)
				injectSystemAudio(m, conn, s)
			}(session, "Just a head's up, we've hit a rate limits on youre selected LLM! We're going to fail over to local processing for a bit", ws)

			// Recurse to use the new fallback model (pass isFallback=true to skip recovery check)
			return streamLLMAndTTSInternal(ctx, prompt, ws, session, true)
		}

		// If no fallback available, block as before
		session.Mutex.Lock()
		t := session.ActiveThread()
		session.appendMessage("system", "[Rate Limit]: "+err.Error(), t)
		session.Mutex.Unlock()
		saveSession(session)
		return err
	}
	if warning != "" {
		log.Printf("[LLM] Rate limit warning for client %s model %s: %s", session.ClientID, model, warning)
		session.Mutex.Lock()
		t := session.ActiveThread()
		session.appendMessage("system", "[Rate Limit Warning]: "+warning, t)
		session.Mutex.Unlock()
	}

	sendOrBroadcastText(nil, session, []byte("[AI_START]"))
	defer sendOrBroadcastText(nil, session, []byte("[AI_END]"))
	defer sendHistory(nil, session)

	session.Mutex.Lock()
	provider := session.Provider
	apiKey := session.APIKey
	modelForLog := session.Model
	session.Mutex.Unlock()

	log.Printf("[LLM] Routing request to Provider: '%s', Model: '%s'", provider, modelForLog)
	log.Printf("[LLM] %s", session.getRateLimitLogString(modelForLog))
	if provider == "gemini" && apiKey != "" {
		return streamGeminiAndTTS(ctx, prompt, ws, session, apiKey)
	}
	return streamOllamaAndTTS(ctx, prompt, ws, session)
}

func streamGeminiAndTTS(ctx context.Context, prompt string, ws *websocket.Conn, session *ClientSession, apiKey string) error {
	session.Mutex.Lock()
	model := session.Model
	voice := session.Voice
	configMutex.RLock()
	defaultVoice := config.DefaultVoice
	configMutex.RUnlock()

	effectiveUserName := session.UserName
	if effectiveUserName == "" {
		effectiveUserName = session.GoogleName
	}

	if model == "" {
		model = "gemini-1.5-flash"
	}
	if voice == "" {
		voice = defaultVoice
	}
	voiceName := extractVoiceName(voice)

	sysContent := prepareSystemPrompt(session, model, voiceName, "gemini")
	saveCalculatedSystemPrompt(session, sysContent)

	// Resolve persona for output sanitisation
	personaNameLower := strings.ToLower(voiceName)
	personasMutex.RLock()
	persona, hasPersona := personas[personaNameLower]
	personasMutex.RUnlock()

	var mutationRes []*regexp.Regexp
	targetPersonaName := ""
	phoneticPersonaName := ""
	if hasPersona {
		targetPersonaName = persona.Name
		phoneticPersonaName = persona.PhoneticPronunciation
		if persona.NameMutations != "" {
			muts := strings.Fields(persona.NameMutations)
			for _, m := range muts {
				re := regexp.MustCompile("(?i)\\b" + regexp.QuoteMeta(m) + "\\b")
				mutationRes = append(mutationRes, re)
			}
		}
	}

	t := session.ActiveThread()
	// Native tools are only supported for official Gemini models; others (Gemma, Llama)
	// get a redirected system prompt and no tool access to improve reliability.
	isToolModel := strings.Contains(strings.ToLower(model), "gemini") &&
		!strings.Contains(strings.ToLower(model), "gemma") &&
		!strings.Contains(strings.ToLower(model), "llama")
	contextMsgs := session.getLLMContext(t, isToolModel)
	formattedHistory := prepareLLMHistory(contextMsgs, "gemini")

	type Part struct {
		Text             string                 `json:"text,omitempty"`
		FunctionCall     map[string]interface{} `json:"functionCall,omitempty"`
		FunctionResponse map[string]interface{} `json:"functionResponse,omitempty"`
	}
	type Content struct {
		Role  string `json:"role"`
		Parts []Part `json:"parts"`
	}

	var contents []Content
	for _, msg := range formattedHistory {
		var p Part
		if msg.ToolCall != nil {
			p = Part{
				FunctionCall: map[string]interface{}{
					"name": msg.ToolCall.Name,
					"args": msg.ToolCall.Args,
				},
			}
		} else if msg.ToolResult != nil {
			p = Part{
				FunctionResponse: map[string]interface{}{
					"name": msg.ToolResult.Name,
					"response": map[string]interface{}{
						"result": msg.ToolResult.Result,
					},
				},
			}
		} else {
			p = Part{Text: msg.Content}
		}

		// Merge consecutive turns with the same role (required by Gemini)
		if len(contents) > 0 && contents[len(contents)-1].Role == msg.Role {
			contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, p)
		} else {
			contents = append(contents, Content{Role: msg.Role, Parts: []Part{p}})
		}
	}

	if len(contents) > 0 && contents[len(contents)-1].Role == "user" {
		contents[len(contents)-1].Parts[0].Text += "\n\n" + prompt
	} else {
		contents = append(contents, Content{Role: "user", Parts: []Part{{Text: prompt}}})
	}

	session.Mutex.Unlock()

	geminiTools := getNativeToolsGemini(session)
	isGeminiToolModel := strings.Contains(strings.ToLower(model), "gemini") &&
		!strings.Contains(strings.ToLower(model), "gemma") &&
		!strings.Contains(strings.ToLower(model), "llama")

	if !isGeminiToolModel {
		log.Printf("[Gemini] Model %s is not a native tool model — stripping tools and adapting system instruction", model)
		geminiTools = nil
		// For non-native-tool models, we prepend instructions to the FIRST turn
		// to ensure the model sees the instructions before the conversational history.
		if len(contents) > 0 {
			// Prepend to the first turn to ensure the model sees the instructions before the context
			// and adheres to standard prompt engineering for models without native system roles.
			contents[0].Parts[0].Text = "[SYSTEM_INSTRUCTION]:\n" + sysContent + "\n\n" + contents[0].Parts[0].Text
		} else {
			contents = append(contents, Content{Role: "user", Parts: []Part{{Text: "[SYSTEM_INSTRUCTION]:\n" + sysContent}}})
		}
	}

	payload := map[string]interface{}{
		"contents": contents,
		"tools":    geminiTools,
		"safetySettings": []map[string]string{
			{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "BLOCK_NONE"},
			{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "BLOCK_NONE"},
			{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "threshold": "BLOCK_NONE"},
			{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "BLOCK_NONE"},
		},
	}
	if isGeminiToolModel {
		payload["system_instruction"] = map[string]interface{}{
			"parts": []map[string]string{
				{"text": sysContent},
			},
		}
	}
	body, _ := json.MarshalIndent(payload, "", "  ")
	saveCalculatedContentWindow(session, body)

	reqURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:streamGenerateContent?alt=sse&key=%s", model, apiKey)
	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[Gemini] Failed to create request: %v", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	geminiClient := &http.Client{Timeout: 60 * time.Second}
	log.Printf("[Gemini] Sending request to: %s", reqURL)
	llmStartTime := time.Now()
	resp, err := geminiClient.Do(req)
	if err != nil {
		log.Printf("[Gemini] HTTP request failed: %v", err)
		return err
	}
	defer resp.Body.Close()
	log.Printf("[Gemini] HTTP Response Status: %s", resp.Status)

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		statusText := resp.Status
		if len(bodyBytes) > 0 {
			var errDetail struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(bodyBytes, &errDetail); err == nil && errDetail.Error.Message != "" {
				statusText = fmt.Sprintf("%s (%s)", resp.Status, errDetail.Error.Message)
			}
		}

		err := fmt.Errorf("Gemini API error: %s", statusText)
		log.Println(err)

		// Check for fallback triggers: 503 (Service Unavailable) or 429 (Too Many Requests)
		if resp.StatusCode == 503 || resp.StatusCode == 429 {
			fallbackProvider, fallbackModel := selectFallbackModel()
			if fallbackProvider != "" {
				explanation := "Service Unavailable"
				if resp.StatusCode == 429 {
					explanation = "Too Many Requests (Rate Limited)"
				}

				msg := fmt.Sprintf("[SYSTEM] Error communicating with API %d %s, switching to %s %s",
					resp.StatusCode, explanation, fallbackProvider, fallbackModel)
				log.Println(msg)

				session.Mutex.Lock()
				t := session.ActiveThread()
				session.appendMessage("system", msg, t)
				session.Provider = fallbackProvider
				session.Model = fallbackModel
				session.Mutex.Unlock()

				// Process session saving and audio notification asynchronously to reduce latency
				go func(s *ClientSession, m string, conn *websocket.Conn) {
					saveSession(s)
					injectSystemAudio(m, conn, s)
				}(session, "LLM limits hit, failing over to local processing", ws)

				// Recursive call into the fallback provider
				if fallbackProvider == "ollama" {
					return streamOllamaAndTTS(ctx, prompt, ws, session)
				}
			}
		}

		session.Mutex.Lock()
		t := session.ActiveThread()
		if !strings.HasPrefix(prompt, "[") {
			session.appendMessage("user", prompt, t)
		}
		session.appendMessage("system", err.Error(), t)
		session.Mutex.Unlock()
		saveSession(session)
		return err
	}

	ttsChan := make(chan string, 50)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for text := range ttsChan {
			if ctx.Err() != nil {
				return
			}
			session.Mutex.Lock()
			isClientTts := session.ClientTts
			session.Mutex.Unlock()
			if isClientTts {
				target := ws
				if target == nil || session.isToolConn(target) {
					target = getLastActiveUIConn(session)
				}
				if target != nil {
					safeWrite(target, session, websocket.TextMessage, []byte("[TTS_CHUNK]"+text))
				}
			} else {
				log.Printf("[Gemini TTS Worker] Processing chunk (len: %d) with voice: '%s', user: '%s', persona: '%s', phonetic: '%s'", 
					len(text), voice, effectiveUserName, targetPersonaName, phoneticPersonaName)
				audioBytes, err := queryTTS(text, voice, effectiveUserName, targetPersonaName, phoneticPersonaName)
				if err != nil {
					log.Println("Gemini TTS Worker Error:", err)
				} else if ctx.Err() == nil {
					target := ws
					if target == nil || session.isToolConn(target) {
						target = getLastActiveUIConn(session)
					}
					if target != nil {
						safeWrite(target, session, websocket.BinaryMessage, audioBytes)
					}
				}
			}
		}
	}()

	var sentence strings.Builder
	var fullResponse strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	var sessionPromptTokens int64
	var sessionRespTokens int64
	var sessionTokens int64

	firstChunkSent := false
	flushPendingText := func(text string) {
		if text == "" {
			return
		}

		// Replace persona name mutations with actual name
		if hasPersona && len(mutationRes) > 0 {
			for _, re := range mutationRes {
				text = re.ReplaceAllString(text, targetPersonaName)
			}
		}

		sendOrBroadcastText(nil, session, []byte(text))
		sentence.WriteString(text)
		fullResponse.WriteString(text)
	}

	processTTSSentence := func() {

		currentStr := sentence.String()

		minLength := 30
		hardLimit := 250
		paragraphOnly := false
		if firstChunkSent {
			minLength = 0
			hardLimit = 1000
			paragraphOnly = true
		}
		splitIdx := findSentenceBoundary(currentStr, minLength, hardLimit, paragraphOnly)

		if splitIdx != -1 {

			chunkToSend := currentStr[:splitIdx+1]

			remainder := currentStr[splitIdx+1:]

			ttsText := strings.TrimSpace(chunkToSend)

			if len(ttsText) > 0 {
				normalizedTTS := tts.Sanitise(ttsText, effectiveUserName, targetPersonaName, phoneticPersonaName)
				select {
				case ttsChan <- normalizedTTS:
					firstChunkSent = true

				case <-ctx.Done():

				}

			}

			sentence.Reset()

			sentence.WriteString(remainder)

		}

	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "" {
				continue
			}
			var result struct {
				Candidates []struct {
					Content struct {
						Parts []struct {
							Text         string `json:"text"`
							FunctionCall *struct {
								Name string                 `json:"name"`
								Args map[string]interface{} `json:"args"`
							} `json:"functionCall"`
						} `json:"parts"`
					} `json:"content"`
				} `json:"candidates"`
				UsageMetadata struct {
					PromptTokenCount     int64 `json:"promptTokenCount"`
					CandidatesTokenCount int64 `json:"candidatesTokenCount"`
					TotalTokenCount      int64 `json:"totalTokenCount"`
				} `json:"usageMetadata"`
			}
			if err := json.Unmarshal([]byte(data), &result); err != nil {
				continue
			}

			if result.UsageMetadata.TotalTokenCount > 0 {
				sessionTokens = result.UsageMetadata.TotalTokenCount
				if result.UsageMetadata.PromptTokenCount > 0 {
					sessionPromptTokens = result.UsageMetadata.PromptTokenCount
				}
				if result.UsageMetadata.CandidatesTokenCount > 0 {
					sessionRespTokens = result.UsageMetadata.CandidatesTokenCount
				}
			}

			if len(result.Candidates) > 0 {
				for _, part := range result.Candidates[0].Content.Parts {
					if part.FunctionCall != nil {
						log.Printf("[Gemini] Detected native function call: %s", part.FunctionCall.Name)
						executeNativeToolCall(session, part.FunctionCall.Name, part.FunctionCall.Args, ws)
					}
					if part.Text != "" {
						flushPendingText(part.Text)
						processTTSSentence()
					}
				}
			}
		}
	}

	if cleanChunk := strings.TrimSpace(sentence.String()); len(cleanChunk) > 0 {
		normalizedTTS := tts.Sanitise(cleanChunk, effectiveUserName, targetPersonaName, phoneticPersonaName)
		select {
		case ttsChan <- normalizedTTS:
		case <-ctx.Done():
		}
	}

	close(ttsChan)
	wg.Wait()

	if ctx.Err() == nil {
		session.Mutex.Lock()
		t := session.ActiveThread()
		if !strings.HasPrefix(prompt, "[") {
			session.appendMessage("user", prompt, t)
		}
		finalResp := fullResponse.String()
		if finalResp != "" {
			session.appendMessage("assistant", finalResp, t)
		}
		if len(t.History) > 30 {
			toSummarize := make([]ChatMessage, 10)
			copy(toSummarize, t.History[:10])
			t.Archive = append(t.Archive, toSummarize...)
			t.History = t.History[10:]
			go generateSummaryAsync(toSummarize, t.ID, session)
		}
		session.Mutex.Unlock()

		if sessionTokens > 0 {
			trackTokens(session, model, sessionTokens)
			// Fall back to total/2 split if Gemini didn't send per-part counts
			pTok := sessionPromptTokens
			rTok := sessionRespTokens
			if pTok == 0 && rTok == 0 {
				pTok = sessionTokens / 2
				rTok = sessionTokens - pTok
			}
			recordLLMCall("gemini", model, pTok, rTok, time.Since(llmStartTime).Milliseconds())
		}
		saveSession(session)
	}
	return nil
}

func streamOllamaAndTTS(ctx context.Context, prompt string, ws *websocket.Conn, session *ClientSession) error {
	log.Printf("[Ollama] streamOllamaAndTTS entered — acquiring session mutex")
	session.Mutex.Lock()
	model := session.Model
	voice := session.Voice
	configMutex.RLock()
	ollamaModelConfig := config.OllamaModel
	defaultVoiceConfig := config.DefaultVoice
	configMutex.RUnlock()

	effectiveUserName := session.UserName
	if effectiveUserName == "" {
		effectiveUserName = session.GoogleName
	}

	if model == "" {
		model = ollamaModelConfig
	}
	if voice == "" {
		voice = defaultVoiceConfig
	}
	voiceName := extractVoiceName(voice)

	sysContent := prepareSystemPrompt(session, model, voiceName, "ollama")
	saveCalculatedSystemPrompt(session, sysContent)

	t := session.ActiveThread()
	ollamaSupportsTools := ollamaModelSupportsTools(model)
	contextMsgs := session.getLLMContext(t, ollamaSupportsTools)
	formattedHistory := prepareLLMHistory(contextMsgs, "ollama")

	var ollamaMsgs []map[string]interface{}
	ollamaMsgs = append(ollamaMsgs, map[string]interface{}{
		"role":    "system",
		"content": sysContent,
	})

	for _, m := range formattedHistory {
		msg := map[string]interface{}{
			"role":    m.Role,
			"content": m.Content,
		}
		if m.ToolCall != nil {
			msg["tool_calls"] = []interface{}{
				map[string]interface{}{
					"function": map[string]interface{}{
						"name":      m.ToolCall.Name,
						"arguments": m.ToolCall.Args,
					},
				},
			}
			// Ollama prefers empty content for tool calls
			msg["content"] = ""
		} else if m.ToolResult != nil {
			msg["role"] = "tool"
			msg["content"] = m.ToolResult.Result
		}
		ollamaMsgs = append(ollamaMsgs, msg)
	}

	ollamaMsgs = append(ollamaMsgs, map[string]interface{}{
		"role":    "user",
		"content": prompt,
	})
	session.Mutex.Unlock()

	// Resolve persona for output sanitisation
	voiceNameLower := strings.ToLower(voiceName)
	personasMutex.RLock()
	persona, hasPersona := personas[voiceNameLower]
	personasMutex.RUnlock()

	var mutationRes []*regexp.Regexp
	targetPersonaName := ""
	phoneticPersonaName := ""
	if hasPersona {
		targetPersonaName = persona.Name
		phoneticPersonaName = persona.PhoneticPronunciation
		if persona.NameMutations != "" {
			muts := strings.Fields(persona.NameMutations)
			for _, m := range muts {
				re := regexp.MustCompile("(?i)\\b" + regexp.QuoteMeta(m) + "\\b")
				mutationRes = append(mutationRes, re)
			}
		}
	}

	ollamaTools := getNativeToolsOllama(session)
	if !ollamaModelSupportsTools(model) {
		log.Printf("[Ollama] Model %s does not support tools — omitting from payload", model)
		ollamaTools = nil
	}

	payload := map[string]interface{}{
		"model":    model,
		"messages": ollamaMsgs,
		"stream":   true,
	}
	if len(ollamaTools) > 0 {
		payload["tools"] = ollamaTools
	}
	body, _ := json.MarshalIndent(payload, "", "  ")
	saveCalculatedContentWindow(session, body)

	configMutex.RLock()
	ollamaChatURLs := config.OllamaChatURL
	configMutex.RUnlock()

	url := getNextURL(ollamaChatURLs, &ollamaChatIndex)
	log.Printf("[Ollama] Sending request to %s | model=%s | messages=%d | tools=%d | payload_bytes=%d",
		url, model, len(ollamaMsgs), len(ollamaTools), len(body))
	log.Printf("[Ollama] Payload JSON (first 2000 chars): %.2000s", string(body))

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[Ollama] Failed to create request: %v", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	llmStartTime := time.Now()
	if err != nil {
		log.Printf("[Ollama] HTTP request failed: %v", err)
		return err
	}
	defer resp.Body.Close()
	log.Printf("[Ollama] HTTP Response Status: %s", resp.Status)

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		err := fmt.Errorf("ollama API error: %s - %s", resp.Status, string(bodyBytes))
		log.Println(err)
		session.Mutex.Lock()
		t := session.ActiveThread()
		if !strings.HasPrefix(prompt, "[") {
			session.appendMessage("user", prompt, t)
		}
		session.appendMessage("system", err.Error(), t)
		session.Mutex.Unlock()
		saveSession(session)
		return err
	}

	decoder := json.NewDecoder(resp.Body)
	var sentence strings.Builder
	var fullResponse strings.Builder
	ttsChan := make(chan string, 50)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for text := range ttsChan {
			if ctx.Err() != nil {
				return
			}
			session.Mutex.Lock()
			isClientTts := session.ClientTts
			session.Mutex.Unlock()
			if isClientTts {
				target := ws
				if target == nil || session.isToolConn(target) {
					target = getLastActiveUIConn(session)
				}
				if target != nil {
					safeWrite(target, session, websocket.TextMessage, []byte("[TTS_CHUNK]"+text))
				}
			} else {
				log.Printf("[Ollama TTS Worker] Processing chunk (len: %d) with voice: '%s', user: '%s', persona: '%s', phonetic: '%s'", 
					len(text), voice, effectiveUserName, targetPersonaName, phoneticPersonaName)
				audioBytes, err := queryTTS(text, voice, effectiveUserName, targetPersonaName, phoneticPersonaName)
				if err != nil {
					log.Println("Ollama TTS Worker Error:", err)
				} else if ctx.Err() == nil {
					target := ws
					if target == nil || session.isToolConn(target) {
						target = getLastActiveUIConn(session)
					}
					if target != nil {
						safeWrite(target, session, websocket.BinaryMessage, audioBytes)
					}
				}
			}
		}
	}()

	firstChunkSent := false
	flushPendingText := func(text string) {
		if text == "" {
			return
		}

		// Replace persona name mutations with actual name
		if hasPersona && len(mutationRes) > 0 {
			for _, re := range mutationRes {
				text = re.ReplaceAllString(text, targetPersonaName)
			}
		}

		sendOrBroadcastText(nil, session, []byte(text))
		sentence.WriteString(text)
		fullResponse.WriteString(text)
	}

	processTTSSentence := func() {
		currentStr := sentence.String()
		minLength := 30
		hardLimit := 250
		paragraphOnly := false
		if firstChunkSent {
			minLength = 0
			hardLimit = 1000
			paragraphOnly = true
		}
		splitIdx := findSentenceBoundary(currentStr, minLength, hardLimit, paragraphOnly)

		if splitIdx != -1 {

			chunkToSend := currentStr[:splitIdx+1]

			remainder := currentStr[splitIdx+1:]

			ttsText := strings.TrimSpace(chunkToSend)

			if len(ttsText) > 0 {
				normalizedTTS := tts.Sanitise(ttsText, effectiveUserName, targetPersonaName, phoneticPersonaName)
				select {
				case ttsChan <- normalizedTTS:
					firstChunkSent = true

				case <-ctx.Done():

				}

			}

			sentence.Reset()

			sentence.WriteString(remainder)

		}

	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var result struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string                 `json:"name"`
						Arguments map[string]interface{} `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			Done            bool  `json:"done"`
			PromptEvalCount int64 `json:"prompt_eval_count"`
			EvalCount       int64 `json:"eval_count"`
			EvalDuration    int64 `json:"eval_duration"`  // nanoseconds (generation only)
			TotalDuration   int64 `json:"total_duration"` // nanoseconds (full request)
		}
		if err := decoder.Decode(&result); err != nil {
			if err != io.EOF {
				log.Printf("[Ollama] Decoder error: %v", err)
			}
			break
		}
		// log.Printf("[Ollama] Chunk: done=%v content=%q tool_calls=%d", result.Done, result.Message.Content, len(result.Message.ToolCalls))

		if len(result.Message.ToolCalls) > 0 {
			for _, tc := range result.Message.ToolCalls {
				log.Printf("[Ollama] Detected native tool call: %s", tc.Function.Name)
				executeNativeToolCall(session, tc.Function.Name, tc.Function.Arguments, ws)
			}
		}
		content := result.Message.Content
		if content != "" {
			flushPendingText(content)
			processTTSSentence()
		}
		if result.Done {
			trackTokens(session, model, result.PromptEvalCount+result.EvalCount)
			recordLLMCall("ollama", model, result.PromptEvalCount, result.EvalCount, time.Since(llmStartTime).Milliseconds())
			break
		}
	}

	if cleanChunk := strings.TrimSpace(sentence.String()); len(cleanChunk) > 0 {
		normalizedTTS := tts.Sanitise(cleanChunk, effectiveUserName, targetPersonaName, phoneticPersonaName)
		select {
		case ttsChan <- normalizedTTS:
		case <-ctx.Done():
		}
	}

	close(ttsChan)
	wg.Wait()

	if ctx.Err() == nil {
		session.Mutex.Lock()
		t := session.ActiveThread()
		if !strings.HasPrefix(prompt, "[") {
			session.appendMessage("user", prompt, t)
		}
		finalResp := fullResponse.String()
		if finalResp != "" {
			session.appendMessage("assistant", finalResp, t)
		}
		if len(t.History) > 30 {
			toSummarize := make([]ChatMessage, 10)
			copy(toSummarize, t.History[:10])
			t.Archive = append(t.Archive, toSummarize...)
			t.History = t.History[10:]
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
	if !exists {
		session.Mutex.Unlock()
		return
	}
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

	configMutex.RLock()
	ollamaModelConfig := config.OllamaModel
	ollamaURLs := config.OllamaURLs
	configMutex.RUnlock()

	// 1. Always attempt local summarization first to save tokens (Fast fail after 10s)
	localPayload := map[string]interface{}{"model": ollamaModelConfig, "prompt": prompt, "stream": false}
	localBody, _ := json.Marshal(localPayload)

	client := &http.Client{Timeout: 60 * time.Second}
	url := getNextURL(ollamaURLs, &ollamaIndex)
	resp, err := client.Post(url, "application/json", bytes.NewReader(localBody))
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
		type Part struct {
			Text string `json:"text"`
		}
		payload := map[string]interface{}{
			"contents": []map[string]interface{}{
				{"role": "user", "parts": []Part{{Text: prompt}}},
			},
		}
		body, _ := json.Marshal(payload)
		if model == "" {
			model = "gemini-1.5-flash"
		}
		reqURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)
		if resp, err := http.Post(reqURL, "application/json", bytes.NewReader(body)); err == nil {
			defer resp.Body.Close()
			var result struct {
				Candidates []struct {
					Content struct {
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
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


func findSentenceBoundary(text string, minLength int, hardLimit int, paragraphOnly bool) int {
	if paragraphOnly {
		// Stage 2: Relaxed splitting — prefer paragraph breaks (\n\n)
		// This allows multiple sentences to be processed as one chunk for better prosody once audio is in-flight.
		if idx := strings.Index(text, "\n\n"); idx != -1 {
			return idx + 1 // Split after the first newline
		}
		// Fallback: If it's getting excessively long without a paragraph break, split on any boundary near the end.
		if hardLimit > 0 && len(text) >= hardLimit {
			for i := len(text) - 1; i >= 0; i-- {
				c := text[i]
				if c == '\n' || c == '.' || c == '!' || c == '?' {
					return i
				}
			}
			return len(text) - 1 // Last resort hard split
		}
		return -1
	}

	// Stage 1: Aggressive splitting to minimize time-to-first-audio
	if idx := strings.Index(text, "\n"); idx != -1 {
		return idx
	}

	if len(text) < minLength {
		// Even if shorter than minLength, respect the hard limit
		if hardLimit > 0 && len(text) >= hardLimit {
			return len(text) - 1
		}
		return -1
	}

	for i := minLength; i < len(text); i++ {
		c := text[i]
		if c == '.' || c == '!' || c == '?' || c == ':' {
			// Don't split if it's the very last character (more context might come)
			if i+1 == len(text) {
				continue
			}

			// Only split if followed by whitespace
			if !unicode.IsSpace(rune(text[i+1])) {
				continue
			}

			if c == '.' {
				// 1. Avoid decimals (e.g. "v1.0") - check if preceded by digit and followed by digit
				// 2. Numbered list check: "[LINE_START][NUMBER]. "
				wordStart := i
				for wordStart > 0 && !unicode.IsSpace(rune(text[wordStart-1])) {
					wordStart--
				}
				word := text[wordStart:i]

				if isNumeric(word) {
					if wordStart == 0 || text[wordStart-1] == '\n' {
						continue // Don't split on "1. " at line start
					}
				}

				// 3. Avoid common abbreviations
				if isCommonAbbreviation(strings.ToLower(word)) {
					continue
				}
			}

			return i
		}
	}

	// Hard limit fallback for Stage 1
	if hardLimit > 0 && len(text) >= hardLimit {
		return len(text) - 1
	}

	return -1
}

func isNumeric(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

func isCommonAbbreviation(word string) bool {
	switch word {
	case "mr", "mrs", "dr", "vs", "eg", "ie", "vol", "v", "st", "prof", "inc", "co", "v1", "v2", "v3", "v4", "v5":
		return true
	}
	return false
}

func injectSystemAudio(text string, ws *websocket.Conn, session *ClientSession) {
	session.Mutex.Lock()
	voice := session.Voice
	userName := session.UserName
	isClientTts := session.ClientTts
	session.Mutex.Unlock()

	// Resolve persona for TTS enhancements
	voiceName := extractVoiceName(voice)
	personaNameLower := strings.ToLower(voiceName)
	personasMutex.RLock()
	persona, hasPersona := personas[personaNameLower]
	personasMutex.RUnlock()

	targetPersonaName := ""
	phoneticPersonaName := ""
	if hasPersona {
		targetPersonaName = persona.Name
		phoneticPersonaName = persona.PhoneticPronunciation
	}

	if isClientTts {
		// Send as text for client-side TTS
		msg := "[TTS_CHUNK]" + text
		// Apply phonetic substitution if available for client TTS too (since it's for speech)
		if phoneticPersonaName != "" {
			re := regexp.MustCompile("(?i)\\b" + regexp.QuoteMeta(targetPersonaName) + "\\b")
			msg = re.ReplaceAllString(msg, phoneticPersonaName)
		}
		target := ws
		if target == nil {
			target = getLastActiveUIConn(session)
		}
		if target != nil {
			safeWrite(target, session, websocket.TextMessage, []byte("[TTS_CHUNK]"+text))
		}
	} else {
		// Generate on server and send binary
		audioBytes, err := queryTTS(text, voice, userName, targetPersonaName, phoneticPersonaName)
		if err == nil {
			target := ws
			if target == nil {
				target = getLastActiveUIConn(session)
			}
			if target != nil {
				safeWrite(target, session, websocket.BinaryMessage, audioBytes)
			}
		} else {
			log.Printf("[SystemAudio] TTS generation failed: %v", err)
		}
	}
}

func queryTTS(text string, voice string, userName string, personaName string, phoneticName string) ([]byte, error) {
	// Resolve voice (persona ID or filename) to the actual voice file
	voiceFile := voice
	personaNameLower := strings.ToLower(extractVoiceName(voice))
	personasMutex.RLock()
	if p, ok := personas[personaNameLower]; ok && p.VoiceFile != "" {
		voiceFile = p.VoiceFile
	}
	personasMutex.RUnlock()

	text = tts.Sanitise(text, userName, personaName, phoneticName)
	modelPath := filepath.Join(".", "piper", "models", voiceFile)

	// Verify model existence and fall back if missing
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		log.Printf("[TTS] Voice file '%s' not found at %s. Attempting fallback.", voiceFile, modelPath)
		configMutex.RLock()
		fallbackVoice := config.DefaultVoice
		configMutex.RUnlock()

		// Try the configured default
		modelPath = filepath.Join(".", "piper", "models", fallbackVoice)
		if _, err := os.Stat(modelPath); os.IsNotExist(err) {
			log.Printf("[TTS] Default voice '%s' also not found. Scanning for any available model.", fallbackVoice)
			// Last effort: pick the first .onnx file in the models directory
			files, _ := os.ReadDir(filepath.Join(".", "piper", "models"))
			found := false
			for _, f := range files {
				if !f.IsDir() && strings.HasSuffix(f.Name(), ".onnx") {
					voiceFile = f.Name()
					modelPath = filepath.Join(".", "piper", "models", voiceFile)
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("no Piper voice models found on disk")
			}
		} else {
			voiceFile = fallbackVoice
		}
	}

	log.Printf("[TTS] Generating audio for text: '%s' with voice: %s (resolved from %s)", text, voiceFile, voice)
	// Execute piper binary: -f - tells it to output the WAV file directly to standard output
	configMutex.RLock()
	piperBin := config.PiperBin
	configMutex.RUnlock()

	// Default piper values
	noiseScale := 0.667
	lengthScale := 0.85
	noiseW := 0.8

	personasMutex.RLock()
	if p, ok := personas[personaNameLower]; ok {
		if p.VoiceNoiseScale != 0 {
			noiseScale = p.VoiceNoiseScale
		}
		if p.VoiceLengthScale != 0 {
			lengthScale = p.VoiceLengthScale
		}
		if p.VoiceNoiseW != 0 {
			noiseW = p.VoiceNoiseW
		}
	}
	personasMutex.RUnlock()

	// Add random variation of +/- 0.08 for natural vocalisation
	randomVar := func(base float64) float64 {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return base
		}
		// Convert to 0..1 then map to -0.08..0.08
		v := float64(binary.LittleEndian.Uint64(b)) / float64(1<<64)
		newVal := base + (v*0.16) - 0.08
		if newVal < 0.01 {
			newVal = 0.01
		}
		return newVal
	}

	noiseScale = randomVar(noiseScale)
	// lengthScale = randomVar(lengthScale)
	noiseW = randomVar(noiseW)

	cmd := exec.Command(piperBin,
		"--model", modelPath,
		"--noise_scale", fmt.Sprintf("%f", noiseScale),
		"--length_scale", fmt.Sprintf("%f", lengthScale),
		"--noise_w", fmt.Sprintf("%f", noiseW),
		"-f", "-")

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
		if r.Header.Get("X-Forwarded-Proto") == "http" || strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.0.0.1") {
			scheme = "http"
		}
		baseURL = scheme + "://" + r.Host
	}
	redirectURI := baseURL + "/auth/callback"
	log.Printf("[Login] Host: %s, X-Forwarded-Proto: '%s', Generated Redirect: %s", r.Host, r.Header.Get("X-Forwarded-Proto"), redirectURI)

	url := fmt.Sprintf("https://accounts.google.com/o/oauth2/v2/auth?client_id=%s&redirect_uri=%s&response_type=code&scope=openid profile email&state=%s", googleClientID, redirectURI, state)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "speax_session", Value: "", Path: "/", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: "speax_avatar", Value: "", Path: "/", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: "speax_google_name", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
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
		if r.Header.Get("X-Forwarded-Proto") == "http" || strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.0.0.1") {
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

	var tokenRes struct {
		AccessToken string `json:"access_token"`
	}
	json.NewDecoder(resp.Body).Decode(&tokenRes)

	req, _ := http.NewRequest("GET", "https://www.googleapis.com/oauth2/v3/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tokenRes.AccessToken)
	userResp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "Failed to get user info", http.StatusInternalServerError)
		return
	}
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

func cacheModelsAsync(apiKey string) {
	go func() {
		ctxDir := filepath.Join(".", "context")
		os.MkdirAll(ctxDir, 0755)

		// Gemini
		if apiKey != "" {
			models := getGeminiModelsInternal(apiKey, true) // Admin mode to fetch all
			if len(models) > 0 {
				data, _ := json.MarshalIndent(models, "", "  ")
				os.WriteFile(filepath.Join(ctxDir, "gemini-models.json"), data, 0644)
			}
		}

		// Ollama
		models := getOllamaModelsInternal(true) // Admin mode to fetch all
		if len(models) > 0 {
			data, _ := json.MarshalIndent(models, "", "  ")
			os.WriteFile(filepath.Join(ctxDir, "ollama-models.json"), data, 0644)
		}
		log.Println("[Models] Background model list caching complete")
	}()
}

type ModelData struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	apiKey := r.URL.Query().Get("apiKey")

	// Authenticate the user to check admin status
	clientID := r.URL.Query().Get("clientID")
	if clientID == "" {
		if cookie, err := r.Cookie("speax_session"); err == nil {
			clientID = cookie.Value
		}
	}
	isAdmin := IsAdminID(clientID)

	var out []ModelData
	if provider == "gemini" && apiKey != "" {
		out = getGeminiModelsInternal(apiKey, isAdmin)
	} else if provider == "ollama" {
		out = getOllamaModelsInternal(isAdmin)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func handleVoices(w http.ResponseWriter, r *http.Request) {
	type PersonaInfo struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		VoiceFile string `json:"voice_file"`
	}

	personasMutex.RLock()
	defer personasMutex.RUnlock()

	var out []PersonaInfo
	for id, p := range personas {
		if p.VoiceFile == "" {
			continue
		}
		// Check if the voice file actually exists
		modelPath := filepath.Join(".", "piper", "models", p.VoiceFile)
		if _, err := os.Stat(modelPath); err == nil {
			out = append(out, PersonaInfo{
				ID:        id,
				Name:      p.Name,
				VoiceFile: p.VoiceFile,
			})
		}
	}

	// Sort by name for UI consistency
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func handlePerformanceMetrics(w http.ResponseWriter, r *http.Request) {
	// Resolve client identity
	clientID := r.URL.Query().Get("clientID")
	if clientID == "" {
		if cookie, err := r.Cookie("speax_session"); err == nil {
			clientID = cookie.Value
		}
	}
	if !IsAdminID(clientID) {
		http.Error(w, `{"error":"admin access required"}`, http.StatusForbidden)
		return
	}

	perfMetricsMu.Lock()
	data, err := json.MarshalIndent(perfMetrics, "", "  ")
	perfMetricsMu.Unlock()
	if err != nil {
		http.Error(w, `{"error":"serialisation error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func main() {
	go startBackupRoutine()

	http.HandleFunc("/auth/login", handleLogin)
	http.HandleFunc("/auth/logout", handleLogout)
	http.HandleFunc("/auth/callback", handleCallback)
	http.HandleFunc("/api/models", handleModels)
	http.HandleFunc("/api/voices", handleVoices)
	http.HandleFunc("/api/performance-metrics", handlePerformanceMetrics)
	http.HandleFunc("/ws", handleConnections)
	http.Handle("/", http.FileServer(http.Dir("./public")))

	fmt.Println("Server started on :3000")
	err := http.ListenAndServe(":3000", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

func startBackupRoutine() {
	ticker := time.NewTicker(24 * time.Hour)
	go func() {
		// Run once immediately on startup
		runBackups()
		for range ticker.C {
			runBackups()
		}
	}()
}

func runBackups() {
	ctxDir := filepath.Join(".", "context")
	backupDir := filepath.Join(ctxDir, "backups")
	os.MkdirAll(backupDir, 0755)

	entries, err := os.ReadDir(ctxDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "backups" {
			continue
		}
		userID := entry.Name()
		userDirPath := filepath.Join(ctxDir, userID)

		// Find most recent modification time in userDirPath
		var latestModTime time.Time
		filepath.Walk(userDirPath, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				if info.ModTime().After(latestModTime) {
					latestModTime = info.ModTime()
				}
			}
			return nil
		})

		if latestModTime.IsZero() {
			continue // Empty dir
		}

		// Find the most recent backup for this user
		var latestBackupTime time.Time
		backupEntries, err := os.ReadDir(backupDir)
		if err == nil {
			for _, bEntry := range backupEntries {
				if strings.HasPrefix(bEntry.Name(), userID+"_") && strings.HasSuffix(bEntry.Name(), ".tar.gz") {
					info, err := bEntry.Info()
					if err == nil && info.ModTime().After(latestBackupTime) {
						latestBackupTime = info.ModTime()
					}
				}
			}
		}

		// If backup doesn't exist, or latest file is newer than latest backup
		if latestBackupTime.IsZero() || latestModTime.After(latestBackupTime) {
			timestamp := time.Now().Format("20060102_150405")
			backupFilename := fmt.Sprintf("%s_%s.tar.gz", userID, timestamp)
			backupPath := filepath.Join(backupDir, backupFilename)

			cmd := exec.Command("tar", "-czf", backupPath, "-C", ctxDir, userID)
			if err := cmd.Run(); err != nil {
				log.Printf("Failed to backup %s: %v", userID, err)
			} else {
				log.Printf("Successfully backed up %s to %s", userID, backupFilename)
			}
		}
	}
}
