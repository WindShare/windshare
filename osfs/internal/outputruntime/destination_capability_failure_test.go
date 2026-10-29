package outputruntime

import (
	"context"
	"errors"
	"testing"

	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestUnsupportedPublicationRejectsDestinationBeforeCreatingState(t *testing.T) {
	platform := &unsupportedPublicationPlatform{}
	authority := &Authority{
		rootPath:        t.TempDir(),
		stage:           authorityStageConstructed,
		platformFactory: func(string, bool) (outputcap.Platform, error) { return platform, nil },
	}
	mode, err := authority.BindDestination(context.Background())
	if mode.Valid() || !errors.Is(err, outputcap.ErrOrdinaryOutputUnsupported) || !platform.closed {
		t.Fatalf("unsupported admission = %v, %v, closed=%t", mode, err, platform.closed)
	}
	assertOutputRuntimeFault(t, err, transferfault.OutputUnsupportedFilesystem)
	if authority.destination != nil || authority.registry != nil || authority.stage != authorityStageConstructed {
		t.Fatal("unsupported publication acquired output or checkpoint authority")
	}
}

type unsupportedPublicationPlatform struct {
	// Every namespace method is intentionally absent: unsupported capability
	// admission must finish before acquiring a guard or creating task state.
	outputcap.Platform
	closed bool
}

func (*unsupportedPublicationPlatform) DestinationCapabilities() (outputcap.DestinationCapabilities, error) {
	unsupported, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnsafePublication)
	return outputcap.NewDestinationCapabilities(unsupported, unsupported, unsupported, unsupported)
}
func (platform *unsupportedPublicationPlatform) Close() error {
	platform.closed = true
	return nil
}
