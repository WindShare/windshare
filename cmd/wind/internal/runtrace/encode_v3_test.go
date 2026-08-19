package runtrace

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

const (
	displayNameCanary = "secret-file-name-token.txt"
	displayPathCanary = `C:\private\catalog\secret-file-name-token.txt`
)

func TestEncodeV3VisitsEveryEventWithSealedPayloads(t *testing.T) {
	events := allTraceEvents(t)
	if got, want := len(events), 26; got != want {
		t.Fatalf("event fixture count = %d, want %d", got, want)
	}
	expectedEvents := []string{
		"ready",
		"sharing_subject_selected",
		"relay_connected",
		"relay_recovering",
		"content_path_selected",
		"fallback",
		"transfer_progress",
		"warning",
		"command_failed",
		"transfer_settled",
		"sharing_stopped",
		"trace_incomplete",
		"lane_adopted",
		"relay_lifecycle",
		"webrtc_lifecycle",
		"peer_attempt",
		"transfer_lifecycle",
		"filesystem_output",
		"sender_terminal_send_observed",
		"sender_session_terminated",
		"catalog_storage",
		"root_prefetch",
		"protocol_operation",
		"lane_settlement",
		"observer_loss",
		"receiver_termination",
	}
	eventNames := make(map[string]struct{}, len(events))
	payloadTypes := make(map[reflect.Type]string, len(events))
	for index, event := range events {
		record, err := encodeV3(
			testRunIdentity(0x10),
			entryMetadata{
				sequence:  uint64(index + 1),
				time:      time.Date(2026, 8, 16, 5, 6, 7, index, time.FixedZone("private-zone", 9*60*60)),
				elapsedMS: int64(index),
			},
			event,
		)
		if err != nil {
			t.Fatalf("encode event %T: %v", event, err)
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("marshal event %T: %v", event, err)
		}
		if record.SchemaVersion != SchemaVersion || record.RuntimeRunID == "" ||
			!strings.HasSuffix(record.Time, "Z") || record.Payload == nil {
			t.Fatalf("required envelope missing for %T: %+v", event, record)
		}
		if record.Event != expectedEvents[index] {
			t.Fatalf("event %T discriminator = %q, want %q", event, record.Event, expectedEvents[index])
		}
		if _, duplicate := eventNames[record.Event]; duplicate {
			t.Fatalf("duplicate event discriminator %q", record.Event)
		}
		eventNames[record.Event] = struct{}{}
		payloadType := reflect.TypeOf(record.Payload)
		if previous, duplicate := payloadTypes[payloadType]; duplicate &&
			payloadType != reflect.TypeFor[emptyPayloadV3]() {
			t.Fatalf("events %q and %q share payload type %v", previous, record.Event, payloadType)
		}
		payloadTypes[payloadType] = record.Event
		assertEnvelopeAllowlist(t, encoded, record.Correlation != nil)
		assertNoPrivacyCanaries(t, encoded)
	}
}

func TestEncodeV3PreservesRevisionIdentityDecisionsInTypedPayloads(t *testing.T) {
	events := allTraceEvents(t)
	transfer, err := encodeV3(
		testRunIdentity(0x12),
		entryMetadata{sequence: 1, time: time.Unix(1, 0)},
		events[16],
	)
	if err != nil {
		t.Fatal(err)
	}
	transferPayload, ok := transfer.Payload.(transferLifecyclePayloadV3)
	if !ok || transferPayload.ItemBlockReason == nil ||
		*transferPayload.ItemBlockReason != "revision_conflict" {
		t.Fatalf("transfer item block payload = %#v", transfer.Payload)
	}

	filesystem, err := encodeV3(
		testRunIdentity(0x13),
		entryMetadata{sequence: 2, time: time.Unix(2, 0)},
		events[17],
	)
	if err != nil {
		t.Fatal(err)
	}
	filesystemPayload, ok := filesystem.Payload.(filesystemOutputPayloadV3)
	if !ok || filesystemPayload.CheckpointDecision == nil ||
		*filesystemPayload.CheckpointDecision != "revision_conflict" {
		t.Fatalf("filesystem checkpoint payload = %#v", filesystem.Payload)
	}
}

func TestV3PayloadSchemasAreClosedAndUseSafeNumericRepresentations(t *testing.T) {
	var roots []reflect.Type
	for _, event := range allTraceEvents(t) {
		record, err := encodeV3(
			testRunIdentity(0x11),
			entryMetadata{sequence: 1, time: time.Unix(1, 0)},
			event,
		)
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, reflect.TypeOf(record.Payload))
	}
	roots = append(roots,
		reflect.TypeFor[traceSummaryPayloadV3](),
		reflect.TypeFor[ProtocolFailureV1](),
		reflect.TypeFor[receivedAuthenticatedSettlementV1](),
		reflect.TypeFor[responseSendSettlementV1](),
	)
	seen := make(map[reflect.Type]struct{})
	var inspect func(reflect.Type)
	inspect = func(value reflect.Type) {
		for value.Kind() == reflect.Pointer {
			value = value.Elem()
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		if value.Kind() != reflect.Struct {
			t.Fatalf("payload node %v is not a struct", value)
		}
		for field := range value.Fields() {
			tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if tag == "" || tag == "-" {
				t.Fatalf("payload field %v.%s lacks an explicit JSON key", value, field.Name)
			}
			fieldType := field.Type
			for fieldType.Kind() == reflect.Pointer {
				fieldType = fieldType.Elem()
			}
			switch fieldType.Kind() {
			case reflect.Struct:
				inspect(fieldType)
			case reflect.Interface:
				if field.Type != reflect.TypeFor[protocolFailureSettlementV1]() {
					t.Fatalf("payload field %v.%s has open interface %v", value, field.Name, field.Type)
				}
			case reflect.Map, reflect.Func, reflect.Chan:
				t.Fatalf("payload field %v.%s has open type %v", value, field.Name, field.Type)
			case reflect.Slice:
				if fieldType.Elem().Kind() != reflect.String {
					t.Fatalf("payload field %v.%s has open slice %v", value, field.Name, field.Type)
				}
			case reflect.Uint64, reflect.Int64:
				t.Fatalf("payload field %v.%s must use canonical decimal text, not %v", value, field.Name, field.Type)
			case reflect.Int:
				if tag != "exit_code" {
					t.Fatalf("payload field %v.%s uses an unreviewed int number", value, field.Name)
				}
			}
		}
	}
	for _, root := range roots {
		inspect(root)
	}
}

func TestEncodeV3EnvelopeUsesFrozenOrderAndDecimalCounters(t *testing.T) {
	runID := testRunIdentity(0x20)
	record, err := encodeV3(
		runID,
		entryMetadata{
			sequence:  math.MaxUint64,
			time:      time.Unix(1, 234).In(time.FixedZone("private", 9*60*60)),
			elapsedMS: math.MaxInt64,
		},
		clievent.NewReady(),
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":3,"sequence":"18446744073709551615","time":"1970-01-01T00:00:01.000000234Z","elapsed_ms":"9223372036854775807","level":"info","event":"ready","command":"share","runtime_run_id":"` +
		runID.encoded() + `","payload":{}}`
	if string(encoded) != want {
		t.Fatalf("envelope = %s, want %s", encoded, want)
	}
}

func TestEncodeV3ProgressUsesNestedDecimalStrings(t *testing.T) {
	snapshot := mustValue(clievent.NewProgressSnapshot(clievent.ProgressSpec{
		DiscoveredFiles:    math.MaxUint64,
		DiscoveredBytes:    math.MaxUint64,
		PublishedFiles:     math.MaxUint64,
		PublishedBytes:     math.MaxUint64,
		VerifiedBytes:      math.MaxUint64,
		NewlyVerifiedBytes: math.MaxUint64,
		FileOutcomes:       clievent.FileOutcomes{DownloadedFiles: math.MaxUint64},
		Discovery:          clievent.DiscoveryFailed,
		CountersExact:      false,
	}))
	event := mustValue(clievent.NewTransferProgress(
		mustValue(clievent.NewReceiveOperationID(testIdentity(t, 0xa1))),
		mustValue(clievent.NewTransferJobID(testIdentity(t, 0xb2))),
		snapshot,
	))
	record, err := encodeV3(
		testRunIdentity(0x21),
		entryMetadata{sequence: 1, time: time.Unix(1, 0), elapsedMS: 2},
		event,
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := record.Payload.(transferProgressPayloadV3)
	if !ok {
		t.Fatalf("payload type = %T", record.Payload)
	}
	max := "18446744073709551615"
	if payload.Progress.DiscoveredFiles != max ||
		payload.Progress.DiscoveredBytes != max ||
		payload.Progress.PublishedFiles != max ||
		payload.Progress.PublishedBytes != max ||
		payload.Progress.VerifiedBytes != max ||
		payload.Progress.NewlyVerifiedBytes != max ||
		payload.Progress.FileOutcomes.DownloadedFiles != max {
		t.Fatalf("unsafe progress projection: %+v", payload.Progress)
	}
	if payload.ReceiveOperationID != base64.RawURLEncoding.EncodeToString(event.ReceiveOperationID().Bytes()) {
		t.Fatalf("local typed identity projection = %q", payload.ReceiveOperationID)
	}
	if record.Correlation != nil {
		t.Fatalf("local progress acquired cross-runtime correlation: %+v", record.Correlation)
	}
}

func TestEncodeV3ProjectsCorrelationAndPreservesLaneEpochZero(t *testing.T) {
	session := mustValue(clievent.NewProtocolSessionID(testIdentity(t, 0x31)))
	operation := mustValue(clievent.NewProtocolOperationID(testIdentity(t, 0x32)))
	lane := mustValue(clievent.NewLaneIdentity(math.MaxUint32, 0))
	event := mustValue(clievent.NewProtocolOperationObserved(clievent.ProtocolOperationSpec{
		Command:           clievent.CommandGet,
		Role:              clievent.ProtocolRoleReceiver,
		Stage:             clievent.ProtocolOperationReceiverEnded,
		ProtocolSession:   session,
		ProtocolOperation: operation,
		RequestKind:       clievent.ProtocolMessageRequestBlocks,
		Lane:              lane,
		HasLane:           true,
		Cause:             clievent.ProtocolOperationCauseNone,
	}))
	record, err := encodeV3(
		testRunIdentity(0x22),
		entryMetadata{sequence: 1, time: time.Unix(1, 0)},
		event,
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.Correlation == nil ||
		record.Correlation.ProtocolSessionID != base64.RawURLEncoding.EncodeToString(session.Bytes()) ||
		record.Correlation.ProtocolOperationID != base64.RawURLEncoding.EncodeToString(operation.Bytes()) ||
		record.Correlation.LaneID == nil || *record.Correlation.LaneID != math.MaxUint32 ||
		record.Correlation.LaneEpoch == nil || *record.Correlation.LaneEpoch != 0 {
		t.Fatalf("correlation = %+v", record.Correlation)
	}
}

func TestEncodeV3ProjectsAuthenticatedProtocolFailure(t *testing.T) {
	session := mustValue(clievent.NewProtocolSessionID(testIdentity(t, 0x41)))
	operation := mustValue(clievent.NewProtocolOperationID(testIdentity(t, 0x42)))
	lane := mustValue(clievent.NewLaneIdentity(7, 2))
	failure := mustValue(clievent.NewReceivedAuthenticatedProtocolFailure(clievent.ProtocolFailureSpec{
		RequestKind:       clievent.ProtocolMessageRequestBlocks,
		WireScope:         clievent.ProtocolFailureRevision,
		WireCode:          0x3008,
		Retryable:         true,
		RetryAfterMillis:  12_345,
		HasRetryAfter:     true,
		ProtocolSession:   session,
		ProtocolOperation: operation,
		Lane:              lane,
		HasLane:           true,
	}))
	event := mustValue(clievent.NewProtocolOperationObserved(clievent.ProtocolOperationSpec{
		Command:           clievent.CommandGet,
		Role:              clievent.ProtocolRoleReceiver,
		Stage:             clievent.ProtocolOperationReceiverFailed,
		ProtocolSession:   session,
		ProtocolOperation: operation,
		RequestKind:       clievent.ProtocolMessageRequestBlocks,
		ResponseKind:      clievent.ProtocolMessageOperationError,
		HasResponse:       true,
		Lane:              lane,
		HasLane:           true,
		Failure:           failure,
		Cause:             clievent.ProtocolOperationCauseProtocolFailure,
	}))
	record, err := encodeV3(
		testRunIdentity(0x23),
		entryMetadata{sequence: 1, time: time.Unix(1, 0)},
		event,
	)
	if err != nil {
		t.Fatal(err)
	}
	payload := record.Payload.(protocolOperationPayloadV3)
	if payload.ProtocolFailure == nil {
		t.Fatal("protocol failure missing")
	}
	projected := payload.ProtocolFailure
	settlement, ok := projected.Settlement.(receivedAuthenticatedSettlementV1)
	if !ok || settlement.Kind != "received_authenticated" ||
		projected.RequestKind != "request_blocks" || projected.WireScope != "revision" ||
		projected.WireCode != 0x3008 || !projected.Retryable ||
		projected.RetryAfterMS == nil || *projected.RetryAfterMS != 12_345 ||
		projected.Correlation.ProtocolSessionID != record.Correlation.ProtocolSessionID ||
		projected.Correlation.ProtocolOperationID != record.Correlation.ProtocolOperationID {
		t.Fatalf("protocol failure = %+v, settlement %T", projected, projected.Settlement)
	}
	encoded, _ := json.Marshal(record)
	for _, forbidden := range []string{"message", "body", "stack", "error"} {
		if strings.Contains(string(encoded), `"`+forbidden+`"`) {
			t.Fatalf("forbidden protocol field %q in %s", forbidden, encoded)
		}
	}
}

func TestEncodeV3ResponseSendFailureOmitsRetryAfterAndKeepsSettlement(t *testing.T) {
	session := mustValue(clievent.NewProtocolSessionID(testIdentity(t, 0x51)))
	operation := mustValue(clievent.NewProtocolOperationID(testIdentity(t, 0x52)))
	failure := mustValue(clievent.NewResponseSendProtocolFailure(
		clievent.ProtocolFailureSpec{
			RequestKind:       clievent.ProtocolMessageReleaseLease,
			WireScope:         clievent.ProtocolFailureRevision,
			WireCode:          9,
			Retryable:         true,
			RetryAfterMillis:  30_000,
			HasRetryAfter:     true,
			ProtocolSession:   session,
			ProtocolOperation: operation,
		},
		clievent.ProtocolFailureResponseSendSettlement{
			Admitted: true, Settled: true, Outcome: clievent.ProtocolSendDelivered,
		},
	))
	event := mustValue(clievent.NewProtocolOperationObserved(clievent.ProtocolOperationSpec{
		Command:           clievent.CommandShare,
		Role:              clievent.ProtocolRoleSender,
		Stage:             clievent.ProtocolOperationSenderResponseSettled,
		ProtocolSession:   session,
		ProtocolOperation: operation,
		RequestKind:       clievent.ProtocolMessageReleaseLease,
		ResponseKind:      clievent.ProtocolMessageOperationError,
		HasResponse:       true,
		HasSend:           true,
		SendSettled:       true,
		SendAdmitted:      true,
		SendOutcome:       clievent.ProtocolSendDelivered,
		Failure:           failure,
		Cause:             clievent.ProtocolOperationCauseNone,
	}))
	record, err := encodeV3(
		testRunIdentity(0x24),
		entryMetadata{sequence: 1, time: time.Unix(1, 0)},
		event,
	)
	if err != nil {
		t.Fatal(err)
	}
	projected := record.Payload.(protocolOperationPayloadV3).ProtocolFailure
	settlement, ok := projected.Settlement.(responseSendSettlementV1)
	if !ok || settlement.Kind != "response_send" || !settlement.Admitted ||
		!settlement.Settled || settlement.Outcome != "delivered" {
		t.Fatalf("response-send settlement = %#v", projected.Settlement)
	}
	if projected.RetryAfterMS != nil {
		t.Fatalf("response-send failure retained authenticated retry delay: %+v", projected)
	}
}

func TestEncodeV3SeparatesSenderTerminalRootFromSendConsequence(t *testing.T) {
	session := mustValue(clievent.NewProtocolSessionID(testIdentity(t, 0x61)))
	lane := mustValue(clievent.NewLaneIdentity(1, 0))
	send := mustValue(clievent.NewSenderTerminalSendObserved(
		session, lane, false,
		clievent.SenderTerminalSendUnsettled,
		clievent.SenderTerminalSendUnknown,
		clievent.SenderTerminalSendFailed,
	))
	root := mustValue(clievent.NewSenderSessionTerminated(
		session,
		clievent.SenderSessionTerminalGracefulStop,
		clievent.SenderSessionTerminalNormalStop,
	))
	sendRecord, err := encodeV3(
		testRunIdentity(0x25),
		entryMetadata{sequence: 1, time: time.Unix(1, 0)},
		send,
	)
	if err != nil {
		t.Fatal(err)
	}
	rootRecord, err := encodeV3(
		testRunIdentity(0x25),
		entryMetadata{sequence: 2, time: time.Unix(2, 0)},
		root,
	)
	if err != nil {
		t.Fatal(err)
	}
	if sendRecord.Event != "sender_terminal_send_observed" ||
		rootRecord.Event != "sender_session_terminated" ||
		sendRecord.Correlation.LaneID == nil || rootRecord.Correlation.LaneID != nil {
		t.Fatalf("terminal records = send %+v, root %+v", sendRecord, rootRecord)
	}
	rootPayload := rootRecord.Payload.(senderSessionTerminatedPayloadV3)
	if rootPayload.Trigger != "graceful_stop" || rootPayload.Provenance != "normal_stop" {
		t.Fatalf("terminal root payload = %+v", rootPayload)
	}
}

func TestSummaryV3IsTypedAndTruthful(t *testing.T) {
	record := summaryV3(
		testRunIdentity(0x26),
		clievent.CommandGet,
		entryMetadata{sequence: math.MaxUint64, time: time.Unix(1, 0), elapsedMS: 4},
		Status{
			Complete:         false,
			LifecycleDropped: math.MaxUint64,
			ProgressDropped:  7,
			EventsWritten:    11,
			WriterFailed:     true,
			FlushFailed:      true,
			SchemaLimited:    true,
		},
	)
	payload, ok := record.Payload.(traceSummaryPayloadV3)
	if !ok || record.Event != "trace_summary" || record.Level != "warn" ||
		!payload.Incomplete || payload.LifecycleDropped != "18446744073709551615" ||
		payload.ProgressDropped != "7" || payload.EventsWritten != "11" ||
		!payload.WriterFailed || !payload.FlushFailed || !payload.SchemaLimited {
		t.Fatalf("summary = %+v payload=%+v", record, payload)
	}
}

func assertEnvelopeAllowlist(t *testing.T, encoded []byte, hasCorrelation bool) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"schema_version", "sequence", "time", "elapsed_ms", "level", "event",
		"command", "runtime_run_id", "payload",
	}
	if hasCorrelation {
		want = append(want, "correlation")
	}
	if len(fields) != len(want) {
		t.Fatalf("envelope fields = %v in %s", reflect.ValueOf(fields).MapKeys(), encoded)
	}
	for _, name := range want {
		if _, ok := fields[name]; !ok {
			t.Fatalf("envelope field %q missing from %s", name, encoded)
		}
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(fields["payload"], &payload); err != nil || payload == nil {
		t.Fatalf("payload is not an object in %s: %v", encoded, err)
	}
}

func allTraceEvents(t *testing.T) []clievent.Event {
	t.Helper()
	failure := mustValue(clievent.NewFailure(clievent.FailureUnexpected))
	retryFailure := mustValue(clievent.NewRetryableFailure(
		clievent.FailureRelayStarting, math.MaxUint64,
	))
	fault := mustValue(clievent.NewFaultContext(
		clievent.FaultOutput, clievent.FaultFileLocal, 1,
	))
	faultFailure := mustValue(clievent.NewFaultFailure(clievent.FailureOutputStateIO, fault))
	authority := mustValue(clievent.NewRelayAuthority(
		clievent.RelayWSS, "relay.example", 443,
	))
	receiveOperation := mustValue(clievent.NewReceiveOperationID(testIdentity(t, 0x11)))
	protocolSession := mustValue(clievent.NewProtocolSessionID(testIdentity(t, 0x22)))
	protocolOperation := mustValue(clievent.NewProtocolOperationID(testIdentity(t, 0x23)))
	transferJob := mustValue(clievent.NewTransferJobID(testIdentity(t, 0x33)))
	peerPath := mustValue(clievent.NewPeerPathID(testIdentity(t, 0x44)))
	peerAttempt := mustValue(clievent.NewPeerAttemptID(testIdentity(t, 0x55)))
	lane := mustValue(clievent.NewLaneIdentity(7, 9))
	relaySession := mustValue(clievent.NewRelaySessionID([]byte{1, 2, 3, 4, 5, 6, 7, 8}))
	progress := mustValue(clievent.NewProgressSnapshot(clievent.ProgressSpec{
		DiscoveredFiles:    3,
		DiscoveredBytes:    500,
		PublishedFiles:     1,
		PublishedBytes:     100,
		VerifiedBytes:      200,
		NewlyVerifiedBytes: 150,
		FileOutcomes:       clievent.FileOutcomes{DownloadedFiles: 1, ModifiedTimeWarnings: 2},
		Discovery:          clievent.DiscoveryComplete,
		CountersExact:      true,
	}))
	subject := mustValue(clievent.NewFileSubject(
		clievent.NewDisplayName(displayNameCanary), math.MaxUint64,
	))
	transferResult := mustValue(clievent.NewTransferResult(clievent.TransferResultSpec{
		Status:              clievent.ResultPartial,
		ExitCode:            clievent.ExitFailure,
		Drift:               clievent.DriftNone,
		Elapsed:             3*time.Second + 4*time.Millisecond,
		Destination:         clievent.NewDisplayPath(displayPathCanary),
		DestinationAdjusted: true,
		Files:               clievent.FileOutcomes{DownloadedFiles: 1, FailedFiles: 1},
		DirectoryFailures:   math.MaxUint64,
		OmittedDiagnostics:  2,
		PublishedBytes:      100,
		CountersExact:       false,
		Failure:             failure,
	}))
	shareResult := mustValue(clievent.NewShareResult(clievent.ShareResultSpec{
		ExitCode: clievent.ExitSuccess,
		Elapsed:  2 * time.Second,
	}))

	return []clievent.Event{
		clievent.NewReady(),
		mustValue(clievent.NewSharingSubjectSelected(subject)),
		mustValue(clievent.NewRelayConnected(clievent.CommandShare, authority)),
		mustValue(clievent.NewRelayRecovering(
			clievent.CommandGet, authority, 2, clievent.RelayRecoveryFailed, retryFailure,
		)),
		mustValue(clievent.NewContentPathSelected(clievent.ContentPathDirect)),
		mustValue(clievent.NewFallback(
			clievent.CommandGet, clievent.TransportWebRTC, clievent.TransportRelay, failure,
		)),
		mustValue(clievent.NewTransferProgress(receiveOperation, transferJob, progress)),
		mustValue(clievent.NewWarning(clievent.CommandGet, faultFailure)),
		mustValue(clievent.NewCommandFailed(
			clievent.CommandShare, clievent.ExitFailure, failure,
		)),
		mustValue(clievent.NewTransferSettled(transferResult)),
		mustValue(clievent.NewSharingStopped(shareResult)),
		mustValue(clievent.NewTraceIncomplete(
			clievent.CommandGet, clievent.TraceIncompleteLifecycleDrop, math.MaxUint64, 4,
		)),
		mustValue(clievent.NewLaneAdopted(
			clievent.CommandGet, protocolSession, lane, clievent.TransportWebRTC,
		)),
		mustValue(clievent.NewRelayLifecycleObserved(clievent.RelayLifecycleSpec{
			Command:          clievent.CommandShare,
			LinkID:           math.MaxUint64,
			RelaySession:     relaySession,
			SendOperationID:  math.MaxUint64,
			Stage:            clievent.RelaySendAdmitted,
			Disposition:      clievent.SendAccepted,
			RetirementSource: clievent.RelayRetirementNone,
			Cause:            clievent.RelayCauseTransport,
			DrainCause:       clievent.RelayCauseNone,
			Dropped:          math.MaxUint64,
		})),
		mustValue(clievent.NewWebRTCLifecycleObserved(clievent.WebRTCLifecycleSpec{
			Command:         clievent.CommandGet,
			ChannelID:       math.MaxUint64,
			SendOperationID: math.MaxUint64,
			Operation:       clievent.WebRTCChannel,
			Transition:      clievent.WebRTCTraceDropped,
			Disposition:     clievent.SendAccepted,
			State:           clievent.ChannelOpen,
			Terminal:        clievent.WebRTCTerminalNone,
			Cause:           clievent.WebRTCCauseNone,
			Dropped:         math.MaxUint64,
		})),
		mustValue(clievent.NewPeerAttemptObserved(clievent.PeerAttemptSpec{
			Command:       clievent.CommandGet,
			Session:       protocolSession,
			PeerPath:      peerPath,
			Attempt:       peerAttempt,
			Sequence:      math.MaxUint64,
			ElapsedMillis: math.MaxUint64,
			Stage:         clievent.PeerAttemptFailed,
			FailedAtStage: clievent.PeerAdmissionResponseSettled,
			Candidates:    clievent.CandidateCounts{LocalEmitted: 1, RemoteAccepted: 2},
			HasCandidates: true,
			FailureScope:  clievent.PeerFailureAttempt,
			Failure:       failure,
		})),
		mustValue(clievent.NewTransferLifecycleObserved(clievent.TransferLifecycleSpec{
			ReceiveOperation: receiveOperation,
			ProtocolSession:  protocolSession,
			TransferJob:      transferJob,
			Stage:            clievent.TransferFileSettled,
			Progress:         progress,
			FileSelection:    clievent.FileSelectionInherited,
			FileSettlement:   clievent.FileItemBlocked,
			ItemBlockReason:  clievent.ItemBlockRevisionConflict,
			TreeSettlement:   clievent.TreeSettlementPartial,
			Failure:          failure,
		})),
		mustValue(clievent.NewFilesystemOutputObserved(clievent.FilesystemOutputSpec{
			Operation:           clievent.FilesystemRuntimeDecision,
			ReceiveOperation:    receiveOperation,
			Certification:       clievent.FilesystemCertificationWindowsNTFSProcessRestart,
			NativeLockScope:     clievent.FilesystemNativeLockSession,
			NativeLockMilestone: clievent.FilesystemNativeLockAcquired,
			RootDisposition:     clievent.FilesystemRootAuthorityCreated,
			RuntimeComponent:    clievent.FilesystemRuntimeFile,
			RuntimeOperation:    clievent.FilesystemRuntimeWriteRange,
			RuntimeDecision:     clievent.FilesystemRuntimeNeedsAttention,
			CheckpointDecision:  clievent.FilesystemCheckpointRevisionConflict,
			OperationID:         math.MaxUint64,
			ClaimID:             math.MaxUint64,
			Counters: clievent.FilesystemOutputCounters{
				NodeClaims:             math.MaxUint64,
				DirectoryClaims:        2,
				FileClaims:             3,
				ActiveFileClaims:       4,
				ReservedFileSlots:      5,
				DirectoryMetadataBytes: math.MaxUint64,
				CheckpointRecords:      7,
			},
			Failure:            faultFailure,
			FailureStage:       clievent.FilesystemFailureNativeDurability,
			ReconciliationStep: clievent.FilesystemReconciliationRecordPromotion,
			NativeErrorClass:   clievent.FilesystemNativeErrorSharingViolation,
		})),
		mustValue(clievent.NewSenderTerminalSendObserved(
			protocolSession, lane, false,
			clievent.SenderTerminalSendUnsettled,
			clievent.SenderTerminalSendUnknown,
			clievent.SenderTerminalSendFailed,
		)),
		mustValue(clievent.NewSenderSessionTerminated(
			protocolSession,
			clievent.SenderSessionTerminalGracefulStop,
			clievent.SenderSessionTerminalNormalStop,
		)),
		mustValue(clievent.NewCatalogStorageObserved(
			clievent.CatalogStorageBudgetRejected,
			clievent.CatalogStorageCauseBudget,
			clievent.CatalogUsage{
				ActiveScans: math.MaxUint64,
				ScanWork:    math.MaxUint64,
				Entries:     math.MaxUint64,
				MemoryBytes: math.MaxUint64,
				SpillBytes:  math.MaxUint64,
			},
			math.MaxUint64,
		)),
		mustValue(clievent.NewRootPrefetchObserved(
			clievent.RootPrefetchCommitted,
			math.MaxUint64,
			math.MaxUint64,
			math.MaxUint64,
		)),
		mustValue(clievent.NewProtocolOperationObserved(clievent.ProtocolOperationSpec{
			Command:                 clievent.CommandGet,
			Role:                    clievent.ProtocolRoleReceiver,
			Stage:                   clievent.ProtocolOperationReceiverFailed,
			ProtocolSession:         protocolSession,
			ProtocolOperation:       protocolOperation,
			RequestKind:             clievent.ProtocolMessageReleaseLease,
			Lane:                    lane,
			HasLane:                 true,
			HasSend:                 true,
			SendSettled:             true,
			SendAdmitted:            true,
			SendOutcome:             clievent.ProtocolSendDelivered,
			DeadlineRemainingMillis: math.MaxUint64,
			HasDeadline:             true,
			OperationElapsedMillis:  math.MaxUint64,
			UsableLanesAtSelection:  math.MaxUint32,
			UsableLanesAtSettlement: math.MaxUint32,
			Cause:                   clievent.ProtocolOperationCauseDeadline,
		})),
		mustValue(clievent.NewLaneSettlementObserved(clievent.LaneSettlementSpec{
			Session:             protocolSession,
			Route:               clievent.LaneRouteDirect,
			Lane:                lane,
			DeliveredBlocks:     math.MaxUint64,
			DeliveredBytes:      math.MaxUint64,
			FailedBlockAttempts: 2,
			ReassignedBlocks:    3,
			Incomplete:          true,
		})),
		mustValue(clievent.NewObserverLossObserved(clievent.ObserverLossSpec{
			Command:  clievent.CommandGet,
			Category: clievent.ObserverLossReceiverTermination,
			Reason:   clievent.ObserverLossAdapterCapacityTimeout,
			Count:    math.MaxUint64,
		})),
		mustValue(clievent.NewReceiverTerminationObserved(clievent.ReceiverTerminationSpec{
			Operation:             protocolOperation,
			HasOperation:          true,
			LocalGeneration:       math.MaxUint64,
			TransitionAuthority:   clievent.ReceiverTerminalRemote,
			Disposition:           clievent.ReceiverSessionUnsafe,
			TransitionProvenance:  clievent.ReceiverProvenanceRemoteFailureMalformed,
			ConsequenceProvenance: clievent.ReceiverProvenanceRemoteFailureMalformed,
			LocalStopReason:       clievent.ReceiverLocalStopNone,
			DiagnosticsTruncated:  true,
			BenignComponents: []clievent.ReceiverBenignComponent{
				clievent.ReceiverBenignContextCanceled,
			},
			RetainedCauseClasses: []clievent.ReceiverCauseClass{
				clievent.ReceiverCauseProtocol,
			},
			TeardownTransitions: []clievent.PeerTeardownTransition{
				clievent.PeerTeardownShutdownInitiated,
			},
			PeerShutdownFailed: true,
			ChannelDrainFailed: true,
		})),
	}
}

func assertNoPrivacyCanaries(t *testing.T, encoded []byte) {
	t.Helper()
	canaries := []string{
		displayNameCanary,
		displayPathCanary,
		"https://relay.example/private/route?auth=token#secret-key",
		"split-key-secret",
		"authentication-token-secret",
		"private-key-material",
		"catalog/private/path",
		"argv-secret-marker",
		"environment-secret-marker",
		"provider-error-secret",
	}
	for _, canary := range canaries {
		if strings.Contains(string(encoded), canary) {
			t.Fatalf("privacy canary %q leaked in %s", canary, encoded)
		}
		digest := sha256.Sum256([]byte(canary))
		encodedDigest := hex.EncodeToString(digest[:])
		if strings.Contains(string(encoded), encodedDigest) ||
			strings.Contains(string(encoded), encodedDigest[:12]) {
			t.Fatalf("privacy canary hash for %q leaked in %s", canary, encoded)
		}
	}
}

func testRunIdentity(marker byte) runIdentity {
	var identity runIdentity
	for index := range identity {
		identity[index] = marker + byte(index)
	}
	return identity
}

func testIdentity(t *testing.T, marker byte) []byte {
	t.Helper()
	identity := make([]byte, clievent.IdentityBytes)
	for index := range identity {
		identity[index] = marker + byte(index)
	}
	return identity
}

func mustValue[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}
