package sessionruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/internal/keyderiv"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

func TestCompositeRuntimesBrowseAndTransferFileLocalBlocks(t *testing.T) {
	fixture := newVerticalFixture(t)
	senderChannel, receiverChannel := newMemoryChannelPair()
	senderResult := make(chan struct {
		runtime *SenderRuntime
		err     error
	}, 1)
	go func() {
		runtime, err := fixture.senderFactory.Accept(context.Background(), senderChannel)
		senderResult <- struct {
			runtime *SenderRuntime
			err     error
		}{runtime: runtime, err: err}
	}()
	receiver, err := fixture.receiverFactory.Connect(context.Background(), receiverChannel)
	if err != nil {
		t.Fatalf("connect receiver: %v", err)
	}
	t.Cleanup(receiver.Close)
	senderAccepted := <-senderResult
	if senderAccepted.err != nil {
		t.Fatalf("accept sender: %v", senderAccepted.err)
	}
	sender := senderAccepted.runtime
	t.Cleanup(sender.Close)

	root, err := receiver.Catalog().LoadDirectory(context.Background(), fixture.syntheticRoot)
	if err != nil {
		t.Fatalf("load synthetic root: %v", err)
	}
	if root.EntryCount() != 1 || fixture.scanCalls.Load() != 0 {
		t.Fatalf("root entries/descendant scans = %d/%d", root.EntryCount(), fixture.scanCalls.Load())
	}

	directoryResult := make(chan error, 1)
	go func() {
		_, loadErr := receiver.Catalog().LoadDirectory(context.Background(), fixture.directoryID)
		directoryResult <- loadErr
	}()
	select {
	case <-fixture.scanStarted:
	case err := <-directoryResult:
		t.Fatalf("child directory failed before scan: %v", err)
	case <-time.After(time.Second):
		t.Fatalf("lazy scan did not start; sender=%v receiver=%v", sender.Err(), receiver.Err())
	}
	if fixture.scanCalls.Load() != 1 {
		t.Fatalf("scan calls = %d", fixture.scanCalls.Load())
	}
	close(fixture.scanGate)
	if err := <-directoryResult; err != nil {
		t.Fatalf("load child directory: %v", err)
	}

	opened, err := receiver.OpenRevision(context.Background(), fixture.fileID)
	if err != nil {
		t.Fatalf("open revision: %v", err)
	}
	output := make([]byte, len(fixture.fileData))
	err = receiver.BlockBroker().ReadRange(
		context.Background(), opened.LeaseID, opened.Descriptor,
		content.Range{Offset: 0, End: uint64(len(output))},
		transfer.RangeSinkFunc(func(_ context.Context, offset uint64, data []byte) error {
			copy(output[offset:], data)
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("read range: %v", err)
	}
	if !bytes.Equal(output, fixture.fileData) {
		t.Fatal("received file differs")
	}
	if got := fixture.contentStore.blockReads.Load(); got != 3 {
		t.Fatalf("file-local block reads = %d, want 3", got)
	}
	if err := receiver.ReleaseRevision(context.Background(), opened.LeaseID); err != nil {
		t.Fatalf("release revision: %v", err)
	}

	if err := sender.Stop(context.Background(), "Sender stopped"); err != nil {
		t.Fatalf("ordered sender stop: %v", err)
	}
	select {
	case <-receiver.Done():
	case <-time.After(time.Second):
		t.Fatal("receiver did not observe authenticated terminal")
	}
	if senderChannel.recvCalls.Load() != 1 || receiverChannel.recvCalls.Load() != 1 {
		t.Fatalf("underlying Recv calls = sender %d receiver %d", senderChannel.recvCalls.Load(), receiverChannel.recvCalls.Load())
	}
}

func TestCompositeRuntimeDeliversAuthenticatedScanProgressBeforeCatalogResult(t *testing.T) {
	fixture := newVerticalFixture(t)
	progressStarted := make(chan CatalogScanProgress, 1)
	releaseProgress := make(chan struct{})
	receiverConfig := fixture.receiverConfig
	receiverConfig.CatalogProgress = CatalogScanProgressObserverFunc(func(
		ctx context.Context,
		progress CatalogScanProgress,
	) error {
		progressStarted <- progress
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-releaseProgress:
			return nil
		}
	})
	receiverFactory, err := NewReceiverFactory(receiverConfig)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	result := make(chan error, 1)
	go func() {
		_, err := receiver.Catalog().LoadDirectory(context.Background(), fixture.directoryID)
		result <- err
	}()
	<-fixture.scanStarted
	close(fixture.scanGate)
	progress := <-progressStarted
	if progress.DirectoryID != fixture.directoryID || progress.AttemptID.IsZero() ||
		progress.DiscoveredEntries != 1 {
		t.Fatalf("scan progress = %+v", progress)
	}
	select {
	case err := <-result:
		t.Fatalf("catalog result crossed blocked progress: %v", err)
	default:
	}
	close(releaseProgress)
	if err := <-result; err != nil {
		t.Fatalf("load directory after progress: %v", err)
	}
}

func TestDescriptorObjectBootstrapAndFactoryValidation(t *testing.T) {
	fixture := newVerticalFixture(t)
	opened, err := catalogflow.OpenDescriptor(
		fixture.descriptorObject, fixture.pkHash, fixture.shareIDRaw, fixture.descriptorKey,
	)
	if err != nil {
		t.Fatalf("open descriptor: %v", err)
	}
	if opened.ShareInstance() != fixture.share || opened.SyntheticRoot() != fixture.syntheticRoot {
		t.Fatal("descriptor identity changed")
	}
	tampered := bytes.Clone(fixture.descriptorObject)
	tampered[len(tampered)-1] ^= 1
	if _, err := catalogflow.OpenDescriptor(tampered, fixture.pkHash, fixture.shareIDRaw, fixture.descriptorKey); err == nil {
		t.Fatal("tampered descriptor accepted")
	}
	bad := fixture.receiverConfig
	bad.SenderPublicKey = ed25519.PublicKey(bytes.Repeat([]byte{9}, ed25519.PublicKeySize))
	if _, err := NewReceiverFactory(bad); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("bad receiver factory error = %v", err)
	}
}

func TestCompositeRuntimeRoutesSignedPeerControlsAndCancellationOnTheOfferOperation(t *testing.T) {
	fixture := newVerticalFixture(t)
	answerBody, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1), 1: "answer"})
	localCandidateBody, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1), 1: "sender-candidate"})
	remoteCandidateBody, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1), 1: "receiver-candidate"})
	rejectedOfferBody, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1), 1: "reject-offer"})
	created := make(chan *verticalPeerHandler, 1)
	fixture.senderFactory.peers = SenderPeerHandlerFactoryFunc(func(session SenderPeerSession) (SenderPeerHandler, error) {
		handler := &verticalPeerHandler{
			session: session, answer: answerBody, localCandidate: localCandidateBody, rejectedOffer: rejectedOfferBody,
			remoteCandidates: make(chan protocolsession.Message, 1), canceled: make(chan protocolsession.OperationID, 1),
		}
		created <- handler
		return handler, nil
	})
	receiverConfig := fixture.receiverConfig
	receiverConfig.PeerControls = receiverPeerSemanticsForTest(protocolsession.SenderControlSemanticValidatorFunc(func(
		kind protocolsession.MessageKind,
		_ protocolsession.OperationID,
		semantic []byte,
	) error {
		switch kind {
		case protocolsession.MessagePeerAnswer:
			if bytes.Equal(semantic, answerBody) {
				return nil
			}
		case protocolsession.MessagePeerCandidate:
			if bytes.Equal(semantic, localCandidateBody) {
				return nil
			}
		}
		return protocolsession.ErrControlSemantic
	}))
	receiverFactory, err := NewReceiverFactory(receiverConfig)
	if err != nil {
		t.Fatalf("create peer-aware receiver: %v", err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	handler := <-created

	offerBody, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1), 1: "offer"})
	operation, err := receiver.OpenPeerOperation(context.Background(), offerBody)
	if err != nil {
		t.Fatalf("begin peer offer: %v", err)
	}
	operationID := operation.OperationID()
	answerResult := operation.Receive(context.Background())
	answer := requireReceiverPeerControl(t, answerResult)
	if answer.Kind() != protocolsession.MessagePeerAnswer {
		t.Fatalf("signed peer answer kind = %d", answer.Kind())
	}
	if !bytes.Equal(answer.Body(), answerBody) {
		t.Fatalf("signed peer answer semantic = %x", answer.Body())
	}
	candidateResult := operation.Receive(context.Background())
	candidate := requireReceiverPeerControl(t, candidateResult)
	if candidate.Kind() != protocolsession.MessagePeerCandidate {
		t.Fatalf("signed peer candidate kind = %d", candidate.Kind())
	}
	if !bytes.Equal(candidate.Body(), localCandidateBody) {
		t.Fatalf("signed peer candidate semantic = %x", candidate.Body())
	}

	if _, err := operation.SendCandidate(context.Background(), remoteCandidateBody); err != nil {
		t.Fatalf("remote candidate delivery: %v", err)
	}
	if received := <-handler.remoteCandidates; received.Kind() != protocolsession.MessagePeerCandidate {
		t.Fatalf("sender peer handler received kind %d", received.Kind())
	}

	assertReceiverPeerTermination(
		t,
		operation,
		operation.Terminate(context.Background()),
		ReceiverPeerTerminalAuthorityLocal,
		ReceiverPeerProvenanceLocalExplicitStop,
		ReceiverPeerTerminalOperationOnly,
		ReceiverPeerProvenanceLocalExplicitStop,
	)
	select {
	case canceled := <-handler.canceled:
		if canceled != operationID {
			t.Fatalf("canceled operation = %x, want %x", canceled, operationID)
		}
	case <-time.After(time.Second):
		t.Fatal("sender peer operation did not observe cancellation")
	}

	rejectedOperation, err := receiver.OpenPeerOperation(context.Background(), rejectedOfferBody)
	if err != nil {
		t.Fatalf("begin rejected peer offer: %v", err)
	}
	rejectedResult := rejectedOperation.Receive(context.Background())
	rejected := requireReceiverPeerTermination(t, rejectedResult)
	assertReceiverPeerTermination(
		t,
		rejectedOperation,
		rejected,
		ReceiverPeerTerminalAuthorityRemote,
		ReceiverPeerProvenanceRemoteOperationRejected,
		ReceiverPeerTerminalOperationOnly,
		ReceiverPeerProvenanceRemoteOperationRejected,
		ReceiverPeerDiagnosticRemoteOperationRejected,
	)
	remote := requireReceiverPeerRemoteFailure(t, rejected, ReceiverPeerDiagnosticRemoteOperationRejected)
	if remote.Code() != protocolsession.PeerOperationCodeNegotiation || remote.Retryable() {
		t.Fatalf("peer operation failure = %#v", remote)
	}
	if _, err := receiver.Catalog().LoadDirectory(context.Background(), fixture.syntheticRoot); err != nil {
		t.Fatalf("catalog failed after isolated peer rejection: %v", err)
	}
}

func TestCompositeRuntimeMultiReceiverStopAndAccessors(t *testing.T) {
	fixture := newVerticalFixture(t)
	firstSender, firstReceiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	secondSender, secondReceiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer firstSender.Close()
	defer firstReceiver.Close()
	defer secondSender.Close()
	defer secondReceiver.Close()

	if fixture.senderFactory.ActiveSessions() != 2 || fixture.senderFactory.String() == "" {
		t.Fatalf("sender registry = %d/%q", fixture.senderFactory.ActiveSessions(), fixture.senderFactory.String())
	}
	if firstSender.ProtocolSessionID() == secondSender.ProtocolSessionID() || firstSender.LaneRegistry() == nil {
		t.Fatal("per-receiver session or lane registry was shared")
	}
	if firstReceiver.Descriptor().ShareInstance() != fixture.share || firstReceiver.LaneSet() == nil {
		t.Fatal("receiver accessors lost authenticated session state")
	}
	laneID, laneEpoch := firstReceiver.LaneIdentity()
	if laneID == 0 || laneEpoch != 0 || firstReceiver.ProtocolSessionID().IsZero() {
		t.Fatalf("receiver lane/session = %d/%d/%x", laneID, laneEpoch, firstReceiver.ProtocolSessionID())
	}
	if _, err := firstReceiver.NewTransferJob(transfer.SelectionRules{}, nil); err == nil {
		t.Fatal("transfer job accepted a nil durable output boundary")
	}

	if err := fixture.senderFactory.Stop(context.Background(), "Maintenance"); err != nil {
		t.Fatalf("stop all receivers: %v", err)
	}
	if fixture.terminal.recoveryStops.Load() != 1 || fixture.terminal.cleanups.Load() != 1 {
		t.Fatalf("terminal connectivity calls = recovery %d cleanup %d",
			fixture.terminal.recoveryStops.Load(), fixture.terminal.cleanups.Load())
	}
	for index, receiver := range []*ReceiverRuntime{firstReceiver, secondReceiver} {
		select {
		case <-receiver.Done():
		case <-time.After(time.Second):
			t.Fatalf("receiver %d did not observe ordered stop", index)
		}
	}
	if _, err := fixture.senderFactory.Accept(context.Background(), newMemoryChannel(t)); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("accept after stop error = %v", err)
	}
}

func TestCompositeRuntimeRenewsAndReleasesLeaseWithInjectedClock(t *testing.T) {
	fixture := newVerticalFixture(t)
	lease, err := content.NewRevisionLease(
		fixture.contentStore.lease.ID(), fixture.contentStore.descriptor,
		contentflow.RevisionLeaseTTL, contentflow.RevisionLeaseRenewAfter,
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.contentStore.lease = lease
	ticks := make(chan time.Time, 4)
	afterCalls := make(chan time.Duration, 4)
	config := fixture.receiverConfig
	config.After = func(delay time.Duration) <-chan time.Time {
		afterCalls <- delay
		return ticks
	}
	receiverFactory, err := NewReceiverFactory(config)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	opened, err := receiver.OpenRevision(context.Background(), fixture.fileID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case delay := <-afterCalls:
		if delay != contentflow.RevisionLeaseRenewAfter {
			t.Fatalf("renew delay = %v", delay)
		}
	case <-time.After(time.Second):
		t.Fatal("renew timer was not armed")
	}
	ticks <- time.Now()
	deadline := time.Now().Add(time.Second)
	for fixture.contentStore.renewCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if fixture.contentStore.renewCalls.Load() != 1 {
		t.Fatalf("renew calls = %d", fixture.contentStore.renewCalls.Load())
	}
	if err := receiver.ReleaseRevision(context.Background(), opened.LeaseID); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRevisionLocalFailureCompensatesTheRemoteLease(t *testing.T) {
	fixture := newVerticalFixture(t)
	released := make(chan content.LeaseID, 1)
	fixture.contentStore.released = released
	localFailure := errors.New("local revision descriptor rejected")
	receiverConfig := fixture.receiverConfig
	receiverConfig.RecordOpener = failingRecordOpener{err: localFailure}
	receiverFactory, err := NewReceiverFactory(receiverConfig)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	if _, err := receiver.OpenRevision(context.Background(), fixture.fileID); !errors.Is(err, localFailure) {
		t.Fatalf("local open failure=%v", err)
	}
	select {
	case leaseID := <-released:
		if leaseID != fixture.contentStore.lease.ID() {
			t.Fatalf("released lease=%x", leaseID)
		}
	case <-time.After(time.Second):
		t.Fatal("local open failure did not release the completed remote lease")
	}
	receiver.rpc.mu.Lock()
	callCount := len(receiver.rpc.calls)
	receiver.rpc.mu.Unlock()
	receiver.revisions.mu.Lock()
	leaseCount := len(receiver.revisions.leases)
	receiver.revisions.mu.Unlock()
	if callCount != 0 || leaseCount != 0 || sender.routes.len() != 0 {
		t.Fatalf("compensation retained calls=%d leases=%d routes=%d", callCount, leaseCount, sender.routes.len())
	}
	if _, err := receiver.RequestLane(context.Background(), 0); err != nil {
		t.Fatalf("lease compensation damaged a sibling operation: %v", err)
	}
}

func TestCompositeRuntimeTransferJobPublishesDurableFilesystemOutput(t *testing.T) {
	fixture := newVerticalFixture(t)
	close(fixture.scanGate)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := osfs.OutputResumeIntentFromBytes(bytes.Repeat([]byte{91}, osfs.OutputResumeIntentBytes))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	outputRoot := t.TempDir()
	opened, err := authority.OpenOrCreate(context.Background(), osfs.FilesystemOutputIntent{
		RootPath: outputRoot, ShareInstance: fixture.share, ResumeIntent: intent,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := receiver.NewTransferJob(rules, opened.Session)
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != transfer.JobSucceeded || result.SucceededFiles != 1 || result.AbortCause != nil {
		t.Fatalf("transfer result = %+v", result)
	}
	written, err := os.ReadFile(filepath.Join(outputRoot, "folder", "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, fixture.fileData) {
		t.Fatal("durably published file changed bytes")
	}
	if fixture.contentStore.blockReads.Load() != 3 {
		t.Fatalf("durable job block reads = %d", fixture.contentStore.blockReads.Load())
	}
}

func connectVerticalPair(
	t *testing.T,
	senderFactory *SenderFactory,
	receiverFactory *ReceiverFactory,
) (*SenderRuntime, *ReceiverRuntime) {
	t.Helper()
	senderChannel, receiverChannel := newMemoryChannelPair()
	accepted := make(chan struct {
		runtime *SenderRuntime
		err     error
	}, 1)
	go func() {
		runtime, err := senderFactory.Accept(context.Background(), senderChannel)
		accepted <- struct {
			runtime *SenderRuntime
			err     error
		}{runtime: runtime, err: err}
	}()
	receiver, err := receiverFactory.Connect(context.Background(), receiverChannel)
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
	senderFactory    *SenderFactory
	receiverFactory  *ReceiverFactory
	receiverConfig   ReceiverFactoryConfig
	contentStore     *verticalContentStore
	terminal         *verticalTerminalConnectivity
	scanCalls        atomic.Int32
	scanStarted      chan struct{}
	scanGate         chan struct{}
	scanStopped      chan struct{}
}

func newVerticalFixture(t *testing.T) *verticalFixture {
	t.Helper()
	fixture := &verticalFixture{
		share: id16[catalog.ShareInstance](1), syntheticRoot: id16[catalog.DirectoryID](2),
		directoryID: id16[catalog.DirectoryID](3), fileID: id16[catalog.FileID](4),
		fileData:    bytes.Repeat([]byte("windshare-v2-"), 193),
		scanStarted: make(chan struct{}), scanGate: make(chan struct{}),
	}
	fixture.terminal = &verticalTerminalConnectivity{}
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
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
	locator, _ := catalog.NewLocator(0, "")
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
		SyntheticRoot: fixture.syntheticRoot, RootCommit: committedRoot, ChunkSize: catalog.MinChunkSize,
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
		fileLocator, _ := catalog.NewLocator(0, "file.bin")
		fileIdentity, _ := catalog.NewSourceIdentity([]byte("file-object"))
		candidate, _ := catalog.NewVersionCandidate([]byte("file-version"))
		err := request.Children.Add(ctx, catalog.ScannedChild{
			FileID: fixture.fileID, Name: "file.bin", Locator: fileLocator,
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
		ShareInstance: fixture.share, Keys: keyTree, VerificationKey: publicKey,
	})
	geometry, _ := content.NewFileGeometry(uint64(len(fixture.fileData)), catalog.MinChunkSize)
	revisionDescriptor, _ := content.NewFileRevisionDescriptor(
		fixture.share, fixture.fileID, id16[content.FileRevision](8), geometry, catalog.ModifiedTime{},
	)
	lease, _ := content.NewRevisionLease(
		id16[content.LeaseID](9), revisionDescriptor,
		contentflow.RevisionLeaseTTL, contentflow.RevisionLeaseRenewAfter,
	)
	fixture.contentStore = &verticalContentStore{descriptor: revisionDescriptor, lease: lease, data: fixture.fileData}
	cacheBudget, _ := contentflow.NewProcessCacheBudget(64 << 20)
	cache, _ := contentflow.NewSharedBlockCache(fixture.share, 16<<20, cacheBudget)
	t.Cleanup(cache.Close)
	var quotaSequence atomic.Int32
	contentFactory := SenderContentFactoryFunc(func() (*contentflow.SenderService, error) {
		quota, _ := content.NewQuotaAccount(
			"session-"+time.Now().Format("150405.000000000"), content.DefaultSessionQuotaLimits(),
		)
		quotaSequence.Add(1)
		return contentflow.NewSenderService(contentflow.SenderServiceConfig{
			Store: fixture.contentStore, SessionQuota: quota, Sealer: recordSealer, Cache: cache,
		})
	})
	fixture.senderFactory, err = NewSenderFactory(SenderFactoryConfig{
		ShareInstance: fixture.share, SessionAuthKey: sessionAuthKey, SenderPrivateKey: privateKey,
		Catalog: SenderCatalogFactoryFunc(func() (*catalogflow.AddressedSenderService, error) {
			return catalogService, nil
		}), Content: contentFactory, Peers: inertSenderPeerFactory(),
		Random: &deterministicReader{next: 17}, TerminalConnectivity: fixture.terminal, TerminalTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier, _ := catalogflow.NewCatalogObjectVerifier(catalogflow.CatalogObjectVerifierConfig{
		ShareInstance: fixture.share, CatalogKey: catalogKey, SenderPublicKey: publicKey,
	})
	processReassembly, _ := contentflow.NewReassemblyAccount("process", contentflow.ReassemblyLimits{Bytes: 1 << 30, Records: 256})
	shareReassembly, _ := contentflow.NewReassemblyAccount("share", contentflow.ReassemblyLimits{Bytes: 256 << 20, Records: 64})
	plaintext, _ := transfer.NewPlaintextBudget(256 << 20)
	fixture.receiverConfig = ReceiverFactoryConfig{
		Descriptor: descriptor, SessionAuthKey: sessionAuthKey, SenderPublicKey: publicKey,
		CatalogVerifier: verifier, RecordOpener: recordOpener,
		ReassemblyProcess: processReassembly, ReassemblyShare: shareReassembly, PlaintextProcess: plaintext,
		Random: &deterministicReader{next: 19},
	}
	fixture.receiverFactory, err = NewReceiverFactory(fixture.receiverConfig)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

type verticalContentStore struct {
	descriptor content.FileRevisionDescriptor
	lease      content.RevisionLease
	data       []byte
	blockReads atomic.Int32
	renewCalls atomic.Int32
	blockStart chan uint64
	blockGate  <-chan struct{}
	blockStop  chan struct{}
	renewStart chan<- struct{}
	renewGate  <-chan struct{}
	released   chan<- content.LeaseID
}

type failingRecordOpener struct{ err error }

func (opener failingRecordOpener) OpenRevision(
	catalog.FileID,
	uint32,
	[]byte,
) (content.FileRevisionDescriptor, error) {
	return content.FileRevisionDescriptor{}, opener.err
}

func (opener failingRecordOpener) OpenBlock(
	content.FileRevisionDescriptor,
	uint64,
	[]byte,
) (records.BlockRecord, error) {
	return records.BlockRecord{}, opener.err
}

type verticalPeerHandler struct {
	session          SenderPeerSession
	answer           []byte
	localCandidate   []byte
	rejectedOffer    []byte
	remoteCandidates chan protocolsession.Message
	canceled         chan protocolsession.OperationID
}

func (handler *verticalPeerHandler) HandleMessage(
	ctx context.Context,
	message protocolsession.Message,
) error {
	operation, ok := message.OperationID()
	if !ok {
		return ErrRuntimeConfig
	}
	switch message.Kind() {
	case protocolsession.MessagePeerOffer:
		if bytes.Equal(message.Body(), handler.rejectedOffer) {
			return handler.session.FailPeerOperation(
				ctx, operation, protocolsession.PeerOperationCodeNegotiation, "Peer negotiation failed",
			)
		}
		if _, err := handler.session.SendPeerControl(
			ctx, protocolsession.MessagePeerAnswer, operation, handler.answer,
		); err != nil {
			return err
		}
		_, err := handler.session.SendPeerControl(
			ctx, protocolsession.MessagePeerCandidate, operation, handler.localCandidate,
		)
		return err
	case protocolsession.MessagePeerCandidate:
		handler.remoteCandidates <- message
		return nil
	default:
		return ErrRuntimeConfig
	}
}

func (handler *verticalPeerHandler) Cancel(
	_ context.Context,
	operation protocolsession.OperationID,
) error {
	handler.canceled <- operation
	return nil
}

func (*verticalPeerHandler) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (store *verticalContentStore) OpenRevisions(
	_ context.Context,
	requests []content.OpenRevisionRequest,
	_ *content.QuotaAccount,
) ([]content.OpenRevisionResult, error) {
	results := make([]content.OpenRevisionResult, len(requests))
	for index, request := range requests {
		results[index] = content.OpenRevisionResult{FileID: request.FileID}
		if request.FileID == store.descriptor.FileID() {
			results[index].Lease = store.lease
		} else {
			results[index].Err = errors.New("not found")
		}
	}
	return results, nil
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

func (store *verticalContentStore) ReleaseLease(leaseID content.LeaseID) error {
	if store.released != nil {
		store.released <- leaseID
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
