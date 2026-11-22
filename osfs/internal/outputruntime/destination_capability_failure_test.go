package outputruntime

import (
	"context"
	"errors"
	"testing"

	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestUnsupportedDestinationCapabilitiesRejectBeforeCreatingState(t *testing.T) {
	supported := outputcap.SupportedCapability()
	unsafePublish, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnsafePublication)
	unverifiableRecovery, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnverifiableOperationRecovery)

	for name, testCase := range map[string]struct {
		safePublish       outputcap.CapabilityEvidence
		operationRecovery outputcap.CapabilityEvidence
		wantErr           error
	}{
		"unsafe publication":    {safePublish: unsafePublish, operationRecovery: supported, wantErr: outputcap.ErrOrdinaryOutputUnsupported},
		"unverifiable recovery": {safePublish: supported, operationRecovery: unverifiableRecovery, wantErr: outputcap.ErrRecoverableOutputUnsupported},
	} {
		t.Run(name, func(t *testing.T) {
			caps, err := outputcap.NewDestinationCapabilities(testCase.safePublish, testCase.operationRecovery)
			if err != nil {
				t.Fatal(err)
			}
			platform := &unsupportedCapabilitiesPlatform{capabilities: caps}
			authority := &Authority{
				rootPath:        t.TempDir(),
				stage:           authorityStageConstructed,
				platformFactory: func(string, bool) (outputcap.Platform, error) { return platform, nil },
			}
			err = authority.BindDestination(context.Background())
			if !errors.Is(err, testCase.wantErr) || !platform.closed {
				t.Fatalf("unsupported admission = %v, closed=%t", err, platform.closed)
			}
			assertOutputRuntimeFault(t, err, transferfault.OutputUnsupportedFilesystem)
			if authority.destination != nil || authority.registry != nil || authority.stage != authorityStageConstructed {
				t.Fatal("unsupported destination acquired output or checkpoint authority")
			}
		})
	}
}

type unsupportedCapabilitiesPlatform struct {
	// Every namespace method is intentionally absent: unsupported capability
	// admission must finish before acquiring a guard or creating task state.
	outputcap.Platform
	capabilities outputcap.DestinationCapabilities
	closed       bool
}

func (platform *unsupportedCapabilitiesPlatform) DestinationCapabilities() (outputcap.DestinationCapabilities, error) {
	return platform.capabilities, nil
}
func (platform *unsupportedCapabilitiesPlatform) Close() error {
	platform.closed = true
	return nil
}
