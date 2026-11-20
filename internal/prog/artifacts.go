package prog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type artifactCache struct {
	config    Config
	root      *os.Root
	lock      *os.File
	lease     *os.File
	directory string
	mu        sync.Mutex
}

func openArtifactCache(config Config) (*artifactCache, error) {
	parent := config.Directory
	if parent == "" {
		directory, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		parent = filepath.Join(directory, "gocacheport")
	}
	parent, err := filepath.Abs(parent)
	if err != nil {
		return nil, err
	}
	if err := privateParent(parent); err != nil {
		return nil, err
	}
	path := filepath.Join(parent, "artifacts")
	if err := privateParent(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm() != 0o700 {
		return nil, errors.New("artifact cache directory must have mode 0700")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	lock, err := openArtifactLock(root, ".lock", true)
	if err != nil {
		root.Close()
		return nil, err
	}
	cache := &artifactCache{config: config, root: root, lock: lock}
	err = cache.withLock(func() error {
		if err := cache.admit(0, 1); err != nil {
			return err
		}
		directory, err := os.MkdirTemp(path, "gocacheport-")
		if err != nil {
			return err
		}
		cache.directory = directory
		lease, err := openArtifactLock(root, filepath.Join(filepath.Base(directory), ".lease"), true)
		if err == nil {
			err = lockArtifactFile(lease, false)
		}
		if err != nil {
			if lease != nil {
				lease.Close()
			}
			os.RemoveAll(directory)
			return err
		}
		cache.lease = lease
		return nil
	})
	if err != nil {
		if cache.lease != nil {
			cache.lease.Close()
		}
		lock.Close()
		root.Close()
		return nil, err
	}
	return cache, nil
}

func (cache *artifactCache) withLock(operation func() error) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if err := lockArtifactFile(cache.lock, false); err != nil {
		return err
	}
	err := operation()
	return errors.Join(err, unlockArtifactFile(cache.lock))
}

func (cache *artifactCache) admit(size int64, entries int) error {
	directory, err := cache.root.Open(".")
	if err != nil {
		return err
	}
	sessions, err := directory.ReadDir(-1)
	directory.Close()
	if err != nil {
		return err
	}
	var usedBytes int64
	var usedEntries int
	for _, session := range sessions {
		if session.Name() == ".lock" {
			continue
		}
		if !session.IsDir() || !strings.HasPrefix(session.Name(), "gocacheport-") {
			return errors.New("unexpected artifact cache entry")
		}
		bytes, count, err := cache.sessionUsage(session.Name())
		if err != nil {
			return err
		}
		usedBytes += bytes
		usedEntries += count
	}
	if size > cache.config.LocalBytes-usedBytes || entries > cache.config.LocalEntries-usedEntries {
		return fmt.Errorf("local artifact cache is full (limit %d bytes, %d entries); increase limits or wait for inactive sessions to expire", cache.config.LocalBytes, cache.config.LocalEntries)
	}
	return nil
}

func (cache *artifactCache) sessionUsage(name string) (int64, int, error) {
	lease, err := openArtifactLock(cache.root, filepath.Join(name, ".lease"), false)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, cache.root.RemoveAll(name)
	}
	if err != nil {
		return 0, 0, err
	}
	defer lease.Close()
	err = lockArtifactFile(lease, true)
	if err != nil && !artifactLockBusy(err) {
		return 0, 0, err
	}
	if err == nil {
		info, err := lease.Stat()
		if err != nil {
			return 0, 0, err
		}
		if time.Since(info.ModTime()) >= cache.config.LocalRetention {
			return 0, 0, cache.root.RemoveAll(name)
		}
	}
	directory, err := cache.root.Open(name)
	if err != nil {
		return 0, 0, err
	}
	defer directory.Close()
	files, err := directory.ReadDir(-1)
	if err != nil {
		return 0, 0, err
	}
	var size int64
	entries := 1
	for _, file := range files {
		if file.Name() == ".lease" {
			continue
		}
		info, err := file.Info()
		if err != nil {
			return 0, 0, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return 0, 0, errors.New("artifact cache files must be regular files with mode 0600")
		}
		size += info.Size()
		entries++
	}
	return size, entries, nil
}

func (cache *artifactCache) touch() error {
	now := time.Now()
	return cache.root.Chtimes(filepath.Join(filepath.Base(cache.directory), ".lease"), now, now)
}

func (cache *artifactCache) close() error {
	err := cache.withLock(func() error {
		err := cache.touch()
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		return errors.Join(err, cache.lease.Close())
	})
	return errors.Join(err, cache.lock.Close(), cache.root.Close())
}
