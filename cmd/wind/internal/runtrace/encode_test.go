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
	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
)

const (
	displayNameCanary = "secret-file-name-token.txt"
	displayPathCanary = `C:\private\catalog\secret-file-name-token.txt`
)

func TestTraceDistinguishesHistoricalDeliveryFromVerifiedContent(t *testing.T) {
	snapshot, err := appobs.NewProgressSnapshot(appobs.ProgressSpec{
		DiscoveredFiles: 1, DiscoveredBytes: 4,
		PublishedFiles: 1, PublishedBytes: 4, PreviouslyPublishedBytes: 4,
		FileOutcomes: appobs.FileOutcomes{PreviouslyPublishedFiles: 1},
		Discovery:    appobs.DiscoveryComplete, CountersExact: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	progress, err := wire.EncodeProgress(snapshot)
	if err != nil || progress.PreviouslyPublishedBytes != "4" || progress.VerifiedBytes != "0" ||
		progress.NewlyVerifiedBytes != "0" || progress.FileOutcomes.PreviouslyPublishedFiles != "1" ||
		progress.FileOutcomes.ResumedFiles != "0" {
		t.Fatalf("historical trace = (%+v, %v)", progress, err)
	}
}

func TestEncodeVisitsEveryEventWithSealedPayloads(t *testing.T) {
	events := allTraceEvents(t)
	if got, want := len(events), 15; got != want {
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
		"observer_loss",
		"platform_setup",
	}
	eventNames := make(map[string]struct{}, len(events))
	payloadTypes := make(map[reflect.Type]string, len(events))
	for index, event := range events {
		record, err := encode(
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
			payloadType != reflect.TypeFor[wire.EmptyPayload]() {
			t.Fatalf("events %q and %q share payload type %v", previous, record.Event, payloadType)
		}
		payloadTypes[payloadType] = record.Event
		assertEnvelopeAllowlist(t, encoded, record.Correlation != nil)
		assertNoPrivacyCanaries(t, encoded)
	}
}

func TestPayloadSchemasAreClosedAndUseSafeNumericRepresentations(t *testing.T) {
	var roots []reflect.Type
	for _, event := range allTraceEvents(t) {
		record, err := encode(
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
		reflect.TypeFor[traceSummaryPayload](),
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
			if field.Anonymous {
				inspect(field.Type)
				continue
			}
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
			case reflect.Interface, reflect.Map, reflect.Func, reflect.Chan:
				t.Fatalf("payload field %v.%s has open type %v", value, field.Name, field.Type)
			case reflect.Slice:
				switch fieldType.Elem().Kind() {
				case reflect.String:
				case reflect.Struct:
					inspect(fieldType.Elem())
				default:
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

func TestEncodeEnvelopeUsesFrozenOrderAndDecimalCounters(t *testing.T) {
	runID := testRunIdentity(0x20)
	record, err := encode(
		runID,
		entryMetadata{
			sequence:  math.MaxUint64,
			time:      time.Unix(1, 234).In(time.FixedZone("private", 9*60*60)),
			elapsedMS: math.MaxInt64,
		},
		appobs.NewReady(),
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":5,"sequence":"18446744073709551615","time":"1970-01-01T00:00:01.000000234Z","elapsed_ms":"9223372036854775807","level":"info","event":"ready","command":"share","runtime_run_id":"` +
		runID.encoded() + `","payload":{}}`
	if string(encoded) != want {
		t.Fatalf("envelope = %s, want %s", encoded, want)
	}
}

func TestEncodeProgressUsesNestedDecimalStrings(t *testing.T) {
	snapshot := mustValue(appobs.NewProgressSnapshot(appobs.ProgressSpec{
		DiscoveredFiles:         math.MaxUint64,
		DiscoveredBytes:         math.MaxUint64,
		PublishedFiles:          math.MaxUint64,
		PublishedBytes:          math.MaxUint64,
		VerifiedBytes:           math.MaxUint64,
		NewlyVerifiedBytes:      math.MaxUint64,
		FileOutcomes:            appobs.FileOutcomes{DownloadedFiles: math.MaxUint64},
		CapacityActiveWaiters:   math.MaxUint32,
		CapacityAccumulatedWait: 1250 * time.Millisecond,
		CapacityWaitAttempts:    math.MaxUint64,
		Discovery:               appobs.DiscoveryFailed,
		CountersExact:           false,
	}))
	event := mustValue(appobs.NewTransferProgress(
		mustValue(appobs.NewReceiveOperationID(testIdentity(t, 0xa1))),
		mustValue(appobs.NewTransferJobID(testIdentity(t, 0xb2))),
		snapshot,
	))
	record, err := encode(
		testRunIdentity(0x21),
		entryMetadata{sequence: 1, time: time.Unix(1, 0), elapsedMS: 2},
		event,
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := record.Payload.(wire.TransferProgressPayload)
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
		payload.Progress.FileOutcomes.DownloadedFiles != max ||
		payload.Progress.CapacityWait.ActiveWaiters != "4294967295" ||
		payload.Progress.CapacityWait.AccumulatedWaitMS != "1250" ||
		payload.Progress.CapacityWait.Attempts != max {
		t.Fatalf("unsafe progress projection: %+v", payload.Progress)
	}
	if payload.ReceiveOperationID != base64.RawURLEncoding.EncodeToString(event.ReceiveOperationID().Bytes()) {
		t.Fatalf("local typed identity projection = %q", payload.ReceiveOperationID)
	}
	if record.Correlation != nil {
		t.Fatalf("local progress acquired cross-runtime correlation: %+v", record.Correlation)
	}
}

func TestSummaryIsTypedAndTruthful(t *testing.T) {
	record := summaryRecord(
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
	payload, ok := record.Payload.(traceSummaryPayload)
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
	failure := mustValue(appobs.NewFailure(appobs.FailureUnexpected))
	retryFailure := mustValue(appobs.NewRetryableFailure(
		appobs.FailureRelayStarting, math.MaxUint64,
	))
	fault := mustValue(appobs.NewFaultContext(
		appobs.FaultOutput, appobs.FaultFileLocal, 1,
	))
	faultFailure := mustValue(appobs.NewFaultFailure(appobs.FailureOutputStateIO, fault))
	origin := mustValue(appobs.NewRelayOrigin(
		appobs.RelayWSS, "relay.example", 443,
	))
	receiveOperation := mustValue(appobs.NewReceiveOperationID(testIdentity(t, 0x11)))
	protocolSession := mustValue(appobs.NewProtocolSessionID(testIdentity(t, 0x22)))
	transferJob := mustValue(appobs.NewTransferJobID(testIdentity(t, 0x33)))
	lane := mustValue(appobs.NewLaneIdentity(7, 9))
	progress := mustValue(appobs.NewProgressSnapshot(appobs.ProgressSpec{
		DiscoveredFiles:    3,
		DiscoveredBytes:    500,
		PublishedFiles:     1,
		PublishedBytes:     100,
		VerifiedBytes:      200,
		NewlyVerifiedBytes: 150,
		FileOutcomes:       appobs.FileOutcomes{DownloadedFiles: 1, ModifiedTimeWarnings: 2},
		Discovery:          appobs.DiscoveryComplete,
		CountersExact:      true,
	}))
	subject := mustValue(appobs.NewFileSubject(
		appobs.NewDisplayName(displayNameCanary), math.MaxUint64,
	))
	transferResult := mustValue(clievent.NewTransferResult(clievent.TransferResultSpec{
		Status:              clievent.ResultPartial,
		ExitCode:            clievent.ExitFailure,
		Drift:               clievent.DriftNone,
		Elapsed:             3*time.Second + 4*time.Millisecond,
		Destination:         appobs.NewDisplayPath(displayPathCanary),
		DestinationAdjusted: true,
		Files:               appobs.FileOutcomes{DownloadedFiles: 1, FailedFiles: 1},
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
		appobs.NewReady(),
		mustValue(appobs.NewSharingSubjectSelected(subject)),
		mustValue(clievent.NewRelayConnected(clievent.CommandShare, origin)),
		mustValue(clievent.NewRelayRecovering(
			clievent.CommandGet, origin, 2, appobs.RelayRecoveryFailed, retryFailure,
		)),
		mustValue(appobs.NewContentPathSelected(appobs.ContentPathDirect)),
		mustValue(clievent.NewFallback(
			clievent.CommandGet, appobs.TransportWebRTC, appobs.TransportRelay, failure,
		)),
		mustValue(appobs.NewTransferProgress(receiveOperation, transferJob, progress)),
		mustValue(clievent.NewWarning(clievent.CommandGet, faultFailure)),
		mustValue(clievent.NewCommandFailed(
			clievent.CommandShare, clievent.ExitFailure, failure,
		)),
		mustValue(clievent.NewTransferSettled(transferResult)),
		mustValue(clievent.NewSharingStopped(shareResult)),
		mustValue(clievent.NewTraceIncomplete(
			clievent.CommandGet, clievent.TraceIncompleteLifecycleDrop, clievent.TraceLoss{LifecycleDrops: math.MaxUint64, ProgressDrops: 4},
		)),
		mustValue(clievent.NewLaneAdopted(
			clievent.CommandGet, protocolSession, lane, appobs.TransportWebRTC,
		)),
		mustValue(appobs.NewObserverLossObserved(appobs.ObserverLossSpec{
			Workflow: appobs.WorkflowReceive,
			Category: appobs.ObserverLossReceiverTermination,
			Reason:   appobs.ObserverLossAdapterCapacityTimeout,
			Count:    math.MaxUint64,
		})),
		mustValue(clievent.NewPlatformSetupObserved(clievent.CommandGet, "declined", "user-skipped")),
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
	identity := make([]byte, appobs.IdentityBytes)
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
