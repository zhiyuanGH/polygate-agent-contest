package registry

import (
	"bytes"
	"io"
	"testing"
)

func TestImageTransferBudgetRejectsExcessBytes(t *testing.T) {
	budget := &byteBudget{limit: 4, remaining: 4}
	first := &budgetedBody{ReadCloser: io.NopCloser(bytes.NewReader([]byte("abc"))), budget: budget}
	if data, err := io.ReadAll(first); err != nil || string(data) != "abc" {
		t.Fatalf("first blob: %q %v", data, err)
	}
	second := &budgetedBody{ReadCloser: io.NopCloser(bytes.NewReader([]byte("de"))), budget: budget}
	if data, err := io.ReadAll(second); err == nil || string(data) != "d" {
		t.Fatalf("aggregate blob budget was not enforced: %q %v", data, err)
	}
}
