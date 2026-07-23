// Package v2endpoint binds the authenticated v2 relay protocol to one binary
// WebSocket without giving relay-visible routing fields any E2E meaning.
package v2endpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coder/websocket"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

const (
	MaximumControlQueueFrames     = 64
	MaximumForwardQueueFrames     = 1_024
	MaximumSessionQueueFrames     = 64
	MaximumForwardQueueBytes      = 64 << 20
	MaximumSessionQueueBytes      = 4 << 20
	MaximumV2WebSocketMessageSize = v2.OpaqueRouteHeaderBytes + v2.MaxOpaqueCiphertextBytes
	defaultWriteTimeout           = 15 * time.Second
)

var (
	ErrConfig          = errors.New("relay v2 endpoint: invalid configuration")
	ErrConnection      = errors.New("relay v2 endpoint: connection failed")
	ErrProtocol        = errors.New("relay v2 endpoint: protocol violation")
	ErrForwardOverflow = errors.New("relay v2 endpoint: destination queue is full")
)

// BinaryConnection is defined at the protocol consumer. The coder WebSocket
// connection implements it directly, while deterministic tests can inject a
// memory connection without opening an OS socket.
type BinaryConnection interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Close(websocket.StatusCode, string) error
	SetReadLimit(int64)
}

type ConnectionIDSource interface {
	NewConnectionID() (v2route.ConnectionID, error)
}

type ConnectionIDSourceFunc func() (v2route.ConnectionID, error)

func (f ConnectionIDSourceFunc) NewConnectionID() (v2route.ConnectionID, error) {
	if f == nil {
		return "", ErrConfig
	}
	return f()
}

type RetirementSource string

const (
	RetirementSourceDisconnect          RetirementSource = "disconnect"
	RetirementSourceRegistrationFailure RetirementSource = "registration_failure"
	RetirementSourceStop                RetirementSource = "stop"
)

type RetirementTarget string

const (
	RetirementTargetOwner           RetirementTarget = "owner"
	RetirementTargetSessionSender   RetirementTarget = "session_sender"
	RetirementTargetSessionReceiver RetirementTarget = "session_receiver"
)

type RetirementCompareResult string

const (
	RetirementCompareExactCurrent      RetirementCompareResult = "exact_current"
	RetirementCompareAbsent            RetirementCompareResult = "absent"
	RetirementCompareGenerationChanged RetirementCompareResult = "generation_changed"
)

type RetirementTrace struct {
	ConnectionID      v2route.ConnectionID
	LocalGeneration   uint64
	CurrentGeneration uint64
	Source            RetirementSource
	Target            RetirementTarget
	CompareResult     RetirementCompareResult
	Applied           bool
}

type RetirementTracer interface {
	TraceRetirement(RetirementTrace)
}

type RetirementTraceFunc func(RetirementTrace)

func (function RetirementTraceFunc) TraceRetirement(event RetirementTrace) {
	if function != nil {
		function(event)
	}
}

type Config struct {
	Registry         *v2route.Registry
	Challenges       *v2.ChallengeLedger
	RelayIdentity    v2.RelayIdentity
	ConnectionIDs    ConnectionIDSource
	RetirementTracer RetirementTracer
	WriteTimeout     time.Duration
}

type Server struct {
	registry         *v2route.Registry
	challenges       *v2.ChallengeLedger
	relayIdentity    v2.RelayIdentity
	connectionIDs    ConnectionIDSource
	retirementTracer RetirementTracer
	writeTimeout     time.Duration

	connections *connectionRegistry
}

func New(config Config) (*Server, error) {
	if config.Registry == nil || config.Challenges == nil || !nonzero(config.RelayIdentity[:]) {
		return nil, ErrConfig
	}
	if config.ConnectionIDs == nil {
		config.ConnectionIDs = ConnectionIDSourceFunc(randomConnectionID)
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = defaultWriteTimeout
	}
	if config.WriteTimeout < 0 {
		return nil, ErrConfig
	}
	return &Server{
		registry: config.Registry, challenges: config.Challenges, relayIdentity: config.RelayIdentity,
		connectionIDs: config.ConnectionIDs, retirementTracer: config.RetirementTracer,
		writeTimeout: config.WriteTimeout, connections: newConnectionRegistry(),
	}, nil
}

func (s *Server) traceRetirement(
	reference v2route.ConnectionRef,
	source RetirementSource,
	target RetirementTarget,
	compareResult connectionCompareResult,
	currentGeneration uint64,
	applied bool,
) {
	if s == nil || s.retirementTracer == nil || !reference.Valid() {
		return
	}
	s.retirementTracer.TraceRetirement(RetirementTrace{
		ConnectionID: reference.ConnectionID(), LocalGeneration: reference.LocalGeneration(), CurrentGeneration: currentGeneration,
		Source: source, Target: target, CompareResult: RetirementCompareResult(compareResult), Applied: applied,
	})
}

func (s *Server) newConnection(socket BinaryConnection, cancel context.CancelFunc) (*connection, error) {
	if s == nil || s.connectionIDs == nil || cancel == nil {
		return nil, ErrConfig
	}
	id, err := s.connectionIDs.NewConnectionID()
	if err != nil || id == "" {
		return nil, errors.Join(ErrConfig, err)
	}
	reference, err := v2route.NewConnectionRef(id)
	if err != nil {
		return nil, errors.Join(ErrConfig, err)
	}
	return newConnection(reference, socket, cancel), nil
}

// Serve owns the complete connection role transition. Registration attempts
// stay invisible until descriptor verification and Registry.Publish both pass.
func (s *Server) Serve(ctx context.Context, socket BinaryConnection) error {
	if s == nil || socket == nil {
		return ErrConfig
	}
	connectionContext, cancel := context.WithCancel(ctx)
	peer, err := s.newConnection(socket, cancel)
	if err != nil {
		cancel()
		return err
	}
	if !s.connections.add(peer) {
		cancel()
		return ErrConfig
	}
	defer s.connections.complete(peer.ref)
	socket.SetReadLimit(MaximumV2WebSocketMessageSize)
	writerDone := make(chan error, 1)
	go func() { writerDone <- s.writeLoop(connectionContext, peer) }()

	serveErr := s.serveConnection(connectionContext, peer)
	peer.requestClose()
	s.cleanup(peer)
	writerErr := <-writerDone
	peer.closed.Store(true)
	_ = socket.Close(websocket.StatusNormalClosure, "")
	if serveErr != nil && !normalClose(serveErr) {
		return serveErr
	}
	if writerErr != nil && !normalClose(writerErr) {
		return writerErr
	}
	return nil
}

// Shutdown prevents new peers, cancels every hijacked connection, and waits for
// their protocol loops to release route/session ownership. HTTP Shutdown alone
// cannot observe WebSockets after the upgrade boundary.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil {
		return ErrConfig
	}
	return s.connections.close(ctx)
}

func (s *Server) serveConnection(ctx context.Context, peer *connection) error {
	first, err := readBinary(ctx, peer.socket)
	if err != nil {
		return err
	}
	if len(first) < 4 {
		return ErrProtocol
	}
	switch string(first[:4]) {
	case "WS2R":
		return s.serveRegistration(ctx, peer, first)
	case "WS2J":
		return s.serveReceiver(ctx, peer, first)
	case "WS2X":
		return s.serveStop(ctx, peer, first)
	default:
		return ErrProtocol
	}
}

func (s *Server) serveRegistration(ctx context.Context, peer *connection, first []byte) error {
	init, err := v2.ParseRegisterInit(first)
	if err != nil {
		return ErrProtocol
	}
	if init.Mode == v2.RegistrationFresh {
		return s.serveFreshRegistration(ctx, peer, init)
	}
	if init.Mode == v2.RegistrationResume {
		return s.serveResume(ctx, peer, init)
	}
	return s.sendError(ctx, peer, v2.ErrorUnsupportedMode, 0)
}

func (s *Server) serveFreshRegistration(ctx context.Context, peer *connection, init v2.RegisterInit) error {
	if err := s.registry.BeginRegistration(init, peer.ref); err != nil {
		return s.sendRegistryError(ctx, peer, err)
	}
	published := false
	defer func() {
		if published || s.registry.AbortRegistration(init.ShareID, peer.ref) {
			return
		}
		if retirement, transitioned := s.registry.UnexpectedDisconnect(init.ShareID, peer.ref); transitioned {
			s.applyRouteRetirement(retirement, RetirementSourceRegistrationFailure)
		}
	}()
	authority, err := s.registrationAuthority(ctx, peer, init)
	if err != nil {
		return err
	}
	uploadBytes, err := readBinary(ctx, peer.socket)
	if err != nil {
		return err
	}
	upload, err := v2.ParseDescriptorUpload(uploadBytes)
	if err != nil {
		return ErrProtocol
	}
	descriptor, err := v2.VerifyDescriptorUpload(init, authority, upload)
	if err != nil {
		return ErrProtocol
	}
	if err := s.registry.Publish(init.ShareID, peer.ref, descriptor); err != nil {
		return s.sendRegistryError(ctx, peer, err)
	}
	published = true
	// Role publication follows the authoritative transition so a losing attempt
	// cannot later place the winner into crash grace during its cleanup.
	peer.setRole(roleSender, init.ShareID)
	if err := s.finishRegistration(ctx, peer, init); err != nil {
		return err
	}
	return s.forwardLoop(ctx, peer)
}

func (s *Server) serveResume(ctx context.Context, peer *connection, init v2.RegisterInit) error {
	credentialBytes, err := readBinary(ctx, peer.socket)
	if err != nil {
		return err
	}
	credential, err := v2.ParseResumeCredential(credentialBytes)
	if err != nil {
		return ErrProtocol
	}
	if err := s.registry.ValidateResumeCredential(init, credential.Token); err != nil {
		return s.sendRegistryError(ctx, peer, err)
	}
	authority, err := s.registrationAuthority(ctx, peer, init)
	if err != nil {
		return err
	}
	if err := s.registry.Resume(init, authority, peer.ref, credential.Token); err != nil {
		return s.sendRegistryError(ctx, peer, err)
	}
	peer.setRole(roleSender, init.ShareID)
	if err := s.finishRegistration(ctx, peer, init); err != nil {
		return err
	}
	return s.forwardLoop(ctx, peer)
}

func (s *Server) registrationAuthority(ctx context.Context, peer *connection, init v2.RegisterInit) (v2.SenderAuthority, error) {
	binding, err := v2.RegistrationChallengeBinding(init, s.relayIdentity)
	if err != nil {
		return v2.SenderAuthority{}, err
	}
	purpose := v2.ChallengeRegister
	if init.Mode == v2.RegistrationResume {
		purpose = v2.ChallengeResume
	}
	challenge, err := s.challenges.Issue(purpose, binding)
	if err != nil {
		return v2.SenderAuthority{}, s.sendError(ctx, peer, v2.ErrorAdmission, 0)
	}
	encoded, _ := challenge.MarshalBinary()
	if err := peer.sendControl(ctx, encoded); err != nil {
		return v2.SenderAuthority{}, err
	}
	proofBytes, err := readBinary(ctx, peer.socket)
	if err != nil {
		return v2.SenderAuthority{}, err
	}
	proof, err := v2.ParseRegisterProof(proofBytes)
	if err != nil {
		return v2.SenderAuthority{}, ErrProtocol
	}
	authority, err := s.challenges.AuthenticateRegistration(challenge.ID, init, s.relayIdentity, proof)
	if err != nil {
		// A proof failure is closed silently so the endpoint never becomes a
		// sender-key or challenge-validity oracle.
		return v2.SenderAuthority{}, ErrProtocol
	}
	return authority, nil
}

func (s *Server) finishRegistration(ctx context.Context, peer *connection, init v2.RegisterInit) error {
	registered, _ := (v2.Registered{
		ShareID: init.ShareID, ShareInstance: init.ShareInstance, DescriptorDigest: init.DescriptorDigest,
	}).MarshalBinary()
	return peer.sendControl(ctx, registered)
}

func (s *Server) serveReceiver(ctx context.Context, peer *connection, first []byte) error {
	join, err := v2.ParseJoin(first)
	if err != nil {
		return ErrProtocol
	}
	result, err := s.registry.Join(join.ShareID, peer.ref)
	if err != nil {
		return s.sendRegistryError(ctx, peer, err)
	}
	switch result.Status {
	case v2route.JoinStarting:
		return s.sendError(ctx, peer, v2.ErrorStarting, result.RetryAfter)
	case v2route.JoinNotFound:
		return s.sendError(ctx, peer, v2.ErrorNotFound, 0)
	case v2route.JoinStopped:
		return s.sendError(ctx, peer, v2.ErrorStopped, 0)
	case v2route.JoinReady:
	default:
		return ErrProtocol
	}
	if !s.activateReceiverSession(peer, join.ShareID, result) {
		peer.requestClose()
		return ErrConnection
	}
	delivery, _ := (v2.DescriptorDelivery{
		RelaySessionID: result.RelaySessionID, Object: result.Descriptor,
	}).MarshalBinary()
	if err := peer.sendControl(ctx, delivery); err != nil {
		if retirement, ended := s.registry.EndSession(result.RelaySessionID, peer.ref); ended {
			s.applySessionRetirement(retirement)
		}
		return err
	}
	return s.forwardLoop(ctx, peer)
}

func (s *Server) activateReceiverSession(
	peer *connection,
	shareID v2.ShareID,
	result v2route.JoinResult,
) bool {
	peer.setRole(roleReceiver, shareID)
	sender, _, _ := s.connections.resolve(result.Sender)
	if sender == nil || !sender.addSession(result.RelaySessionID) || !peer.addSession(result.RelaySessionID) {
		if sender != nil {
			sender.removeSession(result.RelaySessionID)
		}
		peer.removeSession(result.RelaySessionID)
		if retirement, ended := s.registry.EndSession(result.RelaySessionID, peer.ref); ended {
			s.applySessionRetirement(retirement)
		}
		return false
	}
	resolution, err := s.registry.ResolveSession(result.RelaySessionID, peer.ref)
	if err == nil && resolution.Disposition == v2route.SessionForward && resolution.Destination == result.Sender {
		return true
	}
	// A route transition can retire the Registry session between Join and local
	// publication. Revalidation turns the two endpoint installs into a commit:
	// either both still match the exact authority, or both are synchronously undone.
	sender.removeSession(result.RelaySessionID)
	peer.removeSession(result.RelaySessionID)
	if retirement, ended := s.registry.EndSession(result.RelaySessionID, peer.ref); ended {
		s.applySessionRetirement(retirement)
	}
	return false
}

func (s *Server) serveStop(ctx context.Context, peer *connection, first []byte) error {
	init, err := v2.ParseStopInit(first)
	if err != nil || !bytes.Equal(init.RelayIdentity[:], s.relayIdentity[:]) {
		return ErrProtocol
	}
	binding, err := v2.StopChallengeBinding(init)
	if err != nil {
		return ErrProtocol
	}
	challenge, err := s.challenges.Issue(v2.ChallengeStop, binding)
	if err != nil {
		return s.sendError(ctx, peer, v2.ErrorAdmission, 0)
	}
	challengeBytes, _ := challenge.MarshalBinary()
	if err := peer.sendControl(ctx, challengeBytes); err != nil {
		return err
	}
	proofBytes, err := readBinary(ctx, peer.socket)
	if err != nil {
		return err
	}
	proof, err := v2.ParseStopProof(proofBytes)
	if err != nil {
		return ErrProtocol
	}
	authority, err := s.challenges.AuthenticateStop(challenge.ID, init, proof)
	if err != nil {
		return ErrProtocol
	}
	retirement, stopErr := s.registry.Stop(ctx, init, authority)
	// A committed STOP is already authoritative even if WS2Y cannot be written.
	// Apply exact Registry participants before any response path can return.
	s.applyRouteRetirement(retirement, RetirementSourceStop)
	if stopErr != nil {
		return s.sendRegistryError(ctx, peer, stopErr)
	}
	peer.setRole(roleStopper, init.ShareID)
	stopped, _ := (v2.Stopped{StopID: init.StopID}).MarshalBinary()
	if err := peer.sendControl(ctx, stopped); err != nil {
		return err
	}
	return nil
}

func (s *Server) forwardLoop(ctx context.Context, source *connection) error {
	for {
		encoded, err := readBinary(ctx, source.socket)
		if err != nil {
			return err
		}
		if err := s.forwardFrame(source, encoded); err != nil {
			return err
		}
	}
}

func (s *Server) forwardFrame(source *connection, encoded []byte) error {
	route, err := v2.ParseOpaqueRoute(encoded)
	if err != nil {
		return ErrProtocol
	}
	resolution, err := s.registry.ResolveSession(route.RelaySessionID, source.ref)
	if err != nil {
		return ErrProtocol
	}
	if resolution.Disposition == v2route.SessionRetired {
		// Only the exact former participant receives this disposition. Unknown
		// IDs and tombstoned outsiders remain protocol-fatal above.
		return nil
	}
	if resolution.Disposition != v2route.SessionForward || !resolution.Destination.Valid() {
		return ErrProtocol
	}
	destination, _, _ := s.connections.resolve(resolution.Destination)
	if destination == nil || !destination.enqueueForward(route.RelaySessionID, encoded) {
		if retirement, ended := s.endSession(route.RelaySessionID, source.ref); ended {
			if receiver, _, _ := s.connections.resolve(retirement.Receiver); receiver != nil {
				receiver.requestClose()
			}
		}
		if source.roleValue() == roleSender {
			// A receiver can disappear after active resolution but before peer
			// lookup. Retiring that exact session must not tear down unrelated
			// sessions multiplexed by the authenticated sender connection.
			return nil
		}
		return ErrForwardOverflow
	}
	return nil
}

func (s *Server) sendRegistryError(ctx context.Context, peer *connection, cause error) error {
	return s.sendError(ctx, peer, registryErrorCode(cause), 0)
}

func registryErrorCode(cause error) v2.ErrorCode {
	code := v2.ErrorMalformed
	switch {
	case errors.Is(cause, v2route.ErrCollision):
		code = v2.ErrorShareIDCollision
	case errors.Is(cause, v2route.ErrAlreadyRegistered):
		code = v2.ErrorAlreadyRegistered
	case errors.Is(cause, v2route.ErrStopped):
		code = v2.ErrorStopped
	case errors.Is(cause, v2route.ErrNotFound):
		code = v2.ErrorNotFound
	case errors.Is(cause, v2route.ErrAdmission):
		code = v2.ErrorAdmission
	case errors.Is(cause, v2route.ErrStopping):
		code = v2.ErrorAdmission
	case errors.Is(cause, v2route.ErrResume), errors.Is(cause, v2route.ErrOwner):
		code = v2.ErrorInvalidProof
	}
	return code
}

func (s *Server) sendError(ctx context.Context, peer *connection, code v2.ErrorCode, retry time.Duration) error {
	encoded, err := (v2.ErrorFrame{Code: code, RetryAfter: retry}).MarshalBinary()
	if err != nil {
		return errors.Join(ErrProtocol, err)
	}
	if err := peer.sendControl(ctx, encoded); err != nil {
		return err
	}
	return fmt.Errorf("%w: relay error %d", ErrProtocol, code)
}
