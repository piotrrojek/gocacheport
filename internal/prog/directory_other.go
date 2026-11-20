//go:build !darwin && !linux

package prog

import (
	"errors"
	"os"
)

func privateParent(string) error {
	return errors.New("private staging directories require Linux or macOS")
}

func openArtifactLock(*os.Root, string, bool) (*os.File, error) {
	return nil, errors.New("artifact cache requires Linux or macOS filesystem locking")
}

func lockArtifactFile(*os.File, bool) error {
	return errors.New("artifact cache requires Linux or macOS filesystem locking")
}

func unlockArtifactFile(*os.File) error {
	return errors.New("artifact cache requires Linux or macOS filesystem locking")
}

func artifactLockBusy(error) bool { return false }
