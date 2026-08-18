package runtrace

import (
	"encoding/hex"
	"io"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func newRunID(random io.Reader) (string, error) {
	raw := make([]byte, clievent.IdentityBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", err
	}
	allZero := true
	for _, value := range raw {
		allZero = allZero && value == 0
	}
	if allZero {
		return "", ErrRunIDUnavailable
	}
	return hex.EncodeToString(raw), nil
}
