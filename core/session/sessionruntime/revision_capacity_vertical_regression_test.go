package sessionruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/internal/keyderiv"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const syntheticCapacityFileCount = revisioncapacity.DefaultShareStableHandles + 1

func TestTransferJobExplicitlySettlesMoreThanShareStableHandleLimit(t *testing.T) {
	fixture := newSyntheticCapacityFixture(t, syntheticCapacityFileCount)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	intent := syntheticCapacityReceiveIntent(t, fixture.share, fixture.syntheticRoot)
	jobID, err := transfer.TransferJobIDFromBytes(intent.Digest().Bytes()[:transfer.TransferJobIdentityBytes])
	if err != nil {
		t.Fatal(err)
	}
	output := newSyntheticCapacityOutput(t, intent)
	var traceMu sync.Mutex
	var traces []transfer.TransferLifecycleTrace
	job, err := receiver.NewTransferJob(intent, jobID, output, transfer.TransferLifecycleTraceFunc(func(event transfer.TransferLifecycleTrace) {
		traceMu.Lock()
		traces = append(traces, event)
		traceMu.Unlock()
	}))
	if err != nil {
		t.Fatal(err)
	}

	result := job.Run(context.Background())
	wantFiles := uint64(syntheticCapacityFileCount)
	if result.Outcome != transfer.DirectTreeOutcomeSuccess ||
		result.Settlement.Kind() != transfer.DirectTreeSettlementSuccess ||
		result.SucceededFiles != wantFiles || result.TerminationCause != nil || result.SettlementFailure != nil {
		t.Fatalf("job result = %+v", result)
	}
	if len(result.Files) != 0 || len(result.Directories) != 0 ||
		result.OmittedFileFailures != 0 || result.OmittedDirectoryFailures != 0 ||
		result.SelectionResolutionFailure != nil || result.SourceDriftFailure != nil {
		t.Fatalf("job failure accounting: files=%d omitted_files=%d directories=%d omitted_directories=%d selection=%v drift=%v",
			len(result.Files), result.OmittedFileFailures, len(result.Directories), result.OmittedDirectoryFailures,
			result.SelectionResolutionFailure, result.SourceDriftFailure)
	}
	progress := result.Progress
	if progress.DiscoveredFiles != wantFiles || progress.DiscoveredBytes != wantFiles ||
		progress.PublishedFiles != wantFiles || progress.PublishedBytes != wantFiles ||
		progress.VerifiedBytes != wantFiles || !progress.CountersExact || progress.Discovery != transfer.DiscoveryComplete ||
		progress.FileOutcomes.DownloadedFiles != wantFiles || progress.FileOutcomes.PublishedFiles() != wantFiles ||
		progress.FileOutcomes.FailedFiles != 0 || progress.FileOutcomes.PausedFiles != 0 {
		t.Fatalf("job progress = %+v", progress)
	}
	if progress.CapacityWait.ActiveWaiters != 0 || progress.CapacityWait.Attempts != 0 ||
		progress.CapacityWait.AccumulatedWait != 0 {
		t.Fatalf("ordinary explicit releases entered capacity wait: %+v", progress.CapacityWait)
	}
	if published := output.publishedPayloads(); uint64(len(published)) != syntheticCapacityFileCount {
		t.Fatalf("published files = %d, want %d", len(published), syntheticCapacityFileCount)
	} else {
		for path, payload := range published {
			if !bytes.Equal(payload, syntheticCapacityPayload) {
				t.Fatalf("published payload %q = %x", path, payload)
			}
		}
	}

	opens, closes, doubleCloses, closeDuringRead := fixture.revisionSource.snapshot()
	if opens != syntheticCapacityFileCount || closes != syntheticCapacityFileCount || doubleCloses != 0 || closeDuringRead != 0 {
		t.Fatalf("stable source lifecycle: opens=%d closes=%d double_closes=%d close_during_read=%d",
			opens, closes, doubleCloses, closeDuringRead)
	}
	if relinquished, other := fixture.settlements.relinquished.Load(), fixture.settlements.other.Load(); relinquished != syntheticCapacityFileCount || other != 0 {
		t.Fatalf("lease settlements: relinquished=%d other=%d", relinquished, other)
	}
	snapshot := fixture.revisionStore.CapacitySnapshot()
	if used := snapshot.Process().Used(); used != (revisioncapacity.CapacityUsage{}) {
		t.Fatalf("process capacity after settlement = %+v", used)
	}
	if used := snapshot.Share().Used(); used != (revisioncapacity.CapacityUsage{}) {
		t.Fatalf("share capacity after settlement = %+v", used)
	}
	sessions := snapshot.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("capacity sessions after settlement = %d, want 1", len(sessions))
	}
	for _, session := range sessions {
		if used := session.Used(); used != (revisioncapacity.CapacityUsage{}) {
			t.Fatalf("session capacity after settlement = %+v", used)
		}
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	for _, event := range traces {
		if code, ok := event.Fault.SourceCode(); ok && code == transferfault.SourceUnavailable {
			t.Fatalf("transfer projected permanent source-unavailable: %+v", event)
		}
		switch event.Stage {
		case transfer.TransferCapacityRetryScheduled,
			transfer.TransferCapacityRetryReady,
			transfer.TransferCapacityRetrySucceeded,
			transfer.TransferCapacityBudgetPaused,
			transfer.TransferCapacityWaitCanceled,
			transfer.TransferCapacityGenerationEnded:
			t.Fatalf("ordinary explicit releases reached capacity backpressure: %+v", event)
		}
	}
}

var syntheticCapacityPayload = []byte{0x5a}

type syntheticCapacityFixture struct {
	share           catalog.ShareInstance
	syntheticRoot   catalog.DirectoryID
	senderFactory   *SenderFactory
	receiverFactory *ReceiverFactory
	revisionStore   *content.RevisionStore
	revisionSource  *syntheticCapacityRevisionSource
	settlements     *syntheticCapacitySettlements
}

func newSyntheticCapacityFixture(t *testing.T, fileCount uint64) *syntheticCapacityFixture {
	t.Helper()
	share := syntheticCapacityID[catalog.ShareInstance](1)
	syntheticRoot := syntheticCapacityID[catalog.DirectoryID](2)
	directoryID := syntheticCapacityID[catalog.DirectoryID](3)
	seed := bytes.Repeat([]byte{0x31}, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	readSecret := bytes.Repeat([]byte{0x41}, link.ReadSecretBytes)
	catalogKey, err := keyderiv.V2CatalogKey(readSecret, share.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	sessionAuthKey, err := keyderiv.V2SessionAuthKey(readSecret, share.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	sealedCatalog, err := catalogflow.NewSealedCatalogStore(catalogflow.SealedCatalogStoreConfig{
		ShareInstance: share, CatalogKey: catalogKey, SenderPrivateKey: privateKey,
		NonceSource: &deterministicReader{next: 71},
	})
	if err != nil {
		t.Fatal(err)
	}
	processBudget, err := catalog.NewBudgetAccount("synthetic-capacity-process", catalog.DefaultProcessBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	shareBudget, err := catalog.NewBudgetAccount("synthetic-capacity-share", catalog.DefaultShareBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	startupBudget, err := catalog.NewBudgetAccount("synthetic-capacity-startup", catalog.DefaultSessionBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	catalogStore, err := catalog.NewCatalogStore(catalog.StoreConfig{
		ShareInstance: share, Backend: catalog.NewMemoryCatalogBackend(),
		ProcessBudget: processBudget, ShareBudget: shareBudget, PageSealer: sealedCatalog,
	})
	if err != nil {
		t.Fatal(err)
	}
	selectedLocator, _ := catalog.NewLocator(0, "")
	selectedIdentity, _ := catalog.NewSourceIdentity([]byte("synthetic-capacity-directory"))
	selected, err := catalog.NewDirectoryNodeRecord(
		directoryID, syntheticRoot, "files", selectedLocator, selectedIdentity, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rootCommit, err := catalog.NewSyntheticRootCommit(catalog.SyntheticRootCommitSpec{
		ShareInstance: share, SyntheticRoot: syntheticRoot,
		Generation: syntheticCapacityID[catalog.DirectoryGeneration](4), SelectedRoots: []catalog.NodeRecord{selected},
	})
	if err != nil {
		t.Fatal(err)
	}
	committedRoot, err := catalogStore.CommitSyntheticRoot(context.Background(), rootCommit, startupBudget)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := catalog.NewShareDescriptor(catalog.DescriptorSpec{
		WireVersion: catalog.WireVersionV2, Suite: catalog.SuiteV2,
		ShareInstance: share, SyntheticRoot: syntheticRoot, RootCommit: committedRoot,
		ChunkSize: catalog.MinChunkSize, Capabilities: catalog.CapabilityCatalog | catalog.CapabilityRanges,
		SenderPublicKey: publicKey, CreatedAtSeconds: 1, PathPolicy: catalog.PathPolicyV1,
	})
	if err != nil {
		t.Fatal(err)
	}

	files := make(map[catalog.FileID][]byte, fileCount)
	children := make([]catalog.ScannedChild, 0, fileCount)
	for index := range fileCount {
		fileID := syntheticCapacityID[catalog.FileID](index + 10)
		name := fmt.Sprintf("file-%03d.bin", index)
		locator, locatorErr := catalog.NewLocator(0, name)
		identity, identityErr := catalog.NewSourceIdentity(fmt.Appendf(nil, "synthetic-capacity-file-%d", index))
		candidate, candidateErr := catalog.NewVersionCandidate(fmt.Appendf(nil, "synthetic-capacity-version-%d", index))
		if errors.Join(locatorErr, identityErr, candidateErr) != nil {
			t.Fatal(errors.Join(locatorErr, identityErr, candidateErr))
		}
		children = append(children, catalog.ScannedChild{
			FileID: fileID, Name: name, Locator: locator, SourceIdentity: identity,
			VersionCandidate: candidate, ExpectedSize: uint64(len(syntheticCapacityPayload)),
		})
		files[fileID] = bytes.Clone(syntheticCapacityPayload)
	}
	scanner := catalog.DirectoryScannerFunc(func(ctx context.Context, request catalog.ScanRequest) (catalog.ScanResult, error) {
		for _, child := range children {
			if err := request.Children.Add(ctx, child); err != nil {
				return catalog.ScanResult{}, err
			}
		}
		return catalog.ScanResult{}, nil
	})
	catalogSessionBudget, err := catalog.NewBudgetAccount("synthetic-capacity-catalog-session", catalog.DefaultSessionBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	catalogSource, err := catalogflow.NewCatalogStoreSource(catalogflow.CatalogStoreSourceConfig{
		ShareInstance: share, Store: catalogStore, SessionBudget: catalogSessionBudget, Scanner: scanner,
	})
	if err != nil {
		t.Fatal(err)
	}
	catalogService, err := catalogflow.NewAddressedSenderService(share, catalogSource)
	if err != nil {
		t.Fatal(err)
	}

	owner, err := revisioncapacity.NewProcessOwner(revisioncapacity.DefaultProcessConfig())
	if err != nil {
		t.Fatal(err)
	}
	revisionSource := &syntheticCapacityRevisionSource{files: files}
	settlements := &syntheticCapacitySettlements{}
	var revisionKey content.RevisionIdentityKey
	revisionKey[0] = 0x51
	revisionDeriver, err := content.NewHMACRevisionIdentityDeriver(revisionKey)
	if err != nil {
		t.Fatal(err)
	}
	metadataBudget, err := content.NewRevisionMetadataBudget(content.DefaultRevisionInvalidationEntries)
	if err != nil {
		t.Fatal(err)
	}
	revisionStore, err := content.NewRevisionStore(content.RevisionStoreConfig{
		ShareInstance: share, ChunkSize: catalog.MinChunkSize, Catalog: catalogStore, Source: revisionSource,
		CapacityCoordinator: owner.Coordinator(), CapacityStore: revisioncapacity.StoreConfig{
			StoreID: "synthetic-capacity-store", ShareID: "synthetic-capacity-share",
			Limits: revisioncapacity.DefaultShareLimits(),
		},
		LeaseIDs: &syntheticCapacityLeaseIDs{}, RevisionDeriver: revisionDeriver, MetadataBudget: metadataBudget,
		Tracer: content.RevisionTracerFunc(func(event content.RevisionTrace) {
			if event.Stage() != content.RevisionTraceStageLeaseSettlement {
				return
			}
			if event.Cause() == content.RevisionTraceCauseRelinquished {
				settlements.relinquished.Add(1)
			} else {
				settlements.other.Add(1)
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	keyTree, err := content.NewKeyTree(readSecret, share)
	if err != nil {
		t.Fatal(err)
	}
	recordSealer, err := records.NewSealer(records.SealerConfig{
		ShareInstance: share, Keys: keyTree, SigningKey: privateKey,
		NonceSource: &deterministicReader{next: 73},
	})
	if err != nil {
		t.Fatal(err)
	}
	recordOpener, err := records.NewOpener(records.OpenerConfig{
		ShareInstance: share, Keys: keyTree, VerificationKey: publicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	cacheBudget, err := contentflow.NewProcessCacheBudget(64 << 20)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := contentflow.NewSharedBlockCache(share, 16<<20, cacheBudget)
	if err != nil {
		t.Fatal(err)
	}
	contentFactory := SenderContentFactoryFunc(func(sessionID protocolsession.ProtocolSessionID) (*contentflow.SenderService, error) {
		sessionCapacity, registerErr := revisionStore.RegisterSession(revisioncapacity.SessionConfig{
			SessionID: revisioncapacity.SessionID(base64.RawURLEncoding.EncodeToString(sessionID.Bytes())),
			Limits:    revisioncapacity.DefaultSessionLimits(),
		})
		if registerErr != nil {
			return nil, registerErr
		}
		service, serviceErr := contentflow.NewSenderService(contentflow.SenderServiceConfig{
			Store: revisionStore, SessionCapacity: sessionCapacity, Sealer: recordSealer, Cache: cache,
		})
		if serviceErr != nil {
			return nil, errors.Join(serviceErr, sessionCapacity.Close())
		}
		return service, nil
	})
	senderFactory, err := NewSenderFactory(SenderFactoryConfig{
		ShareInstance: share, SessionAuthKey: sessionAuthKey, SenderPrivateKey: privateKey,
		Catalog: SenderCatalogFactoryFunc(func() (*catalogflow.AddressedSenderService, error) { return catalogService, nil }),
		Content: contentFactory, Peers: inertSenderPeerFactory(), Random: &deterministicReader{next: 79},
		TerminalConnectivity: &verticalTerminalConnectivity{}, TerminalTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := catalogflow.NewCatalogObjectVerifier(catalogflow.CatalogObjectVerifierConfig{
		ShareInstance: share, CatalogKey: catalogKey, SenderPublicKey: publicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	processReassembly, err := contentflow.NewReassemblyAccount(
		"synthetic-capacity-process", contentflow.ReassemblyLimits{Bytes: 1 << 30, Records: 256},
	)
	if err != nil {
		t.Fatal(err)
	}
	shareReassembly, err := contentflow.NewReassemblyAccount(
		"synthetic-capacity-share", contentflow.ReassemblyLimits{Bytes: 256 << 20, Records: 64},
	)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := transfer.NewPlaintextBudget(256 << 20)
	if err != nil {
		t.Fatal(err)
	}
	receiverFactory, err := NewReceiverFactory(ReceiverFactoryConfig{
		Descriptor: descriptor, SessionAuthKey: sessionAuthKey, SenderPublicKey: publicKey,
		CatalogVerifier: verifier, RecordOpener: recordOpener,
		ReassemblyProcess: processReassembly, ReassemblyShare: shareReassembly, PlaintextProcess: plaintext,
		Random: &deterministicReader{next: 83},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := revisionStore.Close(); err != nil {
			t.Error(err)
		}
		cache.Close()
		if err := catalogStore.Close(); err != nil {
			t.Error(err)
		}
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
		revisionDeriver.Destroy()
		keyTree.Destroy()
	})
	return &syntheticCapacityFixture{
		share: share, syntheticRoot: syntheticRoot, senderFactory: senderFactory, receiverFactory: receiverFactory,
		revisionStore: revisionStore, revisionSource: revisionSource, settlements: settlements,
	}
}

type syntheticCapacitySettlements struct {
	relinquished atomic.Uint64
	other        atomic.Uint64
}

type syntheticCapacityLeaseIDs struct{ next atomic.Uint64 }

func (ids *syntheticCapacityLeaseIDs) NewLeaseID() (content.LeaseID, error) {
	return syntheticCapacityID[content.LeaseID](ids.next.Add(1)), nil
}

func syntheticCapacityID[T ~[16]byte](value uint64) T {
	var id T
	binary.BigEndian.PutUint64(id[8:], value)
	return id
}

type syntheticCapacityRevisionSource struct {
	files map[catalog.FileID][]byte

	opens           atomic.Uint64
	closes          atomic.Uint64
	doubleCloses    atomic.Uint64
	closeDuringRead atomic.Uint64
}

func (source *syntheticCapacityRevisionSource) OpenStable(
	_ context.Context,
	record catalog.NodeRecord,
) (content.StableFile, error) {
	fileID, ok := record.FileID()
	payload, exists := source.files[fileID]
	if !ok || !exists {
		return nil, content.ErrRevisionNotFound
	}
	source.opens.Add(1)
	return &syntheticCapacityStableFile{source: source, payload: bytes.Clone(payload), modified: record.Entry().ModifiedTime()}, nil
}

func (source *syntheticCapacityRevisionSource) snapshot() (opens, closes, doubleCloses, closeDuringRead uint64) {
	return source.opens.Load(), source.closes.Load(), source.doubleCloses.Load(), source.closeDuringRead.Load()
}

type syntheticCapacityStableFile struct {
	source   *syntheticCapacityRevisionSource
	payload  []byte
	modified catalog.ModifiedTime
	readers  atomic.Int32
	closed   atomic.Bool
}

func (file *syntheticCapacityStableFile) ExactSize() uint64 { return uint64(len(file.payload)) }

func (file *syntheticCapacityStableFile) ModifiedTime() catalog.ModifiedTime { return file.modified }

func (file *syntheticCapacityStableFile) Verify(context.Context) error {
	if file.closed.Load() {
		return content.ErrRevisionUnreadable
	}
	return nil
}

func (file *syntheticCapacityStableFile) ReadAt(
	_ context.Context,
	destination []byte,
	offset uint64,
) (int, error) {
	file.readers.Add(1)
	defer file.readers.Add(-1)
	if file.closed.Load() {
		return 0, content.ErrRevisionUnreadable
	}
	if offset >= uint64(len(file.payload)) {
		return 0, io.EOF
	}
	count := copy(destination, file.payload[offset:])
	if count != len(destination) {
		return count, io.EOF
	}
	return count, nil
}

func (file *syntheticCapacityStableFile) Close() error {
	if !file.closed.CompareAndSwap(false, true) {
		file.source.doubleCloses.Add(1)
		return nil
	}
	// Closing first fences future reads; a non-zero count now proves Close raced
	// an already-admitted reader rather than observing an eventually stale count.
	if file.readers.Load() != 0 {
		file.source.closeDuringRead.Add(1)
	}
	file.source.closes.Add(1)
	return nil
}

func syntheticCapacityReceiveIntent(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
) transfer.ReceiveIntent {
	t.Helper()
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	identityMaterial := append(share.Bytes(), root.Bytes()...)
	operationDigest := sha256.Sum256(append([]byte("windshare/synthetic-capacity-operation/v1\x00"), identityMaterial...))
	reservationDigest := sha256.Sum256(append([]byte("windshare/synthetic-capacity-reservation/v1\x00"), identityMaterial...))
	authorityDigest := sha256.Sum256(append([]byte("windshare/synthetic-capacity-authority/v1\x00"), identityMaterial...))
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
	reservation, err := receivecontract.NewNativeContainerRootReservation(operation, reservationID, artifact, authority)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

type syntheticCapacityOutput struct {
	mu              sync.Mutex
	intent          transfer.ReceiveIntent
	binding         transfer.DirectTreeSessionBinding
	sessionID       transfer.OutputSessionID
	directorySecret [sha256.Size]byte
	published       map[string][]byte
}

func newSyntheticCapacityOutput(t *testing.T, intent transfer.ReceiveIntent) *syntheticCapacityOutput {
	t.Helper()
	binding, err := transfer.BindDirectTreeSession(intent)
	if err != nil {
		t.Fatal(err)
	}
	output := &syntheticCapacityOutput{
		intent: intent, binding: binding, sessionID: syntheticCapacityID[transfer.OutputSessionID](1),
		published: make(map[string][]byte),
	}
	output.directorySecret = sha256.Sum256([]byte("windshare/synthetic-capacity-output/directory-secret/v1"))
	return output
}

func (output *syntheticCapacityOutput) OpenDirectTree(
	_ context.Context,
	intent transfer.ReceiveIntent,
) (transfer.DirectTreeSession, error) {
	if !intent.EqualCanonical(output.intent) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return output, nil
}

func (output *syntheticCapacityOutput) SessionID() transfer.OutputSessionID { return output.sessionID }

func (output *syntheticCapacityOutput) Binding() transfer.DirectTreeSessionBinding {
	return output.binding
}

func (*syntheticCapacityOutput) Capabilities() transfer.DirectTreeCapabilities {
	capabilities, _ := transfer.NewDirectTreeCapabilities(transfer.DirectTreeCapabilities{
		Durability: transfer.DurabilityNone, RandomWrite: true, FileFailureIsolation: true, ModifiedTime: true,
	})
	return capabilities
}

func (output *syntheticCapacityOutput) AdmitDirectory(
	_ context.Context,
	request transfer.DirectoryMaterializationRequest,
) (transfer.DirectoryAdmission, error) {
	scope, err := transfer.NewDirectoryAdmissionScope(output.intent)
	if err != nil {
		return transfer.DirectoryAdmission{}, err
	}
	directory, ok := request.Directory()
	if !ok {
		return transfer.DirectoryAdmission{}, transfer.ErrInvalidDirectoryAdmission
	}
	return transfer.NewDirectoryAdmissionWithSecret(output.directorySecret[:], scope, directory)
}

func (*syntheticCapacityOutput) FinalizeDirectory(
	_ context.Context,
	admission transfer.DirectoryAdmission,
) (transfer.DirectorySettlement, error) {
	return transfer.NewFinalizedDirectorySettlement(admission)
}

func (output *syntheticCapacityOutput) BeginFile(
	_ context.Context,
	file transfer.MaterializationFile,
) (transfer.FileStart, error) {
	digest := sha256.Sum256([]byte(file.ArtifactPath().String()))
	var identity transfer.OwnedObjectID
	copy(identity[:], digest[:])
	binding, err := transfer.BindFileMaterializationTarget(file.Target(), identity)
	if err != nil {
		return transfer.FileStart{}, err
	}
	empty, err := content.NewRangeSet(nil)
	if err != nil {
		return transfer.FileStart{}, err
	}
	durable, err := transfer.VerifyDurableRanges(binding, 1, empty)
	if err != nil {
		return transfer.FileStart{}, err
	}
	transaction := &syntheticCapacityFileTransaction{
		output: output, binding: binding, payload: make([]byte, binding.ExactSize()),
	}
	return transfer.NewFileTransactionStart(transaction, durable)
}

func (*syntheticCapacityOutput) PauseTree(
	context.Context,
	transfer.JobPauseReason,
) (transfer.DirectTreeSettlement, error) {
	return transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementPaused)
}

func (*syntheticCapacityOutput) FinalizeTree(
	_ context.Context,
	outcome transfer.DirectTreeOutcome,
) (transfer.DirectTreeSettlement, error) {
	if outcome != transfer.DirectTreeOutcomeSuccess {
		return transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementPartial)
	}
	return transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementSuccess)
}

func (output *syntheticCapacityOutput) publishedPayloads() map[string][]byte {
	output.mu.Lock()
	defer output.mu.Unlock()
	result := make(map[string][]byte, len(output.published))
	for path, payload := range output.published {
		result[path] = bytes.Clone(payload)
	}
	return result
}

type syntheticCapacityFileTransaction struct {
	output  *syntheticCapacityOutput
	binding transfer.MaterializedFileBinding
	payload []byte
	written uint64
}

func (transaction *syntheticCapacityFileTransaction) Binding() transfer.MaterializedFileBinding {
	return transaction.binding
}

func (transaction *syntheticCapacityFileTransaction) WriteRange(
	_ context.Context,
	offset uint64,
	data []byte,
) error {
	end := offset + uint64(len(data))
	if end < offset || end > uint64(len(transaction.payload)) {
		return transfer.ErrOutputContract
	}
	copy(transaction.payload[offset:end], data)
	transaction.written += uint64(len(data))
	return nil
}

func (transaction *syntheticCapacityFileTransaction) Checkpoint(
	context.Context,
) (transfer.VerifiedDurableRanges, error) {
	empty, err := content.NewRangeSet(nil)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	return transfer.VerifyDurableRanges(transaction.binding, 2, empty)
}

func (transaction *syntheticCapacityFileTransaction) Commit(
	context.Context,
) (transfer.FileSettlement, error) {
	if transaction.written != uint64(len(transaction.payload)) {
		return transfer.FileSettlement{}, transfer.ErrIncompleteMaterializationFile
	}
	path := transaction.binding.Locator().CanonicalPath()
	transaction.output.mu.Lock()
	if _, exists := transaction.output.published[path]; exists {
		transaction.output.mu.Unlock()
		return transfer.FileSettlement{}, transfer.ErrOutputContract
	}
	transaction.output.published[path] = bytes.Clone(transaction.payload)
	transaction.output.mu.Unlock()
	return transfer.NewTransientPublishedFileSettlement(transaction.binding)
}

func (transaction *syntheticCapacityFileTransaction) Pause(
	context.Context,
	transfer.FilePauseReason,
) (transfer.FileSettlement, error) {
	empty, err := content.NewRangeSet(nil)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	checkpoint, err := transfer.VerifyDurableRanges(transaction.binding, 2, empty)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	return transfer.NewVerifiedFileSettlement(transfer.FilePaused, checkpoint)
}

func (transaction *syntheticCapacityFileTransaction) Retire(
	context.Context,
	transfer.FileRetireReason,
) (transfer.FileSettlement, error) {
	return transfer.NewFailedFileSettlement(transaction.binding)
}
