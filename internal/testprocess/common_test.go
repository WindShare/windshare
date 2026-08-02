package testprocess

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testtrace"
)

func TestRequestFromSpecBuildsValidatedContract(t *testing.T) {
	root := t.TempDir()
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	request, err := requestFromSpec(Spec{
		Identity: identity,
		Command: Command{
			Executable: filepath.Join(root, "fixture"), Arguments: []string{"one"},
			WorkingDirectory: root, Environment: []protocol.EnvironmentEntry{}, Stdin: []byte("input"),
		},
		Deadline: time.Second, TerminationGrace: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Identity != identity || request.Command.Stdin == nil || request.Command.Stdin.ByteLength != 5 {
		t.Fatalf("request = %#v", request)
	}
	invalid := []struct {
		name     string
		deadline time.Duration
		grace    time.Duration
	}{
		{name: "zero deadline", grace: time.Millisecond},
		{name: "fractional deadline", deadline: time.Microsecond, grace: time.Millisecond},
		{name: "zero grace", deadline: time.Millisecond},
		{name: "fractional grace", deadline: time.Millisecond, grace: time.Microsecond},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			_, err := requestFromSpec(Spec{
				Identity: identity,
				Command: Command{
					Executable: filepath.Join(root, "fixture"), Arguments: []string{},
					WorkingDirectory: root, Environment: []protocol.EnvironmentEntry{},
				},
				Deadline: test.deadline, TerminationGrace: test.grace,
			})
			if err == nil {
				t.Fatal("invalid duration was accepted")
			}
		})
	}
}

func TestInheritEnvironmentIsCanonicalAndOwnsReservedNames(t *testing.T) {
	t.Setenv(testtrace.EventHandleEnvironment, "123")
	t.Setenv(testtrace.EventFDEnvironment, "7")
	t.Setenv(testrun.RunIDEnvironment, "stale-run")
	t.Setenv("WINDSHARE_ENVIRONMENT_CASE_TEST", "old")
	environment, err := InheritEnvironment(map[string]string{
		"windshare_environment_case_test": "new",
		"WINDSHARE_ADDED":                 "value",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := protocol.Command{
		Executable: filepath.Join(t.TempDir(), "fixture"), Arguments: []string{},
		WorkingDirectory: t.TempDir(), Environment: environment,
	}
	if err := protocol.ValidateCommand(command); err != nil {
		t.Fatal(err)
	}
	foundOverride := false
	for _, entry := range environment {
		if isOwnedEnvironmentName(entry.Name) {
			t.Fatalf("reserved environment leaked: %#v", entry)
		}
		if strings.EqualFold(entry.Name, "WINDSHARE_ENVIRONMENT_CASE_TEST") {
			if foundOverride || entry.Name != "windshare_environment_case_test" || entry.Value != "new" {
				t.Fatalf("override = %#v", entry)
			}
			foundOverride = true
		}
	}
	if !foundOverride {
		t.Fatal("case-insensitive override was not present")
	}
	for _, invalid := range []map[string]string{{"": "value"}, {"A=B": "value"}, {"A": "x\x00y"}} {
		if _, err := InheritEnvironment(invalid); err == nil {
			t.Fatalf("invalid override accepted: %#v", invalid)
		}
	}
}

func TestInheritEnvironmentCanonicalizesFoldedInheritedAliases(t *testing.T) {
	environment, err := inheritEnvironment([]string{
		"zeta=last",
		"HTTPS_PROXY=http://proxy.example",
		"https_proxy=http://proxy.example",
		"Alpha=first",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.EnvironmentEntry{
		{Name: "Alpha", Value: "first"},
		{Name: "HTTPS_PROXY", Value: "http://proxy.example"},
		{Name: "zeta", Value: "last"},
	}
	if !slices.Equal(environment, want) {
		t.Fatalf("environment = %#v, want %#v", environment, want)
	}
	if _, err := inheritEnvironment([]string{"HTTPS_PROXY=one", "https_proxy=two"}, nil); err == nil {
		t.Fatal("conflicting inherited aliases were accepted")
	}
	if _, err := inheritEnvironment(nil, map[string]string{"ALPHA": "one", "alpha": "two"}); err == nil {
		t.Fatal("conflicting override aliases were accepted")
	}
}

func TestOwnedOutputCaptureIsBoundedAndSnapshotsOwnTheirBytes(t *testing.T) {
	capture := newBoundedOutput("stdout")
	if capture.captured != 0 || capture.chunks != nil {
		t.Fatal("idle output capture reserved stream storage")
	}
	prefix := bytes.Repeat([]byte{0x61}, MaximumCapturedOutputBytes-1)
	if written, err := capture.Write(prefix); err != nil || written != len(prefix) {
		t.Fatalf("write capture prefix: bytes=%d err=%v", written, err)
	}
	if written, err := capture.Write([]byte("bc")); err != nil || written != 2 {
		t.Fatalf("cross capture limit: bytes=%d err=%v", written, err)
	}
	if err := capture.terminalError(); !errors.Is(err, ErrOutputCaptureLimit) {
		t.Fatalf("capture terminal error = %v", err)
	}
	allocated := 0
	for _, chunk := range capture.chunks {
		allocated += cap(chunk)
	}
	if allocated != MaximumCapturedOutputBytes || len(capture.chunks) > MaximumCapturedOutputBytes/capturedOutputChunkBytes {
		t.Fatalf("capture storage = bytes=%d chunks=%d", allocated, len(capture.chunks))
	}
	first := capture.snapshot()
	if !first.Truncated || len(first.Bytes) != MaximumCapturedOutputBytes || first.Bytes[len(first.Bytes)-1] != 'b' {
		t.Fatalf("bounded snapshot: bytes=%d truncated=%t tail=%q", len(first.Bytes), first.Truncated, first.Bytes[len(first.Bytes)-1:])
	}
	first.Bytes[0] = 0
	if written, err := capture.Write([]byte("discarded")); err != nil || written != len("discarded") {
		t.Fatalf("drain after capture limit: bytes=%d err=%v", written, err)
	}
	second := capture.snapshot()
	if second.Bytes[0] != 'a' || !bytes.Equal(second.Bytes[1:], first.Bytes[1:]) {
		t.Fatal("caller mutation changed the owned capture")
	}
}

func TestProcessWaitStopAndContextCancellation(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	settlement := successfulSettlement(identity, 0)
	released := make(chan struct{})
	session := newFakeSession(settlement)
	process := newProcess(identity, session, func() { close(released) })
	waitContext, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	actual, err := process.Wait(waitContext)
	if err != nil || actual.Identity != identity {
		t.Fatalf("Wait = %#v, %v", actual, err)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("process release callback did not run")
	}
	if _, err := process.Stop(waitContext); err != nil {
		t.Fatal(err)
	}
	if session.stopCount() != 0 {
		t.Fatalf("Stop called a settled session %d times", session.stopCount())
	}

	blocking := newBlockingFakeSession(settlement)
	process = newProcess(identity, blocking, func() {})
	stopResult, err := process.Stop(waitContext)
	if err != nil || stopResult.Identity != identity || blocking.stopCount() != 1 {
		t.Fatalf("Stop = %#v, %v; calls=%d", stopResult, err, blocking.stopCount())
	}
	if _, err := process.Stop(waitContext); err != nil || blocking.stopCount() != 1 {
		t.Fatalf("idempotent Stop error=%v calls=%d", err, blocking.stopCount())
	}

	cancelled := newBlockingFakeSession(settlement)
	process = newProcess(identity, cancelled, func() {})
	ctx, cancel := context.WithCancel(context.Background())
	go process.stopWhenDone(ctx)
	cancel()
	select {
	case <-process.done:
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not settle process")
	}
	if cancelled.stopCount() != 1 {
		t.Fatalf("context stop calls = %d", cancelled.stopCount())
	}
}

func TestWaitCancellationPublishesLifecycleStop(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	session := newBlockingFakeSession(successfulSettlement(identity, 0))
	process := newProcess(identity, session, func() {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, waitErr := process.Wait(ctx)
	if waitErr != nil && !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("canceled Wait error = %v", waitErr)
	}
	select {
	case <-process.done:
	case <-time.After(time.Second):
		t.Fatal("canceled Wait left its owned process active")
	}
	if session.stopCount() != 1 {
		t.Fatalf("canceled Wait stop calls = %d", session.stopCount())
	}
}

func TestTerminalResultIsImmutableAfterStopJoinBudgetExpires(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "late-stop", Scenario: "immutable-result"}
	settlement := successfulSettlement(identity, 0)
	settlement.TerminationReason = protocol.TerminationStop
	lateErr := io.ErrUnexpectedEOF
	session := &lateStopSession{
		settlement:  settlement,
		waitRelease: make(chan struct{}),
		stopStarted: make(chan struct{}),
		stopRelease: make(chan struct{}),
		stopErr:     lateErr,
	}
	process := newProcess(identity, session, func() {})
	stopResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := process.Stop(ctx)
		stopResult <- err
	}()
	select {
	case <-session.stopStarted:
	case <-time.After(time.Second):
		t.Fatal("stop publication did not begin")
	}
	close(session.waitRelease)
	waitContext, cancelWait := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWait()
	firstSettlement, firstErr := process.Wait(waitContext)
	if firstSettlement.Identity != identity || firstErr == nil ||
		!strings.Contains(firstErr.Error(), "stop publication did not join") {
		t.Fatalf("sealed Wait = %#v, %v", firstSettlement, firstErr)
	}
	close(session.stopRelease)
	if stopErr := <-stopResult; !errors.Is(stopErr, lateErr) {
		t.Fatalf("late Stop error = %v", stopErr)
	}
	secondSettlement, secondErr := process.Wait(waitContext)
	if secondSettlement.Identity != identity || secondErr == nil || secondErr.Error() != firstErr.Error() {
		t.Fatalf("repeated Wait changed from %v to %v", firstErr, secondErr)
	}
}

func TestProcessWaitBindsSettlementToRequestInputAuthority(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	request := protocol.NewRequest(identity, protocol.Command{
		Executable: filepath.Join(t.TempDir(), "fixture"), WorkingDirectory: t.TempDir(),
		Arguments: []string{}, Environment: []protocol.EnvironmentEntry{},
	}, 1_000, 500)
	settlement := successfulSettlement(identity, 0)
	settlement.Input = protocol.InputEvidence{Outcome: protocol.InputDelivered}
	process := newProcessWithRequest(&request, identity, newFakeSession(settlement), func() {})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := process.Wait(ctx)
	if err == nil || !strings.Contains(err.Error(), "input evidence") {
		t.Fatalf("request-bound settlement validation error = %v", err)
	}
}

func TestEventReaderValidatesIdentityAndFraming(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	event := protocol.Event{
		SchemaVersion: protocol.EventSchemaVersion, Identity: identity,
		Component: "fixture", Milestone: "ready", Outcome: "succeeded",
	}
	var encoded bytes.Buffer
	if err := protocol.WriteLineDocument(&encoded, event); err != nil {
		t.Fatal(err)
	}
	reader := newEventReader(io.NopCloser(bytes.NewReader(encoded.Bytes())), identity)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	actual, err := reader.Next(ctx)
	if err != nil || actual.Component != event.Component {
		t.Fatalf("Next = %#v, %v", actual, err)
	}
	if _, err := reader.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Next error = %v", err)
	}

	mismatch := identity
	mismatch.OperationID = "different"
	reader = newEventReader(io.NopCloser(bytes.NewReader(encoded.Bytes())), mismatch)
	if _, err := reader.Next(ctx); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("identity mismatch error = %v", err)
	}
	reader = newEventReader(io.NopCloser(strings.NewReader("not-json\n")), identity)
	if _, err := reader.Next(ctx); err == nil || !strings.Contains(err.Error(), "validate test event") {
		t.Fatalf("framing error = %v", err)
	}
}

func TestEventReaderFailureKeepsDrainInTrackedLifecycle(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	source, writer := io.Pipe()
	reader := newEventReader(source, identity)
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, "not-json\n")
		written <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := reader.Next(ctx); err == nil || !strings.Contains(err.Error(), "validate test event") {
		t.Fatalf("malformed event error = %v", err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	select {
	case <-reader.Done():
		t.Fatal("reader lifecycle ended before its failed stream was drained")
	default:
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reader.Done():
	case <-ctx.Done():
		t.Fatal("reader did not finish its tracked drain")
	}
}

func TestEventReaderTerminalEvidenceBeatsCancellation(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	event := protocol.Event{
		SchemaVersion: protocol.EventSchemaVersion, Identity: identity,
		Component: "fixture", Milestone: "ready", Outcome: "succeeded",
	}
	var encoded bytes.Buffer
	if err := protocol.WriteLineDocument(&encoded, event); err != nil {
		t.Fatal(err)
	}
	reader := newEventReader(io.NopCloser(bytes.NewReader(encoded.Bytes())), identity)
	<-reader.Done()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	actual, err := reader.Next(ctx)
	if err != nil || actual.Identity != event.Identity || actual.Component != event.Component ||
		actual.Milestone != event.Milestone || actual.Outcome != event.Outcome {
		t.Fatalf("Next after simultaneous terminal cancellation = %#v, %v", actual, err)
	}
}

func TestEventReaderOverflowIsBoundedAndTerminal(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	event := protocol.Event{
		SchemaVersion: protocol.EventSchemaVersion, Identity: identity,
		Component: "fixture", Milestone: "ready", Outcome: "succeeded",
	}
	var encoded bytes.Buffer
	for range maximumPendingEvents + 1 {
		if err := protocol.WriteLineDocument(&encoded, event); err != nil {
			t.Fatal(err)
		}
	}
	reader := newEventReader(io.NopCloser(bytes.NewReader(encoded.Bytes())), identity)
	<-reader.Done()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for index := 0; index < maximumPendingEvents; index++ {
		if _, err := reader.Next(ctx); err != nil {
			t.Fatalf("event %d: %v", index, err)
		}
	}
	if _, err := reader.Next(ctx); err == nil || !strings.Contains(err.Error(), "bounded queue") {
		t.Fatalf("overflow terminal error = %v", err)
	}
}

func TestProcessWaitIncludesImmutableEventFailure(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	session := &eventFakeSession{
		fakeSession: newFakeSession(successfulSettlement(identity, 0)),
		source:      io.NopCloser(strings.NewReader("not-json\n")),
	}
	process := newProcess(identity, session, func() {})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	settlement, waitErr := process.Wait(ctx)
	if waitErr == nil || !strings.Contains(waitErr.Error(), "test event stream failed") {
		t.Fatalf("Wait event error = %v", waitErr)
	}
	if err := RequireSuccess(settlement, waitErr); err == nil {
		t.Fatal("RequireSuccess accepted a malformed event lifecycle")
	}
	*settlement.Target.ExitCode = 99
	settlement.Platform.Root.PID = 99
	again, againErr := process.Wait(ctx)
	if againErr == nil || *again.Target.ExitCode != 0 || again.Platform.Root.PID != 1 {
		t.Fatalf("second Wait = %#v, %v", again, againErr)
	}
}

func TestSettlementRequirementsSeparateCleanupFromProductSuccess(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	settlement := successfulSettlement(identity, 23)
	if err := RequireTreeEmpty(settlement); err != nil {
		t.Fatal(err)
	}
	if err := RequireSuccess(settlement, nil); err == nil {
		t.Fatal("nonzero product exit was accepted")
	}
	zero := int64(0)
	settlement.Target.ExitCode = &zero
	settlement.Platform.Root.ExitCode = &zero
	if err := RequireSuccess(settlement, nil); err != nil {
		t.Fatal(err)
	}
	settlement.TreeState = protocol.TreeNonempty
	settlement.Cleanup = protocol.CleanupEvidence{
		Outcome: protocol.CleanupFailed, FailureCode: "TREE_RETAINED", FailureMessage: "descendant remained",
	}
	settlement.OwnerFailure = &protocol.FailureEvidence{Code: "TREE_RETAINED", Message: "descendant remained"}
	if err := RequireTreeEmpty(settlement); err == nil {
		t.Fatal("failed cleanup was accepted")
	}
}

func TestOwnerRejectsInvalidLifecycle(t *testing.T) {
	if _, err := NewOwner("relative-helper"); err == nil {
		t.Fatal("relative helper was accepted")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := NewOwner(missing); err == nil {
		t.Fatal("missing helper was accepted")
	}
	if _, err := NewOwner(t.TempDir()); err == nil {
		t.Fatal("directory helper was accepted")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(executable)
	if err != nil {
		t.Fatal(err)
	}
	owner.active = 1
	if err := owner.Close(); err == nil {
		t.Fatal("busy owner was closed")
	}
	owner.active = 0
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = owner.Start(context.Background(), validSpec(t))
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed owner Start error = %v", err)
	}
}

func TestCleanupHelpersGateSettlement(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	session := newFakeSession(successfulSettlement(identity, 0))
	process := newProcess(identity, session, func() {})
	recorder := &cleanupRecorder{}
	RegisterCleanup(recorder, process, time.Second)
	for _, cleanup := range recorder.cleanups {
		cleanup()
	}
	if len(recorder.errors) != 0 {
		t.Fatalf("cleanup errors = %v", recorder.errors)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := process.StopAndRequireTreeEmpty(ctx); err != nil {
		t.Fatal(err)
	}
}

func validSpec(t *testing.T) Spec {
	t.Helper()
	root := t.TempDir()
	return Spec{
		Identity: protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"},
		Command: Command{
			Executable: filepath.Join(root, "fixture"), Arguments: []string{},
			WorkingDirectory: root, Environment: []protocol.EnvironmentEntry{},
		},
		Deadline: time.Second, TerminationGrace: time.Second,
	}
}

func successfulSettlement(identity protocol.Identity, exitCode int) protocol.Settlement {
	exactExitCode := int64(exitCode)
	active := uint32(0)
	return protocol.Settlement{
		SchemaVersion: protocol.SettlementSchemaVersion, Identity: identity,
		TerminationReason: protocol.TerminationNatural,
		Target:            protocol.TargetEvidence{Outcome: protocol.TargetExited, ExitCode: &exactExitCode},
		Input:             protocol.InputEvidence{Outcome: protocol.InputNotRequested},
		TreeState:         protocol.TreeProvenEmpty,
		Cleanup:           protocol.CleanupEvidence{Outcome: protocol.CleanupCompleted},
		Platform: protocol.PlatformEvidence{
			Kind: protocol.PlatformLinuxSubreaper, OwnerPID: os.Getpid(),
			Root: &protocol.RootEvidence{PID: 1, State: protocol.RootExited, ExitCode: &exactExitCode}, RootStartTimeTicks: "1",
			ActiveProcessCount: &active, InventoryScans: 2, QuietInventoryCount: 2,
		},
	}
}

type fakeSession struct {
	settlement protocol.Settlement
	release    chan struct{}
	stopOnce   sync.Once
	mu         sync.Mutex
	stops      int
}

type eventFakeSession struct {
	*fakeSession
	source io.ReadCloser
}

type lateStopSession struct {
	settlement  protocol.Settlement
	waitRelease chan struct{}
	stopStarted chan struct{}
	stopRelease chan struct{}
	stopErr     error
	stopOnce    sync.Once
}

func (session *lateStopSession) wait() (protocol.Settlement, error) {
	<-session.waitRelease
	return session.settlement, nil
}

func (session *lateStopSession) stop(protocol.Control) error {
	session.stopOnce.Do(func() { close(session.stopStarted) })
	<-session.stopRelease
	return session.stopErr
}

func (*lateStopSession) events() io.ReadCloser { return io.NopCloser(bytes.NewReader(nil)) }

func (*lateStopSession) close() error { return nil }

func (session *eventFakeSession) events() io.ReadCloser { return session.source }

func newFakeSession(settlement protocol.Settlement) *fakeSession {
	release := make(chan struct{})
	close(release)
	session := &fakeSession{settlement: settlement, release: release}
	session.stopOnce.Do(func() {})
	return session
}

func newBlockingFakeSession(settlement protocol.Settlement) *fakeSession {
	return &fakeSession{settlement: settlement, release: make(chan struct{})}
}

func (session *fakeSession) wait() (protocol.Settlement, error) {
	<-session.release
	return session.settlement, nil
}

func (session *fakeSession) stop(protocol.Control) error {
	session.mu.Lock()
	session.stops++
	session.mu.Unlock()
	session.stopOnce.Do(func() { close(session.release) })
	return nil
}

func (session *fakeSession) events() io.ReadCloser {
	return io.NopCloser(bytes.NewReader(nil))
}

func (session *fakeSession) close() error { return nil }

func (session *fakeSession) stopCount() int {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.stops
}

type cleanupRecorder struct {
	cleanups []func()
	errors   []string
}

func (*cleanupRecorder) Helper() {}

func (recorder *cleanupRecorder) Cleanup(cleanup func()) {
	recorder.cleanups = append(recorder.cleanups, cleanup)
}

func (recorder *cleanupRecorder) Errorf(format string, arguments ...any) {
	recorder.errors = append(recorder.errors, format)
}
