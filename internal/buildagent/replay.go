package buildagent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var ErrGrantReplayed = errors.New("build grant was already used")

// ReplayLog is a persistent, append-only record of task attempts accepted by
// this host. It is deliberately small; contest task IDs are never reused.
type ReplayLog struct {
	mu   sync.Mutex
	path string
	used map[string]struct{}
}

func NewReplayLog(root string) (*ReplayLog, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("replay root must be absolute")
	}
	if err := secureOwnerDirectory(root); err != nil {
		return nil, err
	}
	path := filepath.Join(root, "grant-ledger.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 || stat.Size() > 16<<20 {
		return nil, errors.New("invalid grant ledger file")
	}
	ledger := &ReplayLog{path: path, used: make(map[string]struct{})}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key := scanner.Text()
		if !validReplayKey(key) {
			return nil, errors.New("invalid grant ledger record")
		}
		ledger.used[key] = struct{}{}
	}
	if err = scanner.Err(); err != nil {
		return nil, err
	}
	directory, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	if err = directory.Sync(); err != nil {
		directory.Close()
		return nil, err
	}
	if err = directory.Close(); err != nil {
		return nil, err
	}
	return ledger, nil
}

func replayKey(g Grant) string { return fmt.Sprintf("%s:%d", g.TaskID, g.Attempt) }
func validReplayKey(key string) bool {
	parts := strings.Split(key, ":")
	if len(parts) != 2 || !taskIDPattern.MatchString(parts[0]) {
		return false
	}
	attempt, err := strconv.Atoi(parts[1])
	return err == nil && attempt >= 1 && attempt <= 3 && strconv.Itoa(attempt) == parts[1]
}

// Claim must finish and fsync before the guest is started. A failed append
// fails closed: the caller does not execute the build.
func (l *ReplayLog) Claim(g Grant) error {
	if l == nil || !g.validFields() {
		return ErrInvalidGrant
	}
	key := replayKey(g)
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.used[key]; exists {
		return ErrGrantReplayed
	}
	file, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, err = file.WriteString(key + "\n")
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	l.used[key] = struct{}{}
	return nil
}
