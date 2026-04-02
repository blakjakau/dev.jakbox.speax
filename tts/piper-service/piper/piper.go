package piper

/*
#cgo CXXFLAGS: -std=c++17 -I${SRCDIR}/../include
#cgo LDFLAGS: -L${SRCDIR}/../lib -lpiper -lpiper_phonemize -lonnxruntime -lespeak-ng -lfmt -lspdlog -lpthread -lstdc++
#include "bridge.h"
#include <stdlib.h>

extern void goAudioCallback(int16_t* data, int length, void* userdata);

static inline int call_stream_synth(PiperContext ctx, const char* text, void* userdata, float length_scale, float noise_scale, float noise_w) {
    return piper_synthesize_stream(ctx, text, goAudioCallback, (void*)userdata, length_scale, noise_scale, noise_w);
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const (
	MaxCachedModels = 10
	ModelTTL        = 10 * time.Minute
)

var (
	registryMu sync.RWMutex
	registry   = make(map[uintptr]func([]int16))
	nextID     uintptr
)

func registerCallback(cb func([]int16)) uintptr {
	registryMu.Lock()
	defer registryMu.Unlock()
	nextID++
	registry[nextID] = cb
	return nextID
}

func unregisterCallback(id uintptr) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(registry, id)
}

//export goAudioCallback
func goAudioCallback(data *C.int16_t, length C.int, userdata unsafe.Pointer) {
	id := uintptr(userdata)
	registryMu.RLock()
	cb, ok := registry[id]
	registryMu.RUnlock()
	if ok {
		slice := unsafe.Slice((*int16)(unsafe.Pointer(data)), int(length))
		chunk := make([]int16, len(slice))
		copy(chunk, slice)
		cb(chunk)
	}
}

type Engine struct {
	context           C.PiperContext
	modelPath         string
	configPath        string
	size              int64
	sampleRate        int
	lastUsed          time.Time
	totalAudioSeconds float64
	mu                sync.Mutex // Protects synthesis AND context closing
}

func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.context != nil {
		C.piper_free_context(e.context)
		e.context = nil
	}
}

type GlobalMetrics struct {
	StartTime              time.Time
	TotalUtterances        uint64
	TotalWords             uint64
	TotalAudioSeconds      float64
	TotalInferenceDuration time.Duration
	TotalRequestBytes      uint64
	TotalResponseBytes     uint64
	mu                     sync.Mutex
}

type Manager struct {
	cache      map[string]*Engine
	activeName string
	dataPath   string // eSpeak data path
	modelDir   string
	metrics    GlobalMetrics
	mu         sync.RWMutex
}

func NewManager(modelDir, espeakData string) *Manager {
	m := &Manager{
		cache:    make(map[string]*Engine),
		dataPath: espeakData,
		modelDir: modelDir,
		metrics: GlobalMetrics{
			StartTime: time.Now(),
		},
	}

	// Start background reaper
	go m.reaper()

	return m
}

func Initialize(espeakDataPath string) {
	cPath := C.CString(espeakDataPath)
	defer C.free(unsafe.Pointer(cPath))
	C.piper_initialize(cPath)
}

func Terminate() {
	C.piper_terminate()
}

func (m *Manager) RecordBytes(in, out uint64) {
	m.metrics.mu.Lock()
	defer m.metrics.mu.Unlock()
	m.metrics.TotalRequestBytes += in
	m.metrics.TotalResponseBytes += out
}

func (m *Manager) reaper() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		m.mu.Lock()
		now := time.Now()
		for name, engine := range m.cache {
			// Don't reap the currently active model
			if name == m.activeName {
				continue
			}

			if now.Sub(engine.lastUsed) > ModelTTL {
				fmt.Printf("Reaping dormant model: %s (last used %v ago)\n", name, now.Sub(engine.lastUsed))
				delete(m.cache, name)
				m.mu.Unlock()
				// Close outside management lock to avoid blocking other operations
				engine.Close()
				m.mu.Lock()
			}
		}
		m.mu.Unlock()
	}
}

// LoadModel loads a new model or activates one from cache
func (m *Manager) LoadModel(modelName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// If already in cache, just activate it
	if engine, ok := m.cache[modelName]; ok {
		m.activeName = modelName
		engine.lastUsed = time.Now()
		return nil
	}

	// If at capacity, find the LRU model (excluding active) and purge it
	if len(m.cache) >= MaxCachedModels {
		var lruName string
		var lruTime time.Time
		for name, engine := range m.cache {
			if name == m.activeName {
				continue
			}
			if lruName == "" || engine.lastUsed.Before(lruTime) {
				lruName = name
				lruTime = engine.lastUsed
			}
		}

		if lruName != "" {
			fmt.Printf("Cache full, purging LRU model: %s\n", lruName)
			engine := m.cache[lruName]
			delete(m.cache, lruName)
			// Unlock during C-close to prevent blocking the manager
			m.mu.Unlock()
			engine.Close()
			m.mu.Lock()
		}
	}

	modelPath := filepath.Join(m.modelDir, modelName)
	configPath := modelPath + ".json"

	// Get file size for memory estimate
	info, err := os.Stat(modelPath)
	if err != nil {
		return fmt.Errorf("could not stat model file: %v", err)
	}

	cModel := C.CString(modelPath)
	cConfig := C.CString(configPath)
	cData := C.CString(m.dataPath)
	defer C.free(unsafe.Pointer(cModel))
	defer C.free(unsafe.Pointer(cConfig))
	defer C.free(unsafe.Pointer(cData))

	ctx := C.piper_init_context()
	res := C.piper_load_voice(ctx, cModel, cConfig, cData)
	if res != 0 {
		if ctx != nil {
			C.piper_free_context(ctx)
		}
		return fmt.Errorf("failed to load model %s", modelPath)
	}

	// Parse json config to extract sample rate
	sampleRate := 22050
	if b, err := os.ReadFile(configPath); err == nil {
		var configData struct {
			Audio struct {
				SampleRate int `json:"sample_rate"`
			} `json:"audio"`
		}
		if json.Unmarshal(b, &configData) == nil && configData.Audio.SampleRate > 0 {
			sampleRate = configData.Audio.SampleRate
		}
	}

	engine := &Engine{
		context:    ctx,
		modelPath:  modelPath,
		configPath: configPath,
		size:       info.Size(),
		sampleRate: sampleRate,
		lastUsed:   time.Now(),
	}

	m.cache[modelName] = engine
	m.activeName = modelName

	return nil
}

// GetSampleRate returns the audio sample rate of the currently active model.
func (m *Manager) GetSampleRate() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	engine := m.cache[m.activeName]
	if engine != nil {
		return engine.sampleRate
	}
	return 22050
}


// Synthesize text using the currently active model
func (m *Manager) Synthesize(text string, lengthScale, noiseScale, noiseW float32) ([]int16, error) {
	m.mu.RLock()
	activeEngine := m.cache[m.activeName]
	m.mu.RUnlock()

	if activeEngine == nil {
		return nil, errors.New("no model currently loaded")
	}

	wordCount := uint64(len(strings.Fields(text)))
	start := time.Now()

	// Double safety: update lastUsed when synthesis starts
	activeEngine.lastUsed = time.Now()

	// Ensure only one synthesis request per engine instance at a time
	// Also prevents closing the context while synthesis is in progress
	activeEngine.mu.Lock()
	defer activeEngine.mu.Unlock()

	if activeEngine.context == nil {
		return nil, errors.New("engine context was closed")
	}

	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))

	var cBuffer *C.int16_t
	length := C.piper_synthesize(activeEngine.context, cText, &cBuffer, C.float(lengthScale), C.float(noiseScale), C.float(noiseW))
	
	if length < 0 {
		return nil, errors.New("synthesis failed in Cgo")
	}

	duration := time.Since(start)

	if length == 0 || cBuffer == nil {
		return []int16{}, nil
	}
	defer C.piper_free_buffer(cBuffer)

	// Copy bounds to a Go slice safely
	slice := unsafe.Slice((*int16)(unsafe.Pointer(cBuffer)), length)
	result := make([]int16, length)
	copy(result, slice)

	audioDuration := float64(length) / float64(activeEngine.sampleRate)
	
	// Update metrics
	activeEngine.totalAudioSeconds += audioDuration
	m.metrics.mu.Lock()
	m.metrics.TotalUtterances++
	m.metrics.TotalWords += wordCount
	m.metrics.TotalAudioSeconds += audioDuration
	m.metrics.TotalInferenceDuration += duration
	m.metrics.mu.Unlock()

	return result, nil
}

// SynthesizeStream synthesizes text and calls back with audio chunks as they are produced.
func (m *Manager) SynthesizeStream(text string, lengthScale, noiseScale, noiseW float32, cb func([]int16)) error {
	m.mu.RLock()
	activeEngine := m.cache[m.activeName]
	m.mu.RUnlock()

	if activeEngine == nil {
		return errors.New("no model currently loaded")
	}

	wordCount := uint64(len(strings.Fields(text)))
	start := time.Now()

	activeEngine.lastUsed = time.Now()
	activeEngine.mu.Lock()
	defer activeEngine.mu.Unlock()

	if activeEngine.context == nil {
		return errors.New("engine context was closed")
	}

	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))

	var totalSamples int
	wrappedCB := func(samples []int16) {
		totalSamples += len(samples)
		cb(samples)
	}

	cbID := registerCallback(wrappedCB)
	defer unregisterCallback(cbID)

	res := C.call_stream_synth(activeEngine.context, cText, unsafe.Pointer(cbID), C.float(lengthScale), C.float(noiseScale), C.float(noiseW))
	if res != 0 {
		return errors.New("streaming synthesis failed in Cgo")
	}

	duration := time.Since(start)
	audioDuration := float64(totalSamples) / float64(activeEngine.sampleRate)

	// Update metrics
	activeEngine.totalAudioSeconds += audioDuration
	m.metrics.mu.Lock()
	m.metrics.TotalUtterances++
	m.metrics.TotalWords += wordCount
	m.metrics.TotalAudioSeconds += audioDuration
	m.metrics.TotalInferenceDuration += duration
	m.metrics.mu.Unlock()

	return nil
}

// Close cleans up all cached models
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	for _, engine := range m.cache {
		engine.Close()
	}
	m.cache = make(map[string]*Engine)
}

type ModelInfo struct {
	Name              string    `json:"name"`
	SizeBytes         int64     `json:"size_bytes"`
	LastUsed          time.Time `json:"last_used"`
	ExpiresInSeconds  float64   `json:"expires_in_seconds"`
	TotalAudioSeconds float64   `json:"total_audio_seconds"`
}

func (m *Manager) GetDetailedStatus() (string, []ModelInfo, int64, map[string]interface{}) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	active := m.activeName
	cacheInfo := make([]ModelInfo, 0, len(m.cache))
	var totalSize int64

	now := time.Now()
	for name, engine := range m.cache {
		expires := 0.0
		if name != active {
			expires = ModelTTL.Seconds() - now.Sub(engine.lastUsed).Seconds()
			if expires < 0 {
				expires = 0
			}
		}

		cacheInfo = append(cacheInfo, ModelInfo{
			Name:              name,
			SizeBytes:         engine.size,
			LastUsed:          engine.lastUsed,
			ExpiresInSeconds:  expires,
			TotalAudioSeconds: engine.totalAudioSeconds,
		})
		totalSize += engine.size
	}

	m.metrics.mu.Lock()
	uptime := time.Since(m.metrics.StartTime)
	rtf := 0.0
	if m.metrics.TotalAudioSeconds > 0 {
		rtf = m.metrics.TotalInferenceDuration.Seconds() / m.metrics.TotalAudioSeconds
	}

	h := int(m.metrics.TotalAudioSeconds / 3600)
	min := int(m.metrics.TotalAudioSeconds/60) % 60
	sec := int(m.metrics.TotalAudioSeconds) % 60
	audioDurationFormatted := fmt.Sprintf("%dh %dm %ds", h, min, sec)

	metrics := map[string]interface{}{
		"uptime_seconds":           uptime.Seconds(),
		"total_utterances":         m.metrics.TotalUtterances,
		"total_words":              m.metrics.TotalWords,
		"total_audio_seconds":      m.metrics.TotalAudioSeconds,
		"total_audio_duration_hms": audioDurationFormatted,
		"average_real_time_factor": rtf,
		"total_bytes_received":     m.metrics.TotalRequestBytes,
		"total_bytes_sent":         m.metrics.TotalResponseBytes,
	}
	m.metrics.mu.Unlock()

	return active, cacheInfo, totalSize, metrics
}

// Keeping simple status for legacy/minimal if needed
func (m *Manager) Status() (string, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	active := m.activeName
	prev := "" // Manager no longer explicitly tracks just one "Previous"
	return active, prev
}
