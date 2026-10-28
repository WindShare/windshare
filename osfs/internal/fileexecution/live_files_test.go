package fileexecution

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type memoryDeliveryReceipts struct {
	values                      map[transfer.MaterializationLocatorDigest]DeliveryReceipt
	writeErr, readErr, closeErr error
	closed                      bool
}

func (receipts *memoryDeliveryReceipts) Lookup(key transfer.MaterializationLocatorDigest) (DeliveryReceipt, bool, error) {
	value, found := receipts.values[key]
	return value, found, receipts.readErr
}
func (receipts *memoryDeliveryReceipts) Record(key transfer.MaterializationLocatorDigest, receipt DeliveryReceipt) error {
	if receipts.writeErr != nil {
		return receipts.writeErr
	}
	receipts.values[key] = receipt
	return nil
}
func (receipts *memoryDeliveryReceipts) Close() error {
	receipts.closed = true
	return receipts.closeErr
}

type liveTestParent struct {
	outputcap.Directory
	identity int
	handles  *int
	closed   bool
}

func (parent *liveTestParent) Duplicate() (outputcap.Directory, error) {
	*parent.handles++
	return &liveTestParent{identity: parent.identity, handles: parent.handles}, nil
}
func (parent *liveTestParent) SameDirectory(other outputcap.Directory) (bool, error) {
	return parent.identity == other.(*liveTestParent).identity, nil
}
func (parent *liveTestParent) Close() error {
	if !parent.closed {
		parent.closed = true
		*parent.handles--
	}
	return nil
}

type liveTestDestination struct {
	memoryDestination
	parent    *liveTestParent
	parentErr error
}

func (destination *liveTestDestination) WithExactParent(_ context.Context, use func(outputcap.Directory) error) error {
	if destination.parentErr != nil {
		return destination.parentErr
	}
	return use(destination.parent)
}

type liveFilesTest struct {
	fixture    executionFixture
	files      *LiveFiles
	receipts   *memoryDeliveryReceipts
	parent     *liveTestParent
	cleanupErr error
	trace      []TraceEvent
}

func newLiveFilesTest(t *testing.T) *liveFilesTest {
	t.Helper()
	test := &liveFilesTest{
		fixture:  newExecutionFixture(t, 0),
		receipts: &memoryDeliveryReceipts{values: make(map[transfer.MaterializationLocatorDigest]DeliveryReceipt)},
		parent:   &liveTestParent{identity: 1, handles: new(int)},
	}
	var err error
	test.files, err = NewLiveFiles(test.receipts, test.fixture.intent, TraceSinkFunc(func(event TraceEvent) {
		test.trace = append(test.trace, event)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = test.files.Close() })
	return test
}

func (test *liveFilesTest) materialization(t *testing.T, index int, session transfer.OutputSessionID, descriptor content.FileRevisionDescriptor) transfer.MaterializationFile {
	t.Helper()
	name := fmt.Sprintf("file-%d.bin", index)
	source, err := ordinaryoutput.NewSourceCatalogPath(name)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := transfer.NewMaterializationRootRelativePath(name)
	if err != nil {
		t.Fatal(err)
	}
	file, err := transfer.NewMaterializationFile(test.fixture.intent, source, relative, descriptor, session, test.fixture.file.Parent())
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func (test *liveFilesTest) begin(t *testing.T, index int) (transfer.FileTransaction, *liveTransactionFile, *liveTestDestination) {
	t.Helper()
	file := test.materialization(t, index, test.fixture.session, test.fixture.file.Descriptor())
	native := &liveTransactionFile{}
	destination := &liveTestDestination{
		memoryDestination: memoryDestination{target: file.Target(), final: FinalAbsent}, parent: test.parent,
	}
	start, err := test.files.Begin(context.Background(), file, destination, func() (*PartialFileTransaction, error) {
		var object checkpointmodel.ObjectID
		binary.BigEndian.PutUint64(object[:], uint64(index+1))
		owned, err := NewProcessOwnedFile(object, native, native.Close)
		if err != nil {
			return nil, err
		}
		return NewLivePartialFileTransaction(file, destination, owned, func(*LiveOwnedFile) error { return test.cleanupErr })
	})
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatal("new file has no transaction")
	}
	return transaction, native, destination
}

func mustNeverCreate(t *testing.T) func() (*PartialFileTransaction, error) {
	t.Helper()
	return func() (*PartialFileTransaction, error) {
		t.Fatal("settled or blocked output created another native object")
		return nil, nil
	}
}

func TestLiveFilesCompletedDeliveryRecyclesNativeSlots(t *testing.T) {
	test := newLiveFilesTest(t)
	for index := 0; index <= maximumUnsettledLiveFiles; index++ {
		transaction, native, destination := test.begin(t, index)
		settlement, err := transaction.Commit(context.Background())
		if err != nil || settlement.Kind() != transfer.FilePublished {
			t.Fatalf("file %d: %v %v", index, settlement.Kind(), err)
		}
		if !native.closed || !destination.closed || *test.parent.handles != 0 || len(test.files.files) != 0 || len(test.files.pending) != 0 {
			t.Fatalf("file %d retained native resources after delivery", index)
		}
	}
	if len(test.receipts.values) != maximumUnsettledLiveFiles+1 {
		t.Fatal("delivery history was evicted to reclaim slots")
	}
	if event := test.trace[len(test.trace)-1]; event.Operation != TraceReleaseLiveFile ||
		event.RetainedLiveFiles != 0 || event.OperationID != test.fixture.intent.OperationID() {
		t.Fatalf("release diagnostic lost lifetime context: %+v", event)
	}
}

func TestLiveFilesRetryRestoresHistoricalDeliveryWithoutInspectingFinal(t *testing.T) {
	test := newLiveFilesTest(t)
	transaction, _, _ := test.begin(t, 0)
	if _, err := transaction.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	nextSession := test.fixture.session
	nextSession[0]++
	file := test.materialization(t, 0, nextSession, test.fixture.file.Descriptor())
	inspectionFailure := errors.New("public file was moved or edited by its owner")
	destination := &liveTestDestination{
		memoryDestination: memoryDestination{target: file.Target(), observeErr: inspectionFailure},
		parentErr:         inspectionFailure,
	}
	start, err := test.files.Begin(context.Background(), file, destination, mustNeverCreate(t))
	settlement, settled := start.ImmediateSettlement()
	if err != nil || !settled || settlement.Kind() != transfer.FilePreviouslyPublished ||
		settlement.Target().OutputSessionID() != nextSession || !destination.closed {
		t.Fatalf("historical delivery: %v %v", settlement.Kind(), err)
	}
	if _, verified := settlement.VerifiedProgress(); verified {
		t.Fatal("delivery receipt became a proof of current bytes")
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("x")); err == nil {
		t.Fatal("old transaction remained writable")
	}

	descriptor := file.Descriptor()
	revision := descriptor.FileRevision()
	revision[0]++
	changed, err := content.NewFileRevisionDescriptor(descriptor.ShareInstance(), descriptor.FileID(), revision, descriptor.Geometry(), descriptor.ModifiedTime())
	if err != nil {
		t.Fatal(err)
	}
	file = test.materialization(t, 0, nextSession, changed)
	destination = &liveTestDestination{memoryDestination: memoryDestination{target: file.Target()}, parentErr: inspectionFailure}
	start, err = test.files.Begin(context.Background(), file, destination, mustNeverCreate(t))
	settlement, _ = start.ImmediateSettlement()
	if _, reason, blocked := settlement.ItemBlock(); err != nil || !blocked || reason != transfer.ItemBlockRevisionConflict {
		t.Fatalf("changed source reused delivery receipt: %v %v", reason, err)
	}
}

func TestLiveFilesReceiptFailurePausesAdmissionAndRetriesWithoutRedelivery(t *testing.T) {
	test := newLiveFilesTest(t)
	failure := errors.New("receipt scratch storage unavailable")
	test.receipts.writeErr = failure
	transaction, native, _ := test.begin(t, 0)
	if settlement, err := transaction.Commit(context.Background()); err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("bookkeeping retracted publication: %v %v", settlement.Kind(), err)
	}
	if native.closed || len(test.files.pending) != 1 {
		t.Fatal("unrecorded delivery lost its retained fallback")
	}
	file := test.materialization(t, 1, test.fixture.session, test.fixture.file.Descriptor())
	destination := &liveTestDestination{memoryDestination: memoryDestination{target: file.Target()}, parent: test.parent}
	_, err := test.files.Begin(context.Background(), file, destination, mustNeverCreate(t))
	value, ok := fault.NormalizeBoundaryError(err).Fault()
	if !errors.Is(err, failure) || !ok || value.Scope() != fault.ScopeOutputPause || !destination.closed {
		t.Fatalf("receipt failure did not pause output: %v", err)
	}
	test.receipts.writeErr = nil
	file = test.materialization(t, 0, test.fixture.session, test.fixture.file.Descriptor())
	destination = &liveTestDestination{memoryDestination: memoryDestination{target: file.Target()}, parentErr: failure}
	start, err := test.files.Begin(context.Background(), file, destination, mustNeverCreate(t))
	settlement, _ := start.ImmediateSettlement()
	if err != nil || settlement.Kind() != transfer.FilePreviouslyPublished || !native.closed || len(test.files.files) != 0 {
		t.Fatalf("receipt retry lost delivery: %v %v", settlement.Kind(), err)
	}
}

func TestLiveFilesCleanupDebtKeepsOwnershipButNotCompletionHostage(t *testing.T) {
	test := newLiveFilesTest(t)
	test.cleanupErr = errors.New("private stage temporarily cannot be removed")
	transaction, native, _ := test.begin(t, 0)
	if settlement, err := transaction.Commit(context.Background()); err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("cleanup retracted delivery: %v %v", settlement.Kind(), err)
	}
	if native.closed || len(test.receipts.values) != 1 {
		t.Fatal("cleanup debt lost its ownership or delivery evidence")
	}
	file := test.materialization(t, 0, test.fixture.session, test.fixture.file.Descriptor())
	start, err := test.files.Begin(context.Background(), file,
		&liveTestDestination{memoryDestination: memoryDestination{target: file.Target()}}, mustNeverCreate(t))
	settlement, _ := start.ImmediateSettlement()
	if err != nil || settlement.Kind() != transfer.FilePreviouslyPublished {
		t.Fatalf("cleanup debt prevented restoring delivery: %v", err)
	}
	test.cleanupErr = nil
	next, _, _ := test.begin(t, 1)
	if !native.closed || len(test.files.pending) != 0 {
		t.Fatal("cleanup debt was not retried")
	}
	if _, err := next.Retire(context.Background(), transfer.FileRetireInvalidatedRevision); err != nil || len(test.files.files) != 0 {
		t.Fatalf("retirement retained a slot: %v", err)
	}
}

func TestLiveFilesUnfinishedCapacityPausesWithoutDiscardingProgress(t *testing.T) {
	test := newLiveFilesTest(t)
	var first transfer.FileTransaction
	for index := range maximumUnsettledLiveFiles {
		transaction, _, _ := test.begin(t, index)
		if index == 0 {
			first = transaction
		}
	}
	file := test.materialization(t, maximumUnsettledLiveFiles, test.fixture.session, test.fixture.file.Descriptor())
	destination := &liveTestDestination{memoryDestination: memoryDestination{target: file.Target(), final: FinalAbsent}, parent: test.parent}
	_, err := test.files.Begin(context.Background(), file, destination, mustNeverCreate(t))
	value, ok := fault.NormalizeBoundaryError(err).Fault()
	if !errors.Is(err, ErrLiveRetentionLimit) || !ok || value.Scope() != fault.ScopeOutputPause {
		t.Fatalf("unfinished resource pressure did not pause once: %v", err)
	}
	if _, err := first.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	next, _, _ := test.begin(t, maximumUnsettledLiveFiles)
	if _, err := next.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := test.files.Close(); err != nil || *test.parent.handles != 0 || !test.receipts.closed {
		t.Fatalf("operation release leaked resources: %v", err)
	}
}

func TestLiveFilesUnfinishedRetryStillRequiresExactParent(t *testing.T) {
	test := newLiveFilesTest(t)
	first, native, _ := test.begin(t, 0)
	if _, err := first.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal(err)
	}
	test.parent.identity++
	file := test.materialization(t, 0, test.fixture.session, test.fixture.file.Descriptor())
	destination := &liveTestDestination{memoryDestination: memoryDestination{target: file.Target()}, parent: test.parent}
	start, err := test.files.Begin(context.Background(), file, destination, mustNeverCreate(t))
	settlement, _ := start.ImmediateSettlement()
	if _, reason, blocked := settlement.ItemBlock(); err != nil || !blocked || reason != transfer.ItemBlockOwnershipUnknown || native.closed {
		t.Fatalf("unfinished object crossed a replaced parent: %v %v", reason, err)
	}
	test.parent.identity--
	destination = &liveTestDestination{memoryDestination: memoryDestination{target: file.Target(), final: FinalAbsent}, parent: test.parent}
	start, err = test.files.Begin(context.Background(), file, destination, mustNeverCreate(t))
	next, progress, active := start.Transaction()
	if err != nil || !active || progress.Retention() != transfer.RangesInProcess || next.Binding().ObjectIdentity() != first.Binding().ObjectIdentity() {
		t.Fatalf("same-parent retry lost retained ownership: %v", err)
	}
	if _, err := next.Commit(context.Background()); err != nil || !native.closed {
		t.Fatalf("resumed completion retained native ownership: %v", err)
	}
}

func TestLiveFilesPublicationCannotReopenWhileReceiptRecordingIsPending(t *testing.T) {
	test := newLiveFilesTest(t)
	transaction, _, _ := test.begin(t, 0)
	// Stop at the boundary between stable publication and operation bookkeeping.
	// The production wrapper resumes there after Commit releases the file mutex.
	managed := transaction.(*retainedLiveTransaction)
	if _, err := managed.PartialFileTransaction.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	file := test.materialization(t, 0, test.fixture.session, test.fixture.file.Descriptor())
	destination := &liveTestDestination{memoryDestination: memoryDestination{target: file.Target()}, parent: test.parent}
	start, err := test.files.Begin(context.Background(), file, destination, mustNeverCreate(t))
	settlement, settled := start.ImmediateSettlement()
	if err != nil || !settled || settlement.Kind() != transfer.FilePreviouslyPublished {
		t.Fatalf("published file reopened before receipt recording: %v", err)
	}
}

func TestLiveFilesConcurrentCompletionsReleaseIndependentSlots(t *testing.T) {
	test := newLiveFilesTest(t)
	const count = 32
	transactions := make([]transfer.FileTransaction, count)
	for index := range transactions {
		transactions[index], _, _ = test.begin(t, index)
	}
	var workers sync.WaitGroup
	failures := make(chan error, count)
	for _, transaction := range transactions {
		workers.Go(func() {
			settlement, err := transaction.Commit(context.Background())
			if err != nil {
				failures <- err
			} else if settlement.Kind() != transfer.FilePublished {
				failures <- errors.New("concurrent publication lost its settlement")
			}
		})
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if len(test.files.files) != 0 || *test.parent.handles != 0 || len(test.receipts.values) != count {
		t.Fatal("concurrent completion lost receipts or retained native resources")
	}
}

func TestLiveDeliveryDescriptorIncludesMetadataAndGeometry(t *testing.T) {
	test := newLiveFilesTest(t)
	original := test.fixture.file.Descriptor()
	modified, err := catalog.NewModifiedTime(1, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := content.NewFileRevisionDescriptor(original.ShareInstance(), original.FileID(), original.FileRevision(), original.Geometry(), modified)
	if err != nil {
		t.Fatal(err)
	}
	if deliveryDescriptorDigest(original) == deliveryDescriptorDigest(changed) {
		t.Fatal("receipt identity omitted source metadata")
	}
	geometry, err := content.NewFileGeometry(0, original.Geometry().ChunkSize()*2)
	if err != nil {
		t.Fatal(err)
	}
	changed, err = content.NewFileRevisionDescriptor(original.ShareInstance(), original.FileID(), original.FileRevision(), geometry, original.ModifiedTime())
	if err != nil || deliveryDescriptorDigest(original) == deliveryDescriptorDigest(changed) {
		t.Fatal("receipt identity omitted chunk geometry")
	}
}
