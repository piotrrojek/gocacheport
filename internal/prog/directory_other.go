//go:build !darwin && !linux

package prog

import "errors"

func privateParent(string) error {
	return errors.New("private staging directories require Linux or macOS")
}
