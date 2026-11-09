package buildagent

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// AcquireHostLock prevents two Agent processes from claiming the same fixed
// libvirt slots. The OS releases the lock after a crash; startup recovery then
// destroys any guests left by the previous process before accepting work.
func AcquireHostLock(workRoot string) (*os.File, error) {
	if !filepath.IsAbs(workRoot) {
		return nil, errors.New("build work root must be absolute")
	}
	if err := secureOwnerDirectory(workRoot); err != nil {
		return nil, err
	}
	path := filepath.Join(workRoot, "agent.lock")
	before, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil && !before.Mode().IsRegular() {
		return nil, errors.New("build host lock must be a regular file")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, errors.New("build host lock owner or mode is invalid")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) ||
		(before != nil && !os.SameFile(before, info)) {
		f.Close()
		return nil, errors.New("build host lock file changed or has a foreign owner")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another build agent owns the VM slots")
	}
	return f, nil
}
