package server

import (
	"net/http"
	"time"
)

// accountActualUsage aggregates only the apps returned for the authenticated
// account. Individual Pod samples are checked against owned labels by the
// app log reader before they reach this handler.
func (h *Handler) accountActualUsage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if h.appLogReader == nil {
		writeError(w, http.StatusServiceUnavailable, "ACCOUNT_METRICS_DISABLED", "Resource usage samples are unavailable.")
		return
	}
	apps, _, err := h.store.List(r.Context(), p.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var cpuMilli, memoryBytes int64
	var sampledPods, expectedPods int
	for _, app := range apps {
		if app.Status == "Draft" || app.Status == "Stopped" {
			continue
		}
		if app.Status == "Running" || app.Status == "Deploying" {
			expectedPods += int(app.Spec.Resources.Replicas)
		}
		metrics, err := h.appLogReader.Metrics(r.Context(), p.AccountID, app.ID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "ACCOUNT_METRICS_UNAVAILABLE", "Resource usage samples could not be read.")
			return
		}
		for _, metric := range metrics {
			cpuMilli += metric.CPUMilli
			memoryBytes += metric.MemoryBytes
			sampledPods++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cpuMilli": cpuMilli, "memoryBytes": memoryBytes,
		"sampledPods": sampledPods, "expectedPods": expectedPods,
		"complete":  sampledPods >= expectedPods,
		"checkedAt": time.Now().UTC().Format(time.RFC3339),
	})
}
