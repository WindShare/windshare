package receiversession

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/senderauth"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/admissiongate"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
	"github.com/windshare/windshare/core/session/revisionaccess"
	"github.com/windshare/windshare/core/transfer"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"io"
	"sync"
	"time"
)

type RecordOpener interface {
	OpenRevision(catalog.FileID, uint32, []byte) (content.FileRevisionDescriptor, error)
	OpenBlock(content.FileRevisionDescriptor, uint64, []byte) (records.BlockRecord, error)
}

type InstanceSource interface {
	NewReceiverInstanceID() (protocolsession.ReceiverInstanceID, error)
}

type InstanceSourceFunc func() (protocolsession.ReceiverInstanceID, error)

func (function InstanceSourceFunc) NewReceiverInstanceID() (protocolsession.ReceiverInstanceID, error) {
	if function == nil {
		return protocolsession.ReceiverInstanceID{}, sessioncontract.ErrRuntimeConfig
	}
	return function()
}

type RuntimeResourceLease interface {
	Release()
}

type RuntimeResourceSource interface {
	AcquireReceiverRuntimeResources() (RuntimeResourceLease, error)
}

type PeerSemantics interface {
	protocolsession.SenderControlSemanticValidator
	protocolsession.OperationContinuationClassifier
}

type FactoryConfig struct {
	ContentRoutePolicy                transferlanes.ContentRoutePolicy
	Descriptor                        catalog.ShareDescriptor
	SessionAuthKey                    []byte
	Sender                            *senderauth.Verifier
	CatalogVerifier                   catalogflow.ObjectVerifier
	RecordOpener                      RecordOpener
	ReassemblyProcess                 *contentflow.ReassemblyAccount
	ReassemblyShare                   *contentflow.ReassemblyAccount
	PlaintextProcess                  *transfer.PlaintextBudget
	Random                            io.Reader
	ReceiverInstances                 InstanceSource
	CatalogProgress                   CatalogScanProgressObserver
	PeerControls                      PeerSemantics
	RuntimeResources                  RuntimeResourceSource
	OperationLimits                   protocolsession.OperationLimits
	RouterLimits                      protocolsession.RouterLimits
	Now                               func() time.Time
	After                             func(time.Duration) <-chan time.Time
	ProtocolObservations              observationstream.Producer[sessionobs.ProtocolObservation]
	LaneSettlementObservationCapacity transferlanes.SettlementObservationCapacity
}

type Factory struct {
	requests                          func(*sessioncore.Runtime, io.Reader) requestClient
	contentRoutePolicy                transferlanes.ContentRoutePolicy
	descriptor                        catalog.ShareDescriptor
	authKey                           []byte
	sender                            *senderauth.Verifier
	verifier                          catalogflow.ObjectVerifier
	opener                            RecordOpener
	processReassembly                 *contentflow.ReassemblyAccount
	shareReassembly                   *contentflow.ReassemblyAccount
	plaintextProcess                  *transfer.PlaintextBudget
	random                            *sessioncore.LockedReader
	instances                         InstanceSource
	catalogProgress                   CatalogScanProgressObserver
	semantic                          protocolsession.SenderControlSemanticValidator
	peerSemantics                     PeerSemantics
	resources                         RuntimeResourceSource
	operationLimits                   protocolsession.OperationLimits
	routerLimits                      protocolsession.RouterLimits
	now                               func() time.Time
	after                             func(time.Duration) <-chan time.Time
	protocolObservations              observationstream.Producer[sessionobs.ProtocolObservation]
	laneSettlementObservationCapacity transferlanes.SettlementObservationCapacity

	mu         sync.Mutex
	admissions admissiongate.Gate
	closeDone  chan struct{}
}

func NewFactory(config FactoryConfig) (*Factory, error) {
	if config.Descriptor.ShareInstance().IsZero() ||
		len(config.SessionAuthKey) != protocolsession.SessionAuthKeyBytes ||
		!config.Sender.Valid() ||
		!ed25519.PublicKey(config.Descriptor.SenderPublicKey()).Equal(config.Sender.PublicKey()) ||
		config.CatalogVerifier == nil || config.RecordOpener == nil ||
		config.ReassemblyProcess == nil || config.ReassemblyShare == nil || config.PlaintextProcess == nil {
		return nil, sessioncontract.ErrRuntimeConfig
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	lockedRandom := sessioncore.NewLockedReader(config.Random)
	if config.ReceiverInstances == nil {
		config.ReceiverInstances = InstanceSourceFunc(func() (protocolsession.ReceiverInstanceID, error) {
			value := make([]byte, protocolsession.IdentityBytes)
			if _, err := io.ReadFull(lockedRandom, value); err != nil {
				return protocolsession.ReceiverInstanceID{}, err
			}
			return protocolsession.ReceiverInstanceIDFromBytes(value)
		})
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.After == nil {
		config.After = time.After
	}
	semantic, err := newReceiverSenderControlValidator(config.PeerControls)
	if err != nil {
		return nil, errors.Join(sessioncontract.ErrRuntimeConfig, err)
	}
	return &Factory{
		requests: func(runtime *sessioncore.Runtime, random io.Reader) requestClient {
			return sessioncore.NewRequests(runtime, random)
		},
		descriptor: config.Descriptor, authKey: append([]byte(nil), config.SessionAuthKey...),
		sender:   config.Sender,
		verifier: config.CatalogVerifier, opener: config.RecordOpener,
		processReassembly: config.ReassemblyProcess, shareReassembly: config.ReassemblyShare,
		plaintextProcess: config.PlaintextProcess, random: lockedRandom,
		instances:          config.ReceiverInstances,
		contentRoutePolicy: config.ContentRoutePolicy,
		catalogProgress:    config.CatalogProgress, semantic: semantic, peerSemantics: config.PeerControls,
		resources:       config.RuntimeResources,
		operationLimits: config.OperationLimits, routerLimits: config.RouterLimits,
		now: config.Now, after: config.After,
		protocolObservations:              config.ProtocolObservations,
		laneSettlementObservationCapacity: config.LaneSettlementObservationCapacity,
		closeDone:                         make(chan struct{}),
	}, nil
}

func (factory *Factory) Connect(ctx context.Context, channel protocolsession.FrameChannel, route transferlanes.Route) (result *Runtime, resultErr error) {
	if factory == nil || channel == nil || ctx == nil {
		return nil, sessioncontract.ErrRuntimeConfig
	}
	admissionContext, endAdmission, ok := factory.beginAdmission(ctx)
	if !ok {
		return nil, sessioncontract.ErrRuntimeClosed
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
	handOff := sessioncore.HandOff(channel, handshake.receive)
	wire, err := sessioncore.New(sessioncore.Config{
		Share: factory.descriptor.ShareInstance(), Role: protocolsession.RoleReceiver, Keys: keys,
		LaneID: handshake.lane.ID, LaneEpoch: handshake.lane.Epoch, Channel: handOff,
		Random: factory.random, Authenticator: handshake.authenticator,
		Continuations:   factory.peerSemantics,
		OperationLimits: factory.operationLimits, RouterLimits: factory.routerLimits, Now: factory.now,
		ProtocolObservations: factory.protocolObservations,
		InitialRequestTiming: requestlane.New(handshake.responseTime),
	})
	if err != nil {
		keys.Destroy()
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			wire.AbortBeforeStart()
		}
	}()
	rpc := factory.requests(wire, factory.random)
	if err := wire.AddFinalizer(rpc.Close); err != nil {
		return nil, err
	}
	if err := rpc.Register(); err != nil {
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
	lanes, err := transferlanes.New(transferlanes.Config{
		ProtocolSessionID: keys.ProtocolSessionID(), Now: factory.now, After: factory.after,
		SettlementObservationCapacity: factory.laneSettlementObservationCapacity,
		ContentRoutePolicy:            factory.contentRoutePolicy,
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
	if err := lanes.Add(
		transferlanes.Identity{ID: initialLane.ID, Epoch: initialLane.Epoch},
		route,
		blockLane,
	); err != nil {
		return nil, err
	}
	broker, err := transfer.NewBlockBroker(transfer.BlockBrokerConfig{
		ShareInstance: factory.descriptor.ShareInstance(), Blocks: lanes, ProcessBudget: factory.plaintextProcess,
	})
	if err != nil {
		return nil, err
	}
	brokerTransferred := false
	defer func() {
		if !brokerTransferred {
			resultErr = errors.Join(resultErr, broker.Close())
		}
	}()
	receiver := &Runtime{
		core: wire, lifecycle: wire, descriptor: factory.descriptor, rpc: rpc,
		catalog: catalogClient, revisions: revisions, assembler: assembler, laneSet: lanes, broker: broker,
		sender: factory.sender, opener: factory.opener,
		semantic: factory.semantic, resourceLease: resourceLease,
	}
	if err := receiver.bindRevisionAccess(); err != nil {
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
	responseTime  time.Duration
	keys          protocolsession.SessionKeys
	lane          sessioncontract.LaneIdentity
	receive       <-chan framechannel.Frame
	authenticator *protocolsession.SenderControlAuthenticator
}

func (factory *Factory) completeReceiverHandshake(
	ctx context.Context,
	channel protocolsession.FrameChannel,
) (receiverHandshake, error) {
	receive := channel.Recv()
	receiverPrivate, err := ecdh.X25519().GenerateKey(factory.random)
	if err != nil {
		return receiverHandshake{}, errors.Join(sessioncontract.ErrHandshake, err)
	}
	receiverNonce := make([]byte, protocolsession.HandshakeNonceBytes)
	if _, err := io.ReadFull(factory.random, receiverNonce); err != nil {
		return receiverHandshake{}, errors.Join(sessioncontract.ErrHandshake, err)
	}
	receiverID, err := factory.instances.NewReceiverInstanceID()
	if err != nil || receiverID.IsZero() {
		return receiverHandshake{}, errors.Join(sessioncontract.ErrHandshake, err)
	}
	client, err := protocolsession.NewClientHello(
		factory.descriptor.ShareInstance(), receiverID, receiverNonce, receiverPrivate.PublicKey(), factory.authKey,
	)
	if err != nil {
		return receiverHandshake{}, errors.Join(sessioncontract.ErrHandshake, err)
	}
	started := factory.now()
	if err := channel.Send(ctx, framechannel.Frame(client.Encoded())); err != nil {
		return receiverHandshake{}, errors.Join(sessioncontract.ErrHandshake, err)
	}
	serverBytes, err := sessioncore.ReceiveHandshake(ctx, receive)
	if err != nil {
		return receiverHandshake{}, err
	}
	server, err := protocolsession.ParseServerHello(serverBytes, client, factory.sender)
	if err != nil {
		return receiverHandshake{}, sessionProtocolBoundaryError(errors.Join(sessioncontract.ErrHandshake, err))
	}
	responseTime := factory.now().Sub(started)
	keys, err := protocolsession.DeriveReceiverSession(receiverPrivate, factory.authKey, client, server)
	if err != nil {
		return receiverHandshake{}, sessionProtocolBoundaryError(errors.Join(sessioncontract.ErrHandshake, err))
	}
	lane := sessioncontract.LaneIdentity{ID: server.InitialLaneID(), Epoch: server.InitialLaneEpoch()}
	base := protocolsession.ControlBinding{
		ShareInstance: factory.descriptor.ShareInstance(), ProtocolSessionID: keys.ProtocolSessionID(),
		LaneID: lane.ID, LaneEpoch: lane.Epoch, Direction: protocolsession.DirectionSenderToReceiver,
	}
	authenticator, err := protocolsession.NewSenderControlAuthenticator(factory.sender, base, factory.semantic)
	if err != nil {
		keys.Destroy()
		return receiverHandshake{}, err
	}
	return receiverHandshake{
		keys: keys, lane: lane, receive: receive, authenticator: authenticator, responseTime: responseTime,
	}, nil
}

func (factory *Factory) acquireRuntimeResources() (RuntimeResourceLease, error) {
	if factory.resources == nil {
		return nil, nil
	}
	lease, err := factory.resources.AcquireReceiverRuntimeResources()
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, sessioncontract.ErrRuntimeConfig
	}
	return lease, nil
}

type Runtime struct {
	lifecycle     lifecycle
	core          *sessioncore.Runtime
	descriptor    catalog.ShareDescriptor
	sender        *senderauth.Verifier
	opener        RecordOpener
	rpc           requestClient
	catalog       *catalogflow.Client
	revisions     *receiverRevisionClient
	access        *revisionaccess.Reader
	assembler     *contentflow.Assembler
	laneSet       *transferlanes.Set
	broker        *transfer.BlockBroker
	semantic      protocolsession.SenderControlSemanticValidator
	resourceLease RuntimeResourceLease
	cleanupOnce   sync.Once
	cleanupMu     sync.Mutex
	cleanupErr    error
}

func (runtime *Runtime) startReceiver() {
	// Both registries retain the runtime-lane then content-lane lock order.
	runtime.core.ConfigureLanes(func(identity sessioncontract.LaneIdentity) time.Duration {
		return runtime.laneSet.EstimateQueuedContent(transferlanes.Identity{ID: identity.ID, Epoch: identity.Epoch})
	}, func(identity sessioncontract.LaneIdentity) {
		runtime.laneSet.Remove(transferlanes.Identity{ID: identity.ID, Epoch: identity.Epoch})
	})
	runtime.core.Start()
}

func (runtime *Runtime) Descriptor() catalog.ShareDescriptor { return runtime.descriptor }

func (runtime *Runtime) Catalog() *catalogflow.Client { return runtime.catalog }

func (runtime *Runtime) LaneSet() *transferlanes.Set { return runtime.laneSet }

func (runtime *Runtime) BlockBroker() *transfer.BlockBroker { return runtime.broker }

func (runtime *Runtime) OpenRevision(
	ctx context.Context,
	file catalog.FileID,
) (revisionaccess.Lease, error) {
	return (receiverLeaseDependencies{receiverTransferDependencies{runtime: runtime}}).OpenLease(ctx, file)
}

func (runtime *Runtime) ReleaseRevision(ctx context.Context, lease content.LeaseID) error {
	return (receiverLeaseDependencies{receiverTransferDependencies{runtime: runtime}}).ReleaseLease(ctx, lease)
}

// RejectShareIdentity ends authority after the application authenticates a
// contradictory share descriptor. It cannot authorize path recovery, and keeps
// the protocol cause distinct from the component cancellation used for cleanup.
func (runtime *Runtime) RejectShareIdentity(cause error) {
	if runtime == nil || runtime.core == nil || cause == nil {
		return
	}
	runtime.core.Fail(sessionProtocolBoundaryError(cause))
	runtime.BeginClose()
}

func (runtime *Runtime) Close() {
	if runtime == nil {
		return
	}
	runtime.BeginClose()
	runtime.WaitClosed()
}

// CleanupError is final after Done closes. It preserves resource-settlement
// failures independently of the protocol cause that originally stopped a session.
func (runtime *Runtime) CleanupError() error {
	if runtime == nil {
		return nil
	}
	runtime.cleanupMu.Lock()
	defer runtime.cleanupMu.Unlock()
	return runtime.cleanupErr
}

func (runtime *Runtime) BeginClose() {
	if runtime != nil {
		// Receiver callbacks may request shutdown themselves. Component Stop methods
		// freeze/cancel without joining; the runtime owner performs ordered joins in
		// finalization so cached plaintext and new lane attempts disappear immediately.
		if runtime.access != nil {
			runtime.access.Stop()
		}
		runtime.catalog.Stop()
		runtime.revisions.stop()
		runtime.broker.Stop()
		runtime.laneSet.Stop()
		runtime.core.BeginClose()
	}
}

func (runtime *Runtime) WaitClosed() {
	if runtime != nil {
		runtime.core.WaitClosed()
	}
}

func (runtime *Runtime) releaseOwnedResources() {
	runtime.cleanupOnce.Do(func() {
		// Catalog loads can be inside user progress or verifier callbacks after the
		// RPC sink closes. Join them before releasing the verifier/key-tree lease.
		if runtime.access != nil {
			runtime.access.Stop()
		}
		runtime.catalog.Close()
		runtime.revisions.close()
		// A winning fetch may leave canceled hedges owned by LaneSet. Join those
		// workers before interpreting the broker's remaining capacity as a leak.
		runtime.broker.Stop()
		runtime.laneSet.Close()
		brokerErr := runtime.broker.Close()
		runtime.cleanupMu.Lock()
		if brokerErr != nil {
			runtime.cleanupErr = fmt.Errorf("receiver session %v plaintext settlement: %w", runtime.ProtocolSessionID(), brokerErr)
		}
		runtime.cleanupMu.Unlock()
		runtime.assembler.Close()
		// Every callback that can use these aliases has joined. Shed them before the
		// resource lease is released so this closed runtime cannot pin shared crypto.
		runtime.opener = nil
		runtime.semantic = nil
		runtime.sender = nil
		if runtime.resourceLease != nil {
			runtime.resourceLease.Release()
			runtime.resourceLease = nil
		}
	})
}

// BindDownloadMetrics binds one generation's availability and delivery producers
// to the same download observation without sharing their synchronization state.
func (runtime *Runtime) BindDownloadMetrics(metrics *downloadmetrics.Metrics) {
	runtime.laneSet.BindDownloadMetrics(metrics)
	runtime.broker.BindDownloadMetrics(metrics)
}
