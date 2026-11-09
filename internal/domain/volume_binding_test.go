package domain

import "testing"

func TestVolumeBindingRejectsUnsafePathsAndMissingOwnershipReference(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	for _, path := range []string{"/", "/proc", "/proc/self", "/sys/kernel", "/dev/null",
		"/tmp/data", "/run/secrets", "/var/run/secrets", "/data/../etc", "/data//nested", "relative"} {
		if err := ValidateVolumeBinding(true, id, path); err == nil {
			t.Fatalf("accepted unsafe mount path %q", path)
		}
	}
	for _, path := range []string{"/data", "/app/data", "/workspace/cache"} {
		if err := ValidateVolumeBinding(true, id, path); err != nil {
			t.Fatalf("rejected safe mount path %q: %v", path, err)
		}
	}
	if err := ValidateVolumeBinding(true, "", "/data"); err == nil {
		t.Fatal("persistent app accepted without volume ID")
	}
	if err := ValidateVolumeBinding(false, id, "/data"); err == nil {
		t.Fatal("stateless app accepted a volume binding")
	}
}
