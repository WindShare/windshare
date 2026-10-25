package eventproject

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/connectivity/v2peer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/osfs"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func TestEveryRelayAndPeerCodeHasAnExplicitSafeMapping(t *testing.T) {
	relayCodes := []v2.ErrorCode{
		v2.ErrorMalformed, v2.ErrorUnsupportedMode, v2.ErrorShareIDCollision,
		v2.ErrorAlreadyRegistered, v2.ErrorChallengeExpired, v2.ErrorInvalidProof,
		v2.ErrorDescriptorInvalid, v2.ErrorNotFound, v2.ErrorStarting,
		v2.ErrorAdmission, v2.ErrorStopped,
	}
	for _, code := range relayCodes {
		failure, ok := ProjectRelayErrorCode(code)
		if !ok || !failure.Valid() || failure.Code() == appobs.FailureUnexpected {
			t.Fatalf("relay code %d mapping = %+v,%t", code, failure, ok)
		}
	}
	if _, ok := ProjectRelayErrorCode(v2.ErrorStopped + 1); ok {
		t.Fatal("mapped unknown relay code")
	}

	receiverClasses := []v2peer.ReceiverCauseClass{
		v2peer.ReceiverCauseRuntimeClosed, v2peer.ReceiverCauseConfiguration,
		v2peer.ReceiverCauseOperationMissing, v2peer.ReceiverCauseNegotiationTimeout,
		v2peer.ReceiverCauseAdmissionTimeout,
		v2peer.ReceiverCauseCandidateLimit, v2peer.ReceiverCauseChannelAdmission,
		v2peer.ReceiverCauseEventCapacity, v2peer.ReceiverCauseNegotiation,
		v2peer.ReceiverCauseProtocol, v2peer.ReceiverCauseDeadline,
		v2peer.ReceiverCausePeerShutdown, v2peer.ReceiverCauseChannelDrain,
		v2peer.ReceiverCauseUnknown,
	}
	for _, class := range receiverClasses {
		failure, ok := ProjectReceiverCauseClass(class)
		if !ok || !failure.Valid() {
			t.Fatalf("receiver class %q mapping = %+v,%t", class, failure, ok)
		}
	}
	if _, ok := ProjectReceiverCauseClass("future-class"); ok {
		t.Fatal("mapped unknown receiver cause class")
	}
}

func TestEveryTransferFaultCodePreservesClosedDomainScopeAndCode(t *testing.T) {
	type faultCase struct {
		name string
		new  func() (transferfault.Fault, error)
		code appobs.FailureCode
	}
	var tests []faultCase
	for code := transferfault.SourceUnavailable; code <= transferfault.SourcePermanent; code++ {
		code := code
		tests = append(tests, faultCase{fmt.Sprintf("source/%d", code), func() (transferfault.Fault, error) {
			return transferfault.NewSource(transferfault.ScopeFileLocal, code)
		}, appobs.FailureCode(uint16(appobs.FailureSourceUnavailable) + uint16(code) - 1)})
	}
	for code := transferfault.CatalogUnavailable; code <= transferfault.CatalogInvalidGeneration; code++ {
		code := code
		tests = append(tests, faultCase{fmt.Sprintf("catalog/%d", code), func() (transferfault.Fault, error) {
			return transferfault.NewCatalog(transferfault.ScopeDirectoryLocal, code)
		}, appobs.FailureCode(uint16(appobs.FailureCatalogUnavailable) + uint16(code) - 1)})
	}
	for code := transferfault.SessionTransport; code <= transferfault.SessionDependencyContract; code++ {
		code := code
		tests = append(tests, faultCase{fmt.Sprintf("session/%d", code), func() (transferfault.Fault, error) {
			return transferfault.NewSession(transferfault.ScopeSessionTerminal, code)
		}, appobs.FailureCode(uint16(appobs.FailureSessionTransport) + uint16(code) - 1)})
	}
	for code := transferfault.OutputStateIO; code <= transferfault.OutputContract; code++ {
		code := code
		tests = append(tests, faultCase{fmt.Sprintf("output/%d", code), func() (transferfault.Fault, error) {
			return transferfault.NewOutput(transferfault.ScopeOutputPause, code)
		}, appobs.FailureCode(uint16(appobs.FailureOutputStateIO) + uint16(code) - 1)})
	}
	for code := transferfault.CheckpointBusy; code <= transferfault.CheckpointStateIO; code++ {
		code := code
		tests = append(tests, faultCase{fmt.Sprintf("checkpoint/%d", code), func() (transferfault.Fault, error) {
			return transferfault.NewCheckpoint(transferfault.ScopeOutputPause, code)
		}, appobs.FailureCode(uint16(appobs.FailureCheckpointBusy) + uint16(code) - 1)})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := test.new()
			if err != nil {
				t.Fatal(err)
			}
			failure, ok := ProjectFault(value)
			if !ok || failure.Code() != test.code || !failure.Valid() {
				t.Fatalf("fault mapping = code=%d want=%d ok=%t", failure.Code(), test.code, ok)
			}
			context, ok := failure.Fault()
			if !ok || context.Code() != value.Code() {
				t.Fatalf("fault context = %+v,%t want numeric %d", context, ok, value.Code())
			}
			normalized, ok := appobs.ProjectNormalizedFault(uint8(value.Domain()), uint8(value.Scope()), value.Code())
			if !ok || normalized.Code() != failure.Code() {
				t.Fatalf("normalized mapping = %+v,%t want %+v", normalized, ok, failure)
			}
		})
	}
	if _, ok := ProjectFault(transferfault.Fault{}); ok {
		t.Fatal("mapped zero fault")
	}
	if _, ok := appobs.ProjectNormalizedFault(255, 1, 1); ok {
		t.Fatal("mapped unknown normalized fault domain")
	}
}

type opaqueCanaryError struct{ secret string }

func (failure opaqueCanaryError) Error() string { return failure.secret }

type untrustedUnwrapper struct {
	secret string
	child  error
}

func (failure untrustedUnwrapper) Error() string { return failure.secret }
func (failure untrustedUnwrapper) Unwrap() error { return failure.child }

type untrustedFilesystemDiagnosticCarrier struct {
	diagnostic osfs.FilesystemOutputDiagnostic
}

func (failure untrustedFilesystemDiagnosticCarrier) Error() string {
	return "filesystem output failed"
}

func (failure untrustedFilesystemDiagnosticCarrier) FilesystemOutputDiagnostic() osfs.FilesystemOutputDiagnostic {
	return failure.diagnostic
}

type hostileAsError struct{}

func (hostileAsError) Error() string { return "hostile As error" }
func (hostileAsError) As(any) bool   { panic("ClassifyError invoked untrusted As") }

func TestSealedFilesystemFailuresPreserveClosedOutputAndCheckpointCodes(t *testing.T) {
	tests := make([]struct {
		name  string
		fault transferfault.Fault
		code  appobs.FailureCode
		stage osfs.FilesystemOutputFailureStage
	}, 0, int(transferfault.OutputContract)+int(transferfault.CheckpointStateIO))
	for code := transferfault.OutputStateIO; code <= transferfault.OutputContract; code++ {
		fault, err := transferfault.NewOutput(transferfault.ScopeOutputPause, code)
		if err != nil {
			t.Fatal(err)
		}
		tests = append(tests, struct {
			name  string
			fault transferfault.Fault
			code  appobs.FailureCode
			stage osfs.FilesystemOutputFailureStage
		}{
			name: fmt.Sprintf("output/%d", code), fault: fault,
			code:  appobs.FailureCode(uint16(appobs.FailureOutputStateIO) + uint16(code) - 1),
			stage: osfs.FilesystemOutputFailureOperationAdmission,
		})
	}
	for code := transferfault.CheckpointBusy; code <= transferfault.CheckpointStateIO; code++ {
		fault, err := transferfault.NewCheckpoint(transferfault.ScopeOutputPause, code)
		if err != nil {
			t.Fatal(err)
		}
		tests = append(tests, struct {
			name  string
			fault transferfault.Fault
			code  appobs.FailureCode
			stage osfs.FilesystemOutputFailureStage
		}{
			name: fmt.Sprintf("checkpoint/%d", code), fault: fault,
			code:  appobs.FailureCode(uint16(appobs.FailureCheckpointBusy) + uint16(code) - 1),
			stage: osfs.FilesystemOutputFailureCheckpointReconciliation,
		})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			diagnostic := osfs.FilesystemOutputDiagnostic{
				Stage: test.stage, FaultDomain: uint8(test.fault.Domain()),
				NormalizedScope: uint8(test.fault.Scope()), NormalizedCode: test.fault.Code(),
			}
			sealed, ok := filesystem.SealFilesystemOutputFailure(diagnostic)
			if !ok {
				t.Fatal("valid filesystem diagnostic was not sealed")
			}
			failure, present := ClassifyError(sealed)
			if !present || !failure.Valid() || failure.Code() != test.code {
				t.Fatalf("sealed diagnostic = %+v,%t want code %d", failure, present, test.code)
			}
		})
	}
}

func TestFilesystemFailureAuthorityIsExactAndPrimary(t *testing.T) {
	primaryFault, err := transferfault.NewOutput(
		transferfault.ScopeOutputPause,
		transferfault.OutputOwnership,
	)
	if err != nil {
		t.Fatal(err)
	}
	primaryDiagnostic := osfs.FilesystemOutputDiagnostic{
		Stage:       osfs.FilesystemOutputFailureOperationAcquisition,
		FaultDomain: uint8(primaryFault.Domain()), NormalizedScope: uint8(primaryFault.Scope()),
		NormalizedCode: primaryFault.Code(),
	}
	sealed, ok := filesystem.SealFilesystemOutputFailure(primaryDiagnostic)
	if !ok {
		t.Fatal("valid primary diagnostic was not sealed")
	}
	cleanupFault, err := transferfault.NewCheckpoint(
		transferfault.ScopeOutputPause,
		transferfault.CheckpointStateIO,
	)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, ok := filesystem.SealFilesystemOutputFailure(osfs.FilesystemOutputDiagnostic{
		Stage:       osfs.FilesystemOutputFailureAuthorityClose,
		FaultDomain: uint8(cleanupFault.Domain()), NormalizedScope: uint8(cleanupFault.Scope()),
		NormalizedCode: cleanupFault.Code(),
	})
	if !ok {
		t.Fatal("valid cleanup diagnostic was not sealed")
	}
	failure, present := ClassifyError(errors.Join(sealed, cleanup))
	if !present || failure.Code() != appobs.FailureOutputOwnership {
		t.Fatalf("joined filesystem failure = %+v,%t", failure, present)
	}

	for name, lookalike := range map[string]error{
		"diagnostic carrier": untrustedFilesystemDiagnosticCarrier{diagnostic: primaryDiagnostic},
		"hostile As":         hostileAsError{},
		"hostile Unwrap":     untrustedUnwrapper{secret: "hostile Unwrap", child: sealed},
	} {
		failure, present := ClassifyError(lookalike)
		if !present || failure.Code() != appobs.FailureUnexpected {
			t.Fatalf("%s classification = %+v,%t", name, failure, present)
		}
	}
}

func TestRawProviderTextTerminatesAtFailureProjection(t *testing.T) {
	const secret = "wss://relay.example/private?token=AUTH-TOKEN-CANARY"
	tests := []struct {
		name error
		want appobs.FailureCode
	}{
		{opaqueCanaryError{secret}, appobs.FailureUnexpected},
		{untrustedUnwrapper{secret: secret, child: relayv2.ErrProtocol}, appobs.FailureUnexpected},
		{fmt.Errorf("provider %s: %w", secret, relayv2.ErrProtocol), appobs.FailureRelayProtocol},
		{&url.Error{Op: "dial", URL: secret, Err: opaqueCanaryError{secret}}, appobs.FailureRelayTransport},
		{transferfault.Wrap(mustOutputFault(t), opaqueCanaryError{secret}), appobs.FailureOutputStateIO},
		{wsrtc.ErrPeerProtocol, appobs.FailurePeerProtocol},
	}
	for _, test := range tests {
		failure, present := ClassifyError(test.name)
		if !present || failure.Code() != test.want || !failure.Valid() {
			t.Fatalf("ClassifyError(%T) = %+v,%t want code %d", test.name, failure, present, test.want)
		}
		projected := fmt.Sprintf("%#v", failure)
		name, _ := failure.Code().Name()
		key, _ := failure.MessageKey()
		keyName, _ := key.Name()
		projected += name + keyName
		if strings.Contains(projected, secret) || strings.Contains(projected, "AUTH-TOKEN-CANARY") {
			t.Fatalf("safe failure retained provider text: %s", projected)
		}
	}
	retry, present := ClassifyError(&relayv2.RelayError{Code: v2.ErrorStarting, RetryAfter: 1500 * time.Millisecond})
	if !present || retry.Code() != appobs.FailureRelayStarting {
		t.Fatalf("retry classification = %+v,%t", retry, present)
	}
	if millis, ok := retry.RetryAfterMillis(); !ok || millis != 1500 {
		t.Fatalf("retry context = %d,%t", millis, ok)
	}
	joined, present := ClassifyError(errors.Join(opaqueCanaryError{secret}, relayv2.ErrClosed))
	if !present || joined.Code() != appobs.FailureRelayClosed {
		t.Fatalf("trusted join mapping = %+v,%t", joined, present)
	}
}

func mustOutputFault(t *testing.T) transferfault.Fault {
	t.Helper()
	value, err := transferfault.NewOutput(transferfault.ScopeOutputPause, transferfault.OutputStateIO)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
