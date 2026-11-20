package eventproject

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/connectivity/v2peer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	networkruntime "github.com/windshare/windshare/engine/internal/networkruntime"
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/networkdirectory"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/osfs"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func TestStopsKeepTheirScopeInProjectedFailures(t *testing.T) {
	stopped, ok := ProjectRelayErrorCode(v2.ErrorStopped)
	if !ok {
		t.Fatal("stopped relay code is unmapped")
	}
	key, ok := stopped.MessageKey()
	if !ok || key != appobs.MessageRelayStopped || stopped.Code() != appobs.FailureRelayStopped {
		t.Fatalf("relay stop = %+v with message key %v", stopped, key)
	}
	midTransfer, ok := projectReceiveFailure(receive.Failure{Code: receive.FailureShareStopped})
	if key, _ := midTransfer.MessageKey(); !ok || midTransfer.Code() != appobs.FailureShareStopped || key != appobs.MessageShareStopped {
		t.Fatalf("share stop = %+v with message key %v", midTransfer, key)
	}
	for _, code := range []v2.ErrorCode{v2.ErrorStarting, v2.ErrorAdmission} {
		unavailable, ok := ProjectRelayErrorCode(code)
		if !ok {
			t.Fatalf("relay code %d is unmapped", code)
		}
		if other, _ := unavailable.MessageKey(); other != appobs.MessageRelayUnavailable {
			t.Fatalf("relay code %d message key = %v; want %v", code, other, appobs.MessageRelayUnavailable)
		}
	}
}

func TestEveryRelayAndPeerCodeHasAnExplicitSafeMapping(t *testing.T) {
	relayCodes := []v2.ErrorCode{
		v2.ErrorMalformed, v2.ErrorUnsupportedMode, v2.ErrorShareIDCollision,
		v2.ErrorAlreadyRegistered, v2.ErrorChallengeExpired, v2.ErrorInvalidProof,
		v2.ErrorDescriptorInvalid, v2.ErrorNotFound, v2.ErrorStarting,
		v2.ErrorAdmission, v2.ErrorStopped, v2.ErrorResumeStale,
		v2.ErrorAccessDenied, v2.ErrorAccessUnavailable,
	}
	for _, code := range relayCodes {
		failure, ok := ProjectRelayErrorCode(code)
		if !ok || !failure.Valid() || failure.Code() == appobs.FailureUnexpected {
			t.Fatalf("relay code %d mapping = %+v,%t", code, failure, ok)
		}
	}
	if _, ok := ProjectRelayErrorCode(v2.ErrorAccessUnavailable + 1); ok {
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
		{networkruntime.ErrUnavailable, appobs.FailureNetworkUnavailable},
		{&networkdirectory.UnavailableError{Reason: networkdirectory.Unprovisioned, Cause: opaqueCanaryError{secret}}, appobs.FailureNetworkUnavailable},
		{fmt.Errorf("start share: %w", &networkdirectory.UnavailableError{Reason: networkdirectory.TrustStateInvalid, Cause: hostileAsError{}}), appobs.FailureNetworkUnavailable},
		{errors.Join(networkruntime.ErrUnavailable, opaqueCanaryError{secret}), appobs.FailureNetworkUnavailable},
		{fmt.Errorf("start share: %w", errors.Join(networkruntime.ErrUnavailable, opaqueCanaryError{secret})), appobs.FailureNetworkUnavailable},
		{relayprofile.ErrCandidatesRequired, appobs.FailureInvalidInput},
		{&relayv2.RelayError{Code: v2.ErrorAccessDenied}, appobs.FailureRelayAccessDenied},
		{&relayv2.RelayError{Code: v2.ErrorAccessUnavailable}, appobs.FailureRelayAccessUnavailable},
		{&relayv2.RelayError{Code: v2.ErrorResumeStale}, appobs.FailureRelayResumeStale},
		{&relayset.RegistrationFailure{URL: secret, Cause: &relayv2.RelayError{Code: v2.ErrorAccessDenied}}, appobs.FailureRelayAccessDenied},
		{&senderrelay.AuthorizationError{State: senderrelay.AuthorizationDenied, Cause: opaqueCanaryError{secret}}, appobs.FailureRelayAccessDenied},
		{&senderrelay.AuthorizationError{State: senderrelay.AuthorizationDenied, Cause: hostileAsError{}}, appobs.FailureRelayAccessDenied},
		{&relayset.RegistrationFailure{URL: secret, Cause: &senderrelay.AuthorizationError{State: senderrelay.AuthorizationDenied, Cause: opaqueCanaryError{secret}}}, appobs.FailureRelayAccessDenied},
		{&senderrelay.AuthorizationError{State: senderrelay.AuthorizationUserActionRequired, Cause: opaqueCanaryError{secret}}, appobs.FailureRelayAccessDenied},
		{&senderrelay.AuthorizationError{State: senderrelay.AuthorizationTemporaryFailure, Cause: opaqueCanaryError{secret}}, appobs.FailureRelayAccessUnavailable},
		{&senderrelay.AuthorizationError{State: 0, Cause: opaqueCanaryError{secret}}, appobs.FailureUnexpected},
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
	for _, test := range []struct {
		code v2.ErrorCode
		want appobs.FailureCode
	}{
		{v2.ErrorStarting, appobs.FailureRelayStarting},
		{v2.ErrorAccessUnavailable, appobs.FailureRelayAccessUnavailable},
	} {
		retry, present := ClassifyError(&relayv2.RelayError{Code: test.code, RetryAfter: 1500 * time.Millisecond})
		if !present || retry.Code() != test.want {
			t.Fatalf("retry classification for %d = %+v,%t", test.code, retry, present)
		}
		if millis, ok := retry.RetryAfterMillis(); !ok || millis != 1500 {
			t.Fatalf("retry context for %d = %d,%t", test.code, millis, ok)
		}
	}
	denied, _ := ClassifyError(&relayv2.RelayError{Code: v2.ErrorAccessDenied})
	if key, ok := denied.MessageKey(); !ok || key != appobs.MessageRelayAccessDenied {
		t.Fatalf("access denied message key = %v,%t; want %v", key, ok, appobs.MessageRelayAccessDenied)
	}
	joined, present := ClassifyError(errors.Join(opaqueCanaryError{secret}, relayv2.ErrClosed))
	if !present || joined.Code() != appobs.FailureRelayClosed {
		t.Fatalf("trusted join mapping = %+v,%t", joined, present)
	}
}

// A sender that cannot reach its relay is reporting the same condition a
// receiver reports for the same address, so both must reach the same safe code
// instead of the sender falling back to "unexpected error".
func TestSenderRegistrationNotSentKeepsTheClassOfItsCause(t *testing.T) {
	const secret = "wss://relay.example/private?token=AUTH-TOKEN-CANARY"
	tests := []struct {
		name  string
		cause error
		want  appobs.FailureCode
	}{
		{"transport", fmt.Errorf("failed to WebSocket dial: %w", &url.Error{Op: "Get", URL: secret, Err: opaqueCanaryError{secret}}), appobs.FailureRelayTransport},
		{"protocol", relayv2.ErrProtocol, appobs.FailureRelayProtocol},
		{"canceled", context.Canceled, appobs.FailureCanceled},
		{"deadline", context.DeadlineExceeded, appobs.FailureDeadline},
		{"invalid registration endpoint", errors.Join(relayv2.ErrProtocol, v2.ErrIdentity), appobs.FailureRelayProtocol},
		{"unclassified", opaqueCanaryError{secret}, appobs.FailureUnexpected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wrapped := &relayv2.RegistrationNotSentError{Cause: test.cause}
			for _, cause := range []error{wrapped, fmt.Errorf("register: %w", wrapped)} {
				failure, present := ClassifyError(cause)
				if !present || !failure.Valid() || failure.Code() != test.want {
					t.Fatalf("ClassifyError(%v) = %+v,%t; want code %d", cause, failure, present, test.want)
				}
				if strings.Contains(fmt.Sprintf("%#v", failure), "AUTH-TOKEN-CANARY") {
					t.Fatalf("safe failure retained provider text: %#v", failure)
				}
			}
		})
	}
}

func TestSenderWebSocketDialFailureIsRelayTransport(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
	}{
		{"refused", syscall.ECONNREFUSED},
		{"dial deadline", context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Inject at the TCP boundary so HTTP and WebSocket supply their real error wrappers.
			transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, test.cause
			}}
			options := websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}}
			_, _, err := websocket.Dial(t.Context(), "ws://relay.example"+v2.WebSocketPath, &options) //nolint:bodyclose
			if !errors.Is(err, test.cause) {
				t.Fatalf("WebSocket dial = %v; want cause %v", err, test.cause)
			}
			failure, present := ClassifyError(&relayv2.RegistrationNotSentError{Cause: err})
			if !present || failure.Code() != appobs.FailureRelayTransport {
				t.Fatalf("ClassifyError(%v) = %+v,%t; want relay transport", err, failure, present)
			}
		})
	}
}

func TestHTTPUpgradeRejectionIsRelayTransport(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "nope", status)
			}))
			t.Cleanup(server.Close)
			_, _, err := websocket.Dial(t.Context(), "ws"+server.URL[4:]+v2.WebSocketPath, nil) //nolint:bodyclose
			if err == nil {
				t.Fatal("expected upgrade rejection")
			}
			wrapped := &relayv2.RegistrationNotSentError{Cause: &relayv2.WebSocketDialError{Cause: err}}
			failure, present := ClassifyError(fmt.Errorf("register: %w", wrapped))
			if !present || failure.Code() != appobs.FailureRelayTransport {
				t.Fatalf("ClassifyError(%v) = %+v,%t; want relay transport", wrapped, failure, present)
			}
			if strings.Contains(fmt.Sprintf("%#v", failure), "nope") || strings.Contains(fmt.Sprintf("%#v", failure), "101") {
				t.Fatalf("safe failure retained provider text: %#v", failure)
			}
		})
	}
}

func TestHandshakeCloseIsRelayClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		_ = connection.Close(websocket.StatusNormalClosure, "bye")
	}))
	t.Cleanup(server.Close)
	socket, _, err := websocket.Dial(t.Context(), "ws"+server.URL[4:]+v2.WebSocketPath, nil) //nolint:bodyclose
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = socket.Close(websocket.StatusNormalClosure, "") })
	_, _, readErr := socket.Read(t.Context())
	if readErr == nil {
		t.Fatal("expected close after handshake")
	}
	for _, cause := range []error{&relayv2.SocketError{Cause: readErr}, fmt.Errorf("register: %w", &relayv2.SocketError{Cause: readErr})} {
		failure, present := ClassifyError(cause)
		if !present || failure.Code() != appobs.FailureRelayClosed {
			t.Fatalf("ClassifyError(%v) = %+v,%t; want relay closed", cause, failure, present)
		}
		if strings.Contains(fmt.Sprintf("%#v", failure), "bye") {
			t.Fatalf("safe failure retained close reason: %#v", failure)
		}
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
