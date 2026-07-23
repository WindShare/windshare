package relayv2

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"

	framechannel "github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const registrationReadLimit = v2.MaxDescriptorBytes + v2.DescriptorDeliveryHeaderBytes

type DialOptions struct {
	HTTPClient      *http.Client
	Header          http.Header
	LifecycleTracer LifecycleTracer
	// SocketDialer lets deterministic tests and embedded callers own the
	// transport boundary without weakening the protocol handshake.
	SocketDialer func(context.Context, string, http.Header) (BinarySocket, error)
}

// BinarySocket is the narrow WebSocket surface owned by this protocol
// consumer. A coder WebSocket satisfies it directly.
type BinarySocket interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Close(websocket.StatusCode, string) error
	SetReadLimit(int64)
}

func (options DialOptions) dial(ctx context.Context, target string) (BinarySocket, error) {
	if options.SocketDialer != nil {
		return options.SocketDialer(ctx, target, options.Header.Clone())
	}
	socket, _, err := websocket.Dial(ctx, target, &websocket.DialOptions{
		HTTPClient: options.HTTPClient, HTTPHeader: options.Header,
	})
	return socket, err
}

type RegistrationStats struct {
	BytesSent     uint64
	BytesReceived uint64
}

type SenderConfig struct {
	RelayBaseURL     string
	Init             v2.RegisterInit
	SenderPrivateKey ed25519.PrivateKey
	Descriptor       []byte
	ResumeToken      v2.ResumeToken
	Dial             DialOptions
}

type SenderConnection struct {
	endpoint v2.RelayEndpoint
	link     *link
	stats    RegistrationStats
}

func DialSender(ctx context.Context, config SenderConfig) (*SenderConnection, error) {
	endpoint, err := v2.NormalizeRelayEndpoint(config.RelayBaseURL)
	if err != nil {
		return nil, err
	}
	if config.Init.Validate() != nil || len(config.SenderPrivateKey) != ed25519.PrivateKeySize {
		return nil, ErrProtocol
	}
	if config.Init.Mode == v2.RegistrationFresh && len(config.Descriptor) == 0 {
		return nil, ErrProtocol
	}
	socket, err := config.Dial.dial(ctx, endpoint.DialURL)
	if err != nil {
		return nil, err
	}
	socket.SetReadLimit(registrationReadLimit)
	stats := RegistrationStats{}
	fail := func(cause error) (*SenderConnection, error) {
		_ = socket.Close(websocket.StatusProtocolError, "")
		return nil, cause
	}
	initBytes, _ := config.Init.MarshalBinary()
	if err := writeRegistration(ctx, socket, initBytes, &stats); err != nil {
		return fail(err)
	}
	if config.Init.Mode == v2.RegistrationResume {
		credential, marshalErr := (v2.ResumeCredential{Token: config.ResumeToken}).MarshalBinary()
		if marshalErr != nil {
			return fail(marshalErr)
		}
		if err := writeRegistration(ctx, socket, credential, &stats); err != nil {
			return fail(err)
		}
	}
	challengeBytes, err := readRegistration(ctx, socket, &stats)
	if err != nil {
		return fail(err)
	}
	if relayErr := parseRelayError(challengeBytes); relayErr != nil {
		return fail(relayErr)
	}
	challenge, err := v2.ParseChallenge(challengeBytes)
	if err != nil {
		return fail(ErrProtocol)
	}
	proof, err := v2.NewRegisterProof(config.Init, challenge, endpoint.Identity, config.SenderPrivateKey)
	if err != nil {
		return fail(err)
	}
	proofBytes, _ := proof.MarshalBinary()
	if err := writeRegistration(ctx, socket, proofBytes, &stats); err != nil {
		return fail(err)
	}
	if config.Init.Mode == v2.RegistrationFresh {
		upload, marshalErr := (v2.DescriptorUpload{Object: config.Descriptor}).MarshalBinary()
		if marshalErr != nil {
			return fail(marshalErr)
		}
		if err := writeRegistration(ctx, socket, upload, &stats); err != nil {
			return fail(err)
		}
	}
	ackBytes, err := readRegistration(ctx, socket, &stats)
	if err != nil {
		return fail(err)
	}
	if relayErr := parseRelayError(ackBytes); relayErr != nil {
		return fail(relayErr)
	}
	ack, err := v2.ParseRegistered(ackBytes)
	if err != nil || ack.ShareID != config.Init.ShareID || ack.ShareInstance != config.Init.ShareInstance ||
		ack.DescriptorDigest != config.Init.DescriptorDigest {
		return fail(ErrProtocol)
	}
	socket.SetReadLimit(v2.OpaqueRouteHeaderBytes + v2.MaxOpaqueCiphertextBytes)
	link := newLinkWithTracer(context.Background(), socket, false, config.Dial.LifecycleTracer)
	link.start()
	return &SenderConnection{endpoint: endpoint, link: link, stats: stats}, nil
}

func (connection *SenderConnection) Accept(ctx context.Context) (*Channel, error) {
	if connection == nil || connection.link == nil {
		return nil, ErrClosed
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-connection.link.done:
			return nil, errors.Join(ErrClosed, connection.link.Err())
		case channel := <-connection.link.accept:
			// Retirement can race the application accepting the first routed
			// frame. Do not materialize a runtime for an already closed channel.
			if channel.State() == framechannel.Open {
				return channel, nil
			}
		}
	}
}

func (connection *SenderConnection) RegistrationStats() RegistrationStats { return connection.stats }
func (connection *SenderConnection) Endpoint() v2.RelayEndpoint           { return connection.endpoint }
func (connection *SenderConnection) Done() <-chan struct{}                { return connection.link.done }
func (connection *SenderConnection) Err() error                           { return connection.link.Err() }
func (connection *SenderConnection) Close() error {
	if connection == nil || connection.link == nil {
		return nil
	}
	connection.link.stop(nil)
	return nil
}

type ReceiverConfig struct {
	RelayBaseURL string
	ShareID      v2.ShareID
	Dial         DialOptions
}

type ReceiverConnection struct {
	endpoint   v2.RelayEndpoint
	descriptor []byte
	channel    *Channel
	link       *link
}

func DialReceiver(ctx context.Context, config ReceiverConfig) (*ReceiverConnection, error) {
	endpoint, err := v2.NormalizeRelayEndpoint(config.RelayBaseURL)
	if err != nil {
		return nil, err
	}
	joinBytes, err := (v2.Join{ShareID: config.ShareID}).MarshalBinary()
	if err != nil {
		return nil, err
	}
	socket, err := config.Dial.dial(ctx, endpoint.DialURL)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*ReceiverConnection, error) {
		_ = socket.Close(websocket.StatusProtocolError, "")
		return nil, cause
	}
	socket.SetReadLimit(registrationReadLimit)
	if err := socket.Write(ctx, websocket.MessageBinary, joinBytes); err != nil {
		return fail(err)
	}
	messageType, response, err := socket.Read(ctx)
	if err != nil {
		return fail(err)
	}
	if messageType != websocket.MessageBinary {
		return fail(ErrProtocol)
	}
	if relayErr := parseRelayError(response); relayErr != nil {
		return fail(relayErr)
	}
	delivery, err := v2.ParseDescriptorDelivery(response)
	if err != nil {
		return fail(ErrProtocol)
	}
	socket.SetReadLimit(v2.OpaqueRouteHeaderBytes + v2.MaxOpaqueCiphertextBytes)
	link := newLinkWithTracer(context.Background(), socket, true, config.Dial.LifecycleTracer)
	channel := link.installFixed(delivery.RelaySessionID)
	link.start()
	return &ReceiverConnection{
		endpoint: endpoint, descriptor: append([]byte(nil), delivery.Object...), channel: channel, link: link,
	}, nil
}

func (connection *ReceiverConnection) Descriptor() []byte {
	return append([]byte(nil), connection.descriptor...)
}
func (connection *ReceiverConnection) Channel() *Channel          { return connection.channel }
func (connection *ReceiverConnection) Endpoint() v2.RelayEndpoint { return connection.endpoint }
func (connection *ReceiverConnection) Done() <-chan struct{}      { return connection.link.done }
func (connection *ReceiverConnection) Err() error                 { return connection.link.Err() }
func (connection *ReceiverConnection) Close() error {
	if connection == nil || connection.link == nil {
		return nil
	}
	connection.link.stop(nil)
	return nil
}

type StopConfig struct {
	RelayBaseURL     string
	ShareID          v2.ShareID
	ShareInstance    v2.ShareInstance
	PKHash           v2.PKHash
	StopID           v2.StopID
	SenderPrivateKey ed25519.PrivateKey
	Dial             DialOptions
}

func Stop(ctx context.Context, config StopConfig) error {
	endpoint, err := v2.NormalizeRelayEndpoint(config.RelayBaseURL)
	if err != nil {
		return err
	}
	init := v2.StopInit{
		ShareID: config.ShareID, ShareInstance: config.ShareInstance, PKHash: config.PKHash,
		RelayIdentity: endpoint.Identity, StopID: config.StopID,
	}
	initBytes, err := init.MarshalBinary()
	if err != nil || len(config.SenderPrivateKey) != ed25519.PrivateKeySize {
		return ErrProtocol
	}
	socket, err := config.Dial.dial(ctx, endpoint.DialURL)
	if err != nil {
		return err
	}
	defer func() { _ = socket.Close(websocket.StatusNormalClosure, "") }()
	if err := socket.Write(ctx, websocket.MessageBinary, initBytes); err != nil {
		return err
	}
	messageType, challengeBytes, err := socket.Read(ctx)
	if err != nil || messageType != websocket.MessageBinary {
		return errors.Join(ErrProtocol, err)
	}
	if relayErr := parseRelayError(challengeBytes); relayErr != nil {
		return relayErr
	}
	challenge, err := v2.ParseChallenge(challengeBytes)
	if err != nil || challenge.Purpose != v2.ChallengeStop {
		return ErrProtocol
	}
	proof, err := v2.NewStopProof(init, challenge, config.SenderPrivateKey)
	if err != nil {
		return err
	}
	proofBytes, _ := proof.MarshalBinary()
	if err := socket.Write(ctx, websocket.MessageBinary, proofBytes); err != nil {
		return err
	}
	messageType, stoppedBytes, err := socket.Read(ctx)
	if err != nil || messageType != websocket.MessageBinary {
		return errors.Join(ErrProtocol, err)
	}
	if relayErr := parseRelayError(stoppedBytes); relayErr != nil {
		return relayErr
	}
	stopped, err := v2.ParseStopped(stoppedBytes)
	if err != nil || stopped.StopID != config.StopID {
		return ErrProtocol
	}
	return nil
}

type RelayError struct {
	Code       v2.ErrorCode
	RetryAfter time.Duration
}

func (err *RelayError) Error() string {
	return fmt.Sprintf("relay v2 rejected request with code %d", err.Code)
}

func parseRelayError(encoded []byte) error {
	if len(encoded) < 4 || string(encoded[:4]) != "WS2E" {
		return nil
	}
	frame, err := v2.ParseError(encoded)
	if err != nil {
		return ErrProtocol
	}
	return &RelayError{Code: frame.Code, RetryAfter: frame.RetryAfter}
}

func writeRegistration(ctx context.Context, socket BinarySocket, encoded []byte, stats *RegistrationStats) error {
	if err := socket.Write(ctx, websocket.MessageBinary, encoded); err != nil {
		return err
	}
	stats.BytesSent += uint64(len(encoded))
	return nil
}

func readRegistration(ctx context.Context, socket BinarySocket, stats *RegistrationStats) ([]byte, error) {
	messageType, encoded, err := socket.Read(ctx)
	if err != nil {
		return nil, err
	}
	if messageType != websocket.MessageBinary {
		return nil, ErrProtocol
	}
	stats.BytesReceived += uint64(len(encoded))
	return encoded, nil
}

func NewFreshRegisterInit(
	shareID v2.ShareID,
	shareInstance v2.ShareInstance,
	pkHash v2.PKHash,
	descriptor []byte,
	resumeToken v2.ResumeToken,
) (v2.RegisterInit, error) {
	if len(descriptor) == 0 || len(descriptor) > v2.MaxDescriptorBytes {
		return v2.RegisterInit{}, ErrProtocol
	}
	return v2.RegisterInit{
		Mode: v2.RegistrationFresh, ShareID: shareID, ShareInstance: shareInstance, PKHash: pkHash,
		DescriptorDigest: sha256.Sum256(descriptor), ResumeTokenHash: sha256.Sum256(resumeToken[:]),
	}, nil
}

func ResumeInit(fresh v2.RegisterInit) (v2.RegisterInit, error) {
	if fresh.Mode != v2.RegistrationFresh || fresh.Validate() != nil {
		return v2.RegisterInit{}, ErrProtocol
	}
	fresh.Mode = v2.RegistrationResume
	return fresh, nil
}
