package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"speaks.jakbox.dev/tts"
)

type VoiceOption struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type PiperNode struct {
	URL             string
	Zombie          bool
	ServiceType     string
	AvailableVoices []string
	FailureCount    int
	TotalRequests   int
	TotalFailures   int
}

var (
	ttsNodes      []*PiperNode
	ttsNodesMutex sync.RWMutex
	ttsIndex      uint32
)

func matchVoice(pattern string, available []string) string {
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		for _, v := range available {
			if strings.HasPrefix(v, prefix) {
				return v
			}
		}
	} else {
		for _, v := range available {
			if v == pattern {
				return v
			}
			// Check without .onnx extensions to handle Piper filenames vs config names
			if strings.TrimSuffix(v, ".onnx") == strings.TrimSuffix(pattern, ".onnx") {
				return v
			}
		}
	}
	return ""
}

func (s *ClientSession) resolveVoice(p *Persona) (*PiperNode, string, error) {
	ttsNodesMutex.RLock()
	defer ttsNodesMutex.RUnlock()

	allowedPool := s.GetTTSServers()
	allowedMap := make(map[string]bool)
	for _, u := range allowedPool {
		allowedMap[u] = true
	}

	// 1. Try modern Voice array if present
	if len(p.Voice) > 0 {
		for _, opt := range p.Voice {
			// Try all nodes for this option
			for _, node := range ttsNodes {
				if node.Zombie || !allowedMap[node.URL] {
					continue
				}
				// If opt.Type is specified, must match. If empty, any node type works.
				if opt.Type != "" && node.ServiceType != opt.Type {
					continue
				}
				if matched := matchVoice(opt.Name, node.AvailableVoices); matched != "" {
					return node, matched, nil
				}
			}
		}
	}

	// 2. Fallback to legacy VoiceFile
	if p.VoiceFile != "" {
		for _, node := range ttsNodes {
			if node.Zombie || !allowedMap[node.URL] {
				continue
			}
			if matched := matchVoice(p.VoiceFile, node.AvailableVoices); matched != "" {
				return node, matched, nil
			}
		}
	}

	return nil, "", fmt.Errorf("no suitable TTS node found for persona %s", p.Name)
}

func (s *ClientSession) setupRemoteTTS(targetPersonaName string, defaultVoice string) (*tts.RemotePiper, string) {
	s.Mutex.Lock()
	clientVersion := s.Version
	s.Mutex.Unlock()

	if clientVersion < 1 {
		return nil, ""
	}

	personasMutex.RLock()
	p, hasPersona := personas[strings.ToLower(targetPersonaName)]
	personasMutex.RUnlock()

	if hasPersona {
		if node, voiceName, err := s.resolveVoice(&p); err == nil {
			log.Printf("[TTS] Resolved voice '%s' on node %s for persona %s", voiceName, node.URL, targetPersonaName)
			rp, err := tts.NewRemotePiper(node.URL)
			if err == nil {
				return rp, voiceName
			}
			log.Printf("[TTS] Failed to connect to resolved node %s: %v", node.URL, err)
		}
	}

	// Fallback to simple healthy node if resolveVoice failed or connection failed
	if node, err := s.getHealthyTTSNode(); err == nil {
		log.Printf("[TTS] Falling back to healthy node %s", node.URL)
		rp, err := tts.NewRemotePiper(node.URL)
		if err == nil {
			// Try to match the voice name on this node if possible
			voiceName := defaultVoice
			if hasPersona && p.VoiceFile != "" {
				voiceName = p.VoiceFile
			}
			if matched := matchVoice(voiceName, node.AvailableVoices); matched != "" {
				voiceName = matched
			}
			return rp, voiceName
		}
	}

	return nil, ""
}

var ErrNoTTSNodes = fmt.Errorf("no TTS service nodes available")

func (s *ClientSession) getHealthyTTSNode() (*PiperNode, error) {
	ttsNodesMutex.RLock()
	defer ttsNodesMutex.RUnlock()

	numNodes := len(ttsNodes)
	if numNodes == 0 {
		return nil, ErrNoTTSNodes
	}

	allowedPool := s.GetTTSServers()
	allowedMap := make(map[string]bool)
	for _, u := range allowedPool {
		allowedMap[u] = true
	}

	// Try all nodes, but only pick from our session's allowed pool
	for i := 0; i < numNodes; i++ {
		idx := atomic.AddUint32(&ttsIndex, 1) - 1
		node := ttsNodes[idx%uint32(numNodes)]
		if !node.Zombie && allowedMap[node.URL] {
			return node, nil
		}
	}

	return nil, ErrNoTTSNodes
}

func updateTTSNodes(allURLs []string) {
	ttsNodesMutex.Lock()
	defer ttsNodesMutex.Unlock()

	existingTTSNodes := make(map[string]*PiperNode)
	for _, node := range ttsNodes {
		existingTTSNodes[node.URL] = node
	}

	var newTTSNodes []*PiperNode
	for _, url := range allURLs {
		if node, exists := existingTTSNodes[url]; exists {
			newTTSNodes = append(newTTSNodes, node)
		} else {
			newTTSNodes = append(newTTSNodes, &PiperNode{URL: url, Zombie: false})
		}
	}
	ttsNodes = newTTSNodes
}

func startTTSPolling() {
	ticker := time.NewTicker(60 * time.Second)
	// Run once immediately
	pollTTSNodes()
	for range ticker.C {
		pollTTSNodes()
	}
}

func pollTTSNodes() {
	ttsNodesMutex.RLock()
	nodes := make([]*PiperNode, len(ttsNodes))
	copy(nodes, ttsNodes)
	ttsNodesMutex.RUnlock()

	client := http.Client{
		Timeout: 5 * time.Second,
	}

	for _, node := range nodes {
		// 1. Check Status
		resp, err := client.Get(node.URL + "/status")
		if err != nil {
			node.FailureCount++
			if node.FailureCount >= 3 {
				node.Zombie = true
			}
			log.Printf("[TTS Poller] Error polling %s: %v (Failures: %d)", node.URL, err, node.FailureCount)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			node.FailureCount++
			if node.FailureCount >= 3 {
				node.Zombie = true
			}
			log.Printf("[TTS Poller] Node %s returned status %s (Failures: %d)", node.URL, resp.Status, node.FailureCount)
			continue
		}

		var status struct {
			ServiceType string `json:"service_type"`
			Service     string `json:"service"` // Fallback for Spark-TTS if needed
		}
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			resp.Body.Close()
			log.Printf("[TTS Poller] Failed to decode status from %s: %v", node.URL, err)
			continue
		}
		resp.Body.Close()

		node.Zombie = false
		node.FailureCount = 0
		if status.ServiceType != "" {
			node.ServiceType = status.ServiceType
		} else if status.Service != "" {
			node.ServiceType = status.Service
		}

		if node.ServiceType == "" {
			// Heuristic/Default if not provided
			if strings.Contains(node.URL, "4410") {
				node.ServiceType = "piper"
			} else if strings.Contains(node.URL, "4411") || strings.Contains(node.URL, "4412") {
				node.ServiceType = "kokoro"
			} else if strings.Contains(node.URL, "4413") {
				node.ServiceType = "spark-tts"
			}
		}

		// 2. Fetch Models (Voices)
		mResp, err := client.Get(node.URL + "/models")
		if err == nil && mResp.StatusCode == http.StatusOK {
			bodyBytes, _ := io.ReadAll(mResp.Body)
			mResp.Body.Close()

			var voices []string

			// Strategy 1: Simple list of strings ["voice1", "voice2"]
			if err := json.Unmarshal(bodyBytes, &voices); err == nil && len(voices) > 0 {
				node.AvailableVoices = voices
			} else {
				// Strategy 2: Map with an array under a common key like "voices", "models", "items"
				var genericMap map[string]json.RawMessage
				if err := json.Unmarshal(bodyBytes, &genericMap); err == nil {
					potentialKeys := []string{"voices", "models", "items", "data", "list"}
					for _, k := range potentialKeys {
						if data, ok := genericMap[k]; ok {
							// Try string list
							var sList []string
							if err := json.Unmarshal(data, &sList); err == nil && len(sList) > 0 {
								voices = sList
								break
							}
							// Try object list with id/name
							var oList []map[string]interface{}
							if err := json.Unmarshal(data, &oList); err == nil && len(oList) > 0 {
								for _, item := range oList {
									if id, ok := item["id"].(string); ok {
										voices = append(voices, id)
									} else if name, ok := item["name"].(string); ok {
										voices = append(voices, name)
									}
								}
								if len(voices) > 0 {
									break
								}
							}
						}
					}
				}

				// Strategy 3: Just look for *any* array of objects with "id"/"name" at top level
				if len(voices) == 0 {
					var topLevelList []map[string]interface{}
					if err := json.Unmarshal(bodyBytes, &topLevelList); err == nil && len(topLevelList) > 0 {
						for _, item := range topLevelList {
							if id, ok := item["id"].(string); ok {
								voices = append(voices, id)
							} else if name, ok := item["name"].(string); ok {
								voices = append(voices, name)
							}
						}
					}
				}

				node.AvailableVoices = voices
			}
			if len(node.AvailableVoices) > 0 {
				newVoiceCount := len(node.AvailableVoices)
				// Only log if it's a significant event (discovery or change) to reduce noise
				log.Printf("[TTS Poller] Node %s (%s) healthy, %d voices available", node.URL, node.ServiceType, newVoiceCount)
			}
		} else if err == nil {
			mResp.Body.Close()
		}
	}
}
