package source

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

const (
	MaxArchiveBytes  int64 = 100 << 20
	MaxExpandedBytes int64 = 1 << 30
	MaxFiles               = 20_000
)

var ErrInvalidArchive = errors.New("invalid source archive")

// ValidateZIP inspects every entry and streams its content to enforce the
// expanded-size limit. It never extracts untrusted paths on the API host.
func ValidateZIP(r io.ReaderAt, size int64) error {
	if size < 1 || size > MaxArchiveBytes {
		return fmt.Errorf("%w: ZIP must be at most 100MiB", ErrInvalidArchive)
	}
	z, err := zip.NewReader(r, size)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArchive, err)
	}
	if len(z.File) == 0 || len(z.File) > MaxFiles {
		return fmt.Errorf("%w: ZIP must contain 1–20000 entries", ErrInvalidArchive)
	}
	var total int64
	seen := make(map[string]struct{}, len(z.File))
	for _, f := range z.File {
		name := f.Name
		if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || strings.Contains(name, "\x00") {
			return fmt.Errorf("%w: unsafe path", ErrInvalidArchive)
		}
		cleaned := path.Clean(name)
		if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != strings.TrimSuffix(name, "/") || strings.Contains(name, ":") {
			return fmt.Errorf("%w: unsafe path", ErrInvalidArchive)
		}
		key := strings.ToLower(cleaned)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%w: duplicate path", ErrInvalidArchive)
		}
		seen[key] = struct{}{}
		mode := f.Mode()
		if !mode.IsRegular() && !mode.IsDir() {
			return fmt.Errorf("%w: links and special files are forbidden", ErrInvalidArchive)
		}
		if mode.IsDir() {
			continue
		}
		if f.UncompressedSize64 > uint64(MaxExpandedBytes-total) {
			return fmt.Errorf("%w: expanded size exceeds 1GiB", ErrInvalidArchive)
		}
		body, err := f.Open()
		if err != nil {
			return fmt.Errorf("%w: unreadable file", ErrInvalidArchive)
		}
		n, copyErr := io.Copy(io.Discard, io.LimitReader(body, MaxExpandedBytes-total+1))
		closeErr := body.Close()
		if copyErr != nil || closeErr != nil {
			return fmt.Errorf("%w: unreadable file", ErrInvalidArchive)
		}
		total += n
		if total > MaxExpandedBytes {
			return fmt.Errorf("%w: expanded size exceeds 1GiB", ErrInvalidArchive)
		}
	}
	return nil
}
