package sendersession

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/internal/keyderiv"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/senderauth"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/core/transfer"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func connectVerticalPair(
	t *testing.T,
	senderFactory *Factory,
	receiverFactory *receiversession.Factory,
) (*Runtime, *receiversession.Runtime) {
	t.Helper()
	senderChannel, receiverChannel := newMemoryChannelPair()
	accepted := make(chan struct {
		runtime *Runtime
		err     error
	}, 1)
	go func() {
		runtime, err := senderFactory.Accept(context.Background(), senderChannel)
		accepted <- struct {
			runtime *Runtime
			err     error
		}{runtime: runtime, err: err}
	}()
	receiver, err := receiverFactory.Connect(context.Background(), receiverChannel, transferlanes.RouteRelay)
	if err != nil {
		t.Fatalf("connect receiver: %v", err)
	}
	result := <-accepted
	if result.err != nil {
		receiver.Close()
		t.Fatalf("accept sender: %v", result.err)
	}
	return result.runtime, receiver
}

func newMemoryChannel(t *testing.T) *memoryChannel {
	t.Helper()
	channel, peer := newMemoryChannelPair()
	t.Cleanup(func() {
		_ = channel.Close()
		_ = peer.Close()
	})
	return channel
}

type verticalFixture struct {
	share            catalog.ShareInstance
	syntheticRoot    catalog.DirectoryID
	directoryID      catalog.DirectoryID
	fileID           catalog.FileID
	fileData         []byte
	pkHash           []byte
	shareIDRaw       []byte
	descriptorKey    []byte
	descriptorObject []byte
	senderFactory    *Factory
	receiverFactory  *receiversession.Factory
	receiverConfig   receiversession.FactoryConfig
	contentStore     *verticalContentStore
	capacityOwner    *revisioncapacity.ProcessOwner
	capacityStore    *revisioncapacity.StoreRegistration
	terminal         *verticalTerminalConnectivity
	scanCalls        atomic.Int32
	scanStarted      chan struct{}
	scanGate         chan struct{}
	scanStopped      chan struct{}
}

func (fixture *verticalFixture) registerCapacitySession(
	sessionID protocolsession.ProtocolSessionID,
) (*revisioncapacity.SessionRegistration, error) {
	return fixture.capacityStore.RegisterSession(revisioncapacity.SessionConfig{
		SessionID: revisioncapacity.SessionID(base64.RawURLEncoding.EncodeToString(sessionID.Bytes())),
		Limits:    revisioncapacity.DefaultSessionLimits(),
	})
}

func newVerticalFixture(t *testing.T) *verticalFixture {
	t.Helper()
	return newVerticalFixtureWithContent(t, bytes.Repeat([]byte("windshare-v2-"), 193), catalog.MinChunkSize)
}

func newVerticalFixtureWithContent(t *testing.T, data []byte, chunkSize uint32) *verticalFixture {
	t.Helper()
	fixture := &verticalFixture{
		share: id16[catalog.ShareInstance](1), syntheticRoot: id16[catalog.DirectoryID](2),
		directoryID: id16[catalog.DirectoryID](3), fileID: id16[catalog.FileID](4),
		fileData:    data,
		scanStarted: make(chan struct{}), scanGate: make(chan struct{}),
	}
	fixture.terminal = &verticalTerminalConnectivity{}
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	sender := checkedSender(t, publicKey)
	pkHash, err := link.SenderKeyHash(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	shareID, err := link.ShareIDForSenderKeyHash(pkHash[:])
	if err != nil {
		t.Fatal(err)
	}
	shareIDRaw, err := base64.RawURLEncoding.Strict().DecodeString(shareID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.pkHash, fixture.shareIDRaw = pkHash[:], shareIDRaw
	readSecret := bytes.Repeat([]byte{5}, link.ReadSecretBytes)
	catalogKey, _ := keyderiv.V2CatalogKey(readSecret, fixture.share.Bytes())
	descriptorKey, _ := keyderiv.V2DescriptorKey(readSecret, pkHash[:])
	sessionAuthKey, _ := keyderiv.V2SessionAuthKey(readSecret, fixture.share.Bytes())
	fixture.descriptorKey = descriptorKey
	objects, err := catalogflow.NewSealedCatalogStore(catalogflow.SealedCatalogStoreConfig{
		ShareInstance: fixture.share, CatalogKey: catalogKey, SenderPrivateKey: privateKey,
		NonceSource: &deterministicReader{next: 11},
	})
	if err != nil {
		t.Fatal(err)
	}
	processBudget, _ := catalog.NewBudgetAccount("process", catalog.DefaultProcessBudgetLimits())
	shareBudget, _ := catalog.NewBudgetAccount("share", catalog.DefaultShareBudgetLimits())
	startupBudget, _ := catalog.NewBudgetAccount("startup", catalog.DefaultSessionBudgetLimits())
	store, err := catalog.NewCatalogStore(catalog.StoreConfig{
		ShareInstance: fixture.share, Backend: catalog.NewMemoryCatalogBackend(),
		ProcessBudget: processBudget, ShareBudget: shareBudget, PageSealer: objects,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	locator, _ := catalog.NewSourceReference([]byte("object:" + ""))
	identity, _ := catalog.NewSourceIdentity([]byte("selected-directory"))
	selected, err := catalog.NewDirectoryNodeRecord(
		fixture.directoryID, fixture.syntheticRoot, "folder", locator, identity, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rootCommit, err := catalog.NewSyntheticRootCommit(catalog.SyntheticRootCommitSpec{
		ShareInstance: fixture.share, SyntheticRoot: fixture.syntheticRoot,
		Generation: id16[catalog.DirectoryGeneration](6), SelectedRoots: []catalog.NodeRecord{selected},
	})
	if err != nil {
		t.Fatal(err)
	}
	committedRoot, err := store.CommitSyntheticRoot(context.Background(), rootCommit, startupBudget)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := catalog.NewShareDescriptor(catalog.DescriptorSpec{
		WireVersion: catalog.WireVersionV2, Suite: catalog.SuiteV2, ShareInstance: fixture.share,
		SyntheticRoot: fixture.syntheticRoot, RootCommit: committedRoot, ChunkSize: chunkSize,
		Capabilities:    catalog.CapabilityCatalog | catalog.CapabilityRanges,
		SenderPublicKey: publicKey, CreatedAtSeconds: 1, PathPolicy: catalog.PathPolicyV1,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.descriptorObject, err = catalogflow.SealDescriptor(descriptor, catalogflow.DescriptorObjectConfig{
		PKHash: pkHash[:], ShareIDRaw: shareIDRaw, DescriptorKey: descriptorKey,
		SenderPrivateKey: privateKey, Nonce: bytes.Repeat([]byte{3}, 12),
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionBudget, _ := catalog.NewBudgetAccount("catalog-session", catalog.DefaultSessionBudgetLimits())
	scanner := catalog.DirectoryScannerFunc(func(ctx context.Context, request catalog.ScanRequest) (catalog.ScanResult, error) {
		fixture.scanCalls.Add(1)
		select {
		case <-fixture.scanStarted:
		default:
			close(fixture.scanStarted)
		}
		select {
		case <-ctx.Done():
			if fixture.scanStopped != nil {
				select {
				case fixture.scanStopped <- struct{}{}:
				default:
				}
			}
			return catalog.ScanResult{}, ctx.Err()
		case <-fixture.scanGate:
		}
		fileLocator, _ := catalog.NewSourceReference([]byte("object:" + "file.bin"))
		fileIdentity, _ := catalog.NewSourceIdentity([]byte("file-object"))
		candidate, _ := catalog.NewVersionCandidate([]byte("file-version"))
		err := request.Children.Add(ctx, catalog.ScannedChild{
			FileID: fixture.fileID, Name: "file.bin", SourceReference: fileLocator,
			SourceIdentity: fileIdentity, VersionCandidate: candidate, ExpectedSize: uint64(len(fixture.fileData)),
		})
		return catalog.ScanResult{}, err
	})
	source, err := catalogflow.NewCatalogStoreSource(catalogflow.CatalogStoreSourceConfig{
		ShareInstance: fixture.share, Store: store, SessionBudget: sessionBudget, Scanner: scanner,
	})
	if err != nil {
		t.Fatal(err)
	}
	catalogService, _ := catalogflow.NewAddressedSenderService(fixture.share, source)
	keyTree, _ := content.NewKeyTree(readSecret, fixture.share)
	recordSealer, _ := records.NewSealer(records.SealerConfig{
		ShareInstance: fixture.share, Keys: keyTree, SigningKey: privateKey,
		NonceSource: &deterministicReader{next: 13},
	})
	recordOpener, _ := records.NewOpener(records.OpenerConfig{
		ShareInstance: fixture.share, Keys: keyTree, Sender: sender,
	})
	geometry, _ := content.NewFileGeometry(uint64(len(fixture.fileData)), chunkSize)
	revisionDescriptor, _ := content.NewFileRevisionDescriptor(
		fixture.share, fixture.fileID, id16[content.FileRevision](8), geometry, catalog.ModifiedTime{},
	)
	lease, _ := content.NewRevisionLease(
		id16[content.LeaseID](9), revisionDescriptor,
		contentflow.RevisionLeaseTTL, contentflow.RevisionLeaseRenewAfter,
	)
	fixture.contentStore = &verticalContentStore{descriptor: revisionDescriptor, lease: lease, data: fixture.fileData}
	fixture.capacityOwner, err = revisioncapacity.NewProcessOwner(revisioncapacity.DefaultProcessConfig())
	if err != nil {
		t.Fatal(err)
	}
	fixture.capacityStore, err = fixture.capacityOwner.Coordinator().RegisterStore(revisioncapacity.StoreConfig{
		StoreID: "sessionruntime-vertical-store", ShareID: "sessionruntime-vertical-share",
		Limits: revisioncapacity.DefaultShareLimits(),
	}, verticalCapacityTarget{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = fixture.capacityStore.Close()
		_ = fixture.capacityOwner.Close()
	})
	cacheBudget, _ := contentflow.NewProcessCacheBudget(64 << 20)
	cache, _ := contentflow.NewSharedBlockCache(fixture.share, 16<<20, cacheBudget)
	t.Cleanup(cache.Close)
	contentFactory := ContentFactoryFunc(func(sessionID protocolsession.ProtocolSessionID) (*contentflow.SenderService, error) {
		sessionCapacity, err := fixture.registerCapacitySession(sessionID)
		if err != nil {
			return nil, err
		}
		return contentflow.NewSenderService(contentflow.SenderServiceConfig{
			Store: fixture.contentStore, SessionCapacity: sessionCapacity, Sealer: recordSealer, Cache: cache,
		})
	})
	fixture.senderFactory, err = NewFactory(FactoryConfig{
		ShareInstance: fixture.share, SessionAuthKey: sessionAuthKey, SenderPrivateKey: privateKey,
		Catalog: CatalogFactoryFunc(func() (*catalogflow.AddressedSenderService, error) {
			return catalogService, nil
		}), Content: contentFactory, Peers: inertSenderPeerFactory(),
		Random: &deterministicReader{next: 17}, TerminalConnectivity: fixture.terminal, TerminalTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier, _ := catalogflow.NewCatalogObjectVerifier(catalogflow.CatalogObjectVerifierConfig{
		ShareInstance: fixture.share, CatalogKey: catalogKey, Sender: sender,
	})
	processReassembly, _ := contentflow.NewReassemblyAccount("process", contentflow.ReassemblyLimits{Bytes: 1 << 30, Records: 256})
	shareReassembly, _ := contentflow.NewReassemblyAccount("share", contentflow.ReassemblyLimits{Bytes: 256 << 20, Records: 64})
	plaintext, _ := transfer.NewPlaintextBudget(256 << 20)
	fixture.receiverConfig = receiversession.FactoryConfig{
		Descriptor: descriptor, SessionAuthKey: sessionAuthKey, Sender: sender,
		CatalogVerifier: verifier, RecordOpener: recordOpener,
		ReassemblyProcess: processReassembly, ReassemblyShare: shareReassembly, PlaintextProcess: plaintext,
		Random: &deterministicReader{next: 19},
	}
	fixture.receiverFactory, err = receiversession.NewFactory(fixture.receiverConfig)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

type verticalContentStore struct {
	descriptor content.FileRevisionDescriptor
	lease      content.RevisionLease
	data       []byte
	openErr    error
	readErr    error
	blockReads atomic.Int32
	renewCalls atomic.Int32
	blockStart chan uint64
	blockGate  <-chan struct{}
	blockStop  chan struct{}
	renewStart chan<- struct{}
	renewGate  <-chan struct{}
	ended      chan<- verticalLeaseEnd
}

type verticalLeaseEnd struct {
	id   content.LeaseID
	kind content.LeaseEndKind
}

func (store *verticalContentStore) OpenRevisions(
	_ context.Context,
	requests []content.OpenRevisionRequest,
	_ *revisioncapacity.SessionRegistration,
) ([]content.OpenRevisionResult, error) {
	results := make([]content.OpenRevisionResult, len(requests))
	for index, request := range requests {
		results[index] = content.OpenRevisionResult{FileID: request.FileID}
		if request.FileID == store.descriptor.FileID() && store.openErr != nil {
			results[index].Err = store.openErr
		} else if request.FileID == store.descriptor.FileID() {
			results[index].Lease = store.lease
		} else {
			results[index].Err = errors.New("not found")
		}
	}
	return results, nil
}

type verticalCapacityTarget struct{}

func (verticalCapacityTarget) ReclaimIdle(
	_ context.Context,
	claim revisioncapacity.ReclaimClaim,
) revisioncapacity.ReclaimResult {
	return revisioncapacity.ReclaimDeclined(claim)
}

func (store *verticalContentStore) RenewLease(id content.LeaseID) (content.RevisionLease, error) {
	if id != store.lease.ID() {
		return content.RevisionLease{}, errors.New("unknown lease")
	}
	store.renewCalls.Add(1)
	if store.renewStart != nil {
		store.renewStart <- struct{}{}
	}
	if store.renewGate != nil {
		<-store.renewGate
	}
	return store.lease, nil
}

func (store *verticalContentStore) EndLease(leaseID content.LeaseID, kind content.LeaseEndKind) error {
	if store.ended != nil {
		store.ended <- verticalLeaseEnd{id: leaseID, kind: kind}
	}
	return nil
}

func (store *verticalContentStore) ValidateLease(id content.LeaseID, descriptor content.FileRevisionDescriptor) error {
	if id != store.lease.ID() || descriptor.FileRevision() != store.descriptor.FileRevision() {
		return errors.New("invalid lease")
	}
	return nil
}

func (store *verticalContentStore) ReadBlock(
	ctx context.Context,
	id content.LeaseID,
	ref content.BlockRef,
) ([]byte, error) {
	if id != store.lease.ID() || ref.FileRevision() != store.descriptor.FileRevision() {
		return nil, errors.New("invalid block")
	}
	if store.readErr != nil {
		return nil, store.readErr
	}
	offset, err := store.descriptor.Geometry().BlockOffset(ref.LocalBlockIndex())
	if err != nil {
		return nil, err
	}
	length, err := store.descriptor.Geometry().BlockPlainLength(ref.LocalBlockIndex())
	if err != nil {
		return nil, err
	}
	if store.blockStart != nil {
		select {
		case store.blockStart <- ref.LocalBlockIndex():
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if store.blockGate != nil {
		select {
		case <-store.blockGate:
		case <-ctx.Done():
			if store.blockStop != nil {
				select {
				case store.blockStop <- struct{}{}:
				default:
				}
			}
			return nil, ctx.Err()
		}
	}
	store.blockReads.Add(1)
	return bytes.Clone(store.data[offset : offset+uint64(length)]), nil
}

type verticalTerminalConnectivity struct {
	recoveryStops atomic.Int32
	cleanups      atomic.Int32
}

func (connectivity *verticalTerminalConnectivity) StopRecovery() {
	connectivity.recoveryStops.Add(1)
}

func (connectivity *verticalTerminalConnectivity) Cleanup(context.Context) error {
	connectivity.cleanups.Add(1)
	return nil
}

type memoryPipe struct {
	mu     sync.Mutex
	inbox  [2]chan framechannel.Frame
	closed [2]bool
}

type deterministicReader struct {
	mu   sync.Mutex
	next byte
}

func (reader *deterministicReader) Read(destination []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	for index := range destination {
		destination[index] = reader.next
	}
	reader.next++
	if reader.next == 0 {
		reader.next = 1
	}
	return len(destination), nil
}

type memoryChannel struct {
	pipe      *memoryPipe
	index     int
	recvCalls atomic.Int32
	onSend    func(framechannel.Frame)
}

func newMemoryChannelPair() (*memoryChannel, *memoryChannel) {
	pipe := &memoryPipe{inbox: [2]chan framechannel.Frame{make(chan framechannel.Frame, 2_048), make(chan framechannel.Frame, 2_048)}}
	return &memoryChannel{pipe: pipe, index: 0}, &memoryChannel{pipe: pipe, index: 1}
}

func (channel *memoryChannel) Send(_ context.Context, frame framechannel.Frame) error {
	channel.pipe.mu.Lock()
	defer channel.pipe.mu.Unlock()
	target := 1 - channel.index
	if channel.pipe.closed[channel.index] || channel.pipe.closed[target] {
		return io.ErrClosedPipe
	}
	channel.pipe.inbox[target] <- bytes.Clone(frame)
	if channel.onSend != nil {
		channel.onSend(bytes.Clone(frame))
	}
	return nil
}

func (channel *memoryChannel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	return channel.Send(ctx, frame)
}

func (channel *memoryChannel) Recv() <-chan framechannel.Frame {
	channel.recvCalls.Add(1)
	return channel.pipe.inbox[channel.index]
}

func (channel *memoryChannel) State() framechannel.ChannelState {
	channel.pipe.mu.Lock()
	defer channel.pipe.mu.Unlock()
	if channel.pipe.closed[channel.index] {
		return framechannel.Closed
	}
	return framechannel.Open
}

func (channel *memoryChannel) Close() error {
	channel.pipe.mu.Lock()
	if !channel.pipe.closed[channel.index] {
		channel.pipe.closed[channel.index] = true
		close(channel.pipe.inbox[channel.index])
	}
	channel.pipe.mu.Unlock()
	return nil
}

func id16[T ~[16]byte](value byte) T {
	var result T
	for index := range result {
		result[index] = value
	}
	return result
}

func checkedSender(t testing.TB, publicKey []byte) *senderauth.Verifier {
	t.Helper()
	sender, err := senderauth.NewVerifier(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return sender
}
