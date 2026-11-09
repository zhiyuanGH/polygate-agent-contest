package source

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const UploadChunkBytes int64 = 8 << 20
const MaxImageArchiveBytes int64 = 1 << 30
const maxUploadStagingBytes int64 = 20 << 30

var ErrUploadInvalid = errors.New("invalid source upload")
var ErrUploadQuota = errors.New("source upload staging limit reached")

type Upload struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Size      int64     `json:"sizeBytes"`
	Template  string    `json:"template"`
	CreatedAt time.Time `json:"createdAt"`
}

func uploadPath(root, id string) (string, error) {
	if !filepath.IsAbs(root) || !archiveID.MatchString(id) {
		return "", ErrUploadInvalid
	}
	return filepath.Join(root, ".uploads", id), nil
}

func CreateUpload(root string, item Upload) error {
	dir, err := uploadPath(root, item.ID)
	if err != nil || item.Owner == "" || item.Size < 1 || item.Size > MaxImageArchiveBytes {
		return ErrUploadInvalid
	}
	parent := filepath.Dir(dir)
	if err = os.Mkdir(parent, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrUploadInvalid
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	active, owned := 0, 0
	var activeBytes int64
	for _, entry := range entries {
		if !entry.IsDir() || !archiveID.MatchString(entry.Name()) {
			continue
		}
		if _, done := os.Stat(filepath.Join(parent, entry.Name(), "result.json")); done == nil {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(parent, entry.Name(), "metadata.json"))
		if readErr != nil || len(data) > 1024 {
			continue
		}
		var existing Upload
		if json.Unmarshal(data, &existing) != nil || time.Since(existing.CreatedAt) > 24*time.Hour {
			continue
		}
		active++
		activeBytes += existing.Size
		if existing.Owner == item.Owner {
			owned++
		}
	}
	if active >= 200 || owned >= 3 || activeBytes > maxUploadStagingBytes-item.Size {
		return ErrUploadQuota
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return err
	}
	item.CreatedAt = time.Now().UTC()
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "metadata.json"), data, 0600); err != nil {
		_ = os.Remove(dir)
		return err
	}
	return nil
}

func LoadUpload(root, id, owner string) (Upload, string, error) {
	dir, err := uploadPath(root, id)
	if err != nil {
		return Upload{}, "", ErrUploadInvalid
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Upload{}, "", ErrUploadInvalid
	}
	data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil || len(data) > 1024 {
		return Upload{}, "", ErrUploadInvalid
	}
	var item Upload
	if json.Unmarshal(data, &item) != nil || item.ID != id || item.Owner != owner ||
		item.Size < 1 || item.Size > MaxImageArchiveBytes || item.CreatedAt.IsZero() ||
		time.Since(item.CreatedAt) > 24*time.Hour {
		return Upload{}, "", ErrUploadInvalid
	}
	return item, dir, nil
}

func (u Upload) ChunkCount() int {
	return int((u.Size + UploadChunkBytes - 1) / UploadChunkBytes)
}

func (u Upload) ChunkSize(index int) int64 {
	if index < 0 || index >= u.ChunkCount() {
		return 0
	}
	return min(UploadChunkBytes, u.Size-int64(index)*UploadChunkBytes)
}

// SaveUploadChunk links a fully received temporary file into place. Retries
// may send identical bytes, while a different body for the same index fails.
func SaveUploadChunk(dir string, u Upload, index int, body io.Reader) error {
	want := u.ChunkSize(index)
	if want == 0 {
		return ErrUploadInvalid
	}
	tmp, err := os.CreateTemp(dir, ".chunk-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := io.Copy(tmp, io.LimitReader(body, want+1))
	if err != nil || n != want {
		return ErrUploadInvalid
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	target := filepath.Join(dir, fmt.Sprintf("%02d.part", index))
	if err = os.Link(tmp.Name(), target); err == nil {
		return nil
	}
	previous, readErr := os.ReadFile(target)
	if readErr != nil || int64(len(previous)) != want {
		return ErrUploadInvalid
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	current, err := io.ReadAll(tmp)
	if err != nil || !bytes.Equal(previous, current) {
		return ErrUploadInvalid
	}
	return nil
}

// AssembleUpload returns a seekable temporary ZIP for the existing validation
// and reservation path. Missing chunks fail without queuing a build.
func AssembleUpload(dir string, u Upload) (*os.File, error) {
	return AssembleUploadAt(dir, u, "")
}

func AssembleUploadAt(dir string, u Upload, tempRoot string) (*os.File, error) {
	pattern := "polygate-assembled-*"
	if u.Template == "image-tar" {
		pattern = ".image-pending-*.tar"
	}
	out, err := os.CreateTemp(tempRoot, pattern)
	if err != nil {
		return nil, err
	}
	for i := 0; i < u.ChunkCount(); i++ {
		part, openErr := os.Open(filepath.Join(dir, fmt.Sprintf("%02d.part", i)))
		if openErr != nil {
			out.Close()
			os.Remove(out.Name())
			return nil, ErrUploadInvalid
		}
		n, copyErr := io.Copy(out, io.LimitReader(part, u.ChunkSize(i)+1))
		closeErr := part.Close()
		if copyErr != nil || closeErr != nil || n != u.ChunkSize(i) {
			out.Close()
			os.Remove(out.Name())
			return nil, ErrUploadInvalid
		}
	}
	if _, err = out.Seek(0, io.SeekStart); err != nil {
		out.Close()
		os.Remove(out.Name())
		return nil, err
	}
	return out, nil
}

// PruneUploads clears incomplete staging directories after one day. It only
// traverses UUID-named directories beneath the dedicated .uploads directory.
func PruneUploads(root string, now time.Time) (int, error) {
	if !filepath.IsAbs(root) {
		return 0, ErrUploadInvalid
	}
	parent := filepath.Join(root, ".uploads")
	info, err := os.Lstat(parent)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, ErrUploadInvalid
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if !archiveID.MatchString(entry.Name()) || !entry.IsDir() {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		current, err := os.Lstat(path)
		if err != nil {
			return removed, err
		}
		if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || now.Sub(current.ModTime()) < 24*time.Hour {
			continue
		}
		if err = os.RemoveAll(path); err != nil {
			return removed, err
		}
		removed++
	}
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		return removed, err
	}
	for _, entry := range rootEntries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), ".image-pending-") || !strings.HasSuffix(entry.Name(), ".tar") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return removed, err
		}
		if info.Mode().IsRegular() && now.Sub(info.ModTime()) >= 24*time.Hour {
			if err = os.Remove(path); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}
