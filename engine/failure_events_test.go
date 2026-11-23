package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/url"
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
	faultDrift, _ := transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)
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
		{"route 404", &relayv2.WebSocketRouteError{BaseHint: hint}, FailureRelayRouteNotFound},
		{"relay rejected", &relayv2.RelayError{Code: v2.ErrorInvalidProof}, FailureRelayRejected},
		{"relay not found", &relayv2.RelayError{Code: v2.ErrorNotFound}, FailureRelayNotFound},
		{"relay retryable", &relayv2.RelayError{Code: v2.ErrorAdmission, RetryAfter: 2 * time.Second}, FailureRelayUnavailable},
		{"relay stopped", &relayv2.RelayError{Code: v2.ErrorStopped}, FailureRelayStopped},
		{"relay denied", &relayv2.RelayError{Code: v2.ErrorAccessDenied}, FailureRelayAccessDenied},
		{"auth denied", &relayset.AuthorizationError{State: relayset.AuthorizationDenied}, FailureRelayAccessDenied},
		{"auth pending", &relayset.AuthorizationError{State: relayset.AuthorizationPendingSetup}, FailureRelayUnavailable},
		{"boundary fault busy", transferfault.Wrap(faultBusy, errors.New("busy")), FailureOutputBusy},
		{"boundary catalog", transferfault.Wrap(faultCat, errors.New("cat")), FailureCatalogUnavailable},
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
