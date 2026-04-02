package api

import (
	"encoding/json"
	"net/http"
	"runtime"
	"time"
)

var startTime = time.Now()

func (api *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	metrics := api.GetHealthMetrics()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metrics)
}

func (api *API) GetHealthMetrics() map[string]interface{} {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	current, previous := api.Manager.Status()

	return map[string]interface{}{
		"status":             "ok",
		"uptime_seconds":     time.Since(startTime).Seconds(),
		"memory_usage_bytes": memStats.Alloc, // Use bytes to match formatBytes in frontend
		"memory_sys_bytes":   memStats.Sys,
		"goroutines":         runtime.NumGoroutine(),
		"cpu_usage_percent":  0.0, // Placeholder
		"models": map[string]string{
			"current":  current,
			"previous": previous,
		},
	}
}
