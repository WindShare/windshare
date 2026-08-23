package liveshare

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/session/contentflow"
)

type lifecycleRevisionCatalog struct {
	id     catalog.NodeID
	record catalog.NodeRecord
}

func (catalogSource lifecycleRevisionCatalog) Node(
	_ context.Context,
	id catalog.NodeID,
) (catalog.NodeRecord, bool, error) {
	if catalogSource.id != id {
		return catalog.NodeRecord{}, false, nil
	}
	return catalogSource.record, true, nil
}

type lifecycleRevisionSource struct {
	stable content.StableFile
}

func (source lifecycleRevisionSource) OpenStable(
	context.Context,
	catalog.NodeRecord,
) (content.StableFile, error) {
	return source.stable, nil
}

type lifecycleStableFile struct {
	data        []byte
	readStarted chan struct{}
	allowRead   <-chan struct{}
	startOnce   sync.Once
	closed      atomic.Bool
}

func (file *lifecycleStableFile) ExactSize() uint64 {
	return uint64(len(file.data))
}

func (*lifecycleStableFile) ModifiedTime() catalog.ModifiedTime {
	return catalog.ModifiedTime{}
}

func (file *lifecycleStableFile) Verify(context.Context) error {
	if file.closed.Load() {
		return content.ErrRevisionUnreadable
	}
	return nil
}

func (file *lifecycleStableFile) ReadAt(
	_ context.Context,
	destination []byte,
	offset uint64,
) (int, error) {
	if file.readStarted != nil {
		file.startOnce.Do(func() { close(file.readStarted) })
	}
	if file.allowRead != nil {
		<-file.allowRead
	}
	if offset >= uint64(len(file.data)) {
		return 0, io.EOF
	}
	count := copy(destination, file.data[offset:])
	if count != len(destination) {
		return count, io.EOF
	}
	return count, nil
}

func (file *lifecycleStableFile) Close() error {
	file.closed.Store(true)
	return nil
}

type observedRevisionDeriver struct {
	delegate *content.HMACRevisionIdentityDeriver

	deriveStarted chan struct{}
	allowDerive   <-chan struct{}
	deriveExited  chan struct{}
	startOnce     sync.Once
	exitOnce      sync.Once

	workerExited               <-chan struct{}
	destroyed                  chan struct{}
	destroyOnce                sync.Once
	destroyedBeforeWorkerDrain atomic.Bool
}

func (deriver *observedRevisionDeriver) DeriveRevision(
	evidence content.RevisionEvidence,
) (content.FileRevision, error) {
	if deriver.deriveStarted != nil {
		deriver.startOnce.Do(func() { close(deriver.deriveStarted) })
	}
	if deriver.allowDerive != nil {
		<-deriver.allowDerive
	}
	revision, err := deriver.delegate.DeriveRevision(evidence)
	if deriver.deriveExited != nil {
		deriver.exitOnce.Do(func() { close(deriver.deriveExited) })
	}
	return revision, err
}

func (deriver *observedRevisionDeriver) Destroy() {
	deriver.destroyOnce.Do(func() {
		if deriver.workerExited != nil {
			select {
			case <-deriver.workerExited:
			default:
				deriver.destroyedBeforeWorkerDrain.Store(true)
			}
		}
		deriver.delegate.Destroy()
		close(deriver.destroyed)
	})
}

type lifecycleRevisionFixture struct {
	share catalog.ShareInstance
	file  catalog.FileID
	store *content.RevisionStore
}

func newLifecycleRevisionFixture(
	t *testing.T,
	deriver content.RevisionIdentityDeriver,
	stable content.StableFile,
) lifecycleRevisionFixture {
	t.Helper()
	share := lifecycleIdentity[catalog.ShareInstance](0x31)
	file := lifecycleIdentity[catalog.FileID](0x32)
	parent := lifecycleIdentity[catalog.DirectoryID](0x33)
	locator, err := catalog.NewLocator(0, "lifecycle.bin")
	if err != nil {
		t.Fatal(err)
	}
	sourceIdentity, err := catalog.NewSourceIdentity([]byte("liveshare-lifecycle-source"))
	if err != nil {
		t.Fatal(err)
	}
	version, err := catalog.NewVersionCandidate([]byte("liveshare-lifecycle-version"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := catalog.NewFileNodeRecord(
		file,
		parent,
		"lifecycle.bin",
		locator,
		sourceIdentity,
		version,
		uint64(catalog.MinChunkSize),
		catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	capacityOwner, err := revisioncapacity.NewProcessOwner(revisioncapacity.DefaultProcessConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := capacityOwner.Close(); err != nil {
			t.Errorf("close lifecycle capacity owner: %v", err)
		}
	})
	metadataBudget, err := content.NewRevisionMetadataBudget(
		content.DefaultRevisionInvalidationEntries,
	)
	if err != nil {
		t.Fatal(err)
	}
	store, err := content.NewRevisionStore(content.RevisionStoreConfig{
		ShareInstance:       share,
		ChunkSize:           catalog.MinChunkSize,
		Catalog:             lifecycleRevisionCatalog{id: file.NodeID(), record: record},
		Source:              lifecycleRevisionSource{stable: stable},
		CapacityCoordinator: capacityOwner.Coordinator(),
		CapacityStore: revisioncapacity.StoreConfig{
			StoreID: "liveshare-lifecycle-store", ShareID: "liveshare-lifecycle-share",
			Limits: revisioncapacity.DefaultShareLimits(),
		},
		RevisionDeriver: deriver,
		MetadataBudget:  metadataBudget,
	})
	if err != nil {
		t.Fatal(err)
	}
	return lifecycleRevisionFixture{share: share, file: file, store: store}
}

func lifecycleIdentity[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var identity T
	for index := range identity {
		identity[index] = seed + byte(index)
	}
	return identity
}

func lifecycleSessionCapacity(
	t *testing.T,
	store *content.RevisionStore,
	name string,
) *revisioncapacity.SessionRegistration {
	t.Helper()
	registration, err := store.RegisterSession(revisioncapacity.SessionConfig{
		SessionID: revisioncapacity.SessionID(name),
		Limits:    revisioncapacity.DefaultSessionLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := registration.Close(); err != nil {
			t.Errorf("close lifecycle session capacity: %v", err)
		}
	})
	return registration
}

func TestPreparedSenderKeepsRevisionKeyAliveUntilOpenWorkerDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		allowDerive := make(chan struct{})
		deriveStarted := make(chan struct{})
		deriveExited := make(chan struct{})
		delegate, err := content.NewHMACRevisionIdentityDeriver(
			content.RevisionIdentityKey{0x41},
		)
		if err != nil {
			t.Fatal(err)
		}
		deriver := &observedRevisionDeriver{
			delegate:      delegate,
			deriveStarted: deriveStarted,
			allowDerive:   allowDerive,
			deriveExited:  deriveExited,
			workerExited:  deriveExited,
			destroyed:     make(chan struct{}),
		}
		stable := &lifecycleStableFile{data: make([]byte, catalog.MinChunkSize)}
		fixture := newLifecycleRevisionFixture(t, deriver, stable)
		sessionCapacity := lifecycleSessionCapacity(t, fixture.store, "liveshare-lifecycle-open-session")

		openDone := make(chan error, 1)
		go func() {
			_, openErr := fixture.store.OpenRevisions(
				context.Background(),
				[]content.OpenRevisionRequest{{FileID: fixture.file}},
				sessionCapacity,
			)
			openDone <- openErr
		}()
		<-deriveStarted

		sender := &PreparedSender{
			revisionStore:   fixture.store,
			revisionDeriver: deriver,
		}
		closeDone := make(chan error, 1)
		go func() { closeDone <- sender.Close() }()
		synctest.Wait()

		select {
		case <-deriver.destroyed:
			t.Fatal("revision identity key was destroyed while an open worker was deriving")
		default:
		}
		select {
		case closeErr := <-closeDone:
			t.Fatalf("sender close returned before the open worker drained: %v", closeErr)
		default:
		}

		close(allowDerive)
		if closeErr := <-closeDone; closeErr != nil {
			t.Fatal(closeErr)
		}
		<-deriver.destroyed
		if deriver.destroyedBeforeWorkerDrain.Load() {
			t.Fatal("revision identity key destruction raced the open worker")
		}
		<-openDone
	})
}

func TestPreparedSenderKeepsRevisionKeyAliveUntilCacheReadWorkerDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		delegate, err := content.NewHMACRevisionIdentityDeriver(
			content.RevisionIdentityKey{0x51},
		)
		if err != nil {
			t.Fatal(err)
		}
		deriver := &observedRevisionDeriver{
			delegate:  delegate,
			destroyed: make(chan struct{}),
		}
		allowRead := make(chan struct{})
		readStarted := make(chan struct{})
		stable := &lifecycleStableFile{
			data:        make([]byte, catalog.MinChunkSize),
			readStarted: readStarted,
			allowRead:   allowRead,
		}
		fixture := newLifecycleRevisionFixture(t, deriver, stable)
		sessionCapacity := lifecycleSessionCapacity(t, fixture.store, "liveshare-lifecycle-read-session")
		opened, err := fixture.store.OpenRevisions(
			context.Background(),
			[]content.OpenRevisionRequest{{FileID: fixture.file}},
			sessionCapacity,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(opened) != 1 || opened[0].Err != nil {
			t.Fatalf("open result = %+v", opened)
		}
		lease := opened[0].Lease
		ref, err := content.NewBlockRef(
			fixture.file,
			lease.Descriptor().FileRevision(),
			0,
			lease.Descriptor().Geometry(),
		)
		if err != nil {
			t.Fatal(err)
		}
		processCache, err := contentflow.NewProcessCacheBudget(uint64(catalog.MinChunkSize))
		if err != nil {
			t.Fatal(err)
		}
		cache, err := contentflow.NewSharedBlockCache(
			fixture.share,
			uint64(catalog.MinChunkSize),
			processCache,
		)
		if err != nil {
			t.Fatal(err)
		}
		key, err := contentflow.NewBlockCacheKey(lease.Descriptor(), 0)
		if err != nil {
			t.Fatal(err)
		}
		readWorkerExited := make(chan struct{})
		deriver.workerExited = readWorkerExited
		cacheDone := make(chan error, 1)
		go func() {
			_, cacheErr := cache.Get(context.Background(), key, func(loadContext context.Context) ([]byte, error) {
				block, readErr := fixture.store.ReadBlock(loadContext, lease.ID(), ref)
				close(readWorkerExited)
				return block, readErr
			})
			cacheDone <- cacheErr
		}()
		<-readStarted

		sender := &PreparedSender{
			cache:           cache,
			revisionStore:   fixture.store,
			revisionDeriver: deriver,
		}
		closeDone := make(chan error, 1)
		go func() { closeDone <- sender.Close() }()
		synctest.Wait()

		select {
		case <-deriver.destroyed:
			t.Fatal("revision identity key was destroyed while a cache-backed read worker was active")
		default:
		}
		select {
		case closeErr := <-closeDone:
			t.Fatalf("sender close returned before the cache/read worker drained: %v", closeErr)
		default:
		}

		close(allowRead)
		if closeErr := <-closeDone; closeErr != nil {
			t.Fatal(closeErr)
		}
		<-deriver.destroyed
		if deriver.destroyedBeforeWorkerDrain.Load() {
			t.Fatal("revision identity key destruction raced the cache-backed read worker")
		}
		<-cacheDone
	})
}
