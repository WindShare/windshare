package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type jobPageCommitter struct{}

func (jobPageCommitter) Commit(input catalog.PageCommitInput) (catalog.PageCommitment, error) {
	var commitment catalog.PageCommitment
	commitment[0] = input.DirectoryID.Bytes()[0]
	commitment[1] = byte(input.PageIndex + 1)
	commitment[2] = byte(len(input.Entries) + 1)
	return commitment, nil
}

func jobSnapshot(t *testing.T, share catalog.ShareInstance, directory catalog.DirectoryID, generation byte, entries ...catalog.Entry) catalog.DirectorySnapshot {
	t.Helper()
	slices.SortFunc(entries, func(left, right catalog.Entry) int {
		if left.Name() < right.Name() {
			return -1
		}
		if left.Name() > right.Name() {
			return 1
		}
		return 0
	})
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: transferID[catalog.DirectoryGeneration](generation),
		Entries: entries, Terminal: true,
	}, jobPageCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.NewDirectorySnapshot([]catalog.CatalogPage{page})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func jobSnapshotWithOmissions(
	t *testing.T,
	share catalog.ShareInstance,
	directory catalog.DirectoryID,
	generation byte,
	omitted uint64,
	entries ...catalog.Entry,
) catalog.DirectorySnapshot {
	t.Helper()
	slices.SortFunc(entries, func(left, right catalog.Entry) int {
		return strings.Compare(left.Name(), right.Name())
	})
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory,
		Generation: transferID[catalog.DirectoryGeneration](generation),
		Entries:    entries, Terminal: true, OmittedCount: omitted,
	}, jobPageCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.NewDirectorySnapshot([]catalog.CatalogPage{page})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

type snapshotPageCursor struct {
	snapshot catalog.DirectorySnapshot
	index    uint32
	closed   bool
}

func (cursor *snapshotPageCursor) Next(context.Context) (catalog.CatalogPage, bool, error) {
	if cursor.closed {
		return catalog.CatalogPage{}, false, errors.New("test catalog cursor is closed")
	}
	page, ok := cursor.snapshot.Page(cursor.index)
	if !ok {
		return catalog.CatalogPage{}, false, nil
	}
	cursor.index++
	return page, true, nil
}

func (cursor *snapshotPageCursor) Close() error {
	cursor.closed = true
	return nil
}

func snapshotPages(snapshot catalog.DirectorySnapshot) catalog.DirectoryPageCursor {
	return &snapshotPageCursor{snapshot: snapshot}
}

type jobCatalogWire struct {
	snapshots map[catalog.DirectoryID]catalog.DirectorySnapshot
	objects   map[string]catalog.CatalogPage
	mu        sync.Mutex
	loads     []catalog.DirectoryID
}

func newJobCatalogClient(t *testing.T, share catalog.ShareInstance, snapshots ...catalog.DirectorySnapshot) (*catalogflow.Client, *jobCatalogWire) {
	t.Helper()
	wire := &jobCatalogWire{snapshots: make(map[catalog.DirectoryID]catalog.DirectorySnapshot), objects: make(map[string]catalog.CatalogPage)}
	for _, snapshot := range snapshots {
		wire.snapshots[snapshot.DirectoryID()] = snapshot
		for _, page := range snapshot.Pages() {
			wire.objects[jobObjectKey(page.DirectoryID(), page.PageIndex())] = page
		}
	}
	client, err := catalogflow.NewClient(catalogflow.ClientConfig{
		ShareInstance: share, Transport: wire, Verifier: wire, MaxCacheBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, wire
}

func jobObjectKey(directory catalog.DirectoryID, page uint32) string {
	return fmt.Sprintf("%x/%d", directory, page)
}

func (w *jobCatalogWire) FetchPage(_ context.Context, request catalogflow.ListRequest) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.loads = append(w.loads, request.DirectoryID())
	if w.snapshots[request.DirectoryID()].PageCount() == 0 {
		return nil, errors.New("directory unavailable")
	}
	return []byte(jobObjectKey(request.DirectoryID(), request.PageIndex())), nil
}

func (w *jobCatalogWire) Verify(_ context.Context, _ catalog.ShareInstance, _ catalogflow.ListRequest, object []byte) (catalogflow.VerifiedObject, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	page, ok := w.objects[string(object)]
	if !ok {
		return catalogflow.VerifiedObject{}, errors.New("unknown catalog object")
	}
	return catalogflow.VerifiedPage(page), nil
}

type jobRevisionClient struct {
	mu         sync.Mutex
	opened     map[catalog.FileID]OpenedRevision
	failures   map[catalog.FileID]error
	order      []catalog.FileID
	released   []content.LeaseID
	releaseErr error
	openHook   func()
}

func (c *jobRevisionClient) OpenRevision(_ context.Context, file catalog.FileID) (OpenedRevision, error) {
	c.mu.Lock()
	c.order = append(c.order, file)
	failure := c.failures[file]
	opened := c.opened[file]
	hook := c.openHook
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	if failure != nil {
		return OpenedRevision{}, failure
	}
	return opened, nil
}

func (c *jobRevisionClient) ReleaseRevision(_ context.Context, lease content.LeaseID) error {
	c.mu.Lock()
	c.released = append(c.released, lease)
	c.mu.Unlock()
	return c.releaseErr
}

type jobLane struct {
	mu       sync.Mutex
	indices  map[catalog.FileID][]uint64
	failFile catalog.FileID
	failErr  error
}

func (l *jobLane) FetchBlock(_ context.Context, demand BlockDemand) (records.BlockRecord, error) {
	file := demand.Descriptor.FileID()
	l.mu.Lock()
	l.indices[file] = append(l.indices[file], demand.Index)
	fail := file == l.failFile
	l.mu.Unlock()
	if fail {
		return records.BlockRecord{}, l.failErr
	}
	length, err := demand.Descriptor.Geometry().BlockPlainLength(demand.Index)
	if err != nil {
		return records.BlockRecord{}, err
	}
	return records.NewBlockRecord(demand.Descriptor, demand.Index, bytes.Repeat([]byte{byte(demand.Index + 1)}, int(length)))
}

func jobDescriptor(t *testing.T, share catalog.ShareInstance, file catalog.FileID, revision byte, size uint64) content.FileRevisionDescriptor {
	t.Helper()
	geometry, err := content.NewFileGeometry(size, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		share, file, transferID[content.FileRevision](revision), geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func jobEntry(t *testing.T, file catalog.FileID, name string, size uint64) catalog.Entry {
	t.Helper()
	entry, err := catalog.NewFileEntry(file, name, size, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func jobDirectoryEntry(t *testing.T, directory catalog.DirectoryID, name string) catalog.Entry {
	t.Helper()
	entry, err := catalog.NewDirectoryEntry(directory, name, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

const jobMaterializationDirectorySecretDomain = "windshare/test-job-output/directory-secret/v1"

var jobDirectTreeSessionNonce atomic.Uint64

func testReceiveIntent(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules SelectionRules,
) ReceiveIntent {
	t.Helper()
	selection, err := NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	identityMaterial := append(share.Bytes(), root.Bytes()...)
	operationDigest := sha256.Sum256(append([]byte("windshare/test-operation/v1\x00"), identityMaterial...))
	reservationDigest := sha256.Sum256(append([]byte("windshare/test-reservation/v1\x00"), identityMaterial...))
	authorityDigest := sha256.Sum256(append([]byte("windshare/test-authority/v1\x00"), identityMaterial...))
	operation, err := receivecontract.OperationIDFromBytes(operationDigest[:receivecontract.StableIdentityBytes])
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(
		reservationDigest[:receivecontract.StableIdentityBytes],
	)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := receivecontract.AuthorityRefFromBytes(authorityDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeContainerRootReservation(
		operation, reservationID, artifact, authority,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

type testTransferJobConfig struct {
	ShareInstance         catalog.ShareInstance
	SyntheticRoot         catalog.DirectoryID
	Rules                 SelectionRules
	ReceiveIntent         ReceiveIntent
	JobID                 TransferJobID
	ProtocolSessionID     protocolsession.ProtocolSessionID
	FileQueueCapacity     int
	GenerationReplayPages int
	Catalog               CatalogReader
	Revisions             RevisionClient
	Blocks                RangeReader
	Materializer          DirectTreeMaterializer
	SettlementTimeout     time.Duration
	Tracer                TransferLifecycleTracer
}

func (config testTransferJobConfig) production() TransferJobConfig {
	return TransferJobConfig{
		ReceiveIntent:         config.ReceiveIntent,
		JobID:                 config.JobID,
		ProtocolSessionID:     config.ProtocolSessionID,
		FileQueueCapacity:     config.FileQueueCapacity,
		GenerationReplayPages: config.GenerationReplayPages,
		Catalog:               config.Catalog,
		Revisions:             config.Revisions,
		Blocks:                config.Blocks,
		Materializer:          config.Materializer,
		SettlementTimeout:     config.SettlementTimeout,
		Tracer:                config.Tracer,
	}
}

func newTestTransferJob(t *testing.T, config testTransferJobConfig) (*TransferJob, error) {
	t.Helper()
	if config.ReceiveIntent.IsZero() {
		config.ReceiveIntent = testReceiveIntent(t, config.ShareInstance, config.SyntheticRoot, config.Rules)
	}
	if config.JobID.IsZero() {
		digest := config.ReceiveIntent.Digest()
		jobID, err := TransferJobIDFromBytes(digest[:TransferJobIdentityBytes])
		if err != nil {
			t.Fatal(err)
		}
		config.JobID = jobID
	}
	return NewTransferJob(config.production())
}

type jobOutput struct {
	mu                       sync.Mutex
	share                    catalog.ShareInstance
	session                  OutputSessionID
	durable                  map[string]content.RangeSet
	transactions             map[string]*jobFileTransaction
	immediate                map[string]FileSettlement
	directories              []string
	directoryAdmissions      []DirectoryAdmission
	finalized                []string
	finalizedAdmissions      []DirectoryAdmission
	finished                 DirectTreeOutcome
	aborted                  bool
	ensureErr                error
	admitErr                 error
	returnSessionOnOpenError bool
	ensureFailures           map[string]error
	finalizeErr              error
	beginErr                 error
	finishErr                error
	abortErr                 error
	nilTransaction           bool
	transactionScript        jobTransactionScript
	transactionScripts       map[string]jobTransactionScript
	capabilitiesOverride     *DirectTreeCapabilities
	beginHook                func(MaterializationFile)
	intent                   ReceiveIntent
	binding                  DirectTreeSessionBinding
	events                   []string
	pauseCalls               int
	completeCalls            int
	completeSettlement       DirectTreeSettlementKind
	pauseSettlement          DirectTreeSettlementKind
	rawStart                 *FileStart
	directoryAdmission       func(AuthenticatedSourceDirectory, DirectoryAdmission) (DirectoryAdmission, error)
	directorySettlement      func(DirectoryAdmission) (DirectorySettlement, error)
	directorySecret          [directoryAdmissionSecretBytes]byte
}

func newJobOutput(share catalog.ShareInstance) *jobOutput {
	// A monotonic nonce prevents independent fake sessions from sharing receipt
	// authority while keeping the test data deterministic and failure-free.
	directorySecret := sha256.Sum256(fmt.Appendf(nil,
		"%s/%x/%d", jobMaterializationDirectorySecretDomain, share.Bytes(), jobDirectTreeSessionNonce.Add(1),
	))
	return &jobOutput{
		share: share, session: transferID[OutputSessionID](44), durable: make(map[string]content.RangeSet),
		transactions: make(map[string]*jobFileTransaction), immediate: make(map[string]FileSettlement),
		transactionScripts: make(map[string]jobTransactionScript),
		directorySecret:    directorySecret,
	}
}

func (o *jobOutput) SessionID() OutputSessionID { return o.session }
func (o *jobOutput) Binding() DirectTreeSessionBinding {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.binding
}
func (o *jobOutput) Capabilities() DirectTreeCapabilities {
	if o.capabilitiesOverride != nil {
		return *o.capabilitiesOverride
	}
	capabilities, _ := NewDirectTreeCapabilities(DirectTreeCapabilities{
		Durability: DurabilityPowerLoss, RandomWrite: true,
		FileFailureIsolation: true, ModifiedTime: true,
	})
	return capabilities
}

func (o *jobOutput) OpenDirectTree(_ context.Context, intent ReceiveIntent) (DirectTreeSession, error) {
	binding, err := BindDirectTreeSession(intent)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.intent = intent
	o.binding = binding
	o.events = append(o.events, "open")
	o.mu.Unlock()
	if o.admitErr != nil {
		if o.returnSessionOnOpenError {
			return o, o.admitErr
		}
		return nil, o.admitErr
	}
	return o, nil
}

func (o *jobOutput) AdmitDirectory(_ context.Context, request DirectoryMaterializationRequest) (DirectoryAdmission, error) {
	directory := request.Source()
	directory, err := normalizeAuthenticatedSourceDirectory(directory)
	if err != nil {
		return DirectoryAdmission{}, err
	}
	if o.admitErr != nil {
		return DirectoryAdmission{}, o.admitErr
	}
	sourcePath := directory.SourcePath.String()
	if failure := o.ensureFailures[sourcePath]; failure != nil {
		return DirectoryAdmission{}, failure
	}
	if o.ensureErr != nil {
		return DirectoryAdmission{}, o.ensureErr
	}
	o.mu.Lock()
	intent := o.intent
	o.mu.Unlock()
	scope, err := NewDirectoryAdmissionScope(intent)
	if err != nil {
		return DirectoryAdmission{}, err
	}
	admission, err := NewDirectoryAdmissionWithSecret(o.directorySecret[:], scope, directory)
	if err != nil {
		return DirectoryAdmission{}, err
	}
	o.mu.Lock()
	o.directories = append(o.directories, sourcePath)
	o.directoryAdmissions = append(o.directoryAdmissions, admission)
	o.events = append(o.events, "ensure:"+sourcePath)
	result := o.directoryAdmission
	o.mu.Unlock()
	if result != nil {
		return result(directory, admission)
	}
	return admission, nil
}

func (o *jobOutput) FinalizeDirectory(
	_ context.Context,
	admission DirectoryAdmission,
) (DirectorySettlement, error) {
	o.mu.Lock()
	o.finalized = append(o.finalized, admission.Path())
	o.finalizedAdmissions = append(o.finalizedAdmissions, admission)
	settle := o.directorySettlement
	o.mu.Unlock()
	if settle != nil {
		return settle(admission)
	}
	if o.finalizeErr != nil {
		return DirectorySettlement{}, o.finalizeErr
	}
	return NewFinalizedDirectorySettlement(admission)
}

func (o *jobOutput) BeginFile(_ context.Context, file MaterializationFile) (FileStart, error) {
	artifactPath := file.ArtifactPath().String()
	o.mu.Lock()
	o.events = append(o.events, "begin:"+artifactPath)
	o.mu.Unlock()
	if o.beginHook != nil {
		o.beginHook(file)
	}
	if o.beginErr != nil {
		return FileStart{}, o.beginErr
	}
	if o.rawStart != nil {
		return *o.rawStart, nil
	}
	if settlement, ok := o.immediate[artifactPath]; ok {
		return NewFileSettlementStart(settlement)
	}
	var identity OwnedObjectID
	digest := sha256.Sum256([]byte(artifactPath))
	copy(identity[:], digest[:])
	binding, err := BindFileMaterializationTarget(file.Target(), identity)
	if err != nil {
		return FileStart{}, err
	}
	o.mu.Lock()
	durable := o.durable[artifactPath]
	script := o.transactionScript
	if configured, exists := o.transactionScripts[artifactPath]; exists {
		script = configured
	}
	transaction := &jobFileTransaction{output: o, binding: binding, durable: durable, generation: 1, script: script}
	o.transactions[artifactPath] = transaction
	o.mu.Unlock()
	verified, err := VerifyDurableRanges(binding, 1, durable)
	if err != nil {
		return FileStart{}, err
	}
	if o.nilTransaction {
		return FileStart{}, nil
	}
	return NewFileTransactionStart(transaction, verified)
}

func (o *jobOutput) FinalizeTree(_ context.Context, outcome DirectTreeOutcome) (DirectTreeSettlement, error) {
	o.mu.Lock()
	o.finished = outcome
	o.completeCalls++
	o.events = append(o.events, "complete")
	o.mu.Unlock()
	if o.finishErr != nil {
		return DirectTreeSettlement{}, o.finishErr
	}
	kind := o.completeSettlement
	if kind == 0 {
		kind = DirectTreeSettlementSuccess
		if outcome == DirectTreeOutcomePartial {
			kind = DirectTreeSettlementPartial
		}
	}
	return NewDirectTreeSettlement(kind)
}

func (o *jobOutput) PauseTree(context.Context, JobPauseReason) (DirectTreeSettlement, error) {
	o.mu.Lock()
	o.aborted = true
	o.pauseCalls++
	o.events = append(o.events, "pause")
	o.mu.Unlock()
	if o.abortErr != nil {
		return DirectTreeSettlement{}, o.abortErr
	}
	kind := o.pauseSettlement
	if kind == 0 {
		kind = DirectTreeSettlementPaused
	}
	return NewDirectTreeSettlement(kind)
}

type jobTransactionScript struct {
	writeErr                 error
	writeErrAfterWrite       error
	checkpointErr            error
	checkpointErrAfterAccept error
	omitCheckpoint           bool
	dropPriorRanges          bool
	checkpointExtra          content.RangeSet
	commitErr                error
	commitSettlement         FileSettlementKind
	commitResult             *FileSettlement
	pauseSettlement          FileSettlementKind
	pauseErr                 error
	pauseResult              *FileSettlement
	pauseHook                func(context.Context, FilePauseReason)
	retireSettlement         FileSettlementKind
	retireErr                error
	retireErrAfterWrite      error
	retireResult             *FileSettlement
}

type jobFileTransaction struct {
	output        *jobOutput
	binding       MaterializedFileBinding
	durable       content.RangeSet
	transient     content.RangeSet
	pending       content.RangeSet
	generation    CheckpointGeneration
	commitCalls   int
	committed     bool
	aborted       bool
	pauseReasons  []FilePauseReason
	retireReasons []FileRetireReason
	script        jobTransactionScript
}

func (t *jobFileTransaction) Binding() MaterializedFileBinding { return t.binding }

func (t *jobFileTransaction) WriteRange(_ context.Context, offset uint64, data []byte) error {
	if t.script.writeErr != nil {
		return t.script.writeErr
	}
	set, err := content.NewRangeSet([]content.Range{{Offset: offset, End: offset + uint64(len(data))}})
	if err != nil {
		return err
	}
	t.pending, err = MergeRanges(t.pending, set)
	if err == nil && t.script.writeErrAfterWrite != nil {
		return t.script.writeErrAfterWrite
	}
	return err
}

func (t *jobFileTransaction) Checkpoint(context.Context) (VerifiedDurableRanges, error) {
	if t.script.checkpointErr != nil {
		return VerifiedDurableRanges{}, t.script.checkpointErr
	}
	pending := t.pending
	merged, err := MergeRanges(t.durable, pending)
	if err != nil {
		return VerifiedDurableRanges{}, err
	}
	t.transient, err = MergeRanges(t.transient, pending)
	if err != nil {
		return VerifiedDurableRanges{}, err
	}
	if t.output.Capabilities().Durability == DurabilityNone {
		empty, emptyErr := content.NewRangeSet(nil)
		if emptyErr != nil {
			return VerifiedDurableRanges{}, emptyErr
		}
		t.durable, t.pending = empty, content.RangeSet{}
		t.generation++
		if t.script.checkpointErrAfterAccept != nil {
			return VerifiedDurableRanges{}, t.script.checkpointErrAfterAccept
		}
		return VerifyDurableRanges(t.binding, t.generation, empty)
	}
	t.durable, t.pending = merged, content.RangeSet{}
	t.generation++
	t.output.mu.Lock()
	t.output.durable[t.binding.Locator().CanonicalPath()] = merged
	t.output.mu.Unlock()
	if t.script.checkpointErrAfterAccept != nil {
		return VerifiedDurableRanges{}, t.script.checkpointErrAfterAccept
	}
	if t.script.omitCheckpoint {
		empty, _ := content.NewRangeSet(nil)
		return VerifyDurableRanges(t.binding, t.generation, empty)
	}
	if t.script.dropPriorRanges {
		return VerifyDurableRanges(t.binding, t.generation, pending)
	}
	if !t.script.checkpointExtra.IsEmpty() {
		claimed, mergeErr := MergeRanges(merged, t.script.checkpointExtra)
		if mergeErr != nil {
			return VerifiedDurableRanges{}, mergeErr
		}
		return VerifyDurableRanges(t.binding, t.generation, claimed)
	}
	return VerifyDurableRanges(t.binding, t.generation, merged)
}

func (t *jobFileTransaction) Commit(context.Context) (FileSettlement, error) {
	t.commitCalls++
	if t.script.commitErr != nil {
		return FileSettlement{}, t.script.commitErr
	}
	if t.script.commitResult != nil {
		return *t.script.commitResult, nil
	}
	completed := t.durable
	if t.output.Capabilities().Durability == DurabilityNone {
		completed = t.transient
	}
	if !RangesCoverFile(t.binding.ExactSize(), completed) {
		return FileSettlement{}, ErrIncompleteMaterializationFile
	}
	t.committed = true
	kind := t.script.commitSettlement
	if kind == 0 {
		kind = FilePublished
	}
	if kind == FilePublished && t.output.Capabilities().Durability == DurabilityNone {
		return NewTransientPublishedFileSettlement(t.binding)
	}
	checkpoint, err := VerifyDurableRanges(t.binding, t.generation, t.durable)
	if err != nil {
		return FileSettlement{}, err
	}
	return newJobFileSettlement(t.binding, kind, checkpoint)
}

func (t *jobFileTransaction) Pause(ctx context.Context, reason FilePauseReason) (FileSettlement, error) {
	t.aborted = true
	t.pauseReasons = append(t.pauseReasons, reason)
	if t.script.pauseHook != nil {
		t.script.pauseHook(ctx, reason)
	}
	if t.script.pauseErr != nil {
		return FileSettlement{}, t.script.pauseErr
	}
	if t.script.pauseResult != nil {
		return *t.script.pauseResult, nil
	}
	kind := t.script.pauseSettlement
	if kind == 0 {
		kind = FilePaused
	}
	checkpoint, err := VerifyDurableRanges(t.binding, t.generation, t.durable)
	if err != nil {
		return FileSettlement{}, err
	}
	return newJobFileSettlement(t.binding, kind, checkpoint)
}

func (t *jobFileTransaction) Retire(_ context.Context, reason FileRetireReason) (FileSettlement, error) {
	t.aborted = true
	t.retireReasons = append(t.retireReasons, reason)
	if t.script.retireErr != nil {
		return FileSettlement{}, t.script.retireErr
	}
	if t.script.retireErrAfterWrite != nil && !t.transient.IsEmpty() {
		return FileSettlement{}, t.script.retireErrAfterWrite
	}
	if t.script.retireResult != nil {
		return *t.script.retireResult, nil
	}
	kind := t.script.retireSettlement
	if kind == 0 {
		kind = FileFailed
	}
	checkpoint, err := VerifyDurableRanges(t.binding, t.generation, t.durable)
	if err != nil {
		return FileSettlement{}, err
	}
	return newJobFileSettlement(t.binding, kind, checkpoint)
}

func newJobFileSettlement(
	binding MaterializedFileBinding,
	kind FileSettlementKind,
	checkpoint VerifiedDurableRanges,
) (FileSettlement, error) {
	switch kind {
	case FilePublished, FilePaused:
		return NewVerifiedFileSettlement(kind, checkpoint)
	case FileItemBlocked:
		reference, err := NewMaterializationStateRef(binding.OutputSessionID(), binding.Locator().Digest())
		if err != nil {
			return FileSettlement{}, err
		}
		return NewTransactionItemBlockedFileSettlement(binding, reference, ItemBlockOwnershipUnknown)
	case FileFailed:
		return NewFailedFileSettlement(binding)
	default:
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
}
