//go:build darwin || linux

package prog

import (
	"errors"
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
	if !ok || !info.IsDir() || info.Mode().Perm()&0o022 != 0 || identity.Uid != uint32(os.Getuid()) {
		return errors.New("local staging parent must be a private, owned directory, not a symlink")
	}
	return nil
}
