package server

import (
	"net/http"
	"os"

	"polygate/internal/clusterstats"
)

func (h *Handler) adminClusterResources(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if p.Role != "admin" {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "Administrator access is required.")
		return
	}
	reader, err := clusterstats.NewReader(os.Getenv("POLYGATE_PROMETHEUS_URL"))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "METRICS_UNAVAILABLE", "Cluster metrics are unavailable.")
		return
	}
	snapshot, err := reader.Read(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "METRICS_UNAVAILABLE", "Cluster metrics are unavailable.")
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}
