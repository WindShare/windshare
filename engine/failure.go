package engine

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/engine/internal/networkruntime"
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/output"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

// FailureCode is the closed domain error vocabulary exposed by Engine to native
// hosts and CLI presenters without leaking raw provider error strings.
type FailureCode string

const (
	FailureUnexpected                    FailureCode = "unexpected"
	FailureCanceled                      FailureCode = "canceled"
	FailureDeadline                      FailureCode = "deadline"
	FailureInvalidInput                  FailureCode = "invalid_input"
	FailureNetworkUnavailable            FailureCode = "network_unavailable"
	FailureCapabilityInvalid             FailureCode = "capability_invalid"
	FailureSelectionMissing              FailureCode = "selection_missing"
	FailureSelectionCollision            FailureCode = "selection_collision"
	FailurePublication                   FailureCode = "capability_publication"
	FailureAccountStorageError           FailureCode = "account_storage_error"
	FailureRelayRejected                 FailureCode = "relay_rejected"
	FailureRelayUnavailable              FailureCode = "relay_unavailable"
	FailureRelayNotFound                 FailureCode = "relay_not_found"
	FailureRelayStopped                  FailureCode = "relay_stopped"
	FailureRelayAccessDenied             FailureCode = "relay_access_denied"
	FailureRelayRouteNotFound            FailureCode = "relay_route_not_found"
	FailureShareNotReady                 FailureCode = "share_not_ready"
	FailureShareStopped                  FailureCode = "share_stopped"
	FailureDirectUnavailable             FailureCode = "direct_unavailable"
	FailureDirectAdmission               FailureCode = "direct_admission"
	FailureDirectPolicy                  FailureCode = "direct_policy"
	FailureDirectBusy                    FailureCode = "direct_busy"
	FailureSourceUnavailable             FailureCode = "source_unavailable"
	FailureSourceChanged                 FailureCode = "source_changed"
	FailureSourceMissing                 FailureCode = "source_missing"
	FailureSourceAccessDenied            FailureCode = "source_access_denied"
	FailureSourceUnsupported             FailureCode = "source_unsupported"
	FailureSourceCloudPlaceholder        FailureCode = "source_cloud_placeholder"
	FailureSourceOmitted                 FailureCode = "source_omitted"
	FailureCatalogUnavailable            FailureCode = "catalog_unavailable"
	FailureSessionFailed                 FailureCode = "session_failed"
	FailureOutputFailed                  FailureCode = "output_failed"
	FailureOutputBusy                    FailureCode = "output_busy"
	FailureOutputOperationAlreadyRunning FailureCode = "output_operation_already_running"
	FailureOutputNeedsAttention          FailureCode = "output_needs_attention"
	FailureRetainedOutputMissing         FailureCode = "retained_output_missing"
	FailureDestinationCollision          FailureCode = "destination_collision"
	FailureCheckpointFailed              FailureCode = "checkpoint_failed"
	FailureCheckpointRevisionConflict    FailureCode = "checkpoint_revision_conflict"
	FailureCheckpointInvalid             FailureCode = "checkpoint_invalid"
	FailureOwnedObjectUnknown            FailureCode = "owned_object_unknown"
)

type SelectionCollision struct {
	FirstPath  string `json:"first_path"`
	SecondPath string `json:"second_path"`
}

// Failure is the privacy-safe domain failure classification derived from an
// engine error. It never retains raw provider error text or secret material.
type Failure struct {
	Code               FailureCode
	SourcePath         string
	RelayBaseHint      networkendpoint.RelayBaseHint
	SelectionCollision *SelectionCollision
	Fault              transferfault.Fault
	RetryAfter         time.Duration
}

func (f Failure) Valid() bool { return f.Code != "" }

func (f Failure) DiagnosticValue() diagnostics.Value {
	fields := diagnostics.Fields{
		"code": diagnostics.Text(string(f.Code)),
	}
	if f.Fault.Valid() {
		fields["fault"] = diagnostics.Object(diagnostics.Fields{
			"domain": diagnostics.Text(f.Fault.Domain().String()),
			"scope":  diagnostics.Text(f.Fault.Scope().String()),
			"code":   diagnostics.Number(float64(f.Fault.Code())),
		})
	}
	if f.RetryAfter > 0 {
		fields["retry_after_ms"] = diagnostics.Text(strconv.FormatInt(f.RetryAfter.Milliseconds(), 10))
	}
	return diagnostics.Object(fields)
}

// DescribeFailure classifies an engine or adapter error into a stable,
// privacy-safe Failure value. Returns (zero, false) only when cause is nil.
func DescribeFailure(cause error) (Failure, bool) {
	if cause == nil {
		return Failure{}, false
	}
	if f, ok := classifyError(cause); ok {
		return f, true
	}
	return Failure{Code: FailureUnexpected}, true
}

func classifyError(cause error) (Failure, bool) {
	if cause == nil {
		return Failure{}, false
	}
	if f, ok := classifyDomainError(cause); ok {
		return f, true
	}
	if f, ok := classifyRecoveryOutcome(cause); ok {
		return f, true
	}
	if f, ok := classifyTransportError(cause); ok {
		return f, true
	}
	if f, ok := classifySentinelError(cause); ok {
		return f, true
	}
	if _, ok := output.FindDiagnostic(cause); ok {
		return Failure{Code: FailureOutputFailed}, true
	}
	return classifyStdlibError(cause)
}

func classifyDomainError(cause error) (Failure, bool) {
	// Typed receive settlement failures own precedence over wrapped causes.
	if reason, ok := errors.AsType[receive.Failure](cause); ok {
		if f, known := classifyReceiveFailure(reason); known {
			return f, true
		}
		if f, known := classifyError(reason.Cause); known {
			return f, true
		}
		return Failure{Code: FailureUnexpected}, true
	}
	// Preserve source-selection context before generic I/O classification.
	if selected, ok := errors.AsType[*catalog.SelectionError](cause); ok && selected != nil {
		if inner, known := classifyError(selected.Cause); known {
			if isSourcePathFailureCode(inner.Code) && selected.Label != "" {
				inner.SourcePath = selected.Label
			}
			return inner, true
		}
	}
	if source, ok := errors.AsType[*catalog.SourceError](cause); ok && source != nil {
		if f, known := classifySourceFailure(source.Failure); known {
			return f, true
		}
	}
	if diagnostic, ok := output.FindDiagnostic(cause); ok && diagnostic.Fault.Valid() {
		if f, known := FailureFromFault(diagnostic.Fault); known {
			return f, true
		}
	}
	if collision, ok := errors.AsType[*catalog.SelectionNameCollisionError](cause); ok && collision != nil {
		return Failure{
			Code: FailureSelectionCollision,
			SelectionCollision: &SelectionCollision{
				FirstPath:  collision.FirstLabel,
				SecondPath: collision.SecondLabel,
			},
		}, true
	}
	if boundary, ok := errors.AsType[*transferfault.BoundaryError](cause); ok && boundary != nil {
		return FailureFromFault(boundary.Fault())
	}
	return Failure{}, false
}

// classifyRecoveryOutcome promotes aggregate receiver-recovery decisions that
// supersede the individual per-route transport failures joined underneath them.
// ErrReceiverUnavailable intentionally stays below transport errors so initial
// join outages keep specific relay diagnostics (unavailability, RetryAfter,
// route hints) instead of collapsing every network failure into not-found.
func classifyRecoveryOutcome(cause error) (Failure, bool) {
	switch {
	case errors.Is(cause, relayset.ErrReceiverShareStopped):
		return Failure{Code: FailureShareStopped}, true
	case errors.Is(cause, relayset.ErrReceiverWaitExpired):
		return Failure{Code: FailureDeadline}, true
	default:
		return Failure{}, false
	}
}

func classifyTransportError(cause error) (Failure, bool) {
	if route, ok := errors.AsType[*relayv2.WebSocketRouteError](cause); ok && route != nil {
		return Failure{Code: FailureRelayRouteNotFound, RelayBaseHint: route.BaseHint}, true
	}
	if relayErr, ok := errors.AsType[*relayv2.RelayError](cause); ok && relayErr != nil {
		if f, known := classifyRelayError(relayErr); known {
			return f, true
		}
	}
	if authErr, ok := errors.AsType[*relayset.AuthorizationError](cause); ok && authErr != nil {
		if f, known := classifyAuthorizationState(authErr.State); known {
			return f, true
		}
	}
	if blocked, ok := errors.AsType[*networkdirectory.UnavailableError](cause); ok && blocked != nil {
		return Failure{Code: FailureNetworkUnavailable}, true
	}
	if _, ok := errors.AsType[*relayv2.WebSocketDialError](cause); ok {
		return Failure{Code: FailureRelayUnavailable}, true
	}
	if _, ok := errors.AsType[*relayv2.SocketError](cause); ok {
		return Failure{Code: FailureRelayUnavailable}, true
	}
	return Failure{}, false
}

func classifyStdlibError(cause error) (Failure, bool) {
	if _, ok := errors.AsType[*fs.PathError](cause); ok {
		return Failure{Code: FailureOutputFailed}, true
	}
	return classifyNetworkStdlibError(cause)
}

func classifyNetworkStdlibError(cause error) (Failure, bool) {
	if _, ok := errors.AsType[*url.Error](cause); ok {
		return Failure{Code: FailureRelayUnavailable}, true
	}
	if _, ok := errors.AsType[*net.OpError](cause); ok {
		return Failure{Code: FailureRelayUnavailable}, true
	}
	return Failure{}, false
}

func classifySentinelError(cause error) (Failure, bool) {
	switch {
	case errors.Is(cause, context.Canceled), errors.Is(cause, task.ErrCancelled),
		errors.Is(cause, task.ErrApplicationClosed), errors.Is(cause, task.ErrShareStopped):
		return Failure{Code: FailureCanceled}, true
	case errors.Is(cause, context.DeadlineExceeded):
		return Failure{Code: FailureDeadline}, true
	case errors.Is(cause, share.ErrInvalidChunkSize), errors.Is(cause, share.ErrConfiguration),
		errors.Is(cause, relayprofile.ErrCandidatesRequired):
		return Failure{Code: FailureInvalidInput}, true
	}
	if f, ok := classifyNetworkSentinelError(cause); ok {
		return f, true
	}
	switch {
	case errors.Is(cause, output.ErrBusy):
		return Failure{Code: FailureOutputBusy}, true
	case errors.Is(cause, output.ErrShareStopped):
		return Failure{Code: FailureShareStopped}, true
	default:
		return Failure{}, false
	}
}

func classifyNetworkSentinelError(cause error) (Failure, bool) {
	switch {
	case errors.Is(cause, networkruntime.ErrUnavailable):
		return Failure{Code: FailureNetworkUnavailable}, true
	case errors.Is(cause, relayset.ErrReceiverUnavailable):
		return Failure{Code: FailureRelayNotFound}, true
	case errors.Is(cause, relayset.ErrReceiverShareChanged), errors.Is(cause, relayset.ErrReceiverAuthentication):
		return Failure{Code: FailureSessionFailed}, true
	case errors.Is(cause, relayv2.ErrFrameBounds), errors.Is(cause, relayv2.ErrProtocol),
		errors.Is(cause, relayv2.ErrIngressOverflow), errors.Is(cause, relayv2.ErrEgressOverflow),
		errors.Is(cause, relayv2.ErrClosed), errors.Is(cause, relayv2.ErrSessionRetired):
		return Failure{Code: FailureRelayUnavailable}, true
	case errors.Is(cause, wsrtc.ErrNilDataChannel), errors.Is(cause, wsrtc.ErrInvalidDataChannel),
		errors.Is(cause, wsrtc.ErrInvalidFlowControl), errors.Is(cause, wsrtc.ErrChannelNotOpen),
		errors.Is(cause, wsrtc.ErrChannelClosed), errors.Is(cause, wsrtc.ErrRemoteClosed),
		errors.Is(cause, wsrtc.ErrFrameBounds), errors.Is(cause, wsrtc.ErrPeerProtocol),
		errors.Is(cause, wsrtc.ErrTerminalNotAcknowledged), errors.Is(cause, wsrtc.ErrTransport):
		return Failure{Code: FailureDirectUnavailable}, true
	default:
		return Failure{}, false
	}
}

func isSourcePathFailureCode(code FailureCode) bool {
	switch code {
	case FailureSourceMissing, FailureSourceAccessDenied, FailureSourceUnsupported,
		FailureSourceCloudPlaceholder, FailureSourceUnavailable, FailureSourceChanged:
		return true
	default:
		return false
	}
}

func classifySourceFailure(reason catalog.SourceFailure) (Failure, bool) {
	switch reason {
	case catalog.SourceFailureMissing:
		return Failure{Code: FailureSourceMissing}, true
	case catalog.SourceFailureAccessDenied:
		return Failure{Code: FailureSourceAccessDenied}, true
	case catalog.SourceFailureUnsupported:
		return Failure{Code: FailureSourceUnsupported}, true
	case catalog.SourceFailureCloudPlaceholder:
		return Failure{Code: FailureSourceCloudPlaceholder}, true
	case catalog.SourceFailureStale:
		return Failure{Code: FailureSourceChanged}, true
	case catalog.SourceFailureUnavailable:
		return Failure{Code: FailureSourceUnavailable}, true
	default:
		return Failure{}, false
	}
}

func classifyRelayError(relayErr *relayv2.RelayError) (Failure, bool) {
	var code FailureCode
	switch relayErr.Code {
	case v2.ErrorMalformed, v2.ErrorUnsupportedMode, v2.ErrorShareIDCollision,
		v2.ErrorAlreadyRegistered, v2.ErrorChallengeExpired, v2.ErrorInvalidProof,
		v2.ErrorDescriptorInvalid, v2.ErrorResumeStale:
		code = FailureRelayRejected
	case v2.ErrorNotFound:
		code = FailureRelayNotFound
	case v2.ErrorStarting:
		// The relay is reachable, but the share is activating or its sender is
		// in reconnect grace; keep relay-outage diagnostics separate.
		code = FailureShareNotReady
	case v2.ErrorAdmission, v2.ErrorAccessUnavailable:
		code = FailureRelayUnavailable
	case v2.ErrorStopped:
		code = FailureRelayStopped
	case v2.ErrorAccessDenied:
		code = FailureRelayAccessDenied
	default:
		return Failure{}, false
	}
	f := Failure{Code: code}
	if (relayErr.Code == v2.ErrorStarting || relayErr.Code == v2.ErrorAdmission ||
		relayErr.Code == v2.ErrorAccessUnavailable) && relayErr.RetryAfter > 0 {
		f.RetryAfter = relayErr.RetryAfter
	}
	return f, true
}

func classifyAuthorizationState(state relayset.AuthorizationState) (Failure, bool) {
	switch state {
	case relayset.AuthorizationDenied, relayset.AuthorizationUserActionRequired:
		return Failure{Code: FailureRelayAccessDenied}, true
	case relayset.AuthorizationPendingSetup, relayset.AuthorizationPendingAttribution,
		relayset.AuthorizationExpired, relayset.AuthorizationTemporaryFailure:
		return Failure{Code: FailureRelayUnavailable}, true
	default:
		return Failure{}, false
	}
}

func classifyReceiveFailure(value receive.Failure) (Failure, bool) {
	if value.Fault.Valid() {
		if value.ReplacementCause != nil && receive.IsTerminalSessionTransportFault(value.Fault) {
			if f, known := classifyReplacementCause(value.ReplacementCause, value.Fault); known {
				return f, true
			}
		}
		return FailureFromFault(value.Fault)
	}
	switch value.Interruption {
	case transfer.TransferInterruptionCanceled:
		return Failure{Code: FailureCanceled}, true
	case transfer.TransferInterruptionDeadline:
		return Failure{Code: FailureDeadline}, true
	}
	switch value.Local {
	case receive.LocalSourceOmitted:
		return Failure{Code: FailureSourceOmitted}, true
	case receive.LocalSelectionMissing:
		return Failure{Code: FailureSelectionMissing}, true
	case receive.LocalRevisionConflict:
		return Failure{Code: FailureCheckpointRevisionConflict}, true
	case receive.LocalCheckpointInvalid:
		return Failure{Code: FailureCheckpointInvalid}, true
	case receive.LocalOwnedObjectUnknown:
		return Failure{Code: FailureOwnedObjectUnknown}, true
	case receive.LocalDestinationCollision:
		return Failure{Code: FailureDestinationCollision}, true
	}
	if code, ok := classifyReceiveCode(value.Code); ok {
		return Failure{Code: code}, true
	}
	return Failure{}, false
}

// classifyReplacementCause projects the authoritative cause of a failed session
// replacement while preserving the transfer's terminal SessionTransport fault.
// Raw context cancellation/deadline sentinels and local filesystem errors are
// intentionally excluded: caller interruption settles as TransferInterruption,
// whereas child dial timeouts or local wrappers must not disguise a session
// replacement failure as caller cancellation.
func classifyReplacementCause(cause error, transportFault transferfault.Fault) (Failure, bool) {
	if cause == nil {
		return Failure{}, false
	}
	if reason, ok := errors.AsType[receive.Failure](cause); ok {
		if code, known := classifyReceiveCode(reason.Code); known {
			return Failure{Code: code, Fault: transportFault}, true
		}
		if reason.Cause != nil {
			cause = reason.Cause
		}
	}
	if f, ok := classifyRecoveryOutcome(cause); ok {
		return withFallbackFault(f, transportFault), true
	}
	if f, ok := classifyTransportError(cause); ok {
		return withFallbackFault(f, transportFault), true
	}
	if boundary, ok := errors.AsType[*transferfault.BoundaryError](cause); ok && boundary != nil &&
		boundary.Fault().Valid() && !receive.IsTerminalSessionTransportFault(boundary.Fault()) {
		if f, known := FailureFromFault(boundary.Fault()); known {
			return f, true
		}
	}
	if f, ok := classifyNetworkSentinelError(cause); ok {
		return withFallbackFault(f, transportFault), true
	}
	if f, ok := classifyNetworkStdlibError(cause); ok {
		return withFallbackFault(f, transportFault), true
	}
	return Failure{}, false
}

func withFallbackFault(failure Failure, fallback transferfault.Fault) Failure {
	if !failure.Fault.Valid() {
		failure.Fault = fallback
	}
	return failure
}

func classifyReceiveCode(code receive.FailureCode) (FailureCode, bool) {
	switch code {
	case receive.FailureInvalidInput:
		return FailureInvalidInput, true
	case receive.FailureOutputContract, receive.FailureOutputOwnership:
		return FailureOutputFailed, true
	case receive.FailureOutputOperationAlreadyRunning:
		return FailureOutputOperationAlreadyRunning, true
	case receive.FailureOutputNeedsAttention:
		return FailureOutputNeedsAttention, true
	case receive.FailureRetainedOutputMissing:
		return FailureRetainedOutputMissing, true
	case receive.FailurePeerConfiguration, receive.FailurePeerNegotiation,
		receive.FailurePeerProtocol, receive.FailurePeerSignaling, receive.FailurePeerStopped:
		return FailureDirectUnavailable, true
	case receive.FailureShareStopped:
		return FailureShareStopped, true
	default:
		return "", false
	}
}

// FailureFromFault converts a normalized core transfer fault into an engine Failure.
func FailureFromFault(fault transferfault.Fault) (Failure, bool) {
	if !fault.Valid() {
		return Failure{}, false
	}
	if fault.SourceChanged() {
		return Failure{Code: FailureSourceChanged, Fault: fault}, true
	}
	var code FailureCode
	switch fault.Domain() {
	case transferfault.DomainSource:
		code = FailureSourceUnavailable
	case transferfault.DomainCatalog:
		code = FailureCatalogUnavailable
	case transferfault.DomainSession:
		code = FailureSessionFailed
	case transferfault.DomainOutput:
		oc, _ := fault.OutputCode()
		if oc == transferfault.OutputFileAlreadyActive {
			code = FailureOutputBusy
		} else {
			code = FailureOutputFailed
		}
	case transferfault.DomainCheckpoint:
		kc, _ := fault.CheckpointCode()
		if kc == transferfault.CheckpointBusy {
			code = FailureOutputBusy
		} else {
			code = FailureCheckpointFailed
		}
	default:
		return Failure{}, false
	}
	return Failure{Code: code, Fault: fault}, true
}
