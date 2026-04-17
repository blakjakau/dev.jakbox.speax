package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"speaks.jakbox.dev/stt"
)

type AdminConfigOverride struct {
	LLMURL               *string                    `json:"LLMURL,omitempty"`
	LLMModels            *[]string                  `json:"LLMModels,omitempty"`
	OllamaModel          *string                    `json:"OllamaModel,omitempty"`
	TTSServers           *[]string                  `json:"TTSServers,omitempty"`
	STTServers           *[]string                  `json:"STTServers,omitempty"`
	SystemPromptGemini   *string                    `json:"SystemPromptGemini,omitempty"`
	SystemPromptOllama   *string                    `json:"SystemPromptOllama,omitempty"`
	SystemPromptLlamaCpp *string                    `json:"SystemPromptLlamaCpp,omitempty"`
	ToolSystemPrompt     *string                    `json:"ToolSystemPrompt,omitempty"`
	MaxTokensGemini      *int                       `json:"MaxTokensGemini,omitempty"`
	MaxTokensOllama      *int                       `json:"MaxTokensOllama,omitempty"`
	MaxTokensLlamaCpp    *int                       `json:"MaxTokensLlamaCpp,omitempty"`
	PassiveWindowSeconds *int                       `json:"PassiveWindowSeconds,omitempty"`
	WakeWords            *[]string                  `json:"WakeWords,omitempty"`
	LlamaCppURLs         *[]string                  `json:"LlamaCppURLs,omitempty"`
	ModelLimits          *map[string]ModelRateLimit `json:"ModelLimits,omitempty"`
}

type Config struct {
	STTServers           []string                       `json:"STTServers"`
	TTSServers           []string                       `json:"TTSServers"`
	PiperBin             string                         `json:"PiperBin"`
	DefaultVoice         string                         `json:"DefaultVoice"`
	SampleRate           int                            `json:"SampleRate"`
	OllamaURLs           []string                       `json:"OllamaURLs"`
	OllamaChatURL        []string                       `json:"OllamaChatURL"`
	OllamaModel          string                         `json:"OllamaModel"`
	WakeWords            []string                       `json:"WakeWords"`
	PassiveWindowSeconds int                            `json:"PassiveWindowSeconds"`
	MaxArchiveTurns      int                            `json:"MaxArchiveTurns"`
	MaxTokensGemini      int                            `json:"MaxTokensGemini"`
	MaxTokensOllama      int                            `json:"MaxTokensOllama"`
	MaxTokensLlamaCpp    int                            `json:"MaxTokensLlamaCpp"`
	SystemPromptGemini   string                         `json:"SystemPromptGemini"`
	SystemPromptOllama   string                         `json:"SystemPromptOllama"`
	SystemPromptLlamaCpp string                         `json:"SystemPromptLlamaCpp"`
	ToolSystemPrompt     string                         `json:"ToolSystemPrompt"`
	Admins               map[string]AdminConfigOverride `json:"Admins"`
	ModelLimits          map[string]ModelRateLimit      `json:"ModelLimits"`
	DefaultLimit         ModelRateLimit                 `json:"DefaultLimit"`
	FallbackLLM          FallbackLLMConfig              `json:"FallbackLLM"`
	GeminiModels         []string                       `json:"GeminiModels"`
	OllamaModels         []string                       `json:"OllamaModels"`
	LlamaCppModels       []string                       `json:"LlamaCppModels"`
	LlamaCppURLs         []string                       `json:"LlamaCppURLs"`
	LlamaCppModel        string                         `json:"LlamaCppModel"`
	STTMinBuffer         float64                        `json:"STTMinBuffer"`
	STTMaxBuffer         float64                        `json:"STTMaxBuffer"`
	STTEnergyThreshold   float64                        `json:"STTEnergyThreshold"`
	VerboseLogging       bool                           `json:"VerboseLogging"`
}

type ModelRateLimit struct {
	RPM      int    `json:"RPM"` // Requests Per Minute
	TPM      int    `json:"TPM"` // Tokens Per Minute
	RPD      int    `json:"RPD"` // Requests Per Day
	Fallback string `json:"fallback"`
}

func (c *Config) Validate() error {
	if len(c.STTServers) == 0 {
		return fmt.Errorf("STTServers cannot be empty")
	}
	if len(c.OllamaChatURL) == 0 && len(c.LlamaCppURLs) == 0 {
		return fmt.Errorf("must provide either OllamaChatURL or LlamaCppURLs")
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

	// Initialize/Update STT nodes
	allSTTServers := append([]string{}, newConfig.STTServers...)
	seenSTT := make(map[string]bool)
	for _, u := range allSTTServers {
		seenSTT[u] = true
	}
	for _, admin := range newConfig.Admins {
		if admin.STTServers != nil {
			for _, u := range *admin.STTServers {
				if !seenSTT[u] {
					allSTTServers = append(allSTTServers, u)
					seenSTT[u] = true
				}
			}
		}
	}

	if sttManager == nil {
		sttManager = stt.NewManager(allSTTServers, func(msg string) {
			log.Println(msg)
		})
	} else {
		sttManager.UpdateURLs(allSTTServers)
	}

	// Initialize/Update TTS nodes
	allTTSServers := append([]string{}, newConfig.TTSServers...)
	seenTTS := make(map[string]bool)
	for _, u := range allTTSServers {
		seenTTS[u] = true
	}
	for _, admin := range newConfig.Admins {
		if admin.TTSServers != nil {
			for _, u := range *admin.TTSServers {
				if !seenTTS[u] {
					allTTSServers = append(allTTSServers, u)
					seenTTS[u] = true
				}
			}
		}
	}
	updateTTSNodes(allTTSServers)

	// Refresh all sessions' admin cache
	activeSessionsMutex.Lock()
	for _, s := range activeSessions {
		s.Mutex.Lock()
		s.RefreshAdminCache()
		s.Mutex.Unlock()
	}
	activeSessionsMutex.Unlock()

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
				log.Printf("Config file changed, reloading...")
				if err := reloadConfig(path); err != nil {
					log.Printf("Error reloading config: %v", err)
				} else {
					lastModTime = stat.ModTime()
					log.Printf("Config reloaded successfully")
				}
			}
		}
	}()
}
