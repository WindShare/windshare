package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/internal/networkruntime"
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/output"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func TestDescribeFailureCoversDomainCategoriesAndContext(t *testing.T) {
	if f, ok := DescribeFailure(nil); ok || f.Valid() {
		t.Fatalf("nil error classified as %+v", f)
	}
	faultCheckpoint, _ := transferfault.NewCheckpoint(transferfault.ScopeOutputPause, transferfault.CheckpointStateIO)
	sealedOutput, _ := output.SealFailure(errors.New("disk"), output.FailureDiagnostic{Stage: output.FailureCheckpoint, Fault: faultCheckpoint})
	sealedCloseNoFault, _ := output.SealFailure(errors.New("close handle failed"), output.FailureDiagnostic{Stage: output.FailureClose})
	sealedBusyNoFault, _ := output.SealFailure(errors.Join(output.ErrBusy, errors.New("lock held")), output.FailureDiagnostic{Stage: output.FailureOpen})
	faultDrift, _ := transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)
	faultCatalogStale, _ := transferfault.NewCatalog(transferfault.ScopeDirectoryLocal, transferfault.CatalogDirectoryStale)
	faultBusy, _ := transferfault.NewOutput(transferfault.ScopeFileLocal, transferfault.OutputFileAlreadyActive)
	faultCat, _ := transferfault.NewCatalog(transferfault.ScopeDirectoryLocal, transferfault.CatalogUnavailable)
	faultSess, _ := transferfault.NewSession(transferfault.ScopeSessionTerminal, transferfault.SessionTransport)
	faultCkBusy, _ := transferfault.NewCheckpoint(transferfault.ScopeOutputPause, transferfault.CheckpointBusy)
	hint, _ := networkendpoint.WebSocketBaseHint("wss://relay.example/ws")

	cases := []struct {
		name string
		err  error
		want FailureCode
	}{
		{"selection missing", &catalog.SelectionError{Label: "a.txt", Cause: &catalog.SourceError{Failure: catalog.SourceFailureMissing}}, FailureSourceMissing},
		{"source access", &catalog.SourceError{Failure: catalog.SourceFailureAccessDenied}, FailureSourceAccessDenied},
		{"source unsupported", &catalog.SourceError{Failure: catalog.SourceFailureUnsupported}, FailureSourceUnsupported},
		{"source cloud placeholder", &catalog.SelectionError{Label: "cloud.docx", Cause: &catalog.SourceError{Failure: catalog.SourceFailureCloudPlaceholder}}, FailureSourceCloudPlaceholder},
		{"source stale", &catalog.SourceError{Failure: catalog.SourceFailureStale}, FailureSourceChanged},
		{"source unavailable", &catalog.SourceError{Failure: catalog.SourceFailureUnavailable}, FailureSourceUnavailable},
		{"selection collision", &catalog.SelectionNameCollisionError{FirstLabel: "a", SecondLabel: "A"}, FailureSelectionCollision},
		{"receive fault", receive.Failure{Fault: faultDrift}, FailureSourceChanged},
		{"receive canceled", receive.Failure{Interruption: transfer.TransferInterruptionCanceled}, FailureCanceled},
		{"receive deadline", receive.Failure{Interruption: transfer.TransferInterruptionDeadline}, FailureDeadline},
		{"receive local missing", receive.Failure{Local: receive.LocalSelectionMissing}, FailureSelectionMissing},
		{"receive local omitted", receive.Failure{Local: receive.LocalSourceOmitted}, FailureSourceOmitted},
		{"receive local revision", receive.Failure{Local: receive.LocalRevisionConflict}, FailureCheckpointRevisionConflict},
		{"receive local checkpoint", receive.Failure{Local: receive.LocalCheckpointInvalid}, FailureCheckpointInvalid},
		{"receive local unknown", receive.Failure{Local: receive.LocalOwnedObjectUnknown}, FailureOwnedObjectUnknown},
		{"receive local collision", receive.Failure{Local: receive.LocalDestinationCollision}, FailureDestinationCollision},
		{"receive code", receive.Failure{Code: receive.FailureOutputOperationAlreadyRunning}, FailureOutputOperationAlreadyRunning},
		{"receive wrapped", receive.Failure{Cause: context.DeadlineExceeded}, FailureDeadline},
		{"sealed output", sealedOutput, FailureCheckpointFailed},
		{"sealed close without fault", sealedCloseNoFault, FailureOutputFailed},
		{"sealed busy without fault", sealedBusyNoFault, FailureOutputBusy},
		{"route 404", &relayv2.WebSocketRouteError{BaseHint: hint}, FailureRelayRouteNotFound},
		{"relay rejected", &relayv2.RelayError{Code: v2.ErrorInvalidProof}, FailureRelayRejected},
		{"relay not found", &relayv2.RelayError{Code: v2.ErrorNotFound}, FailureRelayNotFound},
		{"relay share not ready", &relayv2.RelayError{Code: v2.ErrorStarting, RetryAfter: 250 * time.Millisecond}, FailureShareNotReady},
		{"relay retryable", &relayv2.RelayError{Code: v2.ErrorAdmission, RetryAfter: 2 * time.Second}, FailureRelayUnavailable},
		{"relay stopped", &relayv2.RelayError{Code: v2.ErrorStopped}, FailureRelayStopped},
		{"relay denied", &relayv2.RelayError{Code: v2.ErrorAccessDenied}, FailureRelayAccessDenied},
		{"auth denied", &relayset.AuthorizationError{State: relayset.AuthorizationDenied}, FailureRelayAccessDenied},
		{"auth pending", &relayset.AuthorizationError{State: relayset.AuthorizationPendingSetup}, FailureRelayUnavailable},
		{"boundary fault busy", transferfault.Wrap(faultBusy, errors.New("busy")), FailureOutputBusy},
		{"boundary catalog", transferfault.Wrap(faultCat, errors.New("cat")), FailureCatalogUnavailable},
		{"boundary catalog stale", transferfault.Wrap(faultCatalogStale, errors.New("stale")), FailureSourceChanged},
		{"boundary session", transferfault.Wrap(faultSess, errors.New("sess")), FailureSessionFailed},
		{"boundary ck busy", transferfault.Wrap(faultCkBusy, errors.New("ck")), FailureOutputBusy},
		{"directory unavailable", &networkdirectory.UnavailableError{}, FailureNetworkUnavailable},
		{"ws dial", &relayv2.WebSocketDialError{}, FailureRelayUnavailable},
		{"socket", &relayv2.SocketError{}, FailureRelayUnavailable},
		{"canceled", context.Canceled, FailureCanceled},
		{"deadline", relayset.ErrReceiverWaitExpired, FailureDeadline},
		{"network runtime", networkruntime.ErrUnavailable, FailureNetworkUnavailable},
		{"receiver unavailable", relayset.ErrReceiverUnavailable, FailureRelayNotFound},
		{"share stopped", relayset.ErrReceiverShareStopped, FailureShareStopped},
		{"receiver auth", relayset.ErrReceiverAuthentication, FailureSessionFailed},
		{"relay protocol", relayv2.ErrProtocol, FailureRelayUnavailable},
		{"webrtc closed", wsrtc.ErrChannelClosed, FailureDirectUnavailable},
		{"output busy", output.ErrBusy, FailureOutputBusy},
		{"path error", &fs.PathError{Op: "open", Path: "x", Err: errors.New("io")}, FailureOutputFailed},
		{"url error", &url.Error{Op: "get", URL: "x", Err: errors.New("net")}, FailureRelayUnavailable},
		{"net op error", &net.OpError{Op: "dial", Err: errors.New("net")}, FailureRelayUnavailable},
		{"unexpected", errors.New("unknown"), FailureUnexpected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DescribeFailure(tc.err)
			if !ok || got.Code != tc.want {
				t.Fatalf("DescribeFailure() = (%+v, %v), want %q", got, ok, tc.want)
			}
			if !got.DiagnosticValue().Present() {
				t.Fatal("missing diagnostic value")
			}
		})
	}
}

func TestDescribeFailurePrecedenceAndFallthrough(t *testing.T) {
	hint, _ := networkendpoint.WebSocketBaseHint("wss://relay.example/ws")

	cases := []struct {
		name           string
		err            error
		wantCode       FailureCode
		wantRetryAfter time.Duration
		wantHint       networkendpoint.RelayBaseHint
	}{
		{
			name:     "receive failure beats wrapped source error",
			err:      receive.Failure{Code: receive.FailureOutputNeedsAttention, Cause: &catalog.SourceError{Failure: catalog.SourceFailureMissing}},
			wantCode: FailureOutputNeedsAttention,
		},
		{
			name:     "unknown source failure falls through to sibling domain error",
			err:      errors.Join(&catalog.SourceError{Failure: 0}, &catalog.SelectionNameCollisionError{FirstLabel: "a", SecondLabel: "A"}),
			wantCode: FailureSelectionCollision,
		},
		{
			name:     "unknown relay error code falls through to dial error",
			err:      errors.Join(&relayv2.RelayError{Code: v2.ErrorCode(255)}, &relayv2.WebSocketDialError{}),
			wantCode: FailureRelayUnavailable,
		},
		{
			name:     "unknown authorization state falls through to socket error",
			err:      errors.Join(&relayset.AuthorizationError{State: relayset.AuthorizationState(255)}, &relayv2.SocketError{}),
			wantCode: FailureRelayUnavailable,
		},
		{
			name:     "aggregate share stopped supersedes single relay stopped",
			err:      errors.Join(relayset.ErrReceiverShareStopped, &relayv2.RelayError{Code: v2.ErrorStopped}),
			wantCode: FailureShareStopped,
		},
		{
			name:     "explicit receiver wait expiry supersedes dial failure",
			err:      errors.Join(relayset.ErrReceiverWaitExpired, &relayv2.WebSocketDialError{}),
			wantCode: FailureDeadline,
		},
		{
			name:     "explicit receiver wait expiry supersedes relay not found",
			err:      errors.Join(relayset.ErrReceiverWaitExpired, &relayv2.RelayError{Code: v2.ErrorNotFound}),
			wantCode: FailureDeadline,
		},
		{
			name:     "initial join timeout preserves dial unavailability",
			err:      errors.Join(relayset.ErrReceiverUnavailable, &relayv2.WebSocketDialError{}),
			wantCode: FailureRelayUnavailable,
		},
		{
			name:           "initial join timeout preserves retry after",
			err:            errors.Join(relayset.ErrReceiverUnavailable, &relayv2.RelayError{Code: v2.ErrorAdmission, RetryAfter: 3 * time.Second}),
			wantCode:       FailureRelayUnavailable,
			wantRetryAfter: 3 * time.Second,
		},
		{
			name:     "initial join timeout preserves route hint",
			err:      errors.Join(relayset.ErrReceiverUnavailable, &relayv2.WebSocketRouteError{BaseHint: hint}),
			wantCode: FailureRelayRouteNotFound,
			wantHint: hint,
		},
		{
			name:     "initial join timeout maps relay not found",
			err:      errors.Join(relayset.ErrReceiverUnavailable, &relayv2.RelayError{Code: v2.ErrorNotFound}),
			wantCode: FailureRelayNotFound,
		},
		{
			name:           "initial join timeout maps share not ready with retry after",
			err:            errors.Join(relayset.ErrReceiverUnavailable, &relayv2.RelayError{Code: v2.ErrorStarting, RetryAfter: 250 * time.Millisecond}),
			wantCode:       FailureShareNotReady,
			wantRetryAfter: 250 * time.Millisecond,
		},
		{
			name:     "cancellation does not hide teardown transport failure",
			err:      errors.Join(context.Canceled, &relayv2.SocketError{}),
			wantCode: FailureRelayUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DescribeFailure(tc.err)
			if !ok || got.Code != tc.wantCode {
				t.Fatalf("DescribeFailure() = (%+v, %v), want code %q", got, ok, tc.wantCode)
			}
			if got.RetryAfter != tc.wantRetryAfter {
				t.Fatalf("RetryAfter = %v, want %v", got.RetryAfter, tc.wantRetryAfter)
			}
			if got.RelayBaseHint != tc.wantHint {
				t.Fatalf("RelayBaseHint = %+v, want %+v", got.RelayBaseHint, tc.wantHint)
			}
		})
	}
}

func TestWorkflowEventTranslationAndDiagnosticRecords(t *testing.T) {
	origin, err := NormalizeRelayOrigin("https://relay.example:8443/path?secret=1")
	if err != nil || origin.Scheme != "wss" || origin.Host != "relay.example" || origin.Port != 8443 || origin.HostPort() != "relay.example:8443" {
		t.Fatalf("origin = %+v, err=%v", origin, err)
	}
	endpoint, err := v2.NormalizeRelayEndpoint("https://relay.example:8443")
	if err != nil {
		t.Fatal(err)
	}

	var emitted []task.Event
	publish := func(obs task.Observation) bool {
		emitted = append(emitted, obs.Event)
		return true
	}
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	run := func(wf string, ev task.Event) {
		translateWorkflowEvent(wf, task.Observation{TaskID: "eng:1", At: now, Event: ev}, publish)
	}

	run("share", share.Observation{RelayAvailability: &relayset.SenderAvailability{Available: 1, Total: 2, EverReady: true}})
	run("share", share.Observation{RelayRecovery: &share.RelayRecovery{
		Endpoint: endpoint,
		Attempt:  relayset.Attempt{Number: 1, State: relayset.AttemptFailed, Failure: &relayv2.WebSocketRouteError{}, Authorization: serviceauthority.AdmissionTemporaryFailure, ShareInstance: [16]byte{1}},
	}})
	run("share", share.SourceOmissionsObserved{Summary: catalog.SourceOmissions{Count: 1, Samples: []catalog.SourceOmission{{Reason: catalog.SourceOmissionSymbolicLink}}}})
	run("receive", receive.RelayConnected{Endpoint: endpoint})
	run("receive", receive.RecoveryObserved{Value: relayset.ReceiverRecoveryObservation{
		Endpoint: "https://relay.example:8443", Attempt: 2, Phase: relayset.ReceiverRecoveryRetrying,
		Err: errors.New("down"), ProtocolSessionID: [16]byte{2},
	}})
	run("receive", receive.ContentPathObserved{Path: receive.ContentPathDirectAndRelay})
	run("receive", receive.FallbackObserved{Code: receive.FailurePeerNegotiation})
	run("receive", receive.LaneAdopted{Session: protocolsession.ProtocolSessionID{3}, Lane: session.LaneIdentity{ID: 1, Epoch: 2}})
	run("receive", receive.PeerTerminated{Value: v2peer.ReceiverTerminationTrace{}})
	run("receive", receive.ProgressObserved{
		Operation: receivecontract.OperationID{4}, Job: transfer.TransferJobID{5},
		Value: transfer.ReceiveProgressSnapshot{Discovery: transfer.DiscoveryComplete, CountersExact: true},
	})

	if len(emitted) != 9 {
		t.Fatalf("emitted=%d, want 9", len(emitted))
	}
	for _, ev := range emitted {
		source, ok := ev.(diagnostics.Source)
		if !ok || !source.DiagnosticRecord().Valid() {
			t.Fatalf("event %T produced invalid diagnostic record", ev)
		}
	}
}

func TestCloudPlaceholderFailurePreservesPathAndOmissionRecordRedactsNames(t *testing.T) {
	f, ok := DescribeFailure(&catalog.SelectionError{
		Label: `C:\OneDrive\cloud.docx`,
		Cause: &catalog.SourceError{Failure: catalog.SourceFailureCloudPlaceholder},
	})
	if !ok || f.Code != FailureSourceCloudPlaceholder || f.SourcePath != `C:\OneDrive\cloud.docx` {
		t.Fatalf("cloud placeholder selection failure = (%+v, %v)", f, ok)
	}

	event := SourceOmissions{Summary: catalog.SourceOmissions{
		DirectoryPath: `C:\Users\alice\OneDrive\Private`,
		Count:         2,
		Samples: []catalog.SourceOmission{
			{Name: "secret-cloud.docx", Reason: catalog.SourceOmissionCloudPlaceholder},
			{Name: "Report.txt", Reason: catalog.SourceOmissionNameCollision, Detail: `collides with sibling "report.txt"`},
		},
	}}
	record := event.DiagnosticRecord()
	if !record.Valid() || record.Name() != "source_omissions" {
		t.Fatalf("unexpected record: %+v", record)
	}
	payload := record.Payload()
	if payload.Get("count").Uint() != 2 {
		t.Fatalf("count = %v", payload.Get("count"))
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	body := string(encoded)
	for _, secret := range []string{"secret-cloud.docx", "Report.txt", "report.txt", "Private", "collides"} {
		if strings.Contains(body, secret) {
			t.Fatalf("default diagnostic record leaked %q: %s", secret, body)
		}
	}
	for _, want := range []string{`"cloud_placeholder"`, `"name_collision"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("diagnostic record missing reason %s: %s", want, body)
		}
	}
}

func TestRelayOriginHostPortFormatsIPv6WithBrackets(t *testing.T) {
	cases := []struct {
		raw          string
		wantScheme   string
		wantHost     string
		wantPort     uint16
		wantHostPort string
	}{
		{"https://relay.example:8443/path?secret=1", "wss", "relay.example", 8443, "relay.example:8443"},
		{"http://127.0.0.1:8080/ws", "ws", "127.0.0.1", 8080, "127.0.0.1:8080"},
		{"https://[::1]:8443/path?secret=1", "wss", "::1", 8443, "[::1]:8443"},
		{"ws://[2001:db8::1]", "ws", "2001:db8::1", 80, "[2001:db8::1]:80"},
	}
	for _, tc := range cases {
		origin, err := NormalizeRelayOrigin(tc.raw)
		if err != nil {
			t.Fatalf("NormalizeRelayOrigin(%q) error = %v", tc.raw, err)
		}
		if origin.Scheme != tc.wantScheme || origin.Host != tc.wantHost || origin.Port != tc.wantPort {
			t.Fatalf("NormalizeRelayOrigin(%q) = %+v, want scheme=%q host=%q port=%d", tc.raw, origin, tc.wantScheme, tc.wantHost, tc.wantPort)
		}
		if got := origin.HostPort(); got != tc.wantHostPort {
			t.Fatalf("NormalizeRelayOrigin(%q).HostPort() = %q, want %q", tc.raw, got, tc.wantHostPort)
		}
		host, port, err := net.SplitHostPort(origin.HostPort())
		if err != nil || host != tc.wantHost || port != strconv.FormatUint(uint64(tc.wantPort), 10) {
			t.Fatalf("SplitHostPort(%q) = (%q, %q, %v), want (%q, %d, nil)", origin.HostPort(), host, port, err, tc.wantHost, tc.wantPort)
		}
		if gotHost := origin.diagnosticValue().Get("host").Text(); gotHost != tc.wantHost {
			t.Fatalf("diagnosticValue().host = %q, want %q", gotHost, tc.wantHost)
		}
	}
}
