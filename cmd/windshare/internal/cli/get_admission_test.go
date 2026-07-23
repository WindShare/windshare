package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

type immediateSmallCatalog struct{}

func (immediateSmallCatalog) LoadDirectory(context.Context, catalog.DirectoryID) (catalog.DirectorySnapshot, error) {
	return catalog.DirectorySnapshot{}, errors.New("empty selection must not load catalog")
}

func (source immediateSmallCatalog) AcquireDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	snapshot, err := source.LoadDirectory(ctx, directory)
	return snapshot, func() {}, err
}

type immediateSmallRevisions struct{}

func (immediateSmallRevisions) OpenRevision(context.Context, catalog.FileID) (transfer.OpenedRevision, error) {
	return transfer.OpenedRevision{}, errors.New("empty selection must not open revisions")
}
func (immediateSmallRevisions) ReleaseRevision(context.Context, content.LeaseID) error { return nil }

type immediateSmallBlocks struct{}

func (immediateSmallBlocks) ReadRange(
	context.Context,
	content.LeaseID,
	content.FileRevisionDescriptor,
	content.Range,
	transfer.RangeSink,
) error {
	return errors.New("empty selection must not read blocks")
}

func TestClassifyTransferAbortSeparatesNetworkAndLocalFailures(t *testing.T) {
	localOutput := errors.New("output commit failed")
	resource := transfer.NewJobResourceBudgetError(transfer.ErrSelectionIdentityBudget)
	session := transfer.NewSessionFailure(errors.New("peer session ended"))
	for name, test := range map[string]struct {
		cause, runtimeErr, connectionErr error
		want                             int
	}{
		"local output":    {cause: localOutput, want: ExitFailure},
		"resource budget": {cause: resource, want: ExitFailure},
		"session failure": {cause: session, want: ExitNetwork},
		"runtime failure": {cause: localOutput, runtimeErr: errors.New("runtime closed"), want: ExitNetwork},
		"relay failure":   {cause: localOutput, connectionErr: errors.New("relay closed"), want: ExitNetwork},
	} {
		t.Run(name, func(t *testing.T) {
			if got := classifyTransferAbort(test.cause, test.runtimeErr, test.connectionErr); got != test.want {
				t.Fatalf("exit=%d want=%d", got, test.want)
			}
		})
	}
}

type immediateSmallOutput struct {
	backend transfer.OutputBackendID
	session transfer.OutputSessionID
}

func newImmediateSmallOutput(t *testing.T) immediateSmallOutput {
	t.Helper()
	backend, err := transfer.NewOutputBackendID("cli-admission-test")
	if err != nil {
		t.Fatal(err)
	}
	rawSession := make([]byte, transfer.OutputSessionIdentityBytes)
	rawSession[0] = 1
	session, err := transfer.OutputSessionIDFromBytes(rawSession)
	if err != nil {
		t.Fatal(err)
	}
	return immediateSmallOutput{backend: backend, session: session}
}

func (output immediateSmallOutput) BackendID() transfer.OutputBackendID { return output.backend }
func (output immediateSmallOutput) SessionID() transfer.OutputSessionID { return output.session }
func (immediateSmallOutput) Capabilities() transfer.OutputCapabilities {
	return transfer.OutputCapabilities{Mode: transfer.OutputNativeTree, FileFailureIsolation: true}
}
func (immediateSmallOutput) EnsureDirectory(context.Context, transfer.OutputDirectory) error {
	return nil
}
func (immediateSmallOutput) FinalizeDirectory(context.Context, transfer.OutputDirectory) error {
	return nil
}
func (immediateSmallOutput) BeginFile(
	context.Context,
	transfer.OutputFile,
) (transfer.FileTransaction, transfer.VerifiedDurableRanges, error) {
	return nil, transfer.VerifiedDurableRanges{}, errors.New("empty selection must not begin files")
}
func (immediateSmallOutput) FinishJob(context.Context, transfer.JobOutcome) error { return nil }
func (immediateSmallOutput) AbortJob(context.Context, error) error                { return nil }

type fakeReceiverAdmissionTimer struct {
	channel chan time.Time
	mu      sync.Mutex
	stopped bool
}

func newFakeReceiverAdmissionTimer() *fakeReceiverAdmissionTimer {
	return &fakeReceiverAdmissionTimer{channel: make(chan time.Time, 1)}
}

func (timer *fakeReceiverAdmissionTimer) C() <-chan time.Time { return timer.channel }
func (timer *fakeReceiverAdmissionTimer) Stop() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	wasActive := !timer.stopped
	timer.stopped = true
	return wasActive
}
func (timer *fakeReceiverAdmissionTimer) fire(at time.Time) { timer.channel <- at }

type fakeReceiverAdmissionClock struct {
	now   time.Time
	timer *fakeReceiverAdmissionTimer
	delay time.Duration
}

func (clock *fakeReceiverAdmissionClock) Now() time.Time { return clock.now }
func (clock *fakeReceiverAdmissionClock) NewTimer(delay time.Duration) receiverAdmissionTimer {
	clock.delay = delay
	clock.timer = newFakeReceiverAdmissionTimer()
	return clock.timer
}

type fakeReceiverContentSuspension struct {
	mu          sync.Mutex
	resumeCount int
	resumeError error
	resumeEvent chan struct{}
	resumeGate  <-chan struct{}
	resumeOnce  sync.Once
}

type receiverContentSuspensionFunc func() error

func (resume receiverContentSuspensionFunc) Resume() error { return resume() }

func newFakeReceiverContentSuspension() *fakeReceiverContentSuspension {
	return &fakeReceiverContentSuspension{resumeEvent: make(chan struct{})}
}

func (suspension *fakeReceiverContentSuspension) Resume() error {
	suspension.mu.Lock()
	suspension.resumeCount++
	err := suspension.resumeError
	gate := suspension.resumeGate
	suspension.mu.Unlock()
	suspension.resumeOnce.Do(func() { close(suspension.resumeEvent) })
	if gate != nil {
		<-gate
	}
	return err
}

func (suspension *fakeReceiverContentSuspension) count() int {
	suspension.mu.Lock()
	defer suspension.mu.Unlock()
	return suspension.resumeCount
}

func receiveReceiverAdmissionDecision(
	t *testing.T,
	admission *relayContentAdmission,
) receiverAdmissionDecision {
	t.Helper()
	select {
	case decision, ok := <-admission.Decision():
		if !ok {
			t.Fatal("admission closed before publishing its decision")
		}
		return decision
	case <-time.After(time.Second):
		t.Fatal("admission did not publish its decision")
		return receiverAdmissionDecision{}
	}
}

type inertReceiverBlockLane struct{}

func (inertReceiverBlockLane) FetchBlock(
	context.Context,
	transfer.BlockDemand,
) (records.BlockRecord, error) {
	return records.BlockRecord{}, errors.New("admission race must not fetch content")
}

func TestRelayContentAdmissionDeadlineDoesNotWaitForSelectionMeasurement(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 4, 0, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	admission, err := newRelayContentAdmission(downloadT0, clock, relay)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Close()

	selectionEntered := make(chan struct{})
	releaseSelection := make(chan struct{})
	selectionDone := make(chan error, 1)
	go func() {
		close(selectionEntered)
		<-releaseSelection
		selectionDone <- admission.ObserveSelection(transfer.SelectionSmall)
	}()
	<-selectionEntered
	clock.timer.fire(downloadT0.Add(receiverRelayAdmissionWindow))
	select {
	case <-relay.resumeEvent:
	case <-time.After(time.Second):
		t.Fatal("the relay deadline waited for blocked selection measurement")
	}
	if decision := receiveReceiverAdmissionDecision(t, admission); decision.Cause != nil {
		t.Fatalf("deadline admission decision=%v", decision.Cause)
	}
	close(releaseSelection)
	if err := <-selectionDone; err != nil {
		t.Fatal(err)
	}
	if clock.delay != receiverRelayAdmissionWindow {
		t.Fatalf("deadline delay=%v", clock.delay)
	}
	if resumed := relay.count(); resumed != 1 {
		t.Fatalf("relay resumed=%d times", resumed)
	}
}

func TestRunTransferJobObservesImmediateSmallWithoutSubscriptionRace(t *testing.T) {
	var share catalog.ShareInstance
	share[0] = 1
	var root catalog.DirectoryID
	root[0] = 2
	rules, err := transfer.NewSelectionRules(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := transfer.NewTransferJob(transfer.TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: immediateSmallCatalog{}, Revisions: immediateSmallRevisions{},
		Blocks: immediateSmallBlocks{}, Output: newImmediateSmallOutput(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Stderr: io.Discard}
	seenSmall := false
	result := app.runTransferJob(context.Background(), job, func(measure transfer.SelectionMeasure) {
		seenSmall = seenSmall || measure.Class() == transfer.SelectionSmall
	})
	if result.Outcome != transfer.JobSucceeded || !seenSmall {
		t.Fatalf("result=%+v saw immediate Small=%v", result, seenSmall)
	}
}

func TestRelayContentAdmissionPeerFailureBeforeDeadlineAdmitsImmediately(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 5, 0, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0.Add(2 * time.Second)}
	relay := newFakeReceiverContentSuspension()
	admission, err := newRelayContentAdmission(
		downloadT0, clock, relay,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Close()

	if err := admission.ObservePeer(receiverPeerFailed); err != nil {
		t.Fatal(err)
	}
	if decision := receiveReceiverAdmissionDecision(t, admission); decision.Cause != nil {
		t.Fatalf("attempt-local peer admission=%v", decision.Cause)
	}
	if clock.delay != 6*time.Second {
		t.Fatalf("remaining deadline=%v", clock.delay)
	}
	clock.timer.fire(downloadT0.Add(receiverRelayAdmissionWindow))
	<-admission.finished
	if resumed := relay.count(); resumed != 1 {
		t.Fatalf("relay resumed=%d times", resumed)
	}
}

func TestRelayContentAdmissionPolicySignalsAreExact(t *testing.T) {
	tests := []struct {
		name      string
		selection transfer.SelectionClass
		peer      receiverPeerSignal
		want      bool
	}{
		{name: "terminal small", selection: transfer.SelectionSmall, want: true},
		{name: "unfinished unknown", selection: transfer.SelectionUnknown},
		{name: "absorbing large", selection: transfer.SelectionLarge},
		{name: "peer ready", peer: receiverPeerReady},
		{name: "peer detached", peer: receiverPeerDetached, want: true},
		{name: "peer session fatal", peer: receiverPeerSessionFatal},
		{name: "peer runtime terminal", peer: receiverPeerRuntimeTerminal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			downloadT0 := time.Date(2026, 7, 18, 6, 0, 0, 0, time.UTC)
			clock := &fakeReceiverAdmissionClock{now: downloadT0}
			relay := newFakeReceiverContentSuspension()
			admission, err := newRelayContentAdmission(
				downloadT0, clock, relay,
			)
			if err != nil {
				t.Fatal(err)
			}
			if test.peer != 0 {
				err = admission.ObservePeer(test.peer)
			} else {
				err = admission.ObserveSelection(test.selection)
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.want {
				if decision := receiveReceiverAdmissionDecision(t, admission); decision.Cause != nil {
					t.Fatalf("admission decision=%v", decision.Cause)
				}
			}
			if resumed := relay.count(); (resumed == 1) != test.want {
				t.Fatalf("resume count=%d want=%v", resumed, test.want)
			}
			admission.Close()
		})
	}
}

func TestRelayContentAdmissionReportsAsynchronousResumeFailure(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 7, 0, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	resumeErr := errors.New("initial lane became stale")
	relay.resumeError = resumeErr
	admission, err := newRelayContentAdmission(downloadT0, clock, relay)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Close()
	clock.timer.fire(downloadT0.Add(receiverRelayAdmissionWindow))
	decision := receiveReceiverAdmissionDecision(t, admission)
	if !errors.Is(decision.Cause, resumeErr) {
		t.Fatalf("reported error=%v", decision.Cause)
	}
	if err := admission.ObservePeer(receiverPeerFailed); err != nil {
		t.Fatalf("peer signal replayed deadline-owned error=%v", err)
	}
	if err := admission.ObserveSelection(transfer.SelectionSmall); err != nil {
		t.Fatalf("selection signal replayed deadline-owned error=%v", err)
	}
	if resumed := relay.count(); resumed != 1 {
		t.Fatalf("deadline-owned failure resumed relay %d times", resumed)
	}
	if _, ok := <-admission.Decision(); ok {
		t.Fatal("deadline-owned failure published more than one decision")
	}
}

func TestRelayContentAdmissionRetainsFirstResumeFailure(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 7, 30, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	resumeErr := errors.New("relay suspension could not resume")
	relay.resumeError = resumeErr
	admission, err := newRelayContentAdmission(downloadT0, clock, relay)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Close()
	if err := admission.ObservePeer(receiverPeerFailed); err != nil {
		t.Fatal(err)
	}
	if decision := receiveReceiverAdmissionDecision(t, admission); !errors.Is(decision.Cause, resumeErr) {
		t.Fatalf("first resume failure=%v", decision.Cause)
	}
	clock.timer.fire(downloadT0.Add(receiverRelayAdmissionWindow))
	<-admission.finished
	if resumed := relay.count(); resumed != 1 {
		t.Fatalf("terminal resume failure retried %d times", resumed)
	}
	if !errors.Is(admission.Err(), resumeErr) {
		t.Fatalf("retained admission error=%v", admission.Err())
	}
}

func TestRelayContentAdmissionConcurrentFailureReportsOwningTransitionOnce(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 7, 45, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	resumeErr := errors.New("relay suspension lost its lane")
	resumeGate := make(chan struct{})
	relay.resumeError = resumeErr
	relay.resumeGate = resumeGate
	admission, err := newRelayContentAdmission(downloadT0, clock, relay)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Close()

	peerResult := make(chan error, 1)
	go func() {
		peerResult <- admission.ObservePeer(receiverPeerFailed)
	}()
	<-relay.resumeEvent

	const contenders = 32
	start := make(chan struct{})
	results := make(chan error, contenders)
	for range contenders {
		go func() {
			<-start
			results <- admission.ObservePeer(receiverPeerDetached)
		}()
	}
	clock.timer.fire(downloadT0.Add(receiverRelayAdmissionWindow))
	close(start)
	close(resumeGate)

	if err := <-peerResult; err != nil {
		t.Fatalf("owning peer signal=%v", err)
	}
	for range contenders {
		if err := <-results; err != nil {
			t.Fatalf("non-owning transition replayed error=%v", err)
		}
	}
	decision := receiveReceiverAdmissionDecision(t, admission)
	if !errors.Is(decision.Cause, resumeErr) {
		t.Fatalf("owning transition decision=%v", decision.Cause)
	}
	if _, ok := <-admission.Decision(); ok {
		t.Fatal("concurrent failure published more than one decision")
	}
	if resumed := relay.count(); resumed != 1 {
		t.Fatalf("concurrent failure resumed relay %d times", resumed)
	}
	if !errors.Is(admission.Err(), resumeErr) {
		t.Fatalf("retained admission error=%v", admission.Err())
	}
}

func TestRelayContentAdmissionResumeMayReenterWithoutDeadlock(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 7, 50, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	resumeErr := errors.New("reentrant relay resume failed")
	resumeDone := make(chan struct{})
	var admission *relayContentAdmission
	relay := receiverContentSuspensionFunc(func() error {
		defer close(resumeDone)
		_ = admission.Err()
		if err := admission.ObserveSelection(transfer.SelectionSmall); err != nil {
			return errors.Join(resumeErr, err)
		}
		if err := admission.ObservePeer(receiverPeerDetached); err != nil {
			return errors.Join(resumeErr, err)
		}
		admission.Close()
		return resumeErr
	})
	var err error
	admission, err = newRelayContentAdmission(downloadT0, clock, relay)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.ObserveSelection(transfer.SelectionSmall); err != nil {
		t.Fatal(err)
	}
	select {
	case <-resumeDone:
	case <-time.After(time.Second):
		t.Fatal("Resume reentry deadlocked admission")
	}
	admission.Wait()
	decision := receiveReceiverAdmissionDecision(t, admission)
	if !errors.Is(decision.Cause, resumeErr) || !errors.Is(admission.Err(), resumeErr) {
		t.Fatalf("reentrant decision=%v retained=%v", decision.Cause, admission.Err())
	}
	if decision.TerminalOwner != receiverAdmissionTerminalLifecycle {
		t.Fatalf("reentrant terminal owner=%s", decision.TerminalOwner)
	}
}

func TestRelayContentAdmissionCloseRevokesQueuedDecisionBeforeResume(t *testing.T) {
	for _, test := range []struct {
		name   string
		signal receiverPeerSignal
		owner  receiverAdmissionTerminalOwner
	}{
		{name: "session fatal", signal: receiverPeerSessionFatal, owner: receiverAdmissionTerminalPeerFatal},
		{name: "runtime terminal", signal: receiverPeerRuntimeTerminal, owner: receiverAdmissionTerminalRuntime},
	} {
		t.Run(test.name, func(t *testing.T) {
			downloadT0 := time.Date(2026, 7, 18, 7, 52, 0, 0, time.UTC)
			clock := &fakeReceiverAdmissionClock{now: downloadT0}
			relay := newFakeReceiverContentSuspension()
			claimGate := make(chan struct{})
			admission, err := newRelayContentAdmissionWithExecution(
				downloadT0,
				clock,
				relay,
				receiverAdmissionExecution{claimGate: claimGate},
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := admission.ObserveSelection(transfer.SelectionSmall); err != nil {
				t.Fatal(err)
			}
			workerDone := admission.decisionWorkerDone()
			if workerDone == nil {
				t.Fatal("queued admission has no owned worker")
			}

			if err := admission.ObservePeer(test.signal); err != nil {
				t.Fatal(err)
			}
			admission.Wait()
			if _, ok := <-admission.Decision(); ok {
				t.Fatal("terminal closure published a queued admission decision")
			}
			if resumed := relay.count(); resumed != 0 {
				t.Fatalf("terminal closure resumed relay %d time(s)", resumed)
			}
			<-workerDone
			if resumed := relay.count(); resumed != 0 {
				t.Fatalf("revoked queued worker resumed relay %d time(s)", resumed)
			}
			traces := admission.Traces()
			if len(traces) != 1 {
				t.Fatalf("authority traces=%v", traces)
			}
			trace := traces[0]
			if trace.Generation != 1 || trace.Trigger != receiverAdmissionTriggerSmall ||
				trace.TerminalOwner != test.owner || trace.Result != receiverAdmissionAuthorityRevoked {
				t.Fatalf("revocation trace=%+v", trace)
			}
		})
	}
}

func TestRelayContentAdmissionTerminalBeforeDecisionQueuesNoWork(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 7, 53, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	admission, err := newRelayContentAdmission(downloadT0, clock, relay)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.ObservePeer(receiverPeerSessionFatal); err != nil {
		t.Fatal(err)
	}
	if err := admission.ObserveSelection(transfer.SelectionSmall); err != nil {
		t.Fatal(err)
	}
	admission.Wait()
	if admission.decisionWorkerDone() != nil {
		t.Fatal("terminal admission created work after authority loss")
	}
	if resumed := relay.count(); resumed != 0 {
		t.Fatalf("terminal admission resumed relay %d time(s)", resumed)
	}
}

func TestRelayContentAdmissionTerminalRevokesQueuedDeadline(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 7, 54, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	claimGate := make(chan struct{})
	admission, err := newRelayContentAdmissionWithExecution(
		downloadT0,
		clock,
		relay,
		receiverAdmissionExecution{claimGate: claimGate},
	)
	if err != nil {
		t.Fatal(err)
	}
	clock.timer.fire(downloadT0.Add(receiverRelayAdmissionWindow))
	<-admission.finished
	workerDone := admission.decisionWorkerDone()
	if workerDone == nil {
		t.Fatal("deadline did not publish an owned decision worker")
	}

	if err := admission.ObservePeer(receiverPeerRuntimeTerminal); err != nil {
		t.Fatal(err)
	}
	admission.Wait()
	<-workerDone
	if resumed := relay.count(); resumed != 0 {
		t.Fatalf("revoked deadline worker resumed relay %d time(s)", resumed)
	}
	traces := admission.Traces()
	if len(traces) != 1 || traces[0].Trigger != receiverAdmissionTriggerDeadline ||
		traces[0].TerminalOwner != receiverAdmissionTerminalRuntime ||
		traces[0].Result != receiverAdmissionAuthorityRevoked {
		t.Fatalf("deadline revocation traces=%+v", traces)
	}
}

func TestRelayContentAdmissionCloseReturnsButWaitJoinsClaimedResume(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 7, 55, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	resumeGate := make(chan struct{})
	relay.resumeGate = resumeGate
	admission, err := newRelayContentAdmission(downloadT0, clock, relay)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.ObservePeer(receiverPeerFailed); err != nil {
		t.Fatal(err)
	}
	<-relay.resumeEvent

	closed := make(chan struct{})
	go func() {
		admission.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close waited for blocked Resume")
	}
	if err := admission.ObservePeer(receiverPeerDetached); err != nil {
		t.Fatal(err)
	}
	select {
	case <-admission.Decision():
		t.Fatal("Close published a partial deciding result")
	default:
	}
	waited := make(chan struct{})
	go func() {
		admission.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("Wait returned before the claimed Resume completed")
	default:
	}
	close(resumeGate)
	<-waited
	if decision := receiveReceiverAdmissionDecision(t, admission); decision.Cause != nil ||
		decision.TerminalOwner != receiverAdmissionTerminalLifecycle {
		t.Fatalf("completed blocked decision=%+v", decision)
	}
	if resumed := relay.count(); resumed != 1 {
		t.Fatalf("blocked Resume calls=%d", resumed)
	}
	traces := admission.Traces()
	if len(traces) != 3 || traces[0].Result != receiverAdmissionAuthorityClaimed ||
		traces[1].Result != receiverAdmissionAuthorityExecutionRetained ||
		traces[2].Result != receiverAdmissionAuthoritySettled {
		t.Fatalf("claimed-close authority traces=%+v", traces)
	}
}

func TestRelayContentAdmissionHighContentionPublishesOneRevocableCapability(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 7, 56, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	claimGate := make(chan struct{})
	admission, err := newRelayContentAdmissionWithExecution(
		downloadT0,
		clock,
		relay,
		receiverAdmissionExecution{claimGate: claimGate},
	)
	if err != nil {
		t.Fatal(err)
	}

	const contenders = 128
	start := make(chan struct{})
	var contendersDone sync.WaitGroup
	contendersDone.Add(contenders)
	for contender := range contenders {
		go func(index int) {
			defer contendersDone.Done()
			<-start
			if index%2 == 0 {
				_ = admission.ObserveSelection(transfer.SelectionSmall)
				return
			}
			_ = admission.ObservePeer(receiverPeerFailed)
		}(contender)
	}
	close(start)
	contendersDone.Wait()
	workerDone := admission.decisionWorkerDone()
	if workerDone == nil {
		t.Fatal("contention did not publish a decision capability")
	}

	terminalStart := make(chan struct{})
	var terminalDone sync.WaitGroup
	terminalDone.Add(contenders)
	for contender := range contenders {
		go func(index int) {
			defer terminalDone.Done()
			<-terminalStart
			if index%2 == 0 {
				_ = admission.ObservePeer(receiverPeerRuntimeTerminal)
				return
			}
			_ = admission.ObservePeer(receiverPeerSessionFatal)
		}(contender)
	}
	close(terminalStart)
	terminalDone.Wait()
	admission.Wait()
	<-workerDone
	if resumed := relay.count(); resumed != 0 {
		t.Fatalf("revoked contended capability resumed relay %d time(s)", resumed)
	}
	traces := admission.Traces()
	if len(traces) != 1 || traces[0].Generation != 1 ||
		traces[0].Result != receiverAdmissionAuthorityRevoked {
		t.Fatalf("contended authority traces=%+v", traces)
	}
}

func TestRelayContentAdmissionContainsResumePanic(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 7, 57, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	admission, err := newRelayContentAdmission(
		downloadT0,
		clock,
		receiverContentSuspensionFunc(func() error { panic("injected resume panic") }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Close()
	if err := admission.ObserveSelection(transfer.SelectionSmall); err != nil {
		t.Fatal(err)
	}
	decision := receiveReceiverAdmissionDecision(t, admission)
	if !errors.Is(decision.Cause, errReceiverAdmissionResumePanics) ||
		!errors.Is(admission.Err(), errReceiverAdmissionResumePanics) ||
		decision.TerminalOwner != receiverAdmissionTerminalResumeFailed {
		t.Fatalf("panic decision=%v retained=%v", decision.Cause, admission.Err())
	}
	traces := admission.Traces()
	if len(traces) != 2 || traces[0].Result != receiverAdmissionAuthorityClaimed ||
		traces[1].Result != receiverAdmissionAuthorityResumeFailed {
		t.Fatalf("panic authority traces=%+v", traces)
	}
}

func TestReceiverAdmissionTraceIsCorrelatedAndTextFree(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 7, 58, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	relay.resumeError = errors.New("secret injected relay failure")
	admission, err := newRelayContentAdmission(downloadT0, clock, relay)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Close()
	if err := admission.ObservePeer(receiverPeerFailed); err != nil {
		t.Fatal(err)
	}
	admission.Wait()

	var stderr bytes.Buffer
	(&App{Stderr: &stderr}).logReceiverAdmissionTraces([]byte{0x01, 0x02}, admission)
	diagnostic := stderr.String()
	for _, expected := range []string{
		"session_id=0102",
		"admission_generation=1",
		"terminal_owner=resume_failure",
		"result=resume_failed",
	} {
		if !strings.Contains(diagnostic, expected) {
			t.Fatalf("authority diagnostic=%q, want %q", diagnostic, expected)
		}
	}
	if strings.Contains(diagnostic, "secret injected relay failure") {
		t.Fatalf("authority diagnostic exposed error text: %q", diagnostic)
	}
}

func TestReceiverAdmissionMonitorConsumesFailureBeforeJoinReturns(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 7, 59, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	relay.resumeError = errors.New("monitor-owned relay resume failure")
	admission, err := newRelayContentAdmission(downloadT0, clock, relay)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	app := &App{Stderr: &stderr}
	monitorDone := app.monitorReceiverAdmission(admission, nil)
	if err := admission.ObservePeer(receiverPeerFailed); err != nil {
		t.Fatal(err)
	}
	admission.Wait()
	<-monitorDone
	admission.Close()
	if count := strings.Count(stderr.String(), "cause_class=relay_resume"); count != 1 {
		t.Fatalf("admission failure logs=%d stderr=%q", count, stderr.String())
	}
}

func TestReceiverAdmissionMonitorSuppressesFailureAfterRuntimeTerminal(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 7, 59, 30, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	resumeErr := errors.New("late relay resume failure")
	resumeGate := make(chan struct{})
	relay.resumeError = resumeErr
	relay.resumeGate = resumeGate
	admission, err := newRelayContentAdmission(downloadT0, clock, relay)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	app := &App{Stderr: &stderr}
	monitorDone := app.monitorReceiverAdmission(admission, nil)
	if err := admission.ObservePeer(receiverPeerFailed); err != nil {
		t.Fatal(err)
	}
	<-relay.resumeEvent
	if err := admission.ObservePeer(receiverPeerRuntimeTerminal); err != nil {
		t.Fatal(err)
	}
	close(resumeGate)
	admission.Wait()
	<-monitorDone
	if !errors.Is(admission.Err(), resumeErr) {
		t.Fatalf("suppressed admission error=%v", admission.Err())
	}
	if stderr.Len() != 0 {
		t.Fatalf("runtime terminal replayed admission failure: %q", stderr.String())
	}
}

func TestRelayContentAdmissionFatalDisarmsDeadline(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	admission, err := newRelayContentAdmission(
		downloadT0, clock, relay,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.ObservePeer(receiverPeerSessionFatal); err != nil {
		t.Fatal(err)
	}
	select {
	case <-admission.finished:
	default:
		t.Fatal("fatal peer signal returned before the deadline worker stopped")
	}
	clock.timer.mu.Lock()
	stopped := clock.timer.stopped
	clock.timer.mu.Unlock()
	if !stopped {
		t.Fatal("fatal peer signal left the deadline timer armed")
	}
	clock.timer.fire(downloadT0.Add(receiverRelayAdmissionWindow))
	if resumed := relay.count(); resumed != 0 {
		t.Fatalf("fatal peer signal admitted relay %d time(s)", resumed)
	}
}

func TestRelayContentAdmissionRuntimeTerminalDisarmsWithoutResume(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 8, 30, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	admission, err := newRelayContentAdmission(downloadT0, clock, relay)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.ObservePeer(receiverPeerRuntimeTerminal); err != nil {
		t.Fatal(err)
	}
	select {
	case <-admission.finished:
	default:
		t.Fatal("runtime-terminal signal returned before the deadline worker stopped")
	}
	clock.timer.fire(downloadT0.Add(receiverRelayAdmissionWindow))
	if resumed := relay.count(); resumed != 0 {
		t.Fatalf("runtime-terminal signal admitted relay %d time(s)", resumed)
	}
}

func TestRelayContentAdmissionDeadlineAndPeerFailureResumeExactlyOnce(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	relay := newFakeReceiverContentSuspension()
	admission, err := newRelayContentAdmission(
		downloadT0, clock, relay,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Close()
	start := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-start
		done <- admission.ObservePeer(receiverPeerFailed)
	}()
	close(start)
	clock.timer.fire(downloadT0.Add(receiverRelayAdmissionWindow))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-admission.finished
	if decision := receiveReceiverAdmissionDecision(t, admission); decision.Cause != nil {
		t.Fatalf("deadline/failure admission=%v", decision.Cause)
	}
	if resumed := relay.count(); resumed != 1 {
		t.Fatalf("deadline/failure race resumed relay %d times", resumed)
	}
}

func TestRelayContentAdmissionPeerFailureSurvivesRelayEpochReplacement(t *testing.T) {
	for attempt := range 100 {
		var sessionID protocolsession.ProtocolSessionID
		sessionID[0] = byte(attempt + 1)
		lanes, err := transfer.NewLaneSet(transfer.LaneSetConfig{ProtocolSessionID: sessionID})
		if err != nil {
			t.Fatal(err)
		}
		initial := transfer.LaneIdentity{ID: 1, Epoch: 1}
		if err := lanes.Add(initial, inertReceiverBlockLane{}); err != nil {
			lanes.Close()
			t.Fatal(err)
		}
		relay, err := lanes.SuspendContent(initial)
		if err != nil {
			lanes.Close()
			t.Fatal(err)
		}
		downloadT0 := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
		clock := &fakeReceiverAdmissionClock{now: downloadT0}
		admission, err := newRelayContentAdmission(downloadT0, clock, relay)
		if err != nil {
			lanes.Close()
			t.Fatal(err)
		}

		start := make(chan struct{})
		replaced := make(chan error, 1)
		admitted := make(chan error, 1)
		go func() {
			<-start
			replaced <- lanes.Add(transfer.LaneIdentity{ID: initial.ID, Epoch: initial.Epoch + 1}, inertReceiverBlockLane{})
		}()
		go func() {
			<-start
			admitted <- admission.ObservePeer(receiverPeerFailed)
		}()
		close(start)
		if err := <-replaced; err != nil {
			t.Fatalf("attempt %d replace relay: %v", attempt, err)
		}
		if err := <-admitted; err != nil {
			t.Fatalf("attempt %d peer failure would close runtime: %v", attempt, err)
		}
		if decision := receiveReceiverAdmissionDecision(t, admission); decision.Cause != nil {
			t.Fatalf("attempt %d admission decision: %v", attempt, decision.Cause)
		}
		admission.Close()
		lanes.Close()
	}
}
