package source

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
)

func templateZIP(t *testing.T, names ...string) []byte {
	t.Helper()
	var body bytes.Buffer
	w := zip.NewWriter(&body)
	for _, name := range names {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(f, "sample\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestBuildTemplateProducesValidatedDockerfile(t *testing.T) {
	for _, item := range []struct {
		kind     string
		names    []string
		expected string
	}{
		{"python", []string{"main.py"}, "USER 65532:65532"},
		{"node", []string{"package.json", "package-lock.json"}, "USER 65532:65532"},
	} {
		input := templateZIP(t, item.names...)
		if err := ValidateZIP(bytes.NewReader(input), int64(len(input))); err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := WriteTemplatedZIP(&output, bytes.NewReader(input), int64(len(input)), item.kind); err != nil {
			t.Fatal(err)
		}
		if err := ValidateZIP(bytes.NewReader(output.Bytes()), int64(output.Len())); err != nil {
			t.Fatal(err)
		}
		z, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
		if err != nil {
			t.Fatal(err)
		}
		var dockerfile string
		for _, f := range z.File {
			if f.Name != "Dockerfile" {
				continue
			}
			body, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(body)
			_ = body.Close()
			if err != nil {
				t.Fatal(err)
			}
			dockerfile = string(data)
		}
		if !strings.Contains(dockerfile, item.expected) {
			t.Fatalf("%s Dockerfile: %q", item.kind, dockerfile)
		}
	}
}

func TestBuildTemplateRejectsConflictsAndMissingInputs(t *testing.T) {
	for _, item := range []struct {
		kind  string
		names []string
	}{
		{"dockerfile", []string{"main.py"}},
		{"python", []string{"Dockerfile", "main.py"}},
		{"python", []string{"package.json"}},
		{"node", []string{"package.json"}},
		{"invalid", []string{"main.py"}},
	} {
		input := templateZIP(t, item.names...)
		if err := CheckBuildInputs(bytes.NewReader(input), int64(len(input)), item.kind); err == nil {
			t.Fatalf("accepted %s with %v", item.kind, item.names)
		}
	}
}
