//go:build darwin || linux

package prog

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func privateParent(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	identity, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("local staging parent has unsupported ownership metadata: %q", path)
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 || identity.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("local staging parent must be a private, owned directory, not a symlink: %q (mode %s, owner %d, process uid %d)", path, info.Mode(), identity.Uid, os.Getuid())
	}
	return nil
}

func openArtifactLock(root *os.Root, name string, create bool) (*os.File, error) {
	flags := os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if create {
		flags |= os.O_CREATE
	}
	file, err := root.OpenFile(name, flags, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		file.Close()
		return nil, errors.New("artifact cache lock must be a private regular file")
	}
	return file, nil
}

func lockArtifactFile(file *os.File, nonblocking bool) error {
	flags := syscall.LOCK_EX
	if nonblocking {
		flags |= syscall.LOCK_NB
	}
	return syscall.Flock(int(file.Fd()), flags)
}

func unlockArtifactFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func artifactLockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK)
}
