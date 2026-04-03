package main

import (
	"fmt"
	"net/http"
	"strings"
)

// MetricsHandler serves queue metrics in Prometheus exposition format
// at GET /mgmt/metrics and JSON at GET /mgmt/metrics?format=json.
type MetricsHandler struct {
	queue    *JobQueue
	registry *Registry
}

// NewMetricsHandler creates a metrics handler.
func NewMetricsHandler(queue *JobQueue, registry *Registry) *MetricsHandler {
	return &MetricsHandler{queue: queue, registry: registry}
}

func (h *MetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	m := h.queue.Metrics()
	handlers := len(h.registry.GetAllToolInfo())

	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, http.StatusOK, map[string]any{
			"sessions":             m.Sessions,
			"max_jobs_per_session": m.MaxJobsPerSession,
			"jobs_by_status":       m.JobsByStatus,
			"active_jobs":          m.ActiveJobs,
			"peak_session_active":  m.PeakSessionActive,
			"chunks":               m.Chunks,
			"chunks_expired":       m.ChunksExpired,
			"handlers":             handlers,
		})
		return
	}

	// Prometheus text exposition format.
	var b strings.Builder
	prom := func(name, help, typ string, value int) {
		fmt.Fprintf(&b, "# HELP %s %s\n", name, help)
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, typ)
		fmt.Fprintf(&b, "%s %d\n", name, value)
	}
	promLabeled := func(name, label, lval string, value int) {
		fmt.Fprintf(&b, "%s{%s=%q} %d\n", name, label, lval, value)
	}

	prom("mcpeto_sessions_total", "Number of active sessions.", "gauge", m.Sessions)
	prom("mcpeto_handlers_total", "Number of registered tool handlers.", "gauge", handlers)
	prom("mcpeto_jobs_active", "Number of queued + running jobs.", "gauge", m.ActiveJobs)
	prom("mcpeto_jobs_per_session_limit", "Max active jobs allowed per session (0=unlimited).", "gauge", m.MaxJobsPerSession)
	prom("mcpeto_jobs_per_session_peak", "Highest active job count across any single session.", "gauge", m.PeakSessionActive)
	prom("mcpeto_chunks_total", "Number of live chunks in store.", "gauge", m.Chunks)
	prom("mcpeto_chunks_expired", "Chunks cleaned up as expired this scrape.", "gauge", m.ChunksExpired)

	fmt.Fprintf(&b, "# HELP mcpeto_jobs_total Jobs by status.\n")
	fmt.Fprintf(&b, "# TYPE mcpeto_jobs_total gauge\n")
	for _, status := range []string{JobStatusQueued, JobStatusRunning, JobStatusDone, JobStatusFailed} {
		promLabeled("mcpeto_jobs_total", "status", status, m.JobsByStatus[status])
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, b.String())
}
