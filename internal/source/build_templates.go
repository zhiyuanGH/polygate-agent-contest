package source

import (
	"archive/zip"
	"fmt"
	"io"
	"strings"
)

const PythonBuildTemplate = `FROM python:3.14-alpine3.24@sha256:a18b20ad69e74df6cf6d2e1b1a260d7a1ecf5380fd83b310f83ba88c461cf894
WORKDIR /app
COPY --chown=65532:65532 . /app
RUN if [ -f requirements.txt ]; then pip install --no-cache-dir -r requirements.txt; fi \
    && python -m pip uninstall -y pip
ENV PYTHONUNBUFFERED=1 HOME=/tmp
USER 65532:65532
EXPOSE 8000
CMD ["python", "main.py"]
`

const NodeBuildTemplate = `FROM node:26-alpine@sha256:8996bb5821d62a8fd9bb27c1cb07462f3fdce6e47d640c6a881ead0b064ce216
WORKDIR /app
COPY --chown=65532:65532 . /app
RUN npm ci --omit=dev \
    && printf '%s\n' '#!/bin/sh' 'set -eu' 'export PATH="/app/node_modules/.bin:$PATH"' \
      'start="$(node -p "require(\"./package.json\").scripts.start || \"\"")"' \
      '[ -n "$start" ] || { echo "package.json needs scripts.start" >&2; exit 1; }' \
      'exec sh -c "$start"' > /usr/local/bin/polygate-start \
    && chmod 755 /usr/local/bin/polygate-start \
    && rm -rf /usr/local/lib/node_modules/npm /usr/local/bin/npm /usr/local/bin/npx \
    && chown -R 65532:65532 /app
ENV HOME=/tmp
USER 65532:65532
EXPOSE 3000
CMD ["/usr/local/bin/polygate-start"]
`

func BuildTemplateDockerfile(kind string) (string, error) {
	switch kind {
	case "python":
		return PythonBuildTemplate, nil
	case "node":
		return NodeBuildTemplate, nil
	default:
		return "", fmt.Errorf("%w: unknown build template", ErrInvalidArchive)
	}
}

// CheckBuildInputs runs after ValidateZIP. Templates are only injected into
// archives without a Dockerfile, so a selected template cannot be silently
// replaced by user-supplied build instructions.
func CheckBuildInputs(r io.ReaderAt, size int64, kind string) error {
	if kind != "dockerfile" && kind != "python" && kind != "node" {
		return fmt.Errorf("%w: unknown build template", ErrInvalidArchive)
	}
	z, err := zip.NewReader(r, size)
	if err != nil {
		return fmt.Errorf("%w: unreadable ZIP", ErrInvalidArchive)
	}
	files := map[string]bool{}
	for _, file := range z.File {
		if !strings.Contains(file.Name, "/") && file.Mode().IsRegular() {
			files[file.Name] = true
		}
	}
	if kind == "dockerfile" {
		if !files["Dockerfile"] {
			return fmt.Errorf("%w: ZIP needs a root Dockerfile", ErrInvalidArchive)
		}
		return nil
	}
	for name := range files {
		if strings.EqualFold(name, "Dockerfile") {
			return fmt.Errorf("%w: remove Dockerfile when selecting a template", ErrInvalidArchive)
		}
	}
	if kind == "python" && !files["main.py"] {
		return fmt.Errorf("%w: Python template needs root main.py", ErrInvalidArchive)
	}
	if kind == "node" && (!files["package.json"] || !files["package-lock.json"]) {
		return fmt.Errorf("%w: Node.js template needs root package.json and package-lock.json", ErrInvalidArchive)
	}
	return nil
}

// WriteTemplatedZIP copies already validated compressed members without
// expanding them on the API host, then appends a fixed Dockerfile.
func WriteTemplatedZIP(dst io.Writer, src io.ReaderAt, size int64, kind string) error {
	dockerfile, err := BuildTemplateDockerfile(kind)
	if err != nil {
		return err
	}
	if err = CheckBuildInputs(src, size, kind); err != nil {
		return err
	}
	z, err := zip.NewReader(src, size)
	if err != nil {
		return fmt.Errorf("%w: unreadable ZIP", ErrInvalidArchive)
	}
	w := zip.NewWriter(dst)
	for _, file := range z.File {
		if err = w.Copy(file); err != nil {
			_ = w.Close()
			return err
		}
	}
	f, err := w.CreateHeader(&zip.FileHeader{Name: "Dockerfile", Method: zip.Deflate})
	if err == nil {
		_, err = io.WriteString(f, dockerfile)
	}
	if closeErr := w.Close(); err == nil {
		err = closeErr
	}
	return err
}
