package runtrace

import (
	"crypto/sha256"
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

func TestEncodeV2VisitsEveryEventAndOmitsPrivacyCanaries(t *testing.T) {
	events := allTraceEvents(t)
	if got, want := len(events), 25; got != want {
		t.Fatalf("event fixture count = %d, want %d", got, want)
	}
	for index, event := range events {
		record, err := encodeV2(
			"11111111111111111111111111111111",
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
		var strict recordV2
		decoder := json.NewDecoder(strings.NewReader(string(encoded)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&strict); err != nil {
			t.Fatalf("strict decode event %T: %v", event, err)
		}
		if strict.SchemaVersion != SchemaVersion || strict.RunID == "" || !strings.HasSuffix(strict.Time, "Z") {
			t.Fatalf("required envelope missing for %T: %+v", event, strict)
		}
		assertNoPrivacyCanaries(t, encoded)
	}
}

func TestEncodeV2UsesDecimalStringsAndSemanticContexts(t *testing.T) {
	identity := testIdentity(t, 0xa1)
	receiveOperation := mustValue(clievent.NewReceiveOperationID(identity))
	transferJob := mustValue(clievent.NewTransferJobID(testIdentity(t, 0xb2)))
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
	progress := mustValue(clievent.NewTransferProgress(receiveOperation, transferJob, snapshot))
	record, err := encodeV2(
		"22222222222222222222222222222222",
		entryMetadata{sequence: 1, time: time.Unix(1, 0), elapsedMS: 2},
		progress,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"discovered_files", "discovered_bytes", "published_files", "published_bytes",
		"verified_bytes", "newly_verified_bytes", "downloaded_files",
	} {
		if got, ok := fields[field].(string); !ok || got != "18446744073709551615" {
			t.Fatalf("%s = %#v, want max uint64 decimal string", field, fields[field])
		}
	}
	if fields["receive_operation_id"] != hex.EncodeToString(identity) {
		t.Fatalf("receive operation identity was not encoded canonically: %#v", fields)
	}
	if _, ok := fields["protocol_session_id"]; ok {
		t.Fatalf("unrelated protocol context leaked into progress: %#v", fields)
	}
	if _, ok := fields["operation_id"]; ok {
		t.Fatalf("generic operation identity must never exist: %#v", fields)
	}
}

func TestEncodeV2ProjectsBoundedProtocolOperationErrorClassification(t *testing.T) {
	session := mustValue(clievent.NewProtocolSessionID(testIdentity(t, 0xc1)))
	operation := mustValue(clievent.NewProtocolOperationID(testIdentity(t, 0xc2)))
	event := mustValue(clievent.NewProtocolOperationObserved(clievent.ProtocolOperationSpec{
		Command: clievent.CommandShare, Role: clievent.ProtocolRoleSender,
		Stage:           clievent.ProtocolOperationSenderResponseSettled,
		ProtocolSession: session, ProtocolOperation: operation,
		RequestKind:  clievent.ProtocolMessageRequestBlocks,
		ResponseKind: clievent.ProtocolMessageOperationError, HasResponse: true,
		OperationErrorScope: clievent.ProtocolOperationErrorRevision,
		OperationErrorCode:  0x3008, OperationErrorRetryable: true, HasOperationError: true,
		Cause: clievent.ProtocolOperationCauseNone,
	}))
	record, err := encodeV2(
		"33333333333333333333333333333333",
		entryMetadata{sequence: 1, time: time.Unix(1, 0)},
		event,
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.ProtocolErrorScope == nil || *record.ProtocolErrorScope != "revision" ||
		record.ProtocolErrorCode == nil || *record.ProtocolErrorCode != 0x3008 ||
		record.ProtocolErrorRetryable == nil || !*record.ProtocolErrorRetryable {
		t.Fatalf("protocol operation error trace = %+v", record)
	}
}

func TestEncodeV2ProjectsSafePeerAdmissionSettlement(t *testing.T) {
	identity := testIdentity(t, 0xa2)
	session := mustValue(clievent.NewProtocolSessionID(identity))
	path := mustValue(clievent.NewPeerPathID(testIdentity(t, 0xa3)))
	attempt := mustValue(clievent.NewPeerAttemptID(testIdentity(t, 0xa4)))
	offer := mustValue(clievent.NewProtocolOperationID(testIdentity(t, 0xa5)))
	grant := mustValue(clievent.NewProtocolOperationID(testIdentity(t, 0xa6)))
	lane := mustValue(clievent.NewLaneIdentity(7, 2))
	event := mustValue(clievent.NewPeerAttemptObserved(clievent.PeerAttemptSpec{
		Command: clievent.CommandShare, Session: session, PeerPath: path, Attempt: attempt,
		OfferOperation: offer, HasOfferOperation: true,
		GrantOperation: grant, HasGrantOperation: true,
		Sequence: 9, ElapsedMillis: 120, Stage: clievent.PeerAdmissionResponseSettled,
		Phase: clievent.PeerPhaseAdmission, Lane: lane, HasLane: true,
		AdmissionDisposition:      clievent.PeerAdmissionRejected,
		ResponseDelivery:          clievent.PeerResponseDelivered,
		RejectionCode:             clievent.PeerLaneRejectAdmissionLimited,
		RejectionRetryAfterMillis: 7_000,
	}))
	record, err := encodeV2(
		"33333333333333333333333333333333",
		entryMetadata{sequence: 1, time: time.Unix(1, 0), elapsedMS: 2},
		event,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]any{
		"stage":                         "admission-response-settled",
		"peer_phase":                    "admission",
		"peer_admission_disposition":    "rejected",
		"peer_response_delivery":        "delivered",
		"peer_lane_rejection_code":      "admission-limited",
		"peer_rejection_retry_after_ms": "7000",
		"peer_offer_operation_id":       offer.Hex(),
		"peer_grant_operation_id":       grant.Hex(),
	} {
		if fields[name] != want {
			t.Fatalf("%s = %#v, want %#v", name, fields[name], want)
		}
	}
}

func TestEncodeV2RootPrefetchUsesOnlyClosedTraceFields(t *testing.T) {
	event := mustValue(clievent.NewRootPrefetchObserved(
		clievent.RootPrefetchCommitted, math.MaxUint64, 11, 2,
	))
	record, err := encodeV2(
		"44444444444444444444444444444444",
		entryMetadata{sequence: 1, time: time.Unix(1, 0), elapsedMS: 2},
		event,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	wantFields := map[string]any{
		"schema_version":              float64(2),
		"sequence":                    float64(1),
		"time":                        time.Unix(1, 0).UTC().Format(time.RFC3339Nano),
		"elapsed_ms":                  float64(2),
		"level":                       "debug",
		"event":                       "root_prefetch",
		"command":                     "share",
		"run_id":                      "44444444444444444444444444444444",
		"decision":                    "committed",
		"root_prefetch_attempt":       "18446744073709551615",
		"root_prefetch_entry_count":   "11",
		"root_prefetch_omitted_count": "2",
	}
	if !reflect.DeepEqual(fields, wantFields) {
		t.Fatalf("root-prefetch record = %#v, want %#v", fields, wantFields)
	}
}

func TestEncodeV2FilesystemFailureClassification(t *testing.T) {
	failure := mustValue(clievent.NewFailure(clievent.FailureOutputNeedsAttention))
	event := mustValue(clievent.NewFilesystemOutputObserved(clievent.FilesystemOutputSpec{
		Operation:          clievent.FilesystemCheckpointReconciled,
		CheckpointDecision: clievent.FilesystemCheckpointRevisionConflict,
		Failure:            failure,
		FailureStage:       clievent.FilesystemFailureNativeDurability,
		ReconciliationStep: clievent.FilesystemReconciliationRecordPromotion,
		NativeErrorClass:   clievent.FilesystemNativeErrorSharingViolation,
	}))
	record, err := encodeV2(
		"55555555555555555555555555555555",
		entryMetadata{sequence: 1, time: time.Unix(1, 0), elapsedMS: 2},
		event,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{
		"filesystem_checkpoint_decision": "revision_conflict",
		"filesystem_failure_stage":       "native_durability",
		"filesystem_reconciliation_step": "record_promotion",
		"filesystem_native_error_class":  "sharing_violation",
	} {
		if got := fields[field]; got != want {
			t.Fatalf("%s = %#v, want %q", field, got, want)
		}
	}
}

func TestEncodeV2LaneSettlementUsesOnlyBoundedSummary(t *testing.T) {
	session := mustValue(clievent.NewProtocolSessionID(testIdentity(t, 0x91)))
	lane := mustValue(clievent.NewLaneIdentity(4, 2))
	event := mustValue(clievent.NewLaneSettlementObserved(clievent.LaneSettlementSpec{
		Session: session, Route: clievent.LaneRouteDirect, Lane: lane,
		DeliveredBlocks: 7, DeliveredBytes: 11, FailedBlockAttempts: 3,
		ReassignedBlocks: 2, Incomplete: true,
	}))
	record, err := encodeV2(
		"66666666666666666666666666666666",
		entryMetadata{sequence: 1, time: time.Unix(1, 0), elapsedMS: 2},
		event,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	wantFields := map[string]any{
		"schema_version":        float64(2),
		"sequence":              float64(1),
		"time":                  time.Unix(1, 0).UTC().Format(time.RFC3339Nano),
		"elapsed_ms":            float64(2),
		"level":                 "debug",
		"event":                 "lane_settlement",
		"command":               "get",
		"run_id":                "66666666666666666666666666666666",
		"protocol_session_id":   session.Hex(),
		"lane_route":            "direct",
		"lane_id":               float64(4),
		"lane_epoch":            float64(2),
		"delivered_blocks":      "7",
		"delivered_bytes":       "11",
		"failed_block_attempts": "3",
		"reassigned_blocks":     "2",
		"incomplete":            true,
	}
	if !reflect.DeepEqual(fields, wantFields) {
		t.Fatalf("lane-settlement record = %#v, want %#v", fields, wantFields)
	}
}

func TestSchemaV2HasNoOpenPayloadOrUnsafeNumericDomainCounter(t *testing.T) {
	schema := reflect.TypeFor[recordV2]()
	seenTags := make(map[string]struct{}, schema.NumField())
	for field := range schema.Fields() {
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if tag == "" || tag == "-" {
			t.Fatalf("field %s has no explicit JSON name", field.Name)
		}
		if _, duplicate := seenTags[tag]; duplicate {
			t.Fatalf("duplicate JSON field %q", tag)
		}
		seenTags[tag] = struct{}{}
		baseType := field.Type
		if baseType.Kind() == reflect.Pointer {
			baseType = baseType.Elem()
		}
		switch baseType.Kind() {
		case reflect.Map, reflect.Interface, reflect.Array, reflect.Struct:
			t.Fatalf("field %s introduces an open or nested payload type %s", field.Name, field.Type)
		case reflect.Slice:
			if baseType.Elem().Kind() != reflect.String {
				t.Fatalf("field %s introduces an open slice type %s", field.Name, field.Type)
			}
		case reflect.Uint64:
			if tag != "sequence" {
				t.Fatalf("domain uint64 %s must be encoded as a decimal string", field.Name)
			}
		}
	}
	if _, exists := seenTags["operation_id"]; exists {
		t.Fatal("schema contains the forbidden generic operation_id")
	}
}

func TestEncodeV2RelayAuthorityContainsNoURLRemainder(t *testing.T) {
	authority := mustValue(clievent.NewRelayAuthority(clievent.RelayWSS, "relay.example", 443))
	event := mustValue(clievent.NewRelayConnected(clievent.CommandShare, authority))
	record, err := encodeV2(
		"33333333333333333333333333333333",
		entryMetadata{sequence: 1, time: time.Unix(1, 0), elapsedMS: 0},
		event,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(record)
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["relay_scheme"] != "wss" || fields["relay_host"] != "relay.example" || fields["relay_port"] != float64(443) {
		t.Fatalf("relay authority not normalized: %#v", fields)
	}
	for _, forbidden := range []string{"relay_url", "userinfo", "path", "query", "fragment"} {
		if _, ok := fields[forbidden]; ok {
			t.Fatalf("relay URL remainder %q was serialized: %#v", forbidden, fields)
		}
	}
}

func allTraceEvents(t *testing.T) []clievent.Event {
	t.Helper()
	failure := mustValue(clievent.NewFailure(clievent.FailureUnexpected))
	retryFailure := mustValue(clievent.NewRetryableFailure(clievent.FailureRelayStarting, math.MaxUint64))
	fault := mustValue(clievent.NewFaultContext(clievent.FaultOutput, clievent.FaultFileLocal, 1))
	faultFailure := mustValue(clievent.NewFaultFailure(clievent.FailureOutputStateIO, fault))
	authority := mustValue(clievent.NewRelayAuthority(clievent.RelayWSS, "relay.example", 443))
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
	subject := mustValue(clievent.NewFileSubject(clievent.NewDisplayName(displayNameCanary), math.MaxUint64))
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
		mustValue(clievent.NewRelayRecovering(clievent.CommandGet, authority, 2, clievent.RelayRecoveryFailed, retryFailure)),
		mustValue(clievent.NewContentPathSelected(clievent.ContentPathDirect)),
		mustValue(clievent.NewFallback(clievent.CommandGet, clievent.TransportWebRTC, clievent.TransportRelay, failure)),
		mustValue(clievent.NewTransferProgress(receiveOperation, transferJob, progress)),
		mustValue(clievent.NewWarning(clievent.CommandGet, faultFailure)),
		mustValue(clievent.NewCommandFailed(clievent.CommandShare, clievent.ExitFailure, failure)),
		mustValue(clievent.NewTransferSettled(transferResult)),
		mustValue(clievent.NewSharingStopped(shareResult)),
		mustValue(clievent.NewTraceIncomplete(clievent.CommandGet, clievent.TraceIncompleteLifecycleDrop, math.MaxUint64, 4)),
		mustValue(clievent.NewLaneAdopted(clievent.CommandGet, protocolSession, lane, clievent.TransportWebRTC)),
		mustValue(clievent.NewRelayLifecycleObserved(clievent.RelayLifecycleSpec{
			Command:          clievent.CommandShare,
			LinkID:           math.MaxUint64,
			RelaySession:     relaySession,
			SendOperationID:  math.MaxUint64,
			Stage:            clievent.RelaySendAdmitted,
			Terminal:         false,
			Disposition:      clievent.SendAccepted,
			RetirementSource: clievent.RelayRetirementNone,
			Cause:            clievent.RelayCauseTransport,
			DrainCause:       clievent.RelayCauseNone,
		})),
		mustValue(clievent.NewWebRTCLifecycleObserved(clievent.WebRTCLifecycleSpec{
			Command:    clievent.CommandGet,
			ChannelID:  math.MaxUint64,
			Operation:  clievent.WebRTCChannel,
			Transition: clievent.WebRTCTraceDropped,
			State:      clievent.ChannelOpen,
			Terminal:   clievent.WebRTCTerminalNone,
			Cause:      clievent.WebRTCCauseNone,
			Dropped:    math.MaxUint64,
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
			FileSettlement:   clievent.FilePublished,
			TreeSettlement:   clievent.TreeSettlementPartial,
			Failure:          failure,
		})),
		mustValue(clievent.NewFilesystemOutputObserved(clievent.FilesystemOutputSpec{
			ReceiveOperation: receiveOperation,
			Operation:        clievent.FilesystemRuntimeDecision,
			Counters: clievent.FilesystemOutputCounters{
				NodeClaims:             math.MaxUint64,
				DirectoryClaims:        2,
				FileClaims:             3,
				ActiveFileClaims:       4,
				ReservedFileSlots:      5,
				DirectoryMetadataBytes: math.MaxUint64,
				CheckpointRecords:      7,
			},
			Failure: faultFailure, FailureStage: clievent.FilesystemFailureNativeDurability,
		})),
		mustValue(clievent.NewSenderTerminalObserved(
			protocolSession,
			lane,
			false,
			clievent.SenderTerminalUnsettled,
			clievent.SenderTerminalUnknown,
			clievent.SenderTerminalFailed,
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
			Command: clievent.CommandGet, Role: clievent.ProtocolRoleReceiver,
			Stage:           clievent.ProtocolOperationReceiverFailed,
			ProtocolSession: protocolSession, ProtocolOperation: protocolOperation,
			RequestKind: clievent.ProtocolMessageReleaseLease,
			Lane:        lane, HasLane: true,
			HasSend: true, SendSettled: true, SendAdmitted: true,
			SendOutcome:             clievent.ProtocolSendDelivered,
			DeadlineRemainingMillis: math.MaxUint64, HasDeadline: true,
			OperationElapsedMillis:  math.MaxUint64,
			UsableLanesAtSelection:  math.MaxUint32,
			UsableLanesAtSettlement: math.MaxUint32,
			Cause:                   clievent.ProtocolOperationCauseDeadline,
		})),
		mustValue(clievent.NewLaneSettlementObserved(clievent.LaneSettlementSpec{
			Session: protocolSession, Route: clievent.LaneRouteDirect, Lane: lane,
			DeliveredBlocks: math.MaxUint64, DeliveredBytes: math.MaxUint64,
			FailedBlockAttempts: 2, ReassignedBlocks: 3, Incomplete: true,
		})),
		mustValue(clievent.NewObserverLossObserved(clievent.ObserverLossSpec{
			Command: clievent.CommandGet, Category: clievent.ObserverLossReceiverTermination,
			Reason: clievent.ObserverLossAdapterCapacityTimeout, Count: math.MaxUint64,
		})),
		mustValue(clievent.NewReceiverTerminationObserved(clievent.ReceiverTerminationSpec{
			Operation: protocolOperation, HasOperation: true, LocalGeneration: math.MaxUint64,
			TransitionAuthority:   clievent.ReceiverTerminalRemote,
			Disposition:           clievent.ReceiverSessionUnsafe,
			TransitionProvenance:  clievent.ReceiverProvenanceRemoteFailureMalformed,
			ConsequenceProvenance: clievent.ReceiverProvenanceRemoteFailureMalformed,
			LocalStopReason:       clievent.ReceiverLocalStopNone, DiagnosticsTruncated: true,
			BenignComponents:     []clievent.ReceiverBenignComponent{clievent.ReceiverBenignContextCanceled},
			RetainedCauseClasses: []clievent.ReceiverCauseClass{clievent.ReceiverCauseProtocol},
			TeardownTransitions:  []clievent.PeerTeardownTransition{clievent.PeerTeardownShutdownInitiated},
			PeerShutdownFailed:   true, ChannelDrainFailed: true,
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
		if strings.Contains(string(encoded), encodedDigest) || strings.Contains(string(encoded), encodedDigest[:12]) {
			t.Fatalf("privacy canary hash for %q leaked in %s", canary, encoded)
		}
	}
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
