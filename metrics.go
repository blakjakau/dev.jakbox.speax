package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

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

var (
	perfMetrics   *PerformanceMetricsStore
	perfMetricsMu sync.Mutex
)

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
	ttsNodesMutex.RLock()

	resp := map[string]interface{}{
		"llm":       perfMetrics,
		"sttStatus": sttManager.GetStatus(),
		"ttsNodes":  ttsNodes,
	}
	data, err := json.MarshalIndent(resp, "", "  ")

	ttsNodesMutex.RUnlock()
	perfMetricsMu.Unlock()

	if err != nil {
		http.Error(w, `{"error":"serialisation error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}
