package commandprojection

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/osfs"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
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
		if !ok || !failure.Valid() || failure.Code() == clievent.FailureUnexpected {
			t.Fatalf("relay code %d mapping = %+v,%t", code, failure, ok)
		}
	}
	if _, ok := ProjectRelayErrorCode(v2.ErrorStopped + 1); ok {
		t.Fatal("mapped unknown relay code")
	}

	peerCodes := []v2peer.TypedPeerErrorCode{
		v2peer.TypedPeerErrorNegotiation, v2peer.TypedPeerErrorTimeout,
		v2peer.TypedPeerErrorCandidates, v2peer.TypedPeerErrorAdmission,
		v2peer.TypedPeerErrorSignaling, v2peer.TypedPeerErrorCancelled,
		v2peer.TypedPeerErrorStopped, v2peer.TypedPeerErrorUnexpected,
	}
	for _, code := range peerCodes {
		failure, ok := ProjectPeerErrorCode(code)
		if !ok || !failure.Valid() {
			t.Fatalf("peer code %q mapping = %+v,%t", code, failure, ok)
		}
	}
	if _, ok := ProjectPeerErrorCode("future-peer-code"); ok {
		t.Fatal("mapped unknown peer error code")
	}

	receiverClasses := []v2peer.ReceiverCauseClass{
		v2peer.ReceiverCauseRuntimeClosed, v2peer.ReceiverCauseConfiguration,
		v2peer.ReceiverCauseOperationMissing, v2peer.ReceiverCauseAttemptTimeout,
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

func TestEveryTransportLifecycleCauseHasAnExplicitMappingOrOmission(t *testing.T) {
	relayCauses := []relayv2.LifecycleCause{
		relayv2.LifecycleCauseNone, relayv2.LifecycleCauseCanceled,
		relayv2.LifecycleCauseDeadline, relayv2.LifecycleCauseFrameBounds,
		relayv2.LifecycleCauseEgressOverflow, relayv2.LifecycleCauseIngressOverflow,
		relayv2.LifecycleCauseSessionRetired, relayv2.LifecycleCauseProtocol,
		relayv2.LifecycleCauseClosed, relayv2.LifecycleCauseTransport,
	}
	for _, cause := range relayCauses {
		failure, present := ProjectRelayLifecycleCause(cause)
		if cause == relayv2.LifecycleCauseNone {
			if present || failure.Valid() {
				t.Fatalf("none relay cause produced failure: %+v,%t", failure, present)
			}
			continue
		}
		if !present || !failure.Valid() {
			t.Fatalf("relay lifecycle cause %q mapping = %+v,%t", cause, failure, present)
		}
	}
	if _, present := ProjectRelayLifecycleCause("future-relay-cause"); present {
		t.Fatal("mapped unknown relay lifecycle cause")
	}

	webRTCCauses := []wsrtc.LifecycleCause{
		wsrtc.LifecycleCauseNone, wsrtc.LifecycleCauseCanceled,
		wsrtc.LifecycleCauseDeadline, wsrtc.LifecycleCauseNotOpen,
		wsrtc.LifecycleCauseNaturalRetirement, wsrtc.LifecycleCauseRemoteClosed,
		wsrtc.LifecycleCauseTerminalUnacknowledged, wsrtc.LifecycleCausePeerProtocol,
		wsrtc.LifecycleCauseTransport, wsrtc.LifecycleCauseOther,
	}
	for _, cause := range webRTCCauses {
		failure, present := ProjectWebRTCLifecycleCause(cause)
		if cause == wsrtc.LifecycleCauseNone {
			if present || failure.Valid() {
				t.Fatalf("none WebRTC cause produced failure: %+v,%t", failure, present)
			}
			continue
		}
		if !present || !failure.Valid() {
			t.Fatalf("WebRTC lifecycle cause %q mapping = %+v,%t", cause, failure, present)
		}
	}
	if _, present := ProjectWebRTCLifecycleCause("future-webrtc-cause"); present {
		t.Fatal("mapped unknown WebRTC lifecycle cause")
	}
}

func TestEveryTransferFaultCodePreservesClosedDomainScopeAndCode(t *testing.T) {
	type faultCase struct {
		name string
		new  func() (transferfault.Fault, error)
		code clievent.FailureCode
	}
	var tests []faultCase
	for code := transferfault.SourceUnavailable; code <= transferfault.SourcePermanent; code++ {
		code := code
		tests = append(tests, faultCase{fmt.Sprintf("source/%d", code), func() (transferfault.Fault, error) {
			return transferfault.NewSource(transferfault.ScopeFileLocal, code)
		}, clievent.FailureCode(uint16(clievent.FailureSourceUnavailable) + uint16(code) - 1)})
	}
	for code := transferfault.CatalogUnavailable; code <= transferfault.CatalogInvalidGeneration; code++ {
		code := code
		tests = append(tests, faultCase{fmt.Sprintf("catalog/%d", code), func() (transferfault.Fault, error) {
			return transferfault.NewCatalog(transferfault.ScopeDirectoryLocal, code)
		}, clievent.FailureCode(uint16(clievent.FailureCatalogUnavailable) + uint16(code) - 1)})
	}
	for code := transferfault.SessionTransport; code <= transferfault.SessionDependencyContract; code++ {
		code := code
		tests = append(tests, faultCase{fmt.Sprintf("session/%d", code), func() (transferfault.Fault, error) {
			return transferfault.NewSession(transferfault.ScopeSessionTerminal, code)
		}, clievent.FailureCode(uint16(clievent.FailureSessionTransport) + uint16(code) - 1)})
	}
	for code := transferfault.OutputStateIO; code <= transferfault.OutputContract; code++ {
		code := code
		tests = append(tests, faultCase{fmt.Sprintf("output/%d", code), func() (transferfault.Fault, error) {
			return transferfault.NewOutput(transferfault.ScopeOutputPause, code)
		}, clievent.FailureCode(uint16(clievent.FailureOutputStateIO) + uint16(code) - 1)})
	}
	for code := transferfault.CheckpointBusy; code <= transferfault.CheckpointStateIO; code++ {
		code := code
		tests = append(tests, faultCase{fmt.Sprintf("checkpoint/%d", code), func() (transferfault.Fault, error) {
			return transferfault.NewCheckpoint(transferfault.ScopeOutputPause, code)
		}, clievent.FailureCode(uint16(clievent.FailureCheckpointBusy) + uint16(code) - 1)})
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
			normalized, ok := ProjectNormalizedFault(uint8(value.Domain()), uint8(value.Scope()), value.Code())
			if !ok || normalized.Code() != failure.Code() {
				t.Fatalf("normalized mapping = %+v,%t want %+v", normalized, ok, failure)
			}
		})
	}
	if _, ok := ProjectFault(transferfault.Fault{}); ok {
		t.Fatal("mapped zero fault")
	}
	if _, ok := ProjectNormalizedFault(255, 1, 1); ok {
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
		code  clievent.FailureCode
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
			code  clievent.FailureCode
			stage osfs.FilesystemOutputFailureStage
		}{
			name: fmt.Sprintf("output/%d", code), fault: fault,
			code:  clievent.FailureCode(uint16(clievent.FailureOutputStateIO) + uint16(code) - 1),
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
			code  clievent.FailureCode
			stage osfs.FilesystemOutputFailureStage
		}{
			name: fmt.Sprintf("checkpoint/%d", code), fault: fault,
			code:  clievent.FailureCode(uint16(clievent.FailureCheckpointBusy) + uint16(code) - 1),
			stage: osfs.FilesystemOutputFailureCheckpointReconciliation,
		})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			diagnostic := osfs.FilesystemOutputDiagnostic{
				Stage: test.stage, FaultDomain: uint8(test.fault.Domain()),
				NormalizedScope: uint8(test.fault.Scope()), NormalizedCode: test.fault.Code(),
			}
			sealed, ok := SealFilesystemOutputFailure(diagnostic)
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
	sealed, ok := SealFilesystemOutputFailure(primaryDiagnostic)
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
	cleanup, ok := SealFilesystemOutputFailure(osfs.FilesystemOutputDiagnostic{
		Stage:       osfs.FilesystemOutputFailureAuthorityClose,
		FaultDomain: uint8(cleanupFault.Domain()), NormalizedScope: uint8(cleanupFault.Scope()),
		NormalizedCode: cleanupFault.Code(),
	})
	if !ok {
		t.Fatal("valid cleanup diagnostic was not sealed")
	}
	failure, present := ClassifyError(errors.Join(sealed, cleanup))
	if !present || failure.Code() != clievent.FailureOutputOwnership {
		t.Fatalf("joined filesystem failure = %+v,%t", failure, present)
	}

	for name, lookalike := range map[string]error{
		"diagnostic carrier": untrustedFilesystemDiagnosticCarrier{diagnostic: primaryDiagnostic},
		"hostile As":         hostileAsError{},
		"hostile Unwrap":     untrustedUnwrapper{secret: "hostile Unwrap", child: sealed},
	} {
		failure, present := ClassifyError(lookalike)
		if !present || failure.Code() != clievent.FailureUnexpected {
			t.Fatalf("%s classification = %+v,%t", name, failure, present)
		}
	}
}

func TestRawProviderTextTerminatesAtFailureProjection(t *testing.T) {
	const secret = "wss://relay.example/private?token=AUTH-TOKEN-CANARY"
	tests := []struct {
		name error
		want clievent.FailureCode
	}{
		{opaqueCanaryError{secret}, clievent.FailureUnexpected},
		{untrustedUnwrapper{secret: secret, child: relayv2.ErrProtocol}, clievent.FailureUnexpected},
		{fmt.Errorf("provider %s: %w", secret, relayv2.ErrProtocol), clievent.FailureRelayProtocol},
		{&url.Error{Op: "dial", URL: secret, Err: opaqueCanaryError{secret}}, clievent.FailureRelayTransport},
		{transferfault.Wrap(mustOutputFault(t), opaqueCanaryError{secret}), clievent.FailureOutputStateIO},
		{wsrtc.ErrPeerProtocol, clievent.FailurePeerProtocol},
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
	if !present || retry.Code() != clievent.FailureRelayStarting {
		t.Fatalf("retry classification = %+v,%t", retry, present)
	}
	if millis, ok := retry.RetryAfterMillis(); !ok || millis != 1500 {
		t.Fatalf("retry context = %d,%t", millis, ok)
	}
	joined, present := ClassifyError(errors.Join(opaqueCanaryError{secret}, relayv2.ErrClosed))
	if !present || joined.Code() != clievent.FailureRelayClosed {
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
