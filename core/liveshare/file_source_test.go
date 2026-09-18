package liveshare

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/contentflow"
)

// Deliberately non-UTF-8, NUL-bearing references catch accidental path parsing
// anywhere between selection, private catalog persistence, and stable reads.
var objectRootReference = mustObjectReference([]byte{0xff, 0, 0x81})
var objectFileReference = mustObjectReference([]byte{0xff, 0, 0x82})
var errObjectSizeUnavailable = errors.New("document provider did not report an exact size")

func mustObjectReference(raw []byte) catalog.SourceReference {
	reference, err := catalog.NewSourceReference(raw)
	if err != nil {
		panic(err)
	}
	return reference
}

type objectFileSource struct {
	mu                sync.Mutex
	root              catalog.NodeRecord
	file              catalog.NodeRecord
	sourceContext     FileSourceContext
	payload           []byte
	version           uint64
	continuity        content.RevisionContinuity
	revoked           bool
	missing           bool
	unsupported       bool
	unknownSize       bool
	scans             int
	opens             int
	activeHandles     int
	activeScans       int
	closed            int
	closedWhileActive bool
	handleCloses      int
	readStarted       chan struct{}
	allowRead         <-chan struct{}
	scanStarted       chan struct{}
	allowScan         <-chan struct{}
	closeErr          error
}

func newObjectFileSource() *objectFileSource {
	return &objectFileSource{payload: bytes.Repeat([]byte("object bytes"), int(catalog.MinChunkSize)/6), version: 1, continuity: content.CatalogRevisionContinuity}
}

func (source *objectFileSource) OpenFileSource(_ context.Context, sourceContext FileSourceContext) (FileSource, error) {
	source.sourceContext = sourceContext
	identity, err := sourceContext.NewIdentity()
	if err != nil {
		return source, err
	}
	directory, err := catalog.DirectoryIDFromBytes(identity[:])
	if err != nil {
		return source, err
	}
	sourceIdentity, _ := catalog.NewSourceIdentity([]byte("provider:directory:81"))
	source.root, err = catalog.NewDirectoryNodeRecord(directory, sourceContext.SyntheticRoot, "Documents", objectRootReference, sourceIdentity, catalog.ModifiedTime{})
	return source, err
}

func (source *objectFileSource) SelectedRoots() []catalog.NodeRecord {
	return []catalog.NodeRecord{source.root}
}

func (source *objectFileSource) ScanDirectory(ctx context.Context, request catalog.ScanRequest) (catalog.ScanResult, error) {
	source.mu.Lock()
	source.scans++
	source.activeScans++
	source.mu.Unlock()
	defer func() { source.mu.Lock(); source.activeScans--; source.mu.Unlock() }()
	if source.scanStarted != nil {
		close(source.scanStarted)
		select {
		case <-source.allowScan:
		case <-ctx.Done():
			return catalog.ScanResult{}, ctx.Err()
		}
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if err := source.checkAccess("scan", request.Directory.SourceReference()); err != nil {
		return catalog.ScanResult{}, catalog.NewPermanentScanError(err)
	}
	if request.Directory != source.root {
		return catalog.ScanResult{}, catalog.ErrDirectoryStale
	}
	if source.unknownSize {
		return catalog.ScanResult{}, catalog.NewPermanentScanError(&catalog.SourceError{Operation: "discover metadata", Reference: objectFileReference, Failure: catalog.SourceFailureUnsupported, Cause: errObjectSizeUnavailable})
	}
	if err := request.Work.Consume(1); err != nil {
		return catalog.ScanResult{}, err
	}
	identity, err := source.sourceContext.NewIdentity()
	if err != nil {
		return catalog.ScanResult{}, err
	}
	file, err := catalog.FileIDFromBytes(identity[:])
	if err != nil {
		return catalog.ScanResult{}, err
	}
	sourceIdentity, _ := catalog.NewSourceIdentity([]byte("provider:document:82"))
	candidate := source.candidate()
	parent, _ := source.root.DirectoryID()
	source.file, err = catalog.NewFileNodeRecord(file, parent, "report.bin", objectFileReference, sourceIdentity, candidate, uint64(len(source.payload)), catalog.ModifiedTime{})
	if err != nil {
		return catalog.ScanResult{}, err
	}
	err = request.Children.Add(ctx, catalog.ScannedChild{FileID: file, Name: "report.bin", SourceReference: objectFileReference, SourceIdentity: sourceIdentity, VersionCandidate: candidate, ExpectedSize: uint64(len(source.payload))})
	return catalog.ScanResult{}, err
}

func (source *objectFileSource) candidate() catalog.VersionCandidate {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], source.version)
	candidate, _ := catalog.NewVersionCandidate(raw[:])
	return candidate
}

func (source *objectFileSource) checkAccess(operation string, reference catalog.SourceReference) error {
	if source.closed != 0 {
		return content.ErrRevisionStoreClosed
	}
	if source.revoked {
		return &catalog.SourceError{Operation: operation, Reference: reference, Failure: catalog.SourceFailureAccessDenied, Cause: errors.Join(content.ErrRevisionUnreadable, fs.ErrPermission)}
	}
	if source.missing {
		return &catalog.SourceError{Operation: operation, Reference: reference, Failure: catalog.SourceFailureMissing, Cause: content.ErrRevisionNotFound}
	}
	return nil
}

func (source *objectFileSource) RevisionContinuity(catalog.NodeRecord) (content.RevisionContinuity, error) {
	return source.continuity, nil
}

func (source *objectFileSource) OpenStable(_ context.Context, record catalog.NodeRecord) (content.StableFile, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	if err := source.checkAccess("open", record.SourceReference()); err != nil {
		return nil, err
	}
	if source.unsupported {
		return nil, &catalog.SourceError{Operation: "open", Reference: record.SourceReference(), Failure: catalog.SourceFailureUnsupported, Cause: content.ErrUnsupportedStability}
	}
	if record.SourceReference() != objectFileReference || record.VersionCandidate() != source.candidate() {
		return nil, content.ErrRevisionStale
	}
	source.opens++
	source.activeHandles++
	return &objectStableFile{source: source, version: source.version, payload: append([]byte(nil), source.payload...)}, nil
}

func (source *objectFileSource) Close() error {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.closed++
	source.closedWhileActive = source.activeHandles != 0 || source.activeScans != 0
	return source.closeErr
}

type objectStableFile struct {
	source  *objectFileSource
	version uint64
	payload []byte
	closed  bool
}

func (file *objectStableFile) ExactSize() uint64             { return uint64(len(file.payload)) }
func (*objectStableFile) ModifiedTime() catalog.ModifiedTime { return catalog.ModifiedTime{} }
func (file *objectStableFile) Verify(context.Context) error {
	file.source.mu.Lock()
	defer file.source.mu.Unlock()
	if err := file.source.checkAccess("read", objectFileReference); err != nil {
		return err
	}
	if file.closed {
		return content.ErrRevisionUnreadable
	}
	if file.source.version != file.version {
		return content.ErrSourceDrift
	}
	return nil
}
func (file *objectStableFile) ReadAt(ctx context.Context, destination []byte, offset uint64) (int, error) {
	if file.source.readStarted != nil {
		close(file.source.readStarted)
		select {
		case <-file.source.allowRead:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if err := file.Verify(ctx); err != nil {
		return 0, err
	}
	if offset >= uint64(len(file.payload)) {
		return 0, io.EOF
	}
	n := copy(destination, file.payload[offset:])
	if n != len(destination) {
		return n, io.EOF
	}
	return n, nil
}
func (file *objectStableFile) Close() error {
	file.source.mu.Lock()
	defer file.source.mu.Unlock()
	if file.closed {
		return errors.New("stable file closed twice")
	}
	file.closed = true
	file.source.activeHandles--
	file.source.handleCloses++
	return nil
}

func objectSenderConfig(t *testing.T, source *objectFileSource) SenderConfig {
	t.Helper()
	return SenderConfig{Source: source, Relays: []string{"ws://source.test"}, ChunkSize: catalog.MinChunkSize, RevisionCapacity: newTestRevisionCapacity(t), CatalogBudget: testCatalogBudget(), CacheBudget: testCacheBudget(), CatalogStorage: CatalogStorageFactoryFunc(func(context.Context, catalog.ShareInstance) (catalog.CatalogBackend, error) {
		return catalog.NewMemoryCatalogBackend(), nil
	})}
}

func prepareObjectSender(t *testing.T, source *objectFileSource) *PreparedSender {
	t.Helper()
	sender, err := PrepareSender(context.Background(), objectSenderConfig(t, source))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sender.Close(); err != nil {
			t.Error(err)
		}
	})
	return sender
}

func discoverObjectFile(t *testing.T, sender *PreparedSender, source *objectFileSource) catalog.FileID {
	t.Helper()
	directory, _ := source.root.DirectoryID()
	if _, err := sender.catalogStore.ListChildren(context.Background(), directory, testCatalogBudget(), catalog.ScanOptions{}, sender.source); err != nil {
		t.Fatal(err)
	}
	file, _ := source.file.FileID()
	stored, found, err := sender.catalogStore.Node(context.Background(), file.NodeID())
	if err != nil || !found || stored != source.file {
		t.Fatalf("private object catalog round trip = %+v, %v, %v", stored, found, err)
	}
	return file
}

func TestFileSourceProvidesLazyDiscoveryAndOffsetReads(t *testing.T) {
	source := newObjectFileSource()
	sender := prepareObjectSender(t, source)
	if source.scans != 0 || source.opens != 0 || source.sourceContext.ShareInstance != sender.descriptor.ShareInstance() {
		t.Fatal("sender preparation crossed the lazy source boundary")
	}
	file := discoverObjectFile(t, sender, source)
	capacity := lifecycleSessionCapacity(t, sender.revisionStore, "object-range")
	lease, err := sender.revisionStore.OpenRevision(context.Background(), file, capacity)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := lease.Descriptor()
	ref, err := content.NewBlockRef(file, descriptor.FileRevision(), 1, descriptor.Geometry())
	if err != nil {
		t.Fatal(err)
	}
	data, err := sender.revisionStore.ReadBlock(context.Background(), lease.ID(), ref)
	if err != nil || !bytes.Equal(data, source.payload[catalog.MinChunkSize:]) {
		t.Fatalf("object range read = %d, %v", len(data), err)
	}
	if err := sender.Close(); err != nil {
		t.Fatal(err)
	}
	if source.closed != 1 || source.handleCloses != 1 || source.closedWhileActive {
		t.Fatalf("source cleanup = %+v", source)
	}
}

func TestFileSourcePreservesRevisionContinuityAcrossReopen(t *testing.T) {
	for _, continuity := range []content.RevisionContinuity{content.CatalogRevisionContinuity, content.OpenHandleRevisionContinuity} {
		t.Run(map[content.RevisionContinuity]string{content.CatalogRevisionContinuity: "catalog", content.OpenHandleRevisionContinuity: "handle"}[continuity], func(t *testing.T) {
			source := newObjectFileSource()
			source.continuity = continuity
			sender := prepareObjectSender(t, source)
			file := discoverObjectFile(t, sender, source)
			capacity := lifecycleSessionCapacity(t, sender.revisionStore, "object-reopen")
			first, err := sender.revisionStore.OpenRevision(context.Background(), file, capacity)
			if err != nil {
				t.Fatal(err)
			}
			if err := sender.revisionStore.EndLease(first.ID(), content.LeaseRelinquished); err != nil {
				t.Fatal(err)
			}
			second, err := sender.revisionStore.OpenRevision(context.Background(), file, capacity)
			if err != nil {
				t.Fatal(err)
			}
			same := first.Descriptor().FileRevision() == second.Descriptor().FileRevision()
			if same != (continuity == content.CatalogRevisionContinuity) || source.opens != 2 || source.handleCloses != 1 {
				t.Fatalf("continuity %v: same=%v opens=%d closes=%d", continuity, same, source.opens, source.handleCloses)
			}
			if err := sender.revisionStore.EndLease(second.ID(), content.LeaseRelinquished); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFileSourceReportsChangedRevokedMissingAndUnsupportedObjects(t *testing.T) {
	for _, mode := range []string{"changed", "revoked", "missing", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			source := newObjectFileSource()
			sender := prepareObjectSender(t, source)
			file := discoverObjectFile(t, sender, source)
			capacity := lifecycleSessionCapacity(t, sender.revisionStore, "object-failure")
			var expected error
			var failure catalog.SourceFailure
			switch mode {
			case "changed":
				source.version++
				expected = content.ErrRevisionStale
			case "revoked":
				source.revoked = true
				expected = fs.ErrPermission
				failure = catalog.SourceFailureAccessDenied
			case "missing":
				source.missing = true
				expected = content.ErrRevisionNotFound
				failure = catalog.SourceFailureMissing
			case "unsupported":
				source.unsupported = true
				expected = content.ErrUnsupportedStability
				failure = catalog.SourceFailureUnsupported
			}
			_, err := sender.revisionStore.OpenRevision(context.Background(), file, capacity)
			if !errors.Is(err, expected) {
				t.Fatalf("source failure = %v, want %v", err, expected)
			}
			if failure != 0 {
				var typed *catalog.SourceError
				if !errors.As(err, &typed) || typed.Failure != failure || typed.Reference != objectFileReference {
					t.Fatalf("source error context = %v", err)
				}
			}
			if source.opens != 0 || source.activeHandles != 0 {
				t.Fatal("failed source open retained a stable handle")
			}
		})
	}
}

func TestFileSourceDetectsMutationDuringOpenHandle(t *testing.T) {
	source := newObjectFileSource()
	sender := prepareObjectSender(t, source)
	file := discoverObjectFile(t, sender, source)
	lease, err := sender.revisionStore.OpenRevision(context.Background(), file, lifecycleSessionCapacity(t, sender.revisionStore, "object-mutation"))
	if err != nil {
		t.Fatal(err)
	}
	source.version++
	ref, _ := content.NewBlockRef(file, lease.Descriptor().FileRevision(), 0, lease.Descriptor().Geometry())
	if _, err := sender.revisionStore.ReadBlock(context.Background(), lease.ID(), ref); !errors.Is(err, content.ErrRevisionDrift) {
		t.Fatalf("mutated handle read = %v", err)
	}
	if source.activeHandles != 0 {
		t.Fatal("drifted handle remained active")
	}
}

func TestFileSourceRollbackOwnsReturnedSourceAndPreservesCleanupFailure(t *testing.T) {
	for _, phase := range []string{"factory", "catalog", "content"} {
		t.Run(phase, func(t *testing.T) {
			source := newObjectFileSource()
			injected := errors.New("source preparation failed")
			cleanup := errors.New("source cleanup failed")
			source.closeErr = cleanup
			config := objectSenderConfig(t, source)
			switch phase {
			case "factory":
				config.Source = FileSourceFactoryFunc(func(ctx context.Context, value FileSourceContext) (FileSource, error) {
					owned, err := source.OpenFileSource(ctx, value)
					return owned, errors.Join(err, injected)
				})
			case "catalog":
				config.CatalogStorage = CatalogStorageFactoryFunc(func(context.Context, catalog.ShareInstance) (catalog.CatalogBackend, error) { return nil, injected })
			case "content":
				config.preparation = productionSenderPreparationDependencies()
				config.preparation.newRecordSealer = func(records.SealerConfig) (*records.Sealer, error) { return nil, injected }
			}
			sender, err := PrepareSender(context.Background(), config)
			if sender != nil || !errors.Is(err, injected) || !errors.Is(err, cleanup) {
				t.Fatalf("preparation rollback = %v, %v", sender, err)
			}
			if source.closed != 1 || source.closedWhileActive || config.CatalogBudget.Snapshot().Used != (catalog.ResourceUsage{}) {
				t.Fatalf("rollback retained source/budget ownership: closed=%d usage=%+v", source.closed, config.CatalogBudget.Snapshot().Used)
			}
		})
	}
}

func TestFileSourceCloseWaitsForReadAndCancelsDiscovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := newObjectFileSource()
		sender := prepareObjectSender(t, source)
		file := discoverObjectFile(t, sender, source)
		lease, err := sender.revisionStore.OpenRevision(context.Background(), file, lifecycleSessionCapacity(t, sender.revisionStore, "object-close"))
		if err != nil {
			t.Fatal(err)
		}
		allow := make(chan struct{})
		source.allowRead = allow
		source.readStarted = make(chan struct{})
		ref, _ := content.NewBlockRef(file, lease.Descriptor().FileRevision(), 0, lease.Descriptor().Geometry())
		read := make(chan error, 1)
		go func() { _, err := sender.revisionStore.ReadBlock(context.Background(), lease.ID(), ref); read <- err }()
		<-source.readStarted
		closed := make(chan error, 1)
		go func() { closed <- sender.Close() }()
		synctest.Wait()
		if source.closed != 0 {
			t.Fatal("file source closed during an active read")
		}
		close(allow)
		if err := <-read; err != nil {
			t.Fatal(err)
		}
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		if source.closed != 1 || source.closedWhileActive {
			t.Fatal("source lifetime ended before its read joined")
		}
	})
	synctest.Test(t, func(t *testing.T) {
		source := newObjectFileSource()
		source.scanStarted = make(chan struct{})
		source.allowScan = make(chan struct{})
		sender := prepareObjectSender(t, source)
		sender.StartRootPrefetch()
		<-source.scanStarted
		if err := sender.Close(); err != nil {
			t.Fatal(err)
		}
		if source.activeScans != 0 || source.closedWhileActive || source.closed != 1 {
			t.Fatal("source lifetime ended before cancelled discovery joined")
		}
	})
}

func TestFileSourcesShareApplicationCatalogAndCacheBudgets(t *testing.T) {
	const cachedObjectBytes = 16
	catalogBudget := testCatalogBudget()
	cacheBudget, err := contentflow.NewProcessCacheBudget(cachedObjectBytes)
	if err != nil {
		t.Fatal(err)
	}
	capacity := newTestRevisionCapacity(t)
	prepare := func() *PreparedSender {
		source := newObjectFileSource()
		config := objectSenderConfig(t, source)
		config.CatalogBudget, config.CacheBudget, config.RevisionCapacity = catalogBudget, cacheBudget, capacity
		sender, err := PrepareSender(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := sender.Close(); err != nil {
				t.Error(err)
			}
		})
		return sender
	}
	first := prepare()
	firstCharge := catalogBudget.Snapshot().Used
	second := prepare()
	bothCharge := catalogBudget.Snapshot().Used
	if firstCharge.Entries == 0 || bothCharge.Entries != 2*firstCharge.Entries || bothCharge.MemoryBytes <= firstCharge.MemoryBytes {
		t.Fatalf("real sender catalog charges: first=%+v both=%+v", firstCharge, bothCharge)
	}
	load := func(sender *PreparedSender, calls *int) {
		t.Helper()
		key := contentflow.BlockCacheKey{ShareInstance: sender.descriptor.ShareInstance(), FileID: catalog.FileID{1}, FileRevision: content.FileRevision{1}}
		_, err := sender.cache.Get(context.Background(), key, func(context.Context) ([]byte, error) { *calls++; return make([]byte, cachedObjectBytes), nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	firstLoads, secondLoads := 0, 0
	load(first, &firstLoads)
	load(second, &secondLoads)
	load(second, &secondLoads)
	if firstLoads != 1 || secondLoads != 2 || cacheBudget.Used() != cachedObjectBytes {
		t.Fatalf("aggregate cache admission: first=%d second=%d used=%d", firstLoads, secondLoads, cacheBudget.Used())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	remaining := catalogBudget.Snapshot().Used
	if remaining.Entries != firstCharge.Entries || remaining.MemoryBytes == 0 || cacheBudget.Used() != 0 {
		t.Fatalf("first share release damaged second share ownership: catalog=%+v cache=%d", remaining, cacheBudget.Used())
	}
	load(second, &secondLoads)
	load(second, &secondLoads)
	if secondLoads != 3 || cacheBudget.Used() != cachedObjectBytes {
		t.Fatal("second share did not reuse the released process cache capacity")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if catalogBudget.Snapshot().Used != (catalog.ResourceUsage{}) || cacheBudget.Used() != 0 {
		t.Fatal("closed senders retained aggregate capacity")
	}
}

func TestFileSourceRejectsUnknownRequiredMetadataWithoutPublishingGuesses(t *testing.T) {
	source := newObjectFileSource()
	source.unknownSize = true
	config := objectSenderConfig(t, source)
	sender, err := PrepareSender(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	initial := config.CatalogBudget.Snapshot().Used
	directory, _ := source.root.DirectoryID()
	_, err = sender.catalogStore.ListChildren(context.Background(), directory, testCatalogBudget(), catalog.ScanOptions{}, sender.source)
	var typed *catalog.SourceError
	if !errors.As(err, &typed) || typed.Failure != catalog.SourceFailureUnsupported || !errors.Is(err, errObjectSizeUnavailable) {
		t.Fatalf("unknown metadata = %v", err)
	}
	if _, committed, err := sender.catalogStore.Directory(context.Background(), directory); err != nil || committed {
		t.Fatalf("unknown exact size published a guessed file: committed=%v err=%v", committed, err)
	}
	after := config.CatalogBudget.Snapshot().Used
	if after.Entries != initial.Entries || after.ActiveScans != 0 || after.SpillBytes != 0 || source.opens != 0 {
		t.Fatalf("failed metadata discovery retained provisional authority: initial=%+v after=%+v", initial, after)
	}
	if err := sender.Close(); err != nil {
		t.Fatal(err)
	}
	if source.closed != 1 || source.closedWhileActive || config.CatalogBudget.Snapshot().Used != (catalog.ResourceUsage{}) {
		t.Fatal("unknown-metadata cleanup retained share resources")
	}
}
