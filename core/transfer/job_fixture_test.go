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
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/catalogflow"
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

var jobOutputBackend, _ = NewOutputBackendID("test/job-output")

func testTransferIntent(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules SelectionRules,
	backend OutputBackendID,
) TransferIntent {
	t.Helper()
	if _, err := NewOutputBackendID(string(backend)); err != nil {
		backend = jobOutputBackend
	}
	identityMaterial := append(share.Bytes(), root.Bytes()...)
	identity := sha256.Sum256(append([]byte("windshare/test-output-target/v1\x00"), identityMaterial...))
	target, err := NewOpaqueOutputTarget(identity[:])
	if err != nil {
		t.Fatal(err)
	}
	intent, err := NewTransferIntent(share, root, rules, target, backend, OutputNativeTree)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func newTestTransferJob(t *testing.T, config TransferJobConfig) (*TransferJob, error) {
	t.Helper()
	if config.Intent.IsZero() {
		backend := jobOutputBackend
		if provider, ok := config.Output.(interface{ BackendID() OutputBackendID }); ok {
			backend = provider.BackendID()
		}
		config.Intent = testTransferIntent(t, config.ShareInstance, config.SyntheticRoot, config.Rules, backend)
	}
	if config.JobID.IsZero() {
		digest := config.Intent.Digest()
		jobID, err := TransferJobIDFromBytes(digest[:TransferJobIdentityBytes])
		if err != nil {
			t.Fatal(err)
		}
		config.JobID = jobID
	}
	return NewTransferJob(config)
}

type jobOutput struct {
	mu                   sync.Mutex
	share                catalog.ShareInstance
	session              OutputSessionID
	durable              map[string]content.RangeSet
	transactions         map[string]*jobFileTransaction
	immediate            map[string]FileSettlement
	directories          []string
	finalized            []string
	finished             JobOutcome
	aborted              bool
	ensureErr            error
	admitErr             error
	ensureFailures       map[string]error
	finalizeErr          error
	beginErr             error
	finishErr            error
	abortErr             error
	nilTransaction       bool
	transactionScript    jobTransactionScript
	transactionScripts   map[string]jobTransactionScript
	capabilitiesOverride *OutputCapabilities
	beginHook            func(OutputFile)
	admitted             []OutputSelection
	intent               TransferIntent
	events               []string
	pauseCalls           int
	completeCalls        int
	completeSettlement   JobSettlementKind
	pauseSettlement      JobSettlementKind
	rawStart             *FileStart
}

func newJobOutput(share catalog.ShareInstance) *jobOutput {
	return &jobOutput{
		share: share, session: transferID[OutputSessionID](44), durable: make(map[string]content.RangeSet),
		transactions: make(map[string]*jobFileTransaction), immediate: make(map[string]FileSettlement),
		transactionScripts: make(map[string]jobTransactionScript),
	}
}

func (o *jobOutput) BackendID() OutputBackendID { return jobOutputBackend }
func (o *jobOutput) SessionID() OutputSessionID { return o.session }
func (o *jobOutput) Capabilities() OutputCapabilities {
	if o.capabilitiesOverride != nil {
		return *o.capabilitiesOverride
	}
	capabilities, _ := NewOutputCapabilities(OutputCapabilities{
		Durability: DurabilityPowerLoss, Mode: OutputNativeTree, RandomWrite: true,
		FileFailureIsolation: true, ModifiedTime: true,
	})
	return capabilities
}

func (o *jobOutput) OpenSelection(_ context.Context, selection OutputSelection) (OutputSession, error) {
	var preparationErr error
	for _, directory := range selection.Directories() {
		o.mu.Lock()
		o.directories = append(o.directories, directory.Path)
		o.events = append(o.events, "ensure:"+directory.Path)
		o.mu.Unlock()
		preparationErr = errors.Join(preparationErr, o.ensureFailures[directory.Path], o.ensureErr)
	}
	if preparationErr != nil {
		return nil, preparationErr
	}
	if o.admitErr != nil {
		return nil, o.admitErr
	}
	o.mu.Lock()
	o.admitted = append(o.admitted, selection)
	o.events = append(o.events, "open")
	o.mu.Unlock()
	return o, nil
}

func (o *jobOutput) OpenOutput(_ context.Context, intent TransferIntent) (OutputSession, error) {
	o.mu.Lock()
	o.intent = intent
	o.events = append(o.events, "open")
	o.mu.Unlock()
	if o.admitErr != nil {
		return nil, o.admitErr
	}
	return o, nil
}

func (o *jobOutput) AdmitDirectory(_ context.Context, directory OutputDirectory) (DirectoryAdmission, error) {
	if o.admitErr != nil {
		return DirectoryAdmission{}, o.admitErr
	}
	if failure := o.ensureFailures[directory.Path]; failure != nil || o.ensureErr != nil {
		return DirectoryAdmission{}, errors.Join(failure, o.ensureErr)
	}
	o.mu.Lock()
	o.directories = append(o.directories, directory.Path)
	o.events = append(o.events, "ensure:"+directory.Path)
	o.mu.Unlock()
	// Test output is in-process and can mint the opaque token directly. Real
	// backends return a token only after validating their root/ancestry handles.
	token := sha256.Sum256([]byte(directory.Path + string(directory.DirectoryID.Bytes()) + string(directory.Generation.Bytes())))
	return DirectoryAdmission{
		token: token, directory: directory.DirectoryID, generation: directory.Generation,
		parent: directory.ParentAdmission.token, path: directory.Path,
	}, nil
}

func (o *jobOutput) FinalizeDirectory(_ context.Context, directory OutputDirectory) error {
	o.mu.Lock()
	o.finalized = append(o.finalized, directory.Path)
	o.mu.Unlock()
	return o.finalizeErr
}

func (o *jobOutput) BeginFile(_ context.Context, file OutputFile) (FileStart, error) {
	o.mu.Lock()
	o.events = append(o.events, "begin:"+file.Path)
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
	if settlement, ok := o.immediate[file.Path]; ok {
		return NewFileSettlementStart(settlement)
	}
	var identity OutputObjectIdentity
	digest := sha256.Sum256([]byte(file.Path))
	copy(identity[:], digest[:])
	binding, err := BindOutputFileTarget(file.Target, identity)
	if err != nil {
		return FileStart{}, err
	}
	o.mu.Lock()
	durable := o.durable[file.Path]
	script := o.transactionScript
	if configured, exists := o.transactionScripts[file.Path]; exists {
		script = configured
	}
	transaction := &jobFileTransaction{output: o, binding: binding, durable: durable, generation: 1, script: script}
	o.transactions[file.Path] = transaction
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

func (o *jobOutput) CompleteJob(_ context.Context, outcome JobOutcome) (JobSettlement, error) {
	o.mu.Lock()
	o.finished = outcome
	o.completeCalls++
	o.events = append(o.events, "complete")
	o.mu.Unlock()
	if o.finishErr != nil {
		return JobSettlement{}, o.finishErr
	}
	kind := o.completeSettlement
	if kind == 0 {
		kind = JobClosed
	}
	return NewJobSettlement(kind)
}

func (o *jobOutput) PauseJob(context.Context, JobPauseReason) (JobSettlement, error) {
	o.mu.Lock()
	o.aborted = true
	o.pauseCalls++
	o.events = append(o.events, "pause")
	o.mu.Unlock()
	if o.abortErr != nil {
		return JobSettlement{}, o.abortErr
	}
	kind := o.pauseSettlement
	if kind == 0 {
		kind = JobPaused
	}
	return NewJobSettlement(kind)
}

type jobTransactionScript struct {
	writeErr            error
	writeErrAfterWrite  error
	checkpointErr       error
	omitCheckpoint      bool
	dropPriorRanges     bool
	checkpointExtra     content.RangeSet
	commitErr           error
	commitSettlement    FileSettlementKind
	commitResult        *FileSettlement
	pauseSettlement     FileSettlementKind
	pauseErr            error
	pauseResult         *FileSettlement
	pauseHook           func(context.Context, FilePauseReason)
	retireSettlement    FileSettlementKind
	retireErr           error
	retireErrAfterWrite error
	retireResult        *FileSettlement
}

type jobFileTransaction struct {
	output        *jobOutput
	binding       OutputFileBinding
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

func (t *jobFileTransaction) Binding() OutputFileBinding { return t.binding }

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
		return VerifyDurableRanges(t.binding, t.generation, empty)
	}
	t.durable, t.pending = merged, content.RangeSet{}
	t.generation++
	t.output.mu.Lock()
	t.output.durable[t.binding.Locator().CanonicalPath()] = merged
	t.output.mu.Unlock()
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
		return FileSettlement{}, ErrIncompleteOutputFile
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
		kind = FileRetired
	}
	checkpoint, err := VerifyDurableRanges(t.binding, t.generation, t.durable)
	if err != nil {
		return FileSettlement{}, err
	}
	return newJobFileSettlement(t.binding, kind, checkpoint)
}

func newJobFileSettlement(
	binding OutputFileBinding,
	kind FileSettlementKind,
	checkpoint VerifiedDurableRanges,
) (FileSettlement, error) {
	switch kind {
	case FilePublished, FilePaused, FilePublishBlocked:
		return NewVerifiedFileSettlement(kind, checkpoint)
	case FileQuarantined:
		reference, err := NewOutputStateRef(binding.OutputSessionID(), binding.Locator().Digest())
		if err != nil {
			return FileSettlement{}, err
		}
		return NewTransactionQuarantinedFileSettlement(binding, reference, QuarantineOwnershipMismatch)
	case FileRetired:
		return NewRetiredFileSettlement(binding)
	default:
		return FileSettlement{}, ErrInvalidOutputSettlement
	}
}
