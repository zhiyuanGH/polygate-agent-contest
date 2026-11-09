package source

import (
	"archive/zip"
	"bytes"
	"testing"
)

func makeZIP(t *testing.T, names ...string) []byte {
	t.Helper()
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	for _, name := range names {
		f, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("FROM scratch\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestValidateZIP(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		valid bool
	}{
		{"valid", []string{"Dockerfile", "src/main.py"}, true},
		{"parent traversal", []string{"../secret"}, false},
		{"nested traversal", []string{"src/../../secret"}, false},
		{"windows path", []string{`C:\temp\a`}, false},
		{"duplicate", []string{"Dockerfile", "dockerfile"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := makeZIP(t, tc.files...)
			err := ValidateZIP(bytes.NewReader(b), int64(len(b)))
			if (err == nil) != tc.valid {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
