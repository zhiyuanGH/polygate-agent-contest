package server

import (
	"context"
	"net/http"
	"sort"

	"github.com/google/uuid"
)

const operationHistoryPageSize = 50

func (h *Handler) listOperationHistory(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	store, ok := h.store.(OperationHistoryStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "ACTIVITY_UNAVAILABLE", "Activity history is unavailable.")
		return
	}
	before := r.URL.Query().Get("before")
	if before != "" {
		if _, err := uuid.Parse(before); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ACTIVITY_CURSOR", "The activity cursor is invalid.")
			return
		}
	}
	page, err := store.ListOperationHistory(r.Context(), p.AccountID, before)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *MemoryStore) ListOperationHistory(_ context.Context, accountID, before string) (OperationPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops := append([]Operation(nil), s.operations[accountID]...)
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].CreatedAt.Equal(ops[j].CreatedAt) {
			return ops[i].ID > ops[j].ID
		}
		return ops[i].CreatedAt.After(ops[j].CreatedAt)
	})
	start := 0
	if before != "" {
		start = len(ops)
		for i, item := range ops {
			if item.ID == before {
				start = i + 1
				break
			}
		}
	}
	page := OperationPage{Items: []Operation{}}
	end := min(start+operationHistoryPageSize, len(ops))
	page.Items = append(page.Items, ops[start:end]...)
	if end < len(ops) {
		page.NextCursor = page.Items[len(page.Items)-1].ID
	}
	return page, nil
}
