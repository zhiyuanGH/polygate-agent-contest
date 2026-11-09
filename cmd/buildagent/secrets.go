package main

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

func readOwnedSecret(envName string) []byte {
	data, err := readPrivateFile(os.Getenv(envName), 0)
	if err != nil {
		log.Fatalf("%s is not a safe root-owned private file: %v", envName, err)
	}
	return data
}

// readPrivateFile checks every ancestor and the opened inode. The ownerUID
// parameter is fixed to root by production; it permits meaningful temp-file
// tests under an unprivileged developer account.
func readPrivateFile(path string, ownerUID uint32) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("secret path must be absolute")
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || !ok || (stat.Uid != 0 && stat.Uid != ownerUID) ||
			info.Mode()&os.ModeSymlink != 0 ||
			(info.Mode().Perm()&0022 != 0 && !(stat.Uid == 0 && info.Mode()&os.ModeSticky != 0)) {
			return nil, errors.New("secret parent directory is replaceable")
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !before.Mode().IsRegular() || !ok || stat.Uid != ownerUID ||
		before.Mode().Perm()&0077 != 0 || before.Size() < 1 || before.Size() > 64<<10 {
		return nil, errors.New("secret file owner, mode or size is invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("secret file changed during validation")
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(data) < 1 || len(data) > 64<<10 {
		return nil, errors.New("secret file content is invalid")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		return nil, errors.New("secret file changed during validation")
	}
	return data, nil
}
