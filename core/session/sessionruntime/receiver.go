package sessionruntime

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

type RecordOpener interface {
	OpenRevision(catalog.FileID, uint32, []byte) (content.FileRevisionDescriptor, error)
	OpenBlock(content.FileRevisionDescriptor, uint64, []byte) (records.BlockRecord, error)
}

type ReceiverInstanceSource interface {
	NewReceiverInstanceID() (protocolsession.ReceiverInstanceID, error)
}

type ReceiverInstanceSourceFunc func() (protocolsession.ReceiverInstanceID, error)

func (function ReceiverInstanceSourceFunc) NewReceiverInstanceID() (protocolsession.ReceiverInstanceID, error) {
	if function == nil {
		return protocolsession.ReceiverInstanceID{}, ErrRuntimeConfig
	}
	return function()
}

type ReceiverRuntimeResourceLease interface {
	Release()
}

type ReceiverRuntimeResourceSource interface {
	AcquireReceiverRuntimeResources() (ReceiverRuntimeResourceLease, error)
}

type ReceiverPeerSemantics interface {
	protocolsession.SenderControlSemanticValidator
	protocolsession.OperationContinuationClassifier
}

type ReceiverFactoryConfig struct {
	Descriptor        catalog.ShareDescriptor
	SessionAuthKey    []byte
	SenderPublicKey   ed25519.PublicKey
	CatalogVerifier   catalogflow.ObjectVerifier
	RecordOpener      RecordOpener
	ReassemblyProcess *contentflow.ReassemblyAccount
	ReassemblyShare   *contentflow.ReassemblyAccount
	PlaintextProcess  *transfer.PlaintextBudget
	Random            io.Reader
	ReceiverInstances ReceiverInstanceSource
	CatalogProgress   CatalogScanProgressObserver
	PeerControls      ReceiverPeerSemantics
	RuntimeResources  ReceiverRuntimeResourceSource
	OperationLimits   protocolsession.OperationLimits
	RouterLimits      protocolsession.RouterLimits
	LaneRaceWidth     int
	Now               func() time.Time
	After             func(time.Duration) <-chan time.Time
}

type ReceiverFactory struct {
	descriptor        catalog.ShareDescriptor
	authKey           []byte
	publicKey         ed25519.PublicKey
	verifier          catalogflow.ObjectVerifier
	opener            RecordOpener
	processReassembly *contentflow.ReassemblyAccount
	shareReassembly   *contentflow.ReassemblyAccount
	plaintextProcess  *transfer.PlaintextBudget
	random            *lockedReader
	admissionContext  context.Context
	cancelAdmissions  context.CancelFunc
	instances         ReceiverInstanceSource
	catalogProgress   CatalogScanProgressObserver
	semantic          protocolsession.SenderControlSemanticValidator
	peerSemantics     ReceiverPeerSemantics
	resources         ReceiverRuntimeResourceSource
	operationLimits   protocolsession.OperationLimits
	routerLimits      protocolsession.RouterLimits
	raceWidth         int
	now               func() time.Time
	after             func(time.Duration) <-chan time.Time

	mu         sync.Mutex
	closing    bool
	admissions sync.WaitGroup
	closeDone  chan struct{}
}

func NewReceiverFactory(config ReceiverFactoryConfig) (*ReceiverFactory, error) {
	if config.Descriptor.ShareInstance().IsZero() ||
		len(config.SessionAuthKey) != protocolsession.SessionAuthKeyBytes ||
		len(config.SenderPublicKey) != ed25519.PublicKeySize ||
		!ed25519.PublicKey(config.Descriptor.SenderPublicKey()).Equal(config.SenderPublicKey) ||
		config.CatalogVerifier == nil || config.RecordOpener == nil ||
		config.ReassemblyProcess == nil || config.ReassemblyShare == nil || config.PlaintextProcess == nil {
		return nil, ErrRuntimeConfig
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	lockedRandom := &lockedReader{reader: config.Random}
	if config.ReceiverInstances == nil {
		config.ReceiverInstances = ReceiverInstanceSourceFunc(func() (protocolsession.ReceiverInstanceID, error) {
			value := make([]byte, protocolsession.IdentityBytes)
			if _, err := io.ReadFull(lockedRandom, value); err != nil {
				return protocolsession.ReceiverInstanceID{}, err
			}
			return protocolsession.ReceiverInstanceIDFromBytes(value)
		})
	}
	if config.After == nil {
		config.After = time.After
	}
	semantic, err := newReceiverSenderControlValidator(config.PeerControls)
	if err != nil {
		return nil, errors.Join(ErrRuntimeConfig, err)
	}
	admissionContext, cancelAdmissions := context.WithCancel(context.Background())
	return &ReceiverFactory{
		descriptor: config.Descriptor, authKey: append([]byte(nil), config.SessionAuthKey...),
		publicKey: append(ed25519.PublicKey(nil), config.SenderPublicKey...),
		verifier:  config.CatalogVerifier, opener: config.RecordOpener,
		processReassembly: config.ReassemblyProcess, shareReassembly: config.ReassemblyShare,
		plaintextProcess: config.PlaintextProcess, random: lockedRandom,
		admissionContext: admissionContext, cancelAdmissions: cancelAdmissions,
		instances:       config.ReceiverInstances,
		catalogProgress: config.CatalogProgress, semantic: semantic, peerSemantics: config.PeerControls,
		resources:       config.RuntimeResources,
		operationLimits: config.OperationLimits, routerLimits: config.RouterLimits,
		raceWidth: config.LaneRaceWidth, now: config.Now, after: config.After,
		closeDone: make(chan struct{}),
	}, nil
}

func (factory *ReceiverFactory) Connect(ctx context.Context, channel protocolsession.FrameChannel) (*ReceiverRuntime, error) {
	if factory == nil || channel == nil || ctx == nil {
		return nil, ErrRuntimeConfig
	}
	admissionContext, endAdmission, ok := factory.beginAdmission(ctx)
	if !ok {
		return nil, ErrRuntimeClosed
	}
	defer endAdmission()
	ctx = admissionContext
	resourceLease, err := factory.acquireRuntimeResources()
	if err != nil {
		return nil, err
	}
	leaseTransferred := false
	defer func() {
		if !leaseTransferred && resourceLease != nil {
			resourceLease.Release()
		}
	}()
	handshake, err := factory.completeReceiverHandshake(ctx, channel)
	if err != nil {
		return nil, err
	}
	keys := handshake.keys
	handOff := &handedOffChannel{FrameChannel: channel, receive: handshake.receive}
	runtime, err := newRuntime(runtimeConfig{
		Share: factory.descriptor.ShareInstance(), Role: protocolsession.RoleReceiver, Keys: keys,
		LaneID: handshake.lane.ID, LaneEpoch: handshake.lane.Epoch, Channel: handOff,
		Random: factory.random, Authenticator: handshake.authenticator,
		Continuations:   factory.peerSemantics,
		OperationLimits: factory.operationLimits, RouterLimits: factory.routerLimits, Now: factory.now,
	})
	if err != nil {
		keys.Destroy()
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			runtime.abortBeforeStart()
		}
	}()
	rpc := newRPCClient(runtime, factory.random)
	if err := runtime.addFinalizer(rpc.Close); err != nil {
		return nil, err
	}
	if err := rpc.register(runtime.router); err != nil {
		return nil, err
	}
	catalogClient, err := catalogflow.NewClient(catalogflow.ClientConfig{
		ShareInstance: factory.descriptor.ShareInstance(),
		Transport:     rpcCatalogTransport{rpc: rpc, progress: factory.catalogProgress}, Verifier: factory.verifier,
	})
	if err != nil {
		return nil, err
	}
	sessionReassembly, err := contentflow.NewReassemblyAccount(
		fmt.Sprintf("receiver-%x", keys.ProtocolSessionID()),
		contentflow.ReassemblyLimits{
			Bytes:   contentflow.DefaultSessionReassemblyBytes,
			Records: contentflow.DefaultSessionReassemblyRecords,
		},
	)
	if err != nil {
		return nil, err
	}
	assembler, err := contentflow.NewAssembler(keys.ProtocolSessionID(), contentflow.ReassemblyHierarchy{
		Process: factory.processReassembly, Share: factory.shareReassembly, Session: sessionReassembly,
	}, factory.now)
	if err != nil {
		return nil, err
	}
	assemblerTransferred := false
	defer func() {
		if !assemblerTransferred {
			assembler.Close()
		}
	}()
	revisions := newReceiverRevisionClient(rpc, factory.opener, factory.descriptor.ChunkSize(), factory.after)
	initialLane := handshake.lane
	blockLane := &receiverBlockLane{
		identity: initialLane, rpc: rpc, assembler: assembler, opener: factory.opener, revisions: revisions,
	}
	lanes, err := transfer.NewLaneSet(transfer.LaneSetConfig{
		ProtocolSessionID: keys.ProtocolSessionID(), RaceWidth: factory.raceWidth, Now: factory.now,
	})
	if err != nil {
		return nil, err
	}
	lanesTransferred := false
	defer func() {
		if !lanesTransferred {
			lanes.Close()
		}
	}()
	if err := lanes.Add(transfer.LaneIdentity{ID: initialLane.ID, Epoch: initialLane.Epoch}, blockLane); err != nil {
		return nil, err
	}
	broker, err := transfer.NewBlockBroker(transfer.BlockBrokerConfig{
		ShareInstance: factory.descriptor.ShareInstance(), Lanes: lanes, ProcessBudget: factory.plaintextProcess,
	})
	if err != nil {
		return nil, err
	}
	brokerTransferred := false
	defer func() {
		if !brokerTransferred {
			broker.Close()
		}
	}()
	receiver := &ReceiverRuntime{
		runtimeCore: runtime, descriptor: factory.descriptor, rpc: rpc,
		catalog: catalogClient, revisions: revisions, assembler: assembler, laneSet: lanes, broker: broker,
		publicKey: append(ed25519.PublicKey(nil), factory.publicKey...), opener: factory.opener,
		semantic: factory.semantic, resourceLease: resourceLease,
	}
	if err := runtime.addFinalizer(receiver.releaseOwnedResources); err != nil {
		return nil, err
	}
	assemblerTransferred = true
	lanesTransferred = true
	brokerTransferred = true
	leaseTransferred = true
	receiver.startReceiver()
	started = true
	return receiver, nil
}

type receiverHandshake struct {
	keys          protocolsession.SessionKeys
	lane          LaneIdentity
	receive       <-chan framechannel.Frame
	authenticator *protocolsession.SenderControlAuthenticator
}

func (factory *ReceiverFactory) completeReceiverHandshake(
	ctx context.Context,
	channel protocolsession.FrameChannel,
) (receiverHandshake, error) {
	receive := channel.Recv()
	receiverPrivate, err := ecdh.X25519().GenerateKey(factory.random)
	if err != nil {
		return receiverHandshake{}, errors.Join(ErrHandshake, err)
	}
	receiverNonce := make([]byte, protocolsession.HandshakeNonceBytes)
	if _, err := io.ReadFull(factory.random, receiverNonce); err != nil {
		return receiverHandshake{}, errors.Join(ErrHandshake, err)
	}
	receiverID, err := factory.instances.NewReceiverInstanceID()
	if err != nil || receiverID.IsZero() {
		return receiverHandshake{}, errors.Join(ErrHandshake, err)
	}
	client, err := protocolsession.NewClientHello(
		factory.descriptor.ShareInstance(), receiverID, receiverNonce, receiverPrivate.PublicKey(), factory.authKey,
	)
	if err != nil {
		return receiverHandshake{}, errors.Join(ErrHandshake, err)
	}
	if err := channel.Send(ctx, framechannel.Frame(client.Encoded())); err != nil {
		return receiverHandshake{}, errors.Join(ErrHandshake, err)
	}
	serverBytes, err := receiveHandshake(ctx, receive)
	if err != nil {
		return receiverHandshake{}, err
	}
	server, err := protocolsession.ParseServerHello(serverBytes, client, factory.publicKey)
	if err != nil {
		return receiverHandshake{}, errors.Join(ErrHandshake, err)
	}
	keys, err := protocolsession.DeriveReceiverSession(receiverPrivate, factory.authKey, client, server)
	if err != nil {
		return receiverHandshake{}, errors.Join(ErrHandshake, err)
	}
	lane := LaneIdentity{ID: server.InitialLaneID(), Epoch: server.InitialLaneEpoch()}
	base := protocolsession.ControlBinding{
		ShareInstance: factory.descriptor.ShareInstance(), ProtocolSessionID: keys.ProtocolSessionID(),
		LaneID: lane.ID, LaneEpoch: lane.Epoch, Direction: protocolsession.DirectionSenderToReceiver,
	}
	authenticator, err := protocolsession.NewSenderControlAuthenticator(factory.publicKey, base, factory.semantic)
	if err != nil {
		keys.Destroy()
		return receiverHandshake{}, err
	}
	return receiverHandshake{
		keys: keys, lane: lane, receive: receive, authenticator: authenticator,
	}, nil
}

func (factory *ReceiverFactory) acquireRuntimeResources() (ReceiverRuntimeResourceLease, error) {
	if factory.resources == nil {
		return nil, nil
	}
	lease, err := factory.resources.AcquireReceiverRuntimeResources()
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, ErrRuntimeConfig
	}
	return lease, nil
}

type ReceiverRuntime struct {
	*runtimeCore
	descriptor    catalog.ShareDescriptor
	publicKey     ed25519.PublicKey
	opener        RecordOpener
	rpc           *rpcClient
	catalog       *catalogflow.Client
	revisions     *receiverRevisionClient
	assembler     *contentflow.Assembler
	laneSet       *transfer.LaneSet
	broker        *transfer.BlockBroker
	semantic      protocolsession.SenderControlSemanticValidator
	resourceLease ReceiverRuntimeResourceLease
	cleanupOnce   sync.Once
}

func (runtime *ReceiverRuntime) startReceiver() {
	runtime.lanes.setDetachHook(func(identity LaneIdentity) {
		runtime.laneSet.Remove(transfer.LaneIdentity{ID: identity.ID, Epoch: identity.Epoch})
	})
	runtime.start()
}

func (runtime *ReceiverRuntime) Descriptor() catalog.ShareDescriptor { return runtime.descriptor }
func (runtime *ReceiverRuntime) Catalog() *catalogflow.Client        { return runtime.catalog }
func (runtime *ReceiverRuntime) LaneSet() *transfer.LaneSet          { return runtime.laneSet }
func (runtime *ReceiverRuntime) BlockBroker() *transfer.BlockBroker  { return runtime.broker }
func (runtime *ReceiverRuntime) OpenRevision(
	ctx context.Context,
	file catalog.FileID,
) (transfer.OpenedRevision, error) {
	return (receiverTransferDependencies{runtime: runtime}).OpenRevision(ctx, file)
}
func (runtime *ReceiverRuntime) ReleaseRevision(ctx context.Context, lease content.LeaseID) error {
	return (receiverTransferDependencies{runtime: runtime}).ReleaseRevision(ctx, lease)
}

func (runtime *ReceiverRuntime) NewTransferJob(
	rules transfer.SelectionRules,
	output transfer.OutputSession,
) (*transfer.TransferJob, error) {
	dependencies := receiverTransferDependencies{runtime: runtime}
	return transfer.NewTransferJob(transfer.TransferJobConfig{
		ShareInstance: runtime.descriptor.ShareInstance(), SyntheticRoot: runtime.descriptor.SyntheticRoot(),
		Rules: rules, Catalog: dependencies, Revisions: dependencies, Blocks: dependencies, Output: output,
	})
}

// receiverTransferDependencies is the semantic boundary between a live session
// and one transfer job. Dependency-specific close errors are useful while their
// owner is live, but once the runtime closes they all describe the same terminal
// fact: retrying another file cannot succeed on this authenticated session.
type receiverTransferDependencies struct {
	runtime *ReceiverRuntime
}

func (dependencies receiverTransferDependencies) AcquireDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	snapshot, release, err := dependencies.runtime.catalog.AcquireDirectory(ctx, directory)
	return snapshot, release, dependencies.classify(err)
}

func (dependencies receiverTransferDependencies) OpenRevision(
	ctx context.Context,
	file catalog.FileID,
) (transfer.OpenedRevision, error) {
	opened, err := dependencies.runtime.revisions.OpenRevision(ctx, file)
	return opened, dependencies.classify(err)
}

func (dependencies receiverTransferDependencies) ReleaseRevision(
	ctx context.Context,
	lease content.LeaseID,
) error {
	return dependencies.classify(dependencies.runtime.revisions.ReleaseRevision(ctx, lease))
}

func (dependencies receiverTransferDependencies) ReadRange(
	ctx context.Context,
	lease content.LeaseID,
	descriptor content.FileRevisionDescriptor,
	requested content.Range,
	sink transfer.RangeSink,
) error {
	return dependencies.classify(
		dependencies.runtime.broker.ReadRange(ctx, lease, descriptor, requested, sink),
	)
}

func (dependencies receiverTransferDependencies) classify(err error) error {
	if err == nil || transfer.IsSessionFailure(err) {
		return err
	}
	runtime := dependencies.runtime
	if errors.Is(err, ErrRuntimeClosed) || runtime == nil || runtime.runtimeCore == nil {
		return transfer.NewSessionFailure(err)
	}
	select {
	case <-runtime.ctx.Done():
		cause := runtime.Err()
		if cause == nil {
			cause = ErrRuntimeClosed
		}
		return transfer.NewSessionFailure(errors.Join(err, cause))
	default:
		return err
	}
}

func (runtime *ReceiverRuntime) Close() {
	if runtime == nil {
		return
	}
	runtime.BeginClose()
	runtime.WaitClosed()
}

func (runtime *ReceiverRuntime) BeginClose() {
	if runtime != nil {
		// Receiver callbacks may request shutdown themselves. Component Stop methods
		// freeze/cancel without joining; the runtime owner performs ordered joins in
		// finalization so cached plaintext and new lane attempts disappear immediately.
		runtime.catalog.Stop()
		runtime.revisions.stop()
		runtime.broker.Stop()
		runtime.laneSet.Stop()
		runtime.beginClose()
	}
}

func (runtime *ReceiverRuntime) WaitClosed() {
	if runtime != nil {
		runtime.waitClosed()
	}
}

func (runtime *ReceiverRuntime) releaseOwnedResources() {
	runtime.cleanupOnce.Do(func() {
		// Catalog loads can be inside user progress or verifier callbacks after the
		// RPC sink closes. Join them before releasing the verifier/key-tree lease.
		runtime.catalog.Close()
		runtime.revisions.close()
		runtime.broker.Close()
		runtime.laneSet.Close()
		runtime.assembler.Close()
		// Every callback that can use these aliases has joined. Shed them before the
		// resource lease is released so this closed runtime cannot pin shared crypto.
		runtime.opener = nil
		runtime.semantic = nil
		clear(runtime.publicKey)
		runtime.publicKey = nil
		if runtime.resourceLease != nil {
			runtime.resourceLease.Release()
			runtime.resourceLease = nil
		}
	})
}

type CatalogScanProgress struct {
	DirectoryID       catalog.DirectoryID
	AttemptID         catalog.ScanAttemptID
	DiscoveredEntries uint64
}

type CatalogScanProgressObserver interface {
	ObserveCatalogScanProgress(context.Context, CatalogScanProgress) error
}

type CatalogScanProgressObserverFunc func(context.Context, CatalogScanProgress) error

func (observe CatalogScanProgressObserverFunc) ObserveCatalogScanProgress(
	ctx context.Context,
	progress CatalogScanProgress,
) error {
	if observe == nil {
		return ErrRuntimeConfig
	}
	return observe(ctx, progress)
}

type rpcCatalogTransport struct {
	rpc      *rpcClient
	progress CatalogScanProgressObserver
}

func (transport rpcCatalogTransport) FetchPage(ctx context.Context, request catalogflow.ListRequest) ([]byte, error) {
	body, err := catalogflow.EncodeListRequest(request)
	if err != nil {
		return nil, err
	}
	call, err := transport.rpc.begin(ctx, protocolsession.MessageListChildren, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transport.rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	var progressState catalogProgressState
	for {
		message, err := transport.rpc.await(ctx, call)
		if err != nil {
			return nil, err
		}
		switch message.Kind() {
		case protocolsession.MessageScanProgress:
			unsigned, err := protocolsession.SenderControlSemanticBody(message)
			if err != nil {
				return nil, transfer.NewSessionFailure(err)
			}
			progress, err := protocolsession.DecodeScanProgress(unsigned)
			notify, progressErr := progressState.observe(progress)
			if err != nil || progressErr != nil {
				return nil, transfer.NewSessionFailure(errors.Join(ErrScanProgress, err, progressErr))
			}
			if !notify {
				continue
			}
			if transport.progress != nil {
				if err := transport.progress.ObserveCatalogScanProgress(ctx, CatalogScanProgress{
					DirectoryID: request.DirectoryID(), AttemptID: progress.AttemptID,
					DiscoveredEntries: progress.DiscoveredEntries,
				}); err != nil {
					return nil, err
				}
			}
			continue
		case protocolsession.MessageOperationError:
			return nil, remoteOperationErrorFor(message, protocolsession.OperationScopeDirectory)
		case protocolsession.MessageCatalogResult:
			unsigned, err := protocolsession.SenderControlSemanticBody(message)
			if err != nil {
				return nil, err
			}
			return catalogflow.DecodeCatalogResult(unsigned)
		default:
			return nil, ErrOperationMissing
		}
	}
}

type catalogProgressState struct {
	attempt    catalog.ScanAttemptID
	discovered uint64
	seen       bool
}

func (state *catalogProgressState) observe(progress protocolsession.ScanProgress) (bool, error) {
	if state.seen && (progress.AttemptID != state.attempt || progress.DiscoveredEntries < state.discovered) {
		return false, ErrScanProgress
	}
	if state.seen && progress.DiscoveredEntries == state.discovered {
		return false, nil
	}
	state.attempt, state.discovered, state.seen = progress.AttemptID, progress.DiscoveredEntries, true
	return true, nil
}
