package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

type immediateSmallCatalog struct {
	share catalog.ShareInstance
	root  catalog.DirectoryID
}

func (source immediateSmallCatalog) OpenDirectoryPages(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if directory != source.root {
		return nil, errors.New("empty selection walked outside its synthetic root")
	}
	var generation catalog.DirectoryGeneration
	generation[0] = 1
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: source.share, DirectoryID: directory, Generation: generation, Terminal: true,
	}, catalog.PageCommitterFunc(func(catalog.PageCommitInput) (catalog.PageCommitment, error) {
		raw := make([]byte, catalog.PageCommitmentBytes)
		raw[0] = 1
		return catalog.NewPageCommitment(raw)
	}))
	if err != nil {
		return nil, err
	}
	return &immediateSmallCursor{page: page}, nil
}

type immediateSmallCursor struct {
	page catalog.CatalogPage
	done bool
}

func (cursor *immediateSmallCursor) Next(ctx context.Context) (catalog.CatalogPage, bool, error) {
	if err := ctx.Err(); err != nil {
		return catalog.CatalogPage{}, false, err
	}
	if cursor.done {
		return catalog.CatalogPage{}, false, nil
	}
	cursor.done = true
	return cursor.page, true, nil
}

func (*immediateSmallCursor) Close() error { return nil }

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

func TestClassifyTransferTerminationSeparatesNetworkAndLocalFailures(t *testing.T) {
	resource := mustCLIFault(transferfault.NewSession(
		transferfault.ScopeOutputPause, transferfault.SessionResourceBudget,
	))
	session := mustCLIFault(transferfault.NewSession(
		transferfault.ScopeSessionTerminal, transferfault.SessionTransport,
	))
	for name, test := range map[string]struct {
		fault                     transferfault.Fault
		runtimeErr, connectionErr error
		want                      int
	}{
		"local output":    {want: ExitFailure},
		"resource budget": {fault: resource, want: ExitFailure},
		"session failure": {fault: session, want: ExitNetwork},
		"runtime failure": {runtimeErr: errors.New("runtime closed"), want: ExitNetwork},
		"relay failure":   {connectionErr: errors.New("relay closed"), want: ExitNetwork},
	} {
		t.Run(name, func(t *testing.T) {
			if got := classifyTransferTermination(test.fault, test.runtimeErr, test.connectionErr); got != test.want {
				t.Fatalf("exit=%d want=%d", got, test.want)
			}
		})
	}
}

type immediateSmallOutputAuthority struct {
	session *immediateSmallOutputSession
}

type immediateSmallOutputSession struct {
	backend      transfer.OutputBackendID
	session      transfer.OutputSessionID
	capabilities transfer.OutputCapabilities
	scope        transfer.DirectoryAdmissionScope
}

func newImmediateSmallOutputAuthority(t *testing.T) *immediateSmallOutputAuthority {
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
	capabilities, err := transfer.NewOutputCapabilities(transfer.OutputCapabilities{
		Mode: transfer.OutputNativeTree, FileFailureIsolation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &immediateSmallOutputAuthority{session: &immediateSmallOutputSession{
		backend: backend, session: session, capabilities: capabilities,
	}}
}

func (authority *immediateSmallOutputAuthority) OpenOutput(
	_ context.Context,
	intent transfer.TransferIntent,
) (transfer.OutputSession, error) {
	scope, err := transfer.NewDirectoryAdmissionScope(intent)
	if err != nil {
		return nil, err
	}
	authority.session.scope = scope
	return authority.session, nil
}

func (output *immediateSmallOutputSession) BackendID() transfer.OutputBackendID {
	return output.backend
}
func (output *immediateSmallOutputSession) SessionID() transfer.OutputSessionID {
	return output.session
}
func (output *immediateSmallOutputSession) Capabilities() transfer.OutputCapabilities {
	return output.capabilities
}
func (output *immediateSmallOutputSession) AdmitDirectory(
	_ context.Context,
	directory transfer.OutputDirectory,
) (transfer.DirectoryAdmission, error) {
	// The scheduler treats the returned proof as the parent capability for every
	// descendant. Minting the same session-scoped proof keeps this empty-root
	// fixture faithful to the production output contract without exposing any
	// mutable authority to the test.
	secret := make([]byte, 32)
	secret[0] = 1
	return transfer.NewDirectoryAdmissionWithSecret(secret, output.scope, directory)
}
func (*immediateSmallOutputSession) FinalizeDirectory(
	_ context.Context,
	admission transfer.DirectoryAdmission,
) (transfer.DirectorySettlement, error) {
	return transfer.NewFinalizedDirectorySettlement(admission)
}
func (*immediateSmallOutputSession) BeginFile(
	context.Context,
	transfer.OutputFile,
) (transfer.FileStart, error) {
	return transfer.FileStart{}, errors.New("empty selection must not begin files")
}
func (*immediateSmallOutputSession) PauseJob(
	context.Context,
	transfer.JobPauseReason,
) (transfer.JobSettlement, error) {
	return transfer.NewJobSettlement(transfer.JobPaused)
}
func (*immediateSmallOutputSession) CompleteJob(
	context.Context,
	transfer.JobOutcome,
) (transfer.JobSettlement, error) {
	return transfer.NewJobSettlement(transfer.JobClosed)
}

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
	outputRoot, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewPathTransferIntent(
		share, root, rules, outputRoot, "cli-admission-test", transfer.OutputNativeTree,
	)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := transfer.NewTransferJobID()
	if err != nil {
		t.Fatal(err)
	}
	job, err := transfer.NewTransferJob(transfer.TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Intent: intent, JobID: jobID,
		Catalog: immediateSmallCatalog{share: share, root: root}, Revisions: immediateSmallRevisions{},
		Blocks: immediateSmallBlocks{}, Output: newImmediateSmallOutputAuthority(t),
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
