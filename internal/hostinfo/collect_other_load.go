//go:build !darwin

package hostinfo

import (
	"context"
	"errors"
)

func CurrentLoad(context.Context) (Load, error) {
	return Load{}, errors.New("host load is only sampled on macOS")
}
