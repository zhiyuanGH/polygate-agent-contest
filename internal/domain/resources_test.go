package domain

import "testing"

func TestValidateSpec(t *testing.T) {
	base := AppSpec{Name: "agent-one", Image: "registry.example/agent@sha256:123", Port: 8080, Resources: Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 4096, Replicas: 1}}
	if err := ValidateSpec(base); err != nil {
		t.Fatal(err)
	}
	base.Resources.Replicas = 2
	base.Persistent = true
	if err := ValidateSpec(base); err == nil {
		t.Fatal("persistent app accepted two replicas")
	}
	base.Persistent = false
	base.Resources.MemoryMiB = 4097
	if err := ValidateSpec(base); err == nil {
		t.Fatal("accepted memory above 4GiB")
	}
}

func TestValidateHealthPath(t *testing.T) {
	base := AppSpec{Name: "agent-one", Image: "registry.example/agent:v1", Port: 8080,
		Resources: Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	for _, path := range []string{"", "/health", "/ready/live_v2"} {
		base.HealthPath = path
		if err := ValidateSpec(base); err != nil {
			t.Fatalf("valid health path %q: %v", path, err)
		}
	}
	for _, path := range []string{"health", "//admin", "/a/../b", "/health?token=secret", "/x#frag", "/with space", "/%2e%2e", "/héalth"} {
		base.HealthPath = path
		if err := ValidateSpec(base); err == nil {
			t.Fatalf("accepted invalid health path %q", path)
		}
	}
}

func TestSecretKeySelection(t *testing.T) {
	base := AppSpec{Name: "agent-one", Image: "registry.example/agent:v1", Port: 8080,
		Resources: Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	base.SecretKeys = []string{"OPENAI_API_KEY", "HF_TOKEN"}
	if err := ValidateSpec(base); err != nil {
		t.Fatal(err)
	}
	for _, keys := range [][]string{{"OPENAI_API_KEY", "OPENAI_API_KEY"}, {"polygate_token"},
		{"POLYGATE_TOKEN"}, {"KUBERNETES_SERVICE_HOST"}, {"BAD-NAME"},
		{"A", "B", "C", "D", "E", "F", "G", "H", "I"}} {
		base.SecretKeys = keys
		if err := ValidateSpec(base); err == nil {
			t.Fatalf("accepted invalid Secret keys %v", keys)
		}
	}
}

func TestAccountAndGlobalAdmission(t *testing.T) {
	item := Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 4096, Replicas: 1}.Usage()
	if err := CheckAdmission(Usage{}, Usage{}, item, StudentQuota, GlobalQuota); err != nil {
		t.Fatal(err)
	}
	if e := AsDomainError(CheckAdmission(item, item, item, StudentQuota, GlobalQuota)); e == nil || e.Code != "QUOTA_EXCEEDED" || e.Field != "memoryMiB" {
		t.Fatalf("account limit: %#v", e)
	}
	nearGlobal := Usage{MemoryMiB: 102400 - 2048}
	if e := AsDomainError(CheckAdmission(Usage{}, nearGlobal, item, StudentQuota, GlobalQuota)); e == nil || e.Code != "GLOBAL_CAPACITY_EXCEEDED" {
		t.Fatalf("global limit: %#v", e)
	}
}

func TestRollingUpdateCountsOldAndNew(t *testing.T) {
	old := Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 3072, Replicas: 1}.Usage()
	newRevision := old
	newRevision.Apps = 0 // The app is already counted; the new pod still needs a reservation.
	if e := AsDomainError(CheckAdmission(old, old, newRevision, StudentQuota, GlobalQuota)); e == nil || e.Field != "memoryMiB" {
		t.Fatalf("peak usage: %#v", e)
	}
	if err := CheckAdmission(Usage{}, Usage{}, old, StudentQuota, GlobalQuota); err != nil {
		t.Fatal(err)
	}
}
