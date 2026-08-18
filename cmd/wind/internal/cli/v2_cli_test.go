package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/internal/testoutputroot"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/relay/httpapi"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2endpoint"
	"github.com/windshare/windshare/relay/signaling/v2route"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestParseInterleavedV2Flags(t *testing.T) {
	app := testApp("")
	flags := app.newFlagSet("share")
	relay := flags.String("relay", "", "")
	positionals, outcome := parseInterleaved(flags, []string{"first", "--relay", "ws://relay.example", "second"})
	if outcome != flagParseReady || *relay != "ws://relay.example" || strings.Join(positionals, ",") != "first,second" {
		t.Fatalf("parse result = %v %q %v", positionals, *relay, outcome)
	}
}

func TestGetRequestRejectsRetiredSuite(t *testing.T) {
	shareID := base64.RawURLEncoding.EncodeToString(
		bytes.Repeat([]byte{1}, link.SenderAuthenticatedShareIDBytes),
	)
	retiredKey := base64.RawURLEncoding.EncodeToString(
		append([]byte{0x01}, bytes.Repeat([]byte{2}, link.ReadSecretBytes)...),
	)
	encoded := "http://localhost:5173/" + shareID + "#" + retiredKey
	app := testApp("")
	if _, outcome := app.parseGetRequest([]string{encoded}); outcome != requestParseUsageFailure {
		t.Fatalf("retired suite parse outcome = %d", outcome)
	}
}

func TestRelayRegistrationIdentityRejectsWrongWidths(t *testing.T) {
	if _, _, _, err := relayRegistrationIdentity(liveshare.RegistrationMaterial{}); err == nil {
		t.Fatal("empty relay identity was accepted")
	}
}

func TestTransferResultDriftClassification(t *testing.T) {
	for _, value := range []transferfault.Fault{
		mustCLIFault(transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionChanged)),
		mustCLIFault(transferfault.NewSource(transferfault.ScopeFileLocal, transferfault.SourceRevisionInvalidated)),
		mustCLIFault(transferfault.NewCatalog(transferfault.ScopeDirectoryLocal, transferfault.CatalogDirectoryStale)),
	} {
		result, err := commandprojection.ProjectGetResult(commandprojection.GetResultInput{
			Result: transfer.JobResult{
				Outcome:          transfer.DirectTreeOutcomePartial,
				SourceDriftFault: value,
			},
			Destination: clievent.NewDisplayPath(t.TempDir()),
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Drift() != clievent.DriftSource || result.ExitCode() != clievent.ExitDrift {
			t.Fatalf("drift projection = %v/%v", result.Drift(), result.ExitCode())
		}
	}
	network, err := commandprojection.ProjectGetResult(commandprojection.GetResultInput{
		Result:       transfer.JobResult{Outcome: transfer.DirectTreeOutcomePaused},
		RuntimeError: errors.New("network"),
		Destination:  clievent.NewDisplayPath(t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if network.Drift() != clievent.DriftNone || network.ExitCode() != clievent.ExitNetwork {
		t.Fatalf("network projection = %v/%v", network.Drift(), network.ExitCode())
	}
}

func mustCLIFault(value transferfault.Fault, err error) transferfault.Fault {
	if err != nil {
		panic(err)
	}
	return value
}

func TestSelectionRulesKeepWholeShareAndPathIntentDistinct(t *testing.T) {
	wholeShare, err := selectionRules(nil)
	if err != nil || wholeShare.Mode() != transfer.SelectionByNodeID || !wholeShare.DefaultSelected() {
		t.Fatalf("whole-share rules mode=%d default=%v error=%v", wholeShare.Mode(), wholeShare.DefaultSelected(), err)
	}
	paths, err := selectionRules([]string{"tree/b.txt", "tree/a.txt"})
	if err != nil || paths.Mode() != transfer.SelectionByCatalogPath || paths.DefaultSelected() ||
		!paths.FileSelectedAt(catalog.FileID{}, "tree/a.txt", false) ||
		paths.FileSelectedAt(catalog.FileID{}, "tree/c.txt", false) {
		t.Fatalf("path rules mode=%d default=%v error=%v", paths.Mode(), paths.DefaultSelected(), err)
	}
	if _, err := selectionRules([]string{"../escape"}); err == nil {
		t.Fatal("non-canonical path selection was accepted")
	}
}

func TestShareCancellationDurablyStopsRelayRoute(t *testing.T) {
	store := &memoryStopStore{}
	server := httptest.NewUnstartedServer(nil)
	endpointIdentity, err := v2.NormalizeRelayEndpoint("http://" + server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := v2route.New(context.Background(), v2route.Config{
		MaxRoutes: 8, MaxSessions: 8, MaxSessionsPerShare: 4, Random: rand.Reader, Tombstones: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	challenges, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{Capacity: 16, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := v2endpoint.New(v2endpoint.Config{
		Registry: registry, Challenges: challenges, RelayIdentity: endpointIdentity.Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = httpapi.NewV2Handler(httpapi.V2Config{
		Server: endpoint, AllowLocalhost: true,
		AdmitConnection: func(string) (func(), bool) { return func() {}, true },
	})
	server.Start()
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = endpoint.Shutdown(shutdown)
		cancel()
		server.Close()
	})

	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("stop contract"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout := &lockedTestBuffer{}
	stderr := &lockedTestBuffer{}
	userTrace := newRecordingUserTrace()
	userTracePath := filepath.Join(t.TempDir(), "share.ndjson")
	privateSink := newRecordingV2ProcessTraceSink()
	privateOperation, err := testrun.NewOperation("run-v2-cli", "operation-v2-cli", "share-session-retirement")
	if err != nil {
		t.Fatal(err)
	}
	privateRecorder, err := testrun.NewRecorder(privateOperation, processTraceShareComponent, privateSink)
	if err != nil {
		t.Fatal(err)
	}
	privateTrace := &processTrace{
		operation: privateOperation,
		events:    privateSink,
		recorders: map[testrun.Component]*testrun.Recorder{processTraceShareComponent: privateRecorder},
	}
	app := &App{
		Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader(""),
		processTrace: privateTrace,
		openUserTrace: func(
			target runtrace.Target,
			command clievent.Command,
			_ runtrace.Config,
			_ runtrace.Dependencies,
		) (userTraceRecorder, error) {
			expected, _ := runtrace.ExactFile(userTracePath)
			if target != expected || !filepath.IsAbs(userTracePath) || command != clievent.CommandShare {
				return nil, errors.New("unexpected user trace request")
			}
			return userTrace, nil
		},
	}
	shareContext, cancelShare := context.WithCancel(context.Background())
	result := make(chan int, 1)
	go func() {
		result <- app.Run(shareContext, []string{"share", file, "--relay", server.URL, "--trace", userTracePath})
	}()
	linkLine := waitTestLine(t, stdout, "Link: ")
	capability, err := link.Parse(strings.TrimPrefix(linkLine, "Link: "))
	if err != nil {
		t.Fatal(err)
	}
	shareIDBytes, _ := base64.RawURLEncoding.Strict().DecodeString(capability.ShareID)
	shareID, _ := v2.ShareIDFromBytes(shareIDBytes)
	joined, err := relayv2.DialReceiver(context.Background(), relayv2.ReceiverConfig{
		RelayBaseURL: server.URL, ShareID: shareID,
	})
	if err != nil {
		t.Fatalf("link was printed before relay readiness: %v", err)
	}
	_ = joined.Close()
	for index, arguments := range [][]string{
		{"get", strings.TrimPrefix(linkLine, "Link: "), "-o", testoutputroot.New(t).RootPath},
		{"get", strings.TrimPrefix(linkLine, "Link: "), "-o", testoutputroot.New(t).RootPath, "--only", filepath.Base(file)},
	} {
		getOutput := &lockedTestBuffer{}
		getErrors := &lockedTestBuffer{}
		getApp := &App{Stdout: getOutput, Stderr: getErrors, Stdin: strings.NewReader("")}
		if code := getApp.Run(context.Background(), arguments); code != ExitOK {
			t.Fatalf("get %d exit=%d stderr=%q", index, code, getErrors.String())
		}
	}
	// Receiver success and sender-side channel retirement are intentionally
	// asynchronous. Stop only after both completed sessions have relinquished
	// sender authority, so this test isolates the durable relay STOP contract.
	waitV2PrivateMilestones(t, privateSink, processTraceSenderSessionRetired, 2)
	cancelShare()
	select {
	case code := <-result:
		if code != ExitOK {
			t.Fatalf("share cancellation exit=%d stderr=%q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("share did not complete explicit stop")
	}
	if err := privateTrace.close(); err != nil {
		t.Fatal(err)
	}
	if userTrace.eventCount() == 0 {
		t.Fatal("user trace received no events while the private process trace was active")
	}
	if store.Count() != 1 {
		t.Fatalf("durable STOP writes = %d", store.Count())
	}
	_, err = relayv2.DialReceiver(context.Background(), relayv2.ReceiverConfig{
		RelayBaseURL: server.URL, ShareID: shareID,
	})
	var relayError *relayv2.RelayError
	if !errors.As(err, &relayError) || relayError.Code != v2.ErrorStopped {
		t.Fatalf("join after explicit stop = %v", err)
	}
}

type lockedTestBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedTestBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(value)
}

func (buffer *lockedTestBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func waitTestLine(t *testing.T, output *lockedTestBuffer, prefix string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for line := range strings.SplitSeq(output.String(), "\n") {
			if strings.HasPrefix(line, prefix) {
				return line
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %q", prefix, output.String())
	return ""
}

type recordingUserTrace struct {
	mu     sync.Mutex
	events []clievent.Event
	health chan clievent.TraceIncomplete
}

func newRecordingUserTrace() *recordingUserTrace {
	return &recordingUserTrace{
		health: make(chan clievent.TraceIncomplete),
	}
}

func (trace *recordingUserTrace) Record(event clievent.Event) bool {
	trace.mu.Lock()
	trace.events = append(trace.events, event)
	trace.mu.Unlock()
	return true
}

func (*recordingUserTrace) ReportUpstreamLoss(uint64, uint64) bool { return true }

func (trace *recordingUserTrace) Health() <-chan clievent.TraceIncomplete { return trace.health }
func (*recordingUserTrace) Path() string                                  { return "trace.ndjson" }

func (*recordingUserTrace) Close() runtrace.Status { return runtrace.Status{Complete: true} }

func (trace *recordingUserTrace) eventCount() int {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return len(trace.events)
}

type recordingV2ProcessTraceSink struct {
	mu      sync.Mutex
	events  []testrun.Event
	changed chan struct{}
}

func newRecordingV2ProcessTraceSink() *recordingV2ProcessTraceSink {
	return &recordingV2ProcessTraceSink{changed: make(chan struct{})}
}

func (sink *recordingV2ProcessTraceSink) WriteEvent(event testrun.Event) error {
	sink.mu.Lock()
	sink.events = append(sink.events, event)
	close(sink.changed)
	sink.changed = make(chan struct{})
	sink.mu.Unlock()
	return nil
}

func (*recordingV2ProcessTraceSink) Close() error { return nil }

func (sink *recordingV2ProcessTraceSink) milestoneSnapshot(
	milestone testrun.Milestone,
) (int, <-chan struct{}) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	count := 0
	for _, event := range sink.events {
		if event.Milestone == string(milestone) && event.Outcome == string(testrun.OutcomeSucceeded) {
			count++
		}
	}
	return count, sink.changed
}

func waitV2PrivateMilestones(
	t *testing.T,
	sink *recordingV2ProcessTraceSink,
	milestone testrun.Milestone,
	count int,
) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		observed, changed := sink.milestoneSnapshot(milestone)
		if observed >= count {
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			t.Fatalf("timed out waiting for %d %q private milestones; observed=%d", count, milestone, observed)
		}
	}
}

type memoryStopStore struct {
	mu     sync.Mutex
	values []v2route.Tombstone
}

func (store *memoryStopStore) Load(context.Context) ([]v2route.Tombstone, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]v2route.Tombstone(nil), store.values...), nil
}

func (store *memoryStopStore) Commit(
	_ context.Context,
	value v2route.Tombstone,
) (v2route.CommitOutcome, error) {
	store.mu.Lock()
	store.values = append(store.values, value)
	store.mu.Unlock()
	return v2route.CommitCommitted, nil
}

func (store *memoryStopStore) Count() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.values)
}

func testApp(stdin string) *App {
	return &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Stdin: strings.NewReader(stdin)}
}

func TestRegistrationMaterialUsesEd25519Width(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(privateKey) != ed25519.PrivateKeySize || catalog.IdentityBytes != v2.ShareInstanceBytes {
		t.Fatal("suite-02 identity widths diverged")
	}
}
