package outputprobe

import (
	"errors"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"testing"
)

func TestCapabilityEvidenceRemainsOrthogonal(t *testing.T) {
	const failureText = "fact unavailable"
	tests := []struct {
		name   string
		set    func(*Results, error)
		fact   func(outputcap.DestinationCapabilities) outputcap.CapabilityEvidence
		reason outputcap.CapabilityReason
	}{
		{
			name: "safe publish",
			set:  func(results *Results, err error) { results.SafePublish = err },
			fact: func(capabilities outputcap.DestinationCapabilities) outputcap.CapabilityEvidence {
				return capabilities.SafePublish()
			},
			reason: outputcap.CapabilityReasonUnsafePublication,
		},
		{
			name: "operation recovery",
			set:  func(results *Results, err error) { results.OperationRecovery = err },
			fact: func(capabilities outputcap.DestinationCapabilities) outputcap.CapabilityEvidence {
				return capabilities.OperationRecovery()
			},
			reason: outputcap.CapabilityReasonUnverifiableOperationRecovery,
		},
		{
			name: "range recovery",
			set:  func(results *Results, err error) { results.RangeRecovery = err },
			fact: func(capabilities outputcap.DestinationCapabilities) outputcap.CapabilityEvidence {
				return capabilities.RangeRecovery()
			},
			reason: outputcap.CapabilityReasonUnverifiableRangeRecovery,
		},
		{
			name: "crash cleanup",
			set:  func(results *Results, err error) { results.CrashCleanup = err },
			fact: func(capabilities outputcap.DestinationCapabilities) outputcap.CapabilityEvidence {
				return capabilities.CrashCleanup()
			},
			reason: outputcap.CapabilityReasonUnverifiableCrashCleanup,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var results Results
			test.set(&results, errors.Join(outputcap.ErrRecoverableOutputUnsupported, errors.New(failureText)))
			capabilities, err := results.Capabilities()
			if err != nil {
				t.Fatal(err)
			}
			evidence := []outputcap.CapabilityEvidence{
				capabilities.SafePublish(), capabilities.OperationRecovery(),
				capabilities.RangeRecovery(), capabilities.CrashCleanup(),
			}
			unsupported := 0
			for _, current := range evidence {
				if !current.Supported() {
					unsupported++
				}
			}
			got := test.fact(capabilities)
			if unsupported != 1 || got.Supported() || got.Reason() != test.reason {
				t.Fatalf("unsupported=%d fact=%+v, want one unsupported reason %v",
					unsupported, got, test.reason)
			}
		})
	}

	results := Results{
		SafePublish: unsafe("probe safe publish", errors.New("contradictory namespace")),
	}
	if _, err := results.Capabilities(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unsafe proof contradiction was reduced to a capability fact: %v", err)
	}
}

func TestTransientProbeFailureDoesNotBecomeUnsupported(t *testing.T) {
	failure := errors.New("temporary I/O failure")
	for _, result := range []Results{{SafePublish: failure}, {OperationRecovery: failure}, {RangeRecovery: failure}, {CrashCleanup: failure}} {
		capabilities, err := result.Capabilities()
		if !errors.Is(err, failure) || capabilities.Valid() {
			t.Fatalf("transient probe = %+v, %v", capabilities, err)
		}
	}
}
