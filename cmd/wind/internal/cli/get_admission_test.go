package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
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

type immediateSmallOutputAuthority struct {
	session *immediateSmallOutputSession
}

type immediateSmallOutputSession struct {
	session      transfer.OutputSessionID
	capabilities transfer.DirectTreeCapabilities
	scope        transfer.DirectoryAdmissionScope
	binding      transfer.DirectTreeSessionBinding
}

func newImmediateSmallOutputAuthority(t *testing.T) *immediateSmallOutputAuthority {
	t.Helper()
	rawSession := make([]byte, transfer.OutputSessionIdentityBytes)
	rawSession[0] = 1
	session, err := transfer.OutputSessionIDFromBytes(rawSession)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := transfer.NewDirectTreeCapabilities(transfer.DirectTreeCapabilities{
		Durability: transfer.DurabilityPowerLoss, RandomWrite: true, FileFailureIsolation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &immediateSmallOutputAuthority{session: &immediateSmallOutputSession{
		session: session, capabilities: capabilities,
	}}
}

func (authority *immediateSmallOutputAuthority) OpenDirectTree(
	_ context.Context,
	intent transfer.ReceiveIntent,
) (transfer.DirectTreeSession, error) {
	scope, err := transfer.NewDirectoryAdmissionScope(intent)
	if err != nil {
		return nil, err
	}
	binding, err := transfer.BindDirectTreeSession(intent)
	if err != nil {
		return nil, err
	}
	authority.session.scope = scope
	authority.session.binding = binding
	return authority.session, nil
}

func (output *immediateSmallOutputSession) SessionID() transfer.OutputSessionID {
	return output.session
}
func (output *immediateSmallOutputSession) Binding() transfer.DirectTreeSessionBinding {
	return output.binding
}
func (output *immediateSmallOutputSession) Capabilities() transfer.DirectTreeCapabilities {
	return output.capabilities
}
func (output *immediateSmallOutputSession) AdmitDirectory(
	_ context.Context,
	request transfer.DirectoryMaterializationRequest,
) (transfer.DirectoryAdmission, error) {
	// The scheduler treats the returned proof as the parent capability for every
	// descendant. Minting the same session-scoped proof keeps this empty-root
	// fixture faithful to the production output contract without exposing any
	// mutable authority to the test.
	secret := make([]byte, 32)
	secret[0] = 1
	return transfer.NewDirectoryAdmissionWithSecret(
		secret, output.scope, request.Source(),
	)
}
func (*immediateSmallOutputSession) FinalizeDirectory(
	_ context.Context,
	admission transfer.DirectoryAdmission,
) (transfer.DirectorySettlement, error) {
	return transfer.NewFinalizedDirectorySettlement(admission)
}
func (*immediateSmallOutputSession) BeginFile(
	context.Context,
	transfer.MaterializationFile,
) (transfer.FileStart, error) {
	return transfer.FileStart{}, errors.New("empty selection must not begin files")
}
func (*immediateSmallOutputSession) PauseTree(
	context.Context,
	transfer.JobPauseReason,
) (transfer.DirectTreeSettlement, error) {
	return transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementPaused)
}
func (*immediateSmallOutputSession) FinalizeTree(
	_ context.Context,
	outcome transfer.DirectTreeOutcome,
) (transfer.DirectTreeSettlement, error) {
	kind := transfer.DirectTreeSettlementSuccess
	if outcome == transfer.DirectTreeOutcomePartial {
		kind = transfer.DirectTreeSettlementPartial
	}
	return transfer.NewDirectTreeSettlement(kind)
}

func newCLIAdmissionDirectTreeIntent(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules transfer.SelectionRules,
) transfer.ReceiveIntent {
	t.Helper()
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	identityMaterial := append(share.Bytes(), root.Bytes()...)
	operationDigest := sha256.Sum256(append([]byte("cli/admission-test-operation/v1\x00"), identityMaterial...))
	operation, err := receivecontract.OperationIDFromBytes(operationDigest[:receivecontract.StableIdentityBytes])
	if err != nil {
		t.Fatal(err)
	}
	reservationDigest := sha256.Sum256(append([]byte("cli/admission-test-reservation/v1\x00"), identityMaterial...))
	reservation, err := receivecontract.DestinationReservationIDFromBytes(
		reservationDigest[:receivecontract.StableIdentityBytes],
	)
	if err != nil {
		t.Fatal(err)
	}
	authorityDigest := sha256.Sum256(append([]byte("cli/admission-test-authority/v1\x00"), identityMaterial...))
	authority, err := receivecontract.AuthorityRefFromBytes(authorityDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	rootReservation, err := receivecontract.NewNativeContainerRootReservation(
		operation, reservation, artifact, authority,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, rootReservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	return intent
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

func TestRelayContentAdmissionPublishesPathDecisionBeforeResume(t *testing.T) {
	downloadT0 := time.Date(2026, 7, 18, 3, 30, 0, 0, time.UTC)
	clock := &fakeReceiverAdmissionClock{now: downloadT0}
	events := make(chan string, 2)
	admission, err := newRelayContentAdmissionWithExecution(
		downloadT0,
		clock,
		receiverContentSuspensionFunc(func() error {
			events <- "resume"
			return nil
		}),
		receiverAdmissionExecution{onClaim: func(trigger receiverAdmissionTrigger) {
			if trigger != receiverAdmissionTriggerConnectionSmall {
				t.Errorf("claim trigger=%s", trigger)
			}
			events <- "path"
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Close()
	if err := admission.ObserveConnectionSize(transfer.ConnectionSizeSmall); err != nil {
		t.Fatal(err)
	}
	if decision := receiveReceiverAdmissionDecision(t, admission); decision.Cause != nil {
		t.Fatal(decision.Cause)
	}
	if first, second := <-events, <-events; first != "path" || second != "resume" {
		t.Fatalf("admission events=%q,%q", first, second)
	}
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
		selectionDone <- admission.ObserveConnectionSize(transfer.ConnectionSizeSmall)
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
	intent := newCLIAdmissionDirectTreeIntent(t, share, root, rules)
	jobID, err := transfer.NewTransferJobID()
	if err != nil {
		t.Fatal(err)
	}
	job, err := transfer.NewTransferJob(transfer.TransferJobConfig{
		ReceiveIntent: intent, JobID: jobID,
		Catalog: immediateSmallCatalog{share: share, root: root}, Revisions: immediateSmallRevisions{},
		Blocks: immediateSmallBlocks{}, Materializer: newImmediateSmallOutputAuthority(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, _ := newGetReportingRuntime(t, false, false)
	observation := getObservation{runtime: runtime}
	seenSmall := false
	completion := (&App{}).runTransferJob(context.Background(), job, runtime.Clock(), observation, func(progress transfer.ReceiveProgressSnapshot) {
		seenSmall = seenSmall || progress.ConnectionSizeClass() == transfer.ConnectionSizeSmall
	})
	runtime.Close()
	if completion.result.Outcome != transfer.DirectTreeOutcomeSuccess || !seenSmall {
		t.Fatalf("result=%+v saw immediate Small=%v", completion.result, seenSmall)
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
		name           string
		connectionSize transfer.ConnectionSizeClass
		peer           receiverPeerSignal
		want           bool
	}{
		{name: "terminal small", connectionSize: transfer.ConnectionSizeSmall, want: true},
		{name: "unfinished unknown", connectionSize: transfer.ConnectionSizeUnknown},
		{name: "absorbing large", connectionSize: transfer.ConnectionSizeLarge},
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
				err = admission.ObserveConnectionSize(test.connectionSize)
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
