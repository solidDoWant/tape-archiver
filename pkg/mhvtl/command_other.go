//go:build !linux

package mhvtl

import (
	"context"
	"errors"
)

func sendCommand(context.Context, int, string) (string, error) {
	return "", errors.New("mhvtl is only supported on linux")
}
