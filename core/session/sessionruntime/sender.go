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
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/core/catalog"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const DefaultTerminalCleanupTimeout = 15 * time.Second

// TerminalConnectivity is defined at the session consumer boundary. Core never
// learns provider, TURN, relay-node, or path-cost details; it only coordinates
// monotonic share termination with the owner of those resources.
type TerminalConnectivity interface {
	// StopRecovery must return promptly after preventing new registration or
	// path-recovery work. Existing lanes remain available for terminal delivery
	// until Cleanup runs.
	StopRecovery()
	// Cleanup deregisters every relay route and closes connectivity-owned lanes.
	Cleanup(context.Context) error
}

type TerminalConnectivityFuncs struct {
	StopRecoveryFunc func()
	CleanupFunc      func(context.Context) error
}

func (functions TerminalConnectivityFuncs) StopRecovery() {
	if functions.StopRecoveryFunc != nil {
		functions.StopRecoveryFunc()
	}
}

func (functions TerminalConnectivityFuncs) Cleanup(ctx context.Context) error {
	if functions.CleanupFunc == nil {
		return ErrRuntimeConfig
	}
	return functions.CleanupFunc(ctx)
}

type SenderContentFactory interface {
	NewSenderContentService() (*contentflow.SenderService, error)
}

type SenderContentFactoryFunc func() (*contentflow.SenderService, error)

func (function SenderContentFactoryFunc) NewSenderContentService() (*contentflow.SenderService, error) {
	if function == nil {
		return nil, ErrRuntimeConfig
	}
	return function()
}

type SenderCatalogFactory interface {
	NewSenderCatalogService() (*catalogflow.AddressedSenderService, error)
}

type SenderCatalogFactoryFunc func() (*catalogflow.AddressedSenderService, error)

func (function SenderCatalogFactoryFunc) NewSenderCatalogService() (*catalogflow.AddressedSenderService, error) {
	if function == nil {
		return nil, ErrRuntimeConfig
	}
	return function()
}

type InitialLaneIDSource interface {
	NextInitialLaneID() (uint32, error)
}

type InitialLaneIDSourceFunc func() (uint32, error)

func (function InitialLaneIDSourceFunc) NextInitialLaneID() (uint32, error) {
	if function == nil {
		return 0, ErrRuntimeConfig
	}
	return function()
}

type SenderFactoryConfig struct {
	ShareInstance        catalog.ShareInstance
	SessionAuthKey       []byte
	SenderPrivateKey     ed25519.PrivateKey
	Catalog              SenderCatalogFactory
	Content              SenderContentFactory
	Peers                SenderPeerHandlerFactory
	ReplayGuard          *protocolsession.ClientHelloReplayGuard
	Random               io.Reader
	InitialLaneIDs       InitialLaneIDSource
	OperationLimits      protocolsession.OperationLimits
	RouterLimits         protocolsession.RouterLimits
	Now                  func() time.Time
	TerminalConnectivity TerminalConnectivity
	TerminalTimeout      time.Duration
	TerminalObserver     SenderTerminalObserver
}

type SenderFactory struct {
	share                catalog.ShareInstance
	authKey              []byte
	privateKey           ed25519.PrivateKey
	catalog              SenderCatalogFactory
	content              SenderContentFactory
	peers                SenderPeerHandlerFactory
	replay               *protocolsession.ClientHelloReplayGuard
	random               *lockedReader
	laneIDs              InitialLaneIDSource
	operationLimits      protocolsession.OperationLimits
	routerLimits         protocolsession.RouterLimits
	now                  func() time.Time
	terminalConnectivity TerminalConnectivity
	terminalTimeout      time.Duration
	terminalObserver     SenderTerminalObserver
	admissionContext     context.Context
	cancelAdmissions     context.CancelFunc

	mu           sync.Mutex
	stopping     bool
	admissions   sync.WaitGroup
	sessions     map[protocolsession.ProtocolSessionID]*SenderRuntime
	terminalDone chan struct{}
	terminalErr  error
}

var defaultLaneSequence atomic.Uint32

func NewSenderFactory(config SenderFactoryConfig) (*SenderFactory, error) {
	if config.ShareInstance.IsZero() || len(config.SessionAuthKey) != protocolsession.SessionAuthKeyBytes ||
		len(config.SenderPrivateKey) != ed25519.PrivateKeySize || config.Catalog == nil || config.Content == nil ||
		config.Peers == nil ||
		config.TerminalConnectivity == nil || config.TerminalTimeout < 0 {
		return nil, ErrRuntimeConfig
	}
	if config.TerminalTimeout == 0 {
		config.TerminalTimeout = DefaultTerminalCleanupTimeout
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.ReplayGuard == nil {
		var err error
		config.ReplayGuard, err = protocolsession.NewClientHelloReplayGuard(DefaultOperationTombstones, config.Now)
		if err != nil {
			return nil, err
		}
	}
	if config.InitialLaneIDs == nil {
		config.InitialLaneIDs = InitialLaneIDSourceFunc(func() (uint32, error) {
			for {
				value := defaultLaneSequence.Add(1)
				if value != 0 {
					return value, nil
				}
			}
		})
	}
	admissionContext, cancelAdmissions := context.WithCancel(context.Background())
	return &SenderFactory{
		share: config.ShareInstance, authKey: append([]byte(nil), config.SessionAuthKey...),
		privateKey: append(ed25519.PrivateKey(nil), config.SenderPrivateKey...),
		catalog:    config.Catalog, content: config.Content, peers: config.Peers, replay: config.ReplayGuard,
		random: &lockedReader{reader: config.Random}, laneIDs: config.InitialLaneIDs,
		operationLimits: config.OperationLimits, routerLimits: config.RouterLimits, now: config.Now,
		terminalConnectivity: config.TerminalConnectivity, terminalTimeout: config.TerminalTimeout,
		terminalObserver: config.TerminalObserver,
		admissionContext: admissionContext, cancelAdmissions: cancelAdmissions,
		sessions: make(map[protocolsession.ProtocolSessionID]*SenderRuntime), terminalDone: make(chan struct{}),
	}, nil
}

type SenderChannelKind uint8

const (
	SenderChannelNewProtocolSession SenderChannelKind = iota + 1
	SenderChannelAttachedLane
)

type SenderChannelAdmission struct {
	Kind    SenderChannelKind
	Session *SenderRuntime
	Lane    LaneIdentity
}

// AdmitChannel owns the first-frame dispatch boundary for opaque relay and P2P
// channels. A valid WS2A can only attach to a still-live ProtocolSession; every
// other candidate must prove a fresh WS2C transcript before it becomes a new
// session. Keeping this decision beside both authenticators prevents a transport
// owner from routing on untrusted session identities.
func (factory *SenderFactory) AdmitChannel(
	ctx context.Context,
	channel protocolsession.FrameChannel,
) (SenderChannelAdmission, error) {
	if factory == nil || channel == nil || ctx == nil {
		return SenderChannelAdmission{}, ErrRuntimeConfig
	}
	admissionContext, endAdmission, ok := factory.beginAdmission(ctx)
	if !ok {
		return SenderChannelAdmission{}, ErrRuntimeClosed
	}
	defer endAdmission()
	ctx = admissionContext
	receive := channel.Recv()
	encoded, err := receiveHandshake(ctx, receive)
	if err != nil {
		return SenderChannelAdmission{}, err
	}
	if _, _, routeErr := protocolsession.UntrustedLaneHelloRoute(encoded); routeErr == nil {
		identity, err := factory.attachCandidate(ctx, channel, receive, encoded)
		if err != nil {
			return SenderChannelAdmission{}, err
		}
		return SenderChannelAdmission{Kind: SenderChannelAttachedLane, Lane: identity}, nil
	}
	session, err := factory.acceptClient(ctx, channel, receive, encoded)
	if err != nil {
		return SenderChannelAdmission{}, err
	}
	return SenderChannelAdmission{Kind: SenderChannelNewProtocolSession, Session: session}, nil
}

func (factory *SenderFactory) Accept(ctx context.Context, channel protocolsession.FrameChannel) (*SenderRuntime, error) {
	if factory == nil || channel == nil || ctx == nil {
		return nil, ErrRuntimeConfig
	}
	admissionContext, endAdmission, ok := factory.beginAdmission(ctx)
	if !ok {
		return nil, ErrRuntimeClosed
	}
	defer endAdmission()
	ctx = admissionContext
	receive := channel.Recv()
	clientBytes, err := receiveHandshake(ctx, receive)
	if err != nil {
		return nil, err
	}
	return factory.acceptClient(ctx, channel, receive, clientBytes)
}

func (factory *SenderFactory) beginAdmission(
	caller context.Context,
) (context.Context, func(), bool) {
	if caller == nil {
		return nil, nil, false
	}
	factory.mu.Lock()
	if factory.stopping {
		factory.mu.Unlock()
		return nil, nil, false
	}
	// Add and the terminal transition share this lock, so the terminal goroutine
	// can never begin Wait concurrently with a newly admitted handshake.
	factory.admissions.Add(1)
	lifecycle := factory.admissionContext
	factory.mu.Unlock()
	ctx, cancel := context.WithCancel(caller)
	stopLifecycle := context.AfterFunc(lifecycle, cancel)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			stopLifecycle()
			cancel()
			factory.admissions.Done()
		})
	}, true
}

func (factory *SenderFactory) acceptClient(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	receive <-chan framechannel.Frame,
	clientBytes []byte,
) (result *SenderRuntime, resultErr error) {
	keys, laneID, err := factory.completeSenderHandshake(ctx, channel, clientBytes)
	if err != nil {
		return nil, err
	}
	sessionID := keys.ProtocolSessionID()
	catalogService, err := factory.catalog.NewSenderCatalogService()
	if err != nil || catalogService == nil {
		keys.Destroy()
		return nil, errors.Join(ErrRuntimeConfig, err)
	}
	contentService, err := factory.content.NewSenderContentService()
	if err != nil || contentService == nil {
		keys.Destroy()
		return nil, errors.Join(ErrRuntimeConfig, err)
	}
	contentTransferred := false
	defer func() {
		if !contentTransferred {
			resultErr = errors.Join(resultErr, contentService.Close())
		}
	}()
	handOff := &handedOffChannel{FrameChannel: channel, receive: receive}
	runtime, err := newRuntime(runtimeConfig{
		Share: factory.share, Role: protocolsession.RoleSender, Keys: keys,
		LaneID: laneID, LaneEpoch: 0, Channel: handOff, Random: factory.random,
		Authenticator: protocolsession.InboundMessageAuthenticatorFunc(
			func(uint64, protocolsession.Message) (protocolsession.InboundAuthenticationResult, error) {
				return protocolsession.InboundAuthenticationResult{}, nil
			},
		),
		Continuations:   factory.peers,
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
	if err := runtime.addFinalizer(func() {
		if closeErr := contentService.Close(); closeErr != nil {
			runtime.recordError(closeErr)
		}
	}); err != nil {
		return nil, err
	}
	contentTransferred = true
	outbound := senderOutbound{
		runtime: runtime, privateKey: factory.privateKey, observer: factory.terminalObserver,
	}
	laneRegistry, err := protocolsession.NewLaneRegistry(protocolsession.LaneRegistryConfig{
		ShareInstance: factory.share, ProtocolSessionID: sessionID,
		ReceiverToSender: keys.ReceiverToSender(), SenderSigningKey: factory.privateKey,
		InitialLaneID: laneID, Now: factory.now,
	})
	if err != nil {
		return nil, err
	}
	registryTransferred := false
	defer func() {
		if !registryTransferred {
			laneRegistry.Close()
		}
	}()
	contentHandler, err := contentflow.NewSenderHandler(contentflow.SenderHandlerConfig{
		Service: contentService, Outbound: outbound,
	})
	if err != nil {
		return nil, err
	}
	catalogHandler := newCatalogHandler(catalogService, outbound)
	laneHandler := newLaneGrantHandler(laneRegistry, outbound, factory.random)
	sender := &SenderRuntime{
		runtimeCore: runtime, outbound: outbound, lanesRegistry: laneRegistry,
		catalogHandler: catalogHandler, contentHandler: contentHandler,
		compositeDone: make(chan struct{}),
	}
	if err := runtime.addFinalizer(func() {
		// Closing the registry at core completion rejects new lane proofs. Factory
		// removal waits for the terminal worker's composite lifetime below.
		laneRegistry.Close()
	}); err != nil {
		return nil, err
	}
	registryTransferred = true
	peerHandler, err := factory.peers.NewSenderPeerHandler(senderPeerSession{runtime: sender, outbound: outbound})
	if err != nil || peerHandler == nil {
		return nil, errors.Join(ErrRuntimeConfig, err)
	}
	if err := registerSenderHandlers(runtime.router, catalogHandler, contentHandler, laneHandler, peerHandler); err != nil {
		return nil, err
	}
	runtime.lanes.setDetachHook(func(identity LaneIdentity) {
		laneRegistry.Release(identity.ID, identity.Epoch)
	})
	factory.mu.Lock()
	if factory.stopping {
		factory.mu.Unlock()
		runtime.cancel()
		return nil, ErrRuntimeClosed
	}
	factory.sessions[sessionID] = sender
	factory.mu.Unlock()
	sender.trackComposite(factory, sessionID)
	runtime.start(contentHandler.Run, catalogHandler.Run, laneHandler.Run, peerHandler.Run)
	started = true
	return sender, nil
}

func (factory *SenderFactory) completeSenderHandshake(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	clientBytes []byte,
) (protocolsession.SessionKeys, uint32, error) {
	client, err := factory.replay.AcceptClientHello(clientBytes, factory.share, factory.authKey)
	if err != nil {
		return protocolsession.SessionKeys{}, 0, errors.Join(ErrHandshake, err)
	}
	senderPrivate, err := ecdh.X25519().GenerateKey(factory.random)
	if err != nil {
		return protocolsession.SessionKeys{}, 0, errors.Join(ErrHandshake, err)
	}
	senderNonce := make([]byte, protocolsession.HandshakeNonceBytes)
	if _, err := io.ReadFull(factory.random, senderNonce); err != nil {
		return protocolsession.SessionKeys{}, 0, errors.Join(ErrHandshake, err)
	}
	laneID, err := factory.laneIDs.NextInitialLaneID()
	if err != nil || laneID == 0 {
		return protocolsession.SessionKeys{}, 0, errors.Join(ErrHandshake, err)
	}
	server, err := protocolsession.NewServerHello(
		client, senderNonce, senderPrivate.PublicKey(), laneID, factory.privateKey,
	)
	if err != nil {
		return protocolsession.SessionKeys{}, 0, errors.Join(ErrHandshake, err)
	}
	if err := channel.Send(ctx, framechannel.Frame(server.Encoded())); err != nil {
		return protocolsession.SessionKeys{}, 0, errors.Join(ErrHandshake, err)
	}
	keys, err := protocolsession.DeriveSenderSession(senderPrivate, factory.authKey, client, server)
	if err != nil {
		return protocolsession.SessionKeys{}, 0, errors.Join(ErrHandshake, err)
	}
	return keys, laneID, nil
}

type handedOffChannel struct {
	protocolsession.FrameChannel
	receive <-chan framechannel.Frame
}

func (channel *handedOffChannel) Recv() <-chan framechannel.Frame { return channel.receive }

func receiveHandshake(ctx context.Context, receive <-chan framechannel.Frame) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case frame, ok := <-receive:
		if !ok || len(frame) == 0 {
			return nil, ErrHandshake
		}
		return append([]byte(nil), frame...), nil
	}
}

type SenderRuntime struct {
	*runtimeCore
	outbound       senderOutbound
	lanesRegistry  *protocolsession.LaneRegistry
	catalogHandler *catalogHandler
	contentHandler *contentflow.SenderHandler
	stopMu         sync.Mutex
	stopStarted    bool
	closeStarted   bool
	stopDone       chan struct{}
	stopErr        error
	compositeDone  chan struct{}
}

func (runtime *SenderRuntime) LaneRegistry() *protocolsession.LaneRegistry {
	return runtime.lanesRegistry
}

func (outbound senderOutbound) SendFragment(ctx context.Context, message protocolsession.Message) error {
	operationID, ok := message.OperationID()
	if !ok {
		return ErrOperationMissing
	}
	transaction, err := beginOutboundTransaction(outbound.runtime, ctx, operationID)
	if err != nil {
		return err
	}
	defer transaction.Close()
	_, err = transaction.Run(ctx, func(
		lane selectedLane,
		permit protocolsession.OutboundReplayPermit,
	) (protocolsession.SendReceipt, error) {
		if !permit.IsZero() {
			return lane.writer.TryDataReplay(message, permit)
		}
		return lane.writer.TryAuthorizedData(message, transaction.authority)
	})
	if ctx.Err() != nil || outbound.runtime.ctx.Err() != nil {
		err = errors.Join(
			err, outbound.runtime.abandonOutboundOperation(
				operationID, transaction.route, transaction.generation,
			),
		)
	}
	return err
}

func (outbound senderOutbound) SendOperationError(
	ctx context.Context,
	operationID protocolsession.OperationID,
	failure contentflow.OperationFailure,
) error {
	body, err := protocolsession.EncodeOperationFailure(failure)
	if err != nil {
		return errors.Join(err, outbound.runtime.abandonBoundOutboundOperation(ctx, operationID))
	}
	_, err = outbound.SendControl(ctx, protocolsession.MessageOperationError, operationID, body)
	return err
}

func (factory *SenderFactory) ActiveSessions() int {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return len(factory.sessions)
}

func (factory *SenderFactory) String() string {
	return fmt.Sprintf("sender factory for share %x", factory.share)
}
