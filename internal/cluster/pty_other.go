//go:build !darwin

package cluster

import (
	"errors"
	"os"
)

func openPTY() (*os.File, *os.File, error) {
	return nil, nil, errors.New("interactive terminals are only available on macOS")
}

func setWindowSize(*os.File, uint16, uint16) {}
