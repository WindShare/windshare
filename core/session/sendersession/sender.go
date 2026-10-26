package sendersession

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/observationstream"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/admissiongate"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"io"
	"sync"
	"sync/atomic"
	"time"
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
		return sessioncontract.ErrRuntimeConfig
	}
	return functions.CleanupFunc(ctx)
}

type ContentFactory interface {
	NewSenderContentService(protocolsession.ProtocolSessionID) (*contentflow.SenderService, error)
}

type ContentFactoryFunc func(protocolsession.ProtocolSessionID) (*contentflow.SenderService, error)

func (function ContentFactoryFunc) NewSenderContentService(
	sessionID protocolsession.ProtocolSessionID,
) (*contentflow.SenderService, error) {
	if function == nil || sessionID.IsZero() {
		return nil, sessioncontract.ErrRuntimeConfig
	}
	return function(sessionID)
}

type CatalogFactory interface {
	NewSenderCatalogService() (*catalogflow.AddressedSenderService, error)
}

type CatalogFactoryFunc func() (*catalogflow.AddressedSenderService, error)

func (function CatalogFactoryFunc) NewSenderCatalogService() (*catalogflow.AddressedSenderService, error) {
	if function == nil {
		return nil, sessioncontract.ErrRuntimeConfig
	}
	return function()
}

type InitialLaneIDSource interface {
	NextInitialLaneID() (uint32, error)
}

type InitialLaneIDSourceFunc func() (uint32, error)

func (function InitialLaneIDSourceFunc) NextInitialLaneID() (uint32, error) {
	if function == nil {
		return 0, sessioncontract.ErrRuntimeConfig
	}
	return function()
}

type FactoryConfig struct {
	ShareInstance           catalog.ShareInstance
	SessionAuthKey          []byte
	SenderPrivateKey        ed25519.PrivateKey
	Catalog                 CatalogFactory
	Content                 ContentFactory
	Peers                   PeerHandlerFactory
	ReplayGuard             *protocolsession.ClientHelloReplayGuard
	Random                  io.Reader
	InitialLaneIDs          InitialLaneIDSource
	OperationLimits         protocolsession.OperationLimits
	RouterLimits            protocolsession.RouterLimits
	Now                     func() time.Time
	TerminalConnectivity    TerminalConnectivity
	TerminalTimeout         time.Duration
	TerminalSendObserver    sessionobs.SenderTerminalSendObserver
	SessionTerminalObserver sessionobs.SenderSessionTerminalObserver
	ProtocolObservations    observationstream.Producer[sessionobs.ProtocolObservation]
}

type Factory struct {
	share                catalog.ShareInstance
	authKey              []byte
	privateKey           ed25519.PrivateKey
	catalog              CatalogFactory
	content              ContentFactory
	peers                PeerHandlerFactory
	replay               *protocolsession.ClientHelloReplayGuard
	random               *sessioncore.LockedReader
	laneIDs              InitialLaneIDSource
	operationLimits      protocolsession.OperationLimits
	routerLimits         protocolsession.RouterLimits
	now                  func() time.Time
	terminalConnectivity TerminalConnectivity
	terminalTimeout      time.Duration
	terminalObserver     *senderTerminalObservers
	protocolObservations observationstream.Producer[sessionobs.ProtocolObservation]

	mu           sync.Mutex
	stopping     bool
	admissions   admissiongate.Gate
	sessions     map[protocolsession.ProtocolSessionID]*Runtime
	terminalDone chan struct{}
	terminalErr  error
}

var defaultLaneSequence atomic.Uint32

func NewFactory(config FactoryConfig) (*Factory, error) {
	if config.ShareInstance.IsZero() || len(config.SessionAuthKey) != protocolsession.SessionAuthKeyBytes ||
		len(config.SenderPrivateKey) != ed25519.PrivateKeySize || config.Catalog == nil || config.Content == nil ||
		config.Peers == nil ||
		config.TerminalConnectivity == nil || config.TerminalTimeout < 0 {
		return nil, sessioncontract.ErrRuntimeConfig
	}
	if config.TerminalTimeout == 0 {
		config.TerminalTimeout = DefaultTerminalCleanupTimeout
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.ReplayGuard == nil {
		var err error
		config.ReplayGuard, err = protocolsession.NewClientHelloReplayGuard(sessioncontract.DefaultTrackedOperations, config.Now)
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
	return &Factory{
		share: config.ShareInstance, authKey: append([]byte(nil), config.SessionAuthKey...),
		privateKey: append(ed25519.PrivateKey(nil), config.SenderPrivateKey...),
		catalog:    config.Catalog, content: config.Content, peers: config.Peers, replay: config.ReplayGuard,
		random: sessioncore.NewLockedReader(config.Random), laneIDs: config.InitialLaneIDs,
		operationLimits: config.OperationLimits, routerLimits: config.RouterLimits, now: config.Now,
		terminalConnectivity: config.TerminalConnectivity, terminalTimeout: config.TerminalTimeout,
		terminalObserver: newSenderTerminalObservers(
			config.TerminalSendObserver,
			config.SessionTerminalObserver,
		),
		protocolObservations: config.ProtocolObservations,
		sessions:             make(map[protocolsession.ProtocolSessionID]*Runtime), terminalDone: make(chan struct{}),
	}, nil
}

type ChannelKind uint8

const (
	ChannelNewProtocolSession ChannelKind = iota + 1
	ChannelAttachedLane
)

type ChannelAdmission struct {
	Kind    ChannelKind
	Session *Runtime
	Lane    sessioncontract.LaneIdentity
}

// AdmitChannel owns the first-frame dispatch boundary for opaque relay and P2P
// channels. A valid WS2A can only attach to a still-live ProtocolSession; every
// other candidate must prove a fresh WS2C transcript before it becomes a new
// session. Keeping this decision beside both authenticators prevents a transport
// owner from routing on untrusted session identities.
func (factory *Factory) AdmitChannel(
	ctx context.Context,
	channel protocolsession.FrameChannel,
) (ChannelAdmission, error) {
	if factory == nil || channel == nil || ctx == nil {
		return ChannelAdmission{}, sessioncontract.ErrRuntimeConfig
	}
	admissionContext, endAdmission, ok := factory.beginAdmission(ctx)
	if !ok {
		return ChannelAdmission{}, sessioncontract.ErrRuntimeClosed
	}
	defer endAdmission()
	ctx = admissionContext
	receive := channel.Recv()
	encoded, err := sessioncore.ReceiveHandshake(ctx, receive)
	if err != nil {
		return ChannelAdmission{}, err
	}
	if _, _, routeErr := protocolsession.UntrustedLaneHelloRoute(encoded); routeErr == nil {
		identity, err := factory.attachCandidate(ctx, channel, receive, encoded)
		if err != nil {
			return ChannelAdmission{}, err
		}
		return ChannelAdmission{Kind: ChannelAttachedLane, Lane: identity}, nil
	}
	session, err := factory.acceptClient(ctx, channel, receive, encoded)
	if err != nil {
		return ChannelAdmission{}, err
	}
	return ChannelAdmission{Kind: ChannelNewProtocolSession, Session: session}, nil
}

func (factory *Factory) Accept(ctx context.Context, channel protocolsession.FrameChannel) (*Runtime, error) {
	if factory == nil || channel == nil || ctx == nil {
		return nil, sessioncontract.ErrRuntimeConfig
	}
	admissionContext, endAdmission, ok := factory.beginAdmission(ctx)
	if !ok {
		return nil, sessioncontract.ErrRuntimeClosed
	}
	defer endAdmission()
	ctx = admissionContext
	receive := channel.Recv()
	clientBytes, err := sessioncore.ReceiveHandshake(ctx, receive)
	if err != nil {
		return nil, err
	}
	return factory.acceptClient(ctx, channel, receive, clientBytes)
}

func (factory *Factory) beginAdmission(
	caller context.Context,
) (context.Context, func(), bool) {
	if factory == nil {
		return nil, nil, false
	}
	return factory.admissions.Begin(caller)
}

func (factory *Factory) acceptClient(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	receive <-chan framechannel.Frame,
	clientBytes []byte,
) (result *Runtime, resultErr error) {
	keys, laneID, err := factory.completeSenderHandshake(ctx, channel, clientBytes)
	if err != nil {
		return nil, err
	}
	sessionID := keys.ProtocolSessionID()
	catalogService, err := factory.catalog.NewSenderCatalogService()
	if err != nil || catalogService == nil {
		keys.Destroy()
		return nil, errors.Join(sessioncontract.ErrRuntimeConfig, err)
	}
	contentService, err := factory.content.NewSenderContentService(sessionID)
	if err != nil || contentService == nil {
		keys.Destroy()
		return nil, errors.Join(sessioncontract.ErrRuntimeConfig, err)
	}
	contentTransferred := false
	defer func() {
		if !contentTransferred {
			resultErr = errors.Join(resultErr, contentService.Close())
		}
	}()
	handOff := sessioncore.HandOff(channel, receive)
	wire, err := sessioncore.New(sessioncore.Config{
		Share: factory.share, Role: protocolsession.RoleSender, Keys: keys,
		LaneID: laneID, LaneEpoch: 0, Channel: handOff, Random: factory.random,
		Authenticator: protocolsession.InboundMessageAuthenticatorFunc(
			func(uint64, protocolsession.Message) (protocolsession.InboundAuthenticationResult, error) {
				return protocolsession.InboundAuthenticationResult{}, nil
			},
		),
		Continuations:   factory.peers,
		OperationLimits: factory.operationLimits, RouterLimits: factory.routerLimits, Now: factory.now,
		ProtocolObservations: factory.protocolObservations,
		TerminationObserver:  factory.terminalObserver.sessionObserver(),
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
	if err := wire.AddFinalizer(func() {
		if closeErr := contentService.Close(); closeErr != nil {
			wire.RecordError(closeErr)
		}
	}); err != nil {
		return nil, err
	}
	contentTransferred = true
	outbound, err := newResponses(wire, factory.privateKey, factory.terminalObserver.sendObserver(), factory.protocolObservations)
	if err != nil {
		return nil, err
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
		DecisionTracer: contentflow.SenderDecisionTraceFunc(func(decision contentflow.SenderDecisionTrace) {
			if wire.TracingEnabled() {
				wire.Observe(sessionobs.NewSenderContentDecision(wire.ObservationContext(decision.OperationID, decision.RequestKind), decision, sessioncontract.LaneIdentity{}, false))
			}
		}),
	})
	if err != nil {
		return nil, err
	}
	catalogHandler := newCatalogHandler(catalogService, outbound)
	laneHandler := newLaneGrantHandler(laneRegistry, outbound, factory.random)
	sender := &Runtime{
		core: wire, lifecycle: wire, outbound: outbound, lanesRegistry: laneRegistry,
		catalogHandler: catalogHandler, contentHandler: contentHandler,
		compositeDone: make(chan struct{}),
	}
	if err := wire.AddFinalizer(func() {
		// Closing the registry at core completion rejects new lane proofs. Factory
		// removal waits for the terminal worker's composite lifetime below.
		laneRegistry.Close()
	}); err != nil {
		return nil, err
	}
	registryTransferred = true
	peerHandler, err := factory.peers.NewSenderPeerHandler(senderPeerSession{runtime: sender, outbound: outbound})
	if err != nil || peerHandler == nil {
		return nil, errors.Join(sessioncontract.ErrRuntimeConfig, err)
	}
	if err := registerSenderHandlers(wire, catalogHandler, contentHandler, laneHandler, peerHandler); err != nil {
		return nil, err
	}
	wire.ConfigureLanes(nil, func(identity sessioncontract.LaneIdentity) {
		laneRegistry.Release(identity.ID, identity.Epoch)
	})
	if err := sessioncore.ConfirmChannelAdmission(ctx, channel); err != nil {
		return nil, err
	}
	factory.mu.Lock()
	if factory.stopping {
		factory.mu.Unlock()
		wire.BeginClose()
		return nil, sessioncontract.ErrRuntimeClosed
	}
	factory.sessions[sessionID] = sender
	factory.mu.Unlock()
	sender.trackComposite(factory, sessionID)
	wire.Start(
		sessioncore.Component{Source: sessioncore.RuntimeFailureSourceContent, Run: contentHandler.Run},
		sessioncore.Component{Source: sessioncore.RuntimeFailureSourceCatalog, Run: catalogHandler.Run},
		sessioncore.Component{Source: sessioncore.RuntimeFailureSourceLaneAdmission, Run: laneHandler.Run},
		sessioncore.Component{Source: sessioncore.RuntimeFailureSourcePeer, Run: peerHandler.Run},
	)
	started = true
	return sender, nil
}

func (factory *Factory) completeSenderHandshake(
	ctx context.Context,
	channel protocolsession.FrameChannel,
	clientBytes []byte,
) (protocolsession.SessionKeys, uint32, error) {
	client, err := factory.replay.AcceptClientHello(clientBytes, factory.share, factory.authKey)
	if err != nil {
		return protocolsession.SessionKeys{}, 0, errors.Join(sessioncontract.ErrHandshake, err)
	}
	senderPrivate, err := ecdh.X25519().GenerateKey(factory.random)
	if err != nil {
		return protocolsession.SessionKeys{}, 0, errors.Join(sessioncontract.ErrHandshake, err)
	}
	senderNonce := make([]byte, protocolsession.HandshakeNonceBytes)
	if _, err := io.ReadFull(factory.random, senderNonce); err != nil {
		return protocolsession.SessionKeys{}, 0, errors.Join(sessioncontract.ErrHandshake, err)
	}
	laneID, err := factory.laneIDs.NextInitialLaneID()
	if err != nil || laneID == 0 {
		return protocolsession.SessionKeys{}, 0, errors.Join(sessioncontract.ErrHandshake, err)
	}
	server, err := protocolsession.NewServerHello(
		client, senderNonce, senderPrivate.PublicKey(), laneID, factory.privateKey,
	)
	if err != nil {
		return protocolsession.SessionKeys{}, 0, errors.Join(sessioncontract.ErrHandshake, err)
	}
	if err := channel.Send(ctx, framechannel.Frame(server.Encoded())); err != nil {
		return protocolsession.SessionKeys{}, 0, errors.Join(sessioncontract.ErrHandshake, err)
	}
	keys, err := protocolsession.DeriveSenderSession(senderPrivate, factory.authKey, client, server)
	if err != nil {
		return protocolsession.SessionKeys{}, 0, errors.Join(sessioncontract.ErrHandshake, err)
	}
	return keys, laneID, nil
}

type Runtime struct {
	lifecycle      lifecycle
	core           *sessioncore.Runtime
	outbound       *sessioncore.Responses
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

func (runtime *Runtime) LaneRegistry() *protocolsession.LaneRegistry {
	return runtime.lanesRegistry
}

func (factory *Factory) ActiveSessions() int {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return len(factory.sessions)
}

func (factory *Factory) String() string {
	return fmt.Sprintf("sender factory for share %x", factory.share)
}
