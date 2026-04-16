package stt

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Node represents a single Whisper transcription service instance.
type Node struct {
	URL                string        `json:"URL"`
	Zombie             bool          `json:"Zombie"`
	LastResponseTime   time.Duration `json:"LastResponseTime"`
	LastExecutionTime  time.Duration `json:"LastExecutionTime"`
	RollingCutoffRatio float64       `json:"RollingCutoffRatio"`
	FailureCount       int           `json:"FailureCount"`
	TotalRequests      int           `json:"TotalRequests"`
	TotalFailures      int           `json:"TotalFailures"`
	TotalAudioMs       int64         `json:"TotalAudioMs"`
}

// Manager coordinates a pool of STT nodes.
type Manager struct {
	nodes []*Node
	index uint32
	mu    sync.RWMutex
	onLog func(string)
}

// NewManager creates a new STT manager with the given node URLs and optional logging callback.
func NewManager(urls []string, onLog func(string)) *Manager {
	m := &Manager{onLog: onLog}
	m.UpdateURLs(urls)
	return m
}

// UpdateURLs synchronizes the manager's node pool with the provided list of URLs.
func (m *Manager) UpdateURLs(urls []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing := make(map[string]*Node)
	for _, n := range m.nodes {
		existing[n.URL] = n
	}

	var newNodes []*Node
	for _, url := range urls {
		if n, ok := existing[url]; ok {
			newNodes = append(newNodes, n)
		} else {
			newNodes = append(newNodes, &Node{URL: url})
		}
	}
	m.nodes = newNodes
}

// GetStatus returns a snapshot of the current state of all nodes.
func (m *Manager) GetStatus() []*Node {
	m.mu.RLock()
	defer m.mu.RUnlock()

	copy := make([]*Node, len(m.nodes))
	for i, n := range m.nodes {
		nCopy := *n
		copy[i] = &nCopy
	}
	return copy
}

// PickNode returns a healthy node using the internal round-robin index.
func (m *Manager) PickNode() (*Node, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	numNodes := len(m.nodes)
	if numNodes == 0 {
		return nil, fmt.Errorf("no STT nodes available")
	}

	for i := 0; i < numNodes; i++ {
		idx := atomic.AddUint32(&m.index, 1) - 1
		node := m.nodes[idx%uint32(numNodes)]
		if !node.Zombie {
			return node, nil
		}
	}
	return nil, fmt.Errorf("all STT nodes are unhealthy")
}

func (m *Manager) getHealthyNode(pool ...[]string) (*Node, error) {
	if len(pool) > 0 && len(pool[0]) > 0 {
		return m.PickNodeFromPool(pool[0])
	}
	return m.PickNode()
}

// PickNodeFromPool returns a healthy node from the provided set of URLs.
func (m *Manager) PickNodeFromPool(pool []string) (*Node, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var subset []*Node
	for _, url := range pool {
		for _, n := range m.nodes {
			if n.URL == url {
				if !n.Zombie {
					subset = append(subset, n)
				}
				break
			}
		}
	}

	if len(subset) == 0 {
		return nil, fmt.Errorf("no healthy STT nodes available in the specified pool")
	}

	idx := atomic.AddUint32(&m.index, 1) - 1
	return subset[idx%uint32(len(subset))], nil
}

// Transcribe sends audio data to a healthy node and returns the transcribed text.
// Enforces a strict 8.0-second maximum cap on sent audio by taking the latest tail if larger.
func (m *Manager) Transcribe(ctx context.Context, pcmData []byte, sampleRate int, pool ...[]string) (string, error) {
	maxBytes := int(float64(sampleRate) * 2 * 8.0)
	if len(pcmData) > maxBytes {
		pcmData = pcmData[len(pcmData)-maxBytes:]
	}
	node, err := m.getHealthyNode(pool...)
	if err != nil {
		return "", err
	}
	return m.TranscribePinned(ctx, pcmData, sampleRate, node.URL)
}

// TranscribePinned sends audio data to a SPECIFIC node (for server affinity) and returns the transcribed text.
// Enforces a strict 8.0-second maximum cap on sent audio by taking the latest tail if larger.
func (m *Manager) TranscribePinned(ctx context.Context, pcmData []byte, sampleRate int, pinnedURL string) (string, error) {
	maxBytes := int(float64(sampleRate) * 2 * 8.0)
	if len(pcmData) > maxBytes {
		pcmData = pcmData[len(pcmData)-maxBytes:]
	}
	if len(pcmData) == 0 {
		return "", fmt.Errorf("empty audio data")
	}

	var node *Node
	m.mu.RLock()
	for _, n := range m.nodes {
		if n.URL == pinnedURL {
			node = n
			break
		}
	}
	m.mu.RUnlock()

	if node == nil {
		return m.Transcribe(ctx, pcmData, sampleRate) // Fallback to healthy if pinned is gone
	}

	durationSecs := float64(len(pcmData)) / float64(sampleRate*2)
	audioMs := int64(durationSecs * 1000.0)
	timeoutSecs := (durationSecs * 0.25) + 2.5
	timeoutDuration := time.Duration(timeoutSecs * float64(time.Second))

	wavData := AddWavHeader(pcmData, sampleRate)

	// Single attempt for pinned node (caller handles re-routing if needed)
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="input.wav"`)
	h.Set("Content-Type", "audio/wav")
	part, _ := writer.CreatePart(h)
	part.Write(wavData)
	writer.Close()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, "POST", node.URL, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	duration := time.Since(start)

	m.mu.Lock()
	node.LastExecutionTime = duration
	node.TotalRequests++
	node.TotalAudioMs += audioMs
	if err != nil {
		node.FailureCount++
		node.TotalFailures++
		m.mu.Unlock()
		return "", fmt.Errorf("pinned transcription request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK || duration > timeoutDuration {
		node.FailureCount++
		node.TotalFailures++
		m.mu.Unlock()
		return "", fmt.Errorf("pinned transcription failed (status: %s, duration: %v)", resp.Status, duration)
	}

	node.Zombie = false
	node.LastResponseTime = duration
	node.FailureCount = 0
	m.mu.Unlock()

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Text, nil
}

// StreamSession manages an active streaming transcription session.
type StreamSession struct {
	Manager            *Manager
	PinnedURL          string
	AudioBuffer        []byte
	StableTranscript   string
	SpeculativeTrans   string
	LastInferenceAudio []byte
	SampleRate         int
	OnUpdate           func(fullTranscript string)
	MinBufferSecs      float64
	MaxBufferSecs      float64
	EnergyThreshold    float64
	Pool               []string

	UseRollingWindow   bool
	lastSentEndIdx     int

	mu               sync.Mutex
	inferenceActive  bool
	inferencePending bool
	finishing        bool
}

// PushAudio appends new audio data and triggers opportunistic inferences.
func (s *StreamSession) PushAudio(data []byte) {
	s.mu.Lock()
	if s.finishing {
		s.mu.Unlock()
		return
	}
	s.AudioBuffer = append(s.AudioBuffer, data...)
	bufferLen := len(s.AudioBuffer)
	
	// We will evaluate trigger conditions directly instead of blindly setting pending.
	// We still need to block overlapping go-routines, but inferencePending controls the loop.
	s.mu.Unlock()

	// Heuristic: Trigger inference every X seconds of new audio
	// or if we have at least X seconds and the energy is low (silence detection).
	minBuffer := s.MinBufferSecs
	if minBuffer <= 0 { minBuffer = 1.5 }
	maxBuffer := s.MaxBufferSecs
	if maxBuffer <= 0 { maxBuffer = 7.5 }
	energyThresh := s.EnergyThreshold
	if energyThresh <= 0 { energyThresh = 0.05 }

	if bufferLen >= int(float64(s.SampleRate)*2*minBuffer) { 
		energy := AudioEnergy(data)
		isSilence := energy < energyThresh 

		s.mu.Lock()
		isFresh := s.StableTranscript == "" && s.SpeculativeTrans == ""
		s.mu.Unlock()

		if s.UseRollingWindow {
			overlapBytes := int(float64(s.SampleRate) * 2 * 3.0)
			
			s.mu.Lock()
			nextStart := s.lastSentEndIdx - overlapBytes
			if nextStart < 0 {
				nextStart = 0
			}
			
			requiredBytes := int(float64(s.SampleRate) * 2 * minBuffer)
			// Ensure strict 5s stride (8s window - 3s overlap) by stepping forward nextStart + 8.0
			if s.lastSentEndIdx > 0 {
				requiredBytes = nextStart + int(float64(s.SampleRate) * 2 * 8.0)
			}
			s.mu.Unlock()

			if bufferLen >= requiredBytes || isSilence {
				s.mu.Lock()
				if !s.inferenceActive {
					s.inferenceActive = true
					s.mu.Unlock()
					go s.runInference()
				} else {
					s.inferencePending = true
					s.mu.Unlock()
				}
			}
		} else {
			// Legacy unbounded logic:
			// If the buffer is fresh (start of a turn), trigger inference immediately at minBuffer 
			// without waiting for silence or a larger buffer. This reduces barge-in latency.
			if isFresh || bufferLen >= int(float64(s.SampleRate)*2*maxBuffer) || isSilence { 
				s.mu.Lock()
				if !s.inferenceActive {
					s.inferenceActive = true
					s.mu.Unlock()
					go s.runInference()
				} else {
					s.inferencePending = true
					s.mu.Unlock()
				}
			}
		}
	}
}

func (s *StreamSession) runInference() {
	for {
		s.mu.Lock()
		audioSlice := s.AudioBuffer
		var isRolling bool

		if s.UseRollingWindow {
			overlapBytes := int(float64(s.SampleRate) * 2 * 3.0)
			nextStart := s.lastSentEndIdx - overlapBytes
			if nextStart < 0 {
				nextStart = 0
			}
			// Only take exactly up to 8 seconds
			endLimit := nextStart + int(float64(s.SampleRate) * 2 * 8.0)
			if endLimit > len(s.AudioBuffer) {
				// Don't expand partially if we're rolling! If we are rolling, we should ideally wait for the full 8s segment.
				// However, if we're triggered by silence, we can send the partial.
				endLimit = len(s.AudioBuffer)
			}
			
			// Strictly enforce that we do NOT send tiny identical blocks. We only roll if we've accumulated meaningful new data.
			if endLimit > s.lastSentEndIdx {
				audioSlice = s.AudioBuffer[nextStart:endLimit]
				s.lastSentEndIdx = endLimit
				isRolling = true
			}
		}

		if !isRolling && s.UseRollingWindow {
			// Fast exit if the loop woke up but doesn't have enough data to form a new block
			if !s.inferencePending || s.finishing {
				s.inferenceActive = false
				s.inferencePending = false
				s.mu.Unlock()
				return
			}
			s.inferencePending = false
			s.mu.Unlock()
			continue
		}

		audio := make([]byte, len(audioSlice))
		copy(audio, audioSlice)
		pinned := s.PinnedURL
		sampleRate := s.SampleRate
		pool := s.Pool
		s.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var text string
		var err error
		if pinned != "" {
			text, err = s.Manager.TranscribePinned(ctx, audio, sampleRate, pinned)
		} else {
			text, err = s.Manager.Transcribe(ctx, audio, sampleRate, pool)
		}
		cancel()

		if err == nil {
			var fullUpdate string
			s.mu.Lock()
			// 1. Basic consensus/alignment logic
			var stable, speculative string
			if isRolling {
				stable, speculative = alignTranscriptsRolling(s.SpeculativeTrans, text)
			} else {
				stable, speculative = alignTranscripts(s.SpeculativeTrans, text)
			}
			
			if stable != "" {
				if s.StableTranscript != "" {
					s.StableTranscript += " "
				}
				s.StableTranscript += stable
			}
			s.SpeculativeTrans = speculative
			
			if s.OnUpdate != nil {
				fullUpdate = s.StableTranscript
				if fullUpdate != "" && s.SpeculativeTrans != "" {
					fullUpdate += " "
				}
				fullUpdate += s.SpeculativeTrans
			}
			s.mu.Unlock()

			if fullUpdate != "" && s.OnUpdate != nil {
				s.OnUpdate(fullUpdate)
			}
		}

		s.mu.Lock()
		if !s.inferencePending || s.finishing {
			s.inferenceActive = false
			s.inferencePending = false
			s.mu.Unlock()
			return
		}
		s.inferencePending = false
		s.mu.Unlock()
		// Loop continue...
	}
}

// Finish signals the end of the stream and performs one last synchronous inference.
// It returns the final, combined transcript.
func (s *StreamSession) Finish() string {
	s.mu.Lock()
	s.finishing = true
	// Wait for any active inference to complete
	for s.inferenceActive {
		s.mu.Unlock()
		time.Sleep(50 * time.Millisecond) // Simple poll instead of cond-var for simplicity
		s.mu.Lock()
	}
	
	// Final synchronous call if there's audio left
	audioSlice := s.AudioBuffer
	var isRolling bool
	if s.UseRollingWindow {
		startIdx := len(s.AudioBuffer) - int(float64(s.SampleRate) * 2 * 8.0)
		if startIdx < 0 {
			startIdx = 0
		}
		audioSlice = s.AudioBuffer[startIdx:]
		isRolling = true
	}

	audio := make([]byte, len(audioSlice))
	copy(audio, audioSlice)
	pinned := s.PinnedURL
	sampleRate := s.SampleRate
	
	// If no audio, just return current state
	if len(audio) == 0 {
		stable := s.StableTranscript
		if stable != "" && s.SpeculativeTrans != "" {
			stable += " "
		}
		stable += s.SpeculativeTrans
		s.mu.Unlock()
		return stable
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Final full transcription
	text, err := s.Manager.TranscribePinned(ctx, audio, sampleRate, pinned)
	if err != nil {
		// Fallback to what we have if final call fails
		s.mu.Lock()
		defer s.mu.Unlock()
		full := s.StableTranscript
		if full != "" && s.SpeculativeTrans != "" {
			full += " "
		}
		full += s.SpeculativeTrans
		return full
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	
	// Final batch transcription is the ground truth for the whole session.
	// Since Whisper transcribes from the beginning, if the final call 
	// succeeded, it is more accurate than any partial segment alignment.
	if text != "" {
		if isRolling {
			stable, speculative := alignTranscriptsRolling(s.SpeculativeTrans, text)
			full := s.StableTranscript
			if full != "" && stable != "" {
				full += " "
			}
			full += stable
			if full != "" && speculative != "" {
				full += " "
			}
			full += speculative
			return full
		}
		return text
	}
	
	full := s.StableTranscript
	if full != "" && s.SpeculativeTrans != "" {
		full += " "
	}
	full += s.SpeculativeTrans
	return full
}

// alignTranscriptsRolling attempts to find a consensus overlap point by starting from the 
// middle of the new transcript and searching backward. It downweights the last 2 words of 'old'
// and the first 2 words of 'new' to avoid edge truncation hallucinations.
func alignTranscriptsRolling(old, new string) (stablePrefix, newSpeculative string) {
	oldWords := strings.Fields(old)
	newWords := strings.Fields(new)

	if len(oldWords) == 0 {
		return "", new
	}
	if len(newWords) == 0 {
		return old, ""
	}

	bestMatchScore := 0
	bestOldIndex := -1
	bestNewIndex := -1

	// Iterate backward from the center of newWords
	center := len(newWords) / 2
	if center == 0 && len(newWords) > 0 {
		center = len(newWords) - 1
	}

	for i := center; i >= 0; i-- {
		for j := len(oldWords) - 1; j >= 0; j-- {
			score := 0
			for k := 0; i+k < len(newWords) && j+k < len(oldWords); k++ {
				w1 := strings.Trim(strings.ToLower(newWords[i+k]), ".,!?")
				w2 := strings.Trim(strings.ToLower(oldWords[j+k]), ".,!?")
				if w1 == w2 {
					score++
				} else {
					break
				}
			}

			reqScore := 2
			if len(newWords) < 3 || len(oldWords) < 3 {
				reqScore = 1
			}
			
			// Downweight edges: if the match starts in the first 2 words of new, or the last 2 words of old, require stronger consensus.
			inDownweightedZone := (i < 2) || (j > len(oldWords)-3)
			if inDownweightedZone && len(newWords) > 3 && len(oldWords) > 3 {
				reqScore = 3
			}

			if score >= reqScore && score > bestMatchScore {
				bestMatchScore = score
				bestOldIndex = j
				bestNewIndex = i
			}
		}
		if bestMatchScore > 0 {
			break
		}
	}

	if bestMatchScore > 0 {
		stablePrefix = strings.Join(oldWords[:bestOldIndex], " ")
		newSpeculative = strings.Join(newWords[bestNewIndex:], " ")
		return stablePrefix, newSpeculative
	}

	// Fallback to old sliding window match if the new logic didn't find anything
	return alignTranscripts(old, new)
}

// alignTranscripts attempts to find a stable prefix between an old speculative result 
// and a new one. It returns the stable prefix and the updated speculative part.
func alignTranscripts(old, new string) (stablePrefix, newSpeculative string) {
	oldWords := strings.Fields(old)
	newWords := strings.Fields(new)
	
	if len(oldWords) == 0 {
		return "", new
	}
	
	// Case 1: Simple prefix match (new is longer and starts with old)
	// If the new result is a strict superset of the old result, 
	// we can safely advance everything in 'old' that isn't the very last word.
	isSuperset := true
	if len(newWords) < len(oldWords) {
		isSuperset = false
	} else {
		for i := 0; i < len(oldWords); i++ {
			if !strings.EqualFold(oldWords[i], newWords[i]) {
				isSuperset = false
				break
			}
		}
	}

	if isSuperset && len(oldWords) >= 3 {
		// Advance all but the last 2 words as stable
		stableIdx := len(oldWords) - 2
		stablePrefix = strings.Join(oldWords[:stableIdx], " ")
		newSpeculative = strings.Join(newWords[stableIdx:], " ")
		return stablePrefix, newSpeculative
	}

	// Case 2: Sliding window match (overlap)
	// Find the longest suffix of oldWords that matches a prefix of newWords.
	maxOverlap := 0
	overlapFoundAt := -1
	for i := 0; i < len(oldWords); i++ {
		overlap := 0
		for j := 0; j < len(newWords) && i+j < len(oldWords); j++ {
			if strings.EqualFold(oldWords[i+j], newWords[j]) {
				overlap++
			} else {
				break
			}
		}
		// If it matches until the end of oldWords, it's a candidate
		if i+overlap == len(oldWords) && overlap > maxOverlap {
			maxOverlap = overlap
			overlapFoundAt = i
		}
	}
	
	if maxOverlap >= 2 && overlapFoundAt >= 0 {
		// Stable prefix is everything in old before the match started
		stablePrefix = strings.Join(oldWords[:overlapFoundAt], " ")
		newSpeculative = new
		return stablePrefix, newSpeculative
	}
	
	// No strong consensus, just update speculative part
	return "", new
}

// AudioEnergy calculates the RMS energy of 16-bit PCM mono audio data.
func AudioEnergy(pcmData []byte) float64 {
	if len(pcmData) == 0 {
		return 0
	}
	var sum float64
	count := 0
	for i := 0; i < len(pcmData)-1; i += 2 {
		sample := int16(binary.LittleEndian.Uint16(pcmData[i : i+2]))
		sum += float64(sample) * float64(sample)
		count++
	}
	if count == 0 {
		return 0
	}
	return math.Sqrt(sum / float64(count)) / 32768.0
}

// Filter determines if a transcription should be ignored as an artifact/hallucination.
func Filter(text string) (string, bool) {
	text = strings.TrimSpace(text)
	text = strings.ReplaceAll(text, "[BLANK_AUDIO]", "")
	text = strings.ReplaceAll(text, "[Audio]", "")
	text = strings.ReplaceAll(text, "(silence)", "")
	text = strings.TrimSpace(text)

	if text == "" || text == "." || text == "..." {
		return "", true
	}

	lower := strings.ToLower(text)
	cleaner := strings.Trim(lower, ".,!? ")

	// Common Whisper artifacts/hallucinations (robotic phrases)
	artifacts := []string{
		"thank you.", "thank you for watching", "thanks for watching",
		"please like and subscribe", "youtube.com", "re-encoded by",
	}

	for _, a := range artifacts {
		if cleaner == a {
			return "", true
		}
	}

	// Handle bracketed/parenthesized annotations
	if (strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]")) ||
		(strings.HasPrefix(text, "(") && strings.HasSuffix(text, ")")) {
		lowerText := strings.ToLower(text)
		if lowerText == "[pause]" || lowerText == "[resume]" || lowerText == "[request_sync]" || lowerText == "[whisper_status]" {
			return text, false // Pass through special markers
		}
		return "", true
	}

	if strings.Contains(lower, "thank you") && len(text) < 15 {
		return "", true
	}

	return text, false
}

// IsSubstantial returns true if the text contains meaningful content beyond common filler words.
func IsSubstantial(text string) bool {
	lower := strings.ToLower(strings.Trim(text, ".,!? "))
	if lower == "" {
		return false
	}

	// List of suppressed filler/noise words
	fillers := map[string]bool{
		"uh":    true,
		"um":    true,
		"ah":    true,
		"er":    true,
		"oh":    true,
		"ok":    true,
		"okay":  true,
		"hmm":   true,
		"mhm":   true,
		"yeah":  true,
		"yes":   true,
		"yep":   true,
		"right": true,
		"well":  true,
		"so":    true,
		"like":  true,
		"just":  true,
	}

	words := strings.Fields(lower)
	if len(words) == 0 {
		return false
	}

	// If we have more than 2 words, it's likely a real sentence/request
	if len(words) > 2 {
		return true
	}

	// Check if the remaining words are all fillers
	substantialDetected := false
	for _, w := range words {
		clean := strings.Trim(w, ".,!? ")
		if !fillers[clean] {
			substantialDetected = true
			break
		}
	}

	return substantialDetected
}

// AddWavHeader wraps PCM data in a WAV container.
func AddWavHeader(pcmData []byte, sampleRate int) []byte {
	buf := new(bytes.Buffer)
	buf.Write([]byte("RIFF"))
	binary.Write(buf, binary.LittleEndian, uint32(36+len(pcmData)))
	buf.Write([]byte("WAVE"))
	buf.Write([]byte("fmt "))
	binary.Write(buf, binary.LittleEndian, uint32(16))
	binary.Write(buf, binary.LittleEndian, uint16(1)) // AudioFormat: PCM
	binary.Write(buf, binary.LittleEndian, uint16(1)) // NumChannels: Mono
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(buf, binary.LittleEndian, uint16(2))
	binary.Write(buf, binary.LittleEndian, uint16(16))
	buf.Write([]byte("data"))
	binary.Write(buf, binary.LittleEndian, uint32(len(pcmData)))
	buf.Write(pcmData)
	return buf.Bytes()
}
