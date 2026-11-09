package source

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var archiveID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// SaveArchive stores a validated ZIP in a trusted, dedicated directory.
// The random artifact ID is the only filename input. The caller must register
// the returned digest in PostgreSQL after this function succeeds.
func SaveArchive(root, id string, src io.Reader, size int64) (string, error) {
	if !archiveID.MatchString(id) || !filepath.IsAbs(root) || size < 1 || size > MaxArchiveBytes {
		return "", errors.New("invalid archive destination or size")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("archive root must be a real directory")
	}
	tmp, err := os.CreateTemp(root, ".pending-*.zip")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err = tmp.Chmod(0600); err != nil {
		return "", err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(src, size+1))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", errors.New("archive size changed during persistence")
	}
	if err = tmp.Sync(); err != nil {
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	// Link is atomic and fails if the ID is already present. It does not replace
	// an existing archive, including on a shared NFS backed source volume.
	if err = os.Link(tmp.Name(), filepath.Join(root, id+".zip")); err != nil {
		return "", err
	}
	dir, err := os.Open(root)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(hash.Sum(nil))), nil
}

func ArchivePath(root, id string) (string, error) {
	if !archiveID.MatchString(id) || !filepath.IsAbs(root) {
		return "", errors.New("invalid archive path")
	}
	return filepath.Join(root, id+".zip"), nil
}

// InspectStoredArchive verifies a completed archive before crash recovery.
// It does not trust the filename alone or a partially written temporary file.
func InspectStoredArchive(root, id string, size int64) (string, error) {
	path, err := ArchivePath(root, id)
	if err != nil {
		return "", err
	}
	if size < 1 || size > MaxArchiveBytes {
		return "", errors.New("invalid stored archive size")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return "", errors.New("stored archive is missing or incomplete")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, opened) || opened.Size() != size {
		return "", errors.New("stored archive changed during inspection")
	}
	if err = ValidateZIP(f, size); err != nil {
		return "", err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	if n, copyErr := io.Copy(hash, io.LimitReader(f, size+1)); copyErr != nil || n != size {
		return "", errors.New("stored archive could not be hashed")
	}
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(hash.Sum(nil))), nil
}
