//go:build !darwin && !linux

package store

import (
	"errors"
	"os"
)

func lockDirectory(_ *os.Root) (*os.File, error) {
	return nil, errors.New("cache server requires Linux or macOS filesystem locking")
}

func openRegular(root *os.Root, name string) (*os.File, error)   { return root.Open(name) }
func openDirectory(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
