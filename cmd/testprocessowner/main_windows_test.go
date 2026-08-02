//go:build windows

package main

import (
	"io"
	"testing"
)

func TestWindowsDispatcherCoversCommonAndLegacyBoundaries(t *testing.T) {
	previous := ownerInput
	ownerInput = zeroReader{}
	t.Cleanup(func() { ownerInput = previous })
	if err := runPlatform([]string{commandSupervise}); err == nil {
		t.Fatal("malformed common supervise invocation was accepted")
	}
	if err := runPlatform([]string{"launcher"}); err == nil {
		t.Fatal("malformed legacy launcher invocation was accepted")
	}
}

type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, io.EOF }

var _ io.Reader = zeroReader{}
