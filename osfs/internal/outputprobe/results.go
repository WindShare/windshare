package outputprobe

import (
	"errors"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// Results keeps independent capability failures separate from namespace
// contradictions, which must reject the whole destination.
type Results struct {
	SafePublish       error
	OperationRecovery error
	RangeRecovery     error
	CrashCleanup      error
}

func (results Results) Err() error {
	return errors.Join(results.SafePublish, results.OperationRecovery, results.RangeRecovery, results.CrashCleanup)
}

func (results Results) Capabilities() (outputcap.DestinationCapabilities, error) {
	facts := [...]struct {
		err    error
		reason outputcap.CapabilityReason
	}{
		{results.SafePublish, outputcap.CapabilityReasonUnsafePublication},
		{results.OperationRecovery, outputcap.CapabilityReasonUnverifiableOperationRecovery},
		{results.RangeRecovery, outputcap.CapabilityReasonUnverifiableRangeRecovery},
		{results.CrashCleanup, outputcap.CapabilityReasonUnverifiableCrashCleanup},
	}
	var evidence [len(facts)]outputcap.CapabilityEvidence
	for index, fact := range facts {
		switch {
		case fact.err == nil:
			evidence[index] = outputcap.SupportedCapability()
		case errors.Is(fact.err, outputcap.ErrUnsafeNamespace):
			return outputcap.DestinationCapabilities{}, fact.err
		default:
			evidence[index], _ = outputcap.UnsupportedCapability(fact.reason)
		}
	}
	return outputcap.NewDestinationCapabilities(evidence[0], evidence[1], evidence[2], evidence[3])
}
