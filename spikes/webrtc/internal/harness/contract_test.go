package harness

import (
	"testing"

	"github.com/windshare/windshare/core/framechannel"
)

func TestPublicConfigUsesWindShareContract(t *testing.T) {
	t.Parallel()

	config := publicConfig()
	if config.MaxFrameSize != framechannel.MaxFrameSize {
		t.Fatalf("MaxFrameSize = %d, want WindShare framechannel.MaxFrameSize %d", config.MaxFrameSize, framechannel.MaxFrameSize)
	}
	if config.MaxFrameSize != 64*1024 {
		t.Fatalf("MaxFrameSize = %d, want exact 64 KiB spike frame", config.MaxFrameSize)
	}
	parsed, err := parseHarnessSessionID(config.SessionID)
	if err != nil {
		t.Fatalf("parse configured session ID: %v", err)
	}
	if parsed != [harnessSessionIDBytes]byte{1, 2, 3, 4, 5, 6, 7, 8} {
		t.Fatalf("session ID = %v, want the fixed harness identity", parsed)
	}
	if config.ChannelLabel != ChannelLabel || config.ChannelProtocol != ChannelProtocol {
		t.Fatalf("channel contract = %q/%q, want %q/%q", config.ChannelLabel, config.ChannelProtocol, ChannelLabel, ChannelProtocol)
	}
	if config.LowWaterMark >= config.HighWaterMark {
		t.Fatalf("low water mark %d must be below high water mark %d", config.LowWaterMark, config.HighWaterMark)
	}
}

func TestPatternedFrameValidation(t *testing.T) {
	t.Parallel()

	const (
		marker = byte(0xa7)
		size   = 64 * 1024
	)
	frame := patternedFrame(marker, size)
	if !validPattern(frame, marker, size) {
		t.Fatal("generated frame did not validate")
	}

	tests := []struct {
		name  string
		frame []byte
		mark  byte
		size  int
	}{
		{name: "wrong marker", frame: frame, mark: marker + 1, size: size},
		{name: "wrong size", frame: frame, mark: marker, size: size - 1},
		{name: "empty", frame: nil, mark: marker, size: 0},
	}
	corrupt := append([]byte(nil), frame...)
	corrupt[len(corrupt)-1] ^= 0xff
	tests = append(tests, struct {
		name  string
		frame []byte
		mark  byte
		size  int
	}{name: "corrupt payload", frame: corrupt, mark: marker, size: size})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if validPattern(test.frame, test.mark, test.size) {
				t.Fatal("invalid frame unexpectedly validated")
			}
		})
	}
}
