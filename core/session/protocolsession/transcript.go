package protocolsession

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

const (
	WireVersion          = byte(catalog.WireVersionV2)
	HandshakeNonceBytes  = 32
	X25519KeyBytes       = 32
	SessionAuthKeyBytes  = sha256.Size
	TrafficKeyBytes      = sha256.Size
	TranscriptHashBytes  = sha256.Size
	ClientHelloBodySize  = 4 + 1 + catalog.IdentityBytes + IdentityBytes + HandshakeNonceBytes + X25519KeyBytes
	ClientHelloSize      = ClientHelloBodySize + sha256.Size
	ServerHelloBodySize  = 4 + 1 + sha256.Size + HandshakeNonceBytes + X25519KeyBytes + 4 + 4
	ServerHelloSize      = ServerHelloBodySize + ed25519.SignatureSize
	ClientHelloReplayTTL = 5 * time.Minute
)

const (
	clientHelloMagic             = "WS2C"
	serverHelloMagic             = "WS2S"
	clientHelloDomain            = "windshare/v2 client-hello\x00"
	serverHelloDomain            = "windshare/v2 server-hello\x00"
	protocolSessionDomain        = "windshare/v2 protocol-session\x00"
	handshakeDomain              = "windshare/v2 handshake"
	receiverToSenderTrafficLabel = "windshare/v2 traffic/receiver-to-sender"
	senderToReceiverTrafficLabel = "windshare/v2 traffic/sender-to-receiver"
)

var (
	ErrHandshakeInput          = errors.New("protocol session handshake input is invalid")
	ErrClientHelloMalformed    = errors.New("protocol session client hello is malformed")
	ErrClientHelloProof        = errors.New("protocol session client hello proof is invalid")
	ErrClientHelloReplay       = errors.New("protocol session client hello was already admitted")
	ErrClientHelloReplayBudget = errors.New("protocol session client hello replay budget is exhausted")
	ErrServerHelloMalformed    = errors.New("protocol session server hello is malformed")
	ErrServerHelloSignature    = errors.New("protocol session server hello signature is invalid")
	ErrUnexpectedShare         = errors.New("protocol session hello belongs to another share")
	ErrUnsupportedVersion      = errors.New("protocol session wire version is unsupported")
	ErrKeyLength               = errors.New("protocol session key must be exactly 32 bytes")
	ErrTrafficKeyDirection     = errors.New("protocol session traffic key has the wrong direction")
	ErrKeyAgreement            = errors.New("protocol session X25519 key agreement failed")
)

type TranscriptHash [TranscriptHashBytes]byte

func (h TranscriptHash) Bytes() []byte { return append([]byte(nil), h[:]...) }
func (h TranscriptHash) Equal(other TranscriptHash) bool {
	return subtle.ConstantTimeCompare(h[:], other[:]) == 1
}

// TrafficKey carries its derivation direction so the two transcript keys cannot
// be interchanged at a lane boundary. Direction in envelope AAD detects a peer
// mismatch, but rejecting the mistake locally avoids emitting an undecryptable
// first frame and consuming the lane sequence.
type TrafficKey struct {
	value     [TrafficKeyBytes]byte
	direction Direction
	valid     bool
}

func TrafficKeyFromBytes(raw []byte, direction Direction) (TrafficKey, error) {
	var key TrafficKey
	if len(raw) != TrafficKeyBytes {
		return key, fmt.Errorf("%w: got %d", ErrKeyLength, len(raw))
	}
	if !direction.valid() {
		return key, ErrTrafficKeyDirection
	}
	copy(key.value[:], raw)
	key.direction = direction
	key.valid = true
	return key, nil
}

func (k TrafficKey) Bytes() []byte { return append([]byte(nil), k.value[:]...) }
func (k TrafficKey) Equal(other TrafficKey) bool {
	return k.valid == other.valid && k.direction == other.direction &&
		subtle.ConstantTimeCompare(k.value[:], other.value[:]) == 1
}
func (k *TrafficKey) Destroy() {
	if k != nil {
		clear(k.value[:])
		k.direction = 0
		k.valid = false
	}
}

type ClientHello struct {
	encoded        [ClientHelloSize]byte
	share          catalog.ShareInstance
	receiver       ReceiverInstanceID
	receiverNonce  [HandshakeNonceBytes]byte
	receiverPublic *ecdh.PublicKey
	valid          bool
	admitted       bool
}

// ClientHelloReplayGuard owns the atomic admission tombstone. ReceiverInstanceID
// is intentionally excluded because it is diagnostic and must not let a holder
// of the share capability replay the same nonce and ephemeral key.
type ClientHelloReplayGuard struct {
	mu sync.Mutex

	now        func() time.Time
	capacity   int
	tombstones map[[sha256.Size]byte]time.Time
}

func NewClientHelloReplayGuard(capacity int, now func() time.Time) (*ClientHelloReplayGuard, error) {
	if capacity <= 0 {
		return nil, ErrClientHelloReplayBudget
	}
	if now == nil {
		now = time.Now
	}
	return &ClientHelloReplayGuard{
		now: now, capacity: capacity, tombstones: make(map[[sha256.Size]byte]time.Time),
	}, nil
}

func (g *ClientHelloReplayGuard) admit(client ClientHello) error {
	if g == nil || !client.valid {
		return ErrHandshakeInput
	}
	fingerprint := client.replayFingerprint()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.now == nil {
		g.now = time.Now
	}
	if g.tombstones == nil {
		g.tombstones = make(map[[sha256.Size]byte]time.Time)
	}
	now := g.now()
	for candidate, expiresAt := range g.tombstones {
		if !now.Before(expiresAt) {
			delete(g.tombstones, candidate)
		}
	}
	if expiresAt, found := g.tombstones[fingerprint]; found && now.Before(expiresAt) {
		return ErrClientHelloReplay
	}
	if len(g.tombstones) >= g.capacity {
		return ErrClientHelloReplayBudget
	}
	g.tombstones[fingerprint] = now.Add(ClientHelloReplayTTL)
	return nil
}

// AcceptClientHello makes proof verification and replay admission one atomic
// sender-side API. Keeping the parser private prevents a future caller from
// admitting catalog work after authenticating a hello but forgetting its
// share-scoped replay tombstone.
func (g *ClientHelloReplayGuard) AcceptClientHello(
	encoded []byte,
	expectedShare catalog.ShareInstance,
	sessionAuthKey []byte,
) (ClientHello, error) {
	client, err := parseClientHello(encoded, expectedShare, sessionAuthKey)
	if err != nil {
		return ClientHello{}, err
	}
	if err := g.admit(client); err != nil {
		return ClientHello{}, err
	}
	client.admitted = true
	return client, nil
}

func (h ClientHello) replayFingerprint() [sha256.Size]byte {
	preimage := make([]byte, 0, catalog.IdentityBytes+HandshakeNonceBytes+X25519KeyBytes)
	preimage = append(preimage, h.share.Bytes()...)
	preimage = append(preimage, h.receiverNonce[:]...)
	preimage = append(preimage, h.receiverPublic.Bytes()...)
	return sha256.Sum256(preimage)
}

func NewClientHello(
	share catalog.ShareInstance,
	receiver ReceiverInstanceID,
	receiverNonce []byte,
	receiverPublic *ecdh.PublicKey,
	sessionAuthKey []byte,
) (ClientHello, error) {
	if share.IsZero() || receiver.IsZero() || len(receiverNonce) != HandshakeNonceBytes {
		return ClientHello{}, ErrHandshakeInput
	}
	public, err := normalizeX25519Public(receiverPublic)
	if err != nil {
		return ClientHello{}, err
	}
	if err := validateSessionAuthKey(sessionAuthKey); err != nil {
		return ClientHello{}, err
	}

	body := make([]byte, 0, ClientHelloBodySize)
	body = append(body, clientHelloMagic...)
	body = append(body, WireVersion)
	body = append(body, share.Bytes()...)
	body = append(body, receiver[:]...)
	body = append(body, receiverNonce...)
	body = append(body, public.Bytes()...)
	proof := clientProof(sessionAuthKey, body)

	var hello ClientHello
	copy(hello.encoded[:ClientHelloBodySize], body)
	copy(hello.encoded[ClientHelloBodySize:], proof)
	hello.share = share
	hello.receiver = receiver
	copy(hello.receiverNonce[:], receiverNonce)
	hello.receiverPublic = public
	hello.valid = true
	return hello, nil
}

func parseClientHello(encoded []byte, expectedShare catalog.ShareInstance, sessionAuthKey []byte) (ClientHello, error) {
	if len(encoded) != ClientHelloSize {
		return ClientHello{}, fmt.Errorf("%w: got %d bytes", ErrClientHelloMalformed, len(encoded))
	}
	if string(encoded[:len(clientHelloMagic)]) != clientHelloMagic {
		return ClientHello{}, ErrClientHelloMalformed
	}
	if encoded[len(clientHelloMagic)] != WireVersion {
		return ClientHello{}, ErrUnsupportedVersion
	}
	if expectedShare.IsZero() {
		return ClientHello{}, ErrHandshakeInput
	}
	if err := validateSessionAuthKey(sessionAuthKey); err != nil {
		return ClientHello{}, err
	}

	shareOffset := len(clientHelloMagic) + 1
	share, err := catalog.ShareInstanceFromBytes(encoded[shareOffset : shareOffset+catalog.IdentityBytes])
	if err != nil {
		return ClientHello{}, fmt.Errorf("%w: %w", ErrClientHelloMalformed, err)
	}
	if !share.Equal(expectedShare) {
		return ClientHello{}, ErrUnexpectedShare
	}
	body := encoded[:ClientHelloBodySize]
	if !hmac.Equal(clientProof(sessionAuthKey, body), encoded[ClientHelloBodySize:]) {
		return ClientHello{}, ErrClientHelloProof
	}

	receiverOffset := shareOffset + catalog.IdentityBytes
	receiver, err := ReceiverInstanceIDFromBytes(encoded[receiverOffset : receiverOffset+IdentityBytes])
	if err != nil || receiver.IsZero() {
		return ClientHello{}, ErrClientHelloMalformed
	}
	nonceOffset := receiverOffset + IdentityBytes
	publicOffset := nonceOffset + HandshakeNonceBytes
	public, err := normalizeX25519PublicBytes(encoded[publicOffset:ClientHelloBodySize])
	if err != nil {
		return ClientHello{}, fmt.Errorf("%w: %w", ErrClientHelloMalformed, err)
	}

	var hello ClientHello
	copy(hello.encoded[:], encoded)
	hello.share = share
	hello.receiver = receiver
	copy(hello.receiverNonce[:], encoded[nonceOffset:publicOffset])
	hello.receiverPublic = public
	hello.valid = true
	return hello, nil
}

func (h ClientHello) Encoded() []byte                        { return append([]byte(nil), h.encoded[:]...) }
func (h ClientHello) Body() []byte                           { return append([]byte(nil), h.encoded[:ClientHelloBodySize]...) }
func (h ClientHello) ShareInstance() catalog.ShareInstance   { return h.share }
func (h ClientHello) ReceiverInstanceID() ReceiverInstanceID { return h.receiver }
func (h ClientHello) ReceiverNonce() []byte                  { return append([]byte(nil), h.receiverNonce[:]...) }
func (h ClientHello) ReceiverPublicKey() *ecdh.PublicKey     { return h.receiverPublic }

type ServerHello struct {
	encoded       [ServerHelloSize]byte
	clientDigest  [sha256.Size]byte
	senderNonce   [HandshakeNonceBytes]byte
	senderPublic  *ecdh.PublicKey
	initialLaneID uint32
	valid         bool
}

func NewServerHello(
	client ClientHello,
	senderNonce []byte,
	senderPublic *ecdh.PublicKey,
	initialLaneID uint32,
	senderSigningKey ed25519.PrivateKey,
) (ServerHello, error) {
	if !client.valid || !client.admitted || len(senderNonce) != HandshakeNonceBytes || initialLaneID == 0 || len(senderSigningKey) != ed25519.PrivateKeySize {
		return ServerHello{}, ErrHandshakeInput
	}
	public, err := normalizeX25519Public(senderPublic)
	if err != nil {
		return ServerHello{}, err
	}
	clientDigest := sha256.Sum256(client.encoded[:])
	body := make([]byte, 0, ServerHelloBodySize)
	body = append(body, serverHelloMagic...)
	body = append(body, WireVersion)
	body = append(body, clientDigest[:]...)
	body = append(body, senderNonce...)
	body = append(body, public.Bytes()...)
	body = binary.BigEndian.AppendUint32(body, initialLaneID)
	body = binary.BigEndian.AppendUint32(body, 0)
	digest := sha256.Sum256(body)
	signature := ed25519.Sign(senderSigningKey, append([]byte(serverHelloDomain), digest[:]...))

	var hello ServerHello
	copy(hello.encoded[:ServerHelloBodySize], body)
	copy(hello.encoded[ServerHelloBodySize:], signature)
	hello.clientDigest = clientDigest
	copy(hello.senderNonce[:], senderNonce)
	hello.senderPublic = public
	hello.initialLaneID = initialLaneID
	hello.valid = true
	return hello, nil
}

func ParseServerHello(encoded []byte, client ClientHello, senderVerificationKey ed25519.PublicKey) (ServerHello, error) {
	if len(encoded) != ServerHelloSize || !client.valid {
		return ServerHello{}, fmt.Errorf("%w: got %d bytes", ErrServerHelloMalformed, len(encoded))
	}
	if len(senderVerificationKey) != ed25519.PublicKeySize {
		return ServerHello{}, ErrHandshakeInput
	}
	if string(encoded[:len(serverHelloMagic)]) != serverHelloMagic {
		return ServerHello{}, ErrServerHelloMalformed
	}
	if encoded[len(serverHelloMagic)] != WireVersion {
		return ServerHello{}, ErrUnsupportedVersion
	}

	clientDigestOffset := len(serverHelloMagic) + 1
	clientDigest := sha256.Sum256(client.encoded[:])
	if subtle.ConstantTimeCompare(encoded[clientDigestOffset:clientDigestOffset+sha256.Size], clientDigest[:]) != 1 {
		return ServerHello{}, ErrServerHelloMalformed
	}
	body := encoded[:ServerHelloBodySize]
	bodyDigest := sha256.Sum256(body)
	preimage := append([]byte(serverHelloDomain), bodyDigest[:]...)
	if !ed25519.Verify(senderVerificationKey, preimage, encoded[ServerHelloBodySize:]) {
		return ServerHello{}, ErrServerHelloSignature
	}

	nonceOffset := clientDigestOffset + sha256.Size
	publicOffset := nonceOffset + HandshakeNonceBytes
	laneOffset := publicOffset + X25519KeyBytes
	public, err := normalizeX25519PublicBytes(encoded[publicOffset:laneOffset])
	if err != nil {
		return ServerHello{}, fmt.Errorf("%w: %w", ErrServerHelloMalformed, err)
	}
	laneID := binary.BigEndian.Uint32(encoded[laneOffset : laneOffset+4])
	laneEpoch := binary.BigEndian.Uint32(encoded[laneOffset+4 : ServerHelloBodySize])
	if laneID == 0 || laneEpoch != 0 {
		return ServerHello{}, ErrServerHelloMalformed
	}

	var hello ServerHello
	copy(hello.encoded[:], encoded)
	hello.clientDigest = clientDigest
	copy(hello.senderNonce[:], encoded[nonceOffset:publicOffset])
	hello.senderPublic = public
	hello.initialLaneID = laneID
	hello.valid = true
	return hello, nil
}

func (h ServerHello) Encoded() []byte                  { return append([]byte(nil), h.encoded[:]...) }
func (h ServerHello) Body() []byte                     { return append([]byte(nil), h.encoded[:ServerHelloBodySize]...) }
func (h ServerHello) SenderNonce() []byte              { return append([]byte(nil), h.senderNonce[:]...) }
func (h ServerHello) SenderPublicKey() *ecdh.PublicKey { return h.senderPublic }
func (h ServerHello) InitialLaneID() uint32            { return h.initialLaneID }
func (h ServerHello) InitialLaneEpoch() uint32         { return 0 }

type SessionKeys struct {
	id               ProtocolSessionID
	transcriptHash   TranscriptHash
	receiverToSender TrafficKey
	senderToReceiver TrafficKey
}

func DeriveReceiverSession(
	receiverPrivate *ecdh.PrivateKey,
	sessionAuthKey []byte,
	client ClientHello,
	server ServerHello,
) (SessionKeys, error) {
	return deriveSession(receiverPrivate, client.receiverPublic, server.senderPublic, sessionAuthKey, client, server)
}

func DeriveSenderSession(
	senderPrivate *ecdh.PrivateKey,
	sessionAuthKey []byte,
	client ClientHello,
	server ServerHello,
) (SessionKeys, error) {
	if !client.admitted {
		return SessionKeys{}, ErrClientHelloReplay
	}
	return deriveSession(senderPrivate, server.senderPublic, client.receiverPublic, sessionAuthKey, client, server)
}

func (k SessionKeys) ProtocolSessionID() ProtocolSessionID { return k.id }
func (k SessionKeys) TranscriptHash() TranscriptHash       { return k.transcriptHash }
func (k SessionKeys) ReceiverToSender() TrafficKey         { return k.receiverToSender }
func (k SessionKeys) SenderToReceiver() TrafficKey         { return k.senderToReceiver }
func (k *SessionKeys) Destroy() {
	if k == nil {
		return
	}
	k.receiverToSender.Destroy()
	k.senderToReceiver.Destroy()
}

func deriveSession(
	localPrivate *ecdh.PrivateKey,
	expectedLocalPublic *ecdh.PublicKey,
	peerPublic *ecdh.PublicKey,
	sessionAuthKey []byte,
	client ClientHello,
	server ServerHello,
) (SessionKeys, error) {
	if !client.valid || !server.valid {
		return SessionKeys{}, ErrHandshakeInput
	}
	clientDigest := sha256.Sum256(client.encoded[:])
	if subtle.ConstantTimeCompare(clientDigest[:], server.clientDigest[:]) != 1 {
		return SessionKeys{}, ErrHandshakeInput
	}
	if err := validateSessionAuthKey(sessionAuthKey); err != nil {
		return SessionKeys{}, err
	}
	private, err := normalizeX25519Private(localPrivate)
	if err != nil {
		return SessionKeys{}, err
	}
	localPublic := private.PublicKey().Bytes()
	if expectedLocalPublic == nil || subtle.ConstantTimeCompare(localPublic, expectedLocalPublic.Bytes()) != 1 {
		return SessionKeys{}, ErrKeyAgreement
	}
	peer, err := normalizeX25519Public(peerPublic)
	if err != nil {
		return SessionKeys{}, err
	}
	shared, err := private.ECDH(peer)
	if err != nil {
		return SessionKeys{}, fmt.Errorf("%w: %w", ErrKeyAgreement, err)
	}
	defer clear(shared)

	transcriptInput := make([]byte, 0, ClientHelloSize+ServerHelloSize)
	transcriptInput = append(transcriptInput, client.encoded[:]...)
	transcriptInput = append(transcriptInput, server.encoded[:]...)
	transcriptDigest := sha256.Sum256(transcriptInput)
	transcriptHash := TranscriptHash(transcriptDigest)

	sessionPreimage := append([]byte(protocolSessionDomain), transcriptDigest[:]...)
	sessionDigest := sha256.Sum256(sessionPreimage)
	var sessionID ProtocolSessionID
	copy(sessionID[:], sessionDigest[:IdentityBytes])

	handshakeInfo := domainInfo(handshakeDomain, transcriptDigest[:])
	handshakeSecret, err := hkdf.Key(sha256.New, shared, sessionAuthKey, string(handshakeInfo), SessionAuthKeyBytes)
	if err != nil {
		return SessionKeys{}, fmt.Errorf("derive handshake secret: %w", err)
	}
	defer clear(handshakeSecret)
	receiverToSender, err := deriveTrafficKey(
		handshakeSecret,
		receiverToSenderTrafficLabel,
		transcriptDigest[:],
		DirectionReceiverToSender,
	)
	if err != nil {
		return SessionKeys{}, err
	}
	senderToReceiver, err := deriveTrafficKey(
		handshakeSecret,
		senderToReceiverTrafficLabel,
		transcriptDigest[:],
		DirectionSenderToReceiver,
	)
	if err != nil {
		return SessionKeys{}, err
	}
	return SessionKeys{
		id: sessionID, transcriptHash: transcriptHash,
		receiverToSender: receiverToSender, senderToReceiver: senderToReceiver,
	}, nil
}

func deriveTrafficKey(secret []byte, label string, context []byte, direction Direction) (TrafficKey, error) {
	raw, err := hkdf.Key(sha256.New, secret, nil, string(domainInfo(label, context)), TrafficKeyBytes)
	if err != nil {
		return TrafficKey{}, fmt.Errorf("derive traffic key: %w", err)
	}
	defer clear(raw)
	var key TrafficKey
	copy(key.value[:], raw)
	key.direction = direction
	key.valid = true
	return key, nil
}

func domainInfo(domain string, context []byte) []byte {
	info := make([]byte, 0, len(domain)+1+len(context))
	info = append(info, domain...)
	info = append(info, 0)
	return append(info, context...)
}

func clientProof(sessionAuthKey, body []byte) []byte {
	digest := sha256.Sum256(body)
	mac := hmac.New(sha256.New, sessionAuthKey)
	_, _ = mac.Write([]byte(clientHelloDomain))
	_, _ = mac.Write(digest[:])
	return mac.Sum(nil)
}

func validateSessionAuthKey(key []byte) error {
	if len(key) != SessionAuthKeyBytes {
		return fmt.Errorf("%w: got %d", ErrKeyLength, len(key))
	}
	return nil
}

func normalizeX25519Public(public *ecdh.PublicKey) (*ecdh.PublicKey, error) {
	if public == nil {
		return nil, ErrHandshakeInput
	}
	return normalizeX25519PublicBytes(public.Bytes())
}

func normalizeX25519PublicBytes(raw []byte) (*ecdh.PublicKey, error) {
	if len(raw) != X25519KeyBytes {
		return nil, ErrHandshakeInput
	}
	public, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHandshakeInput, err)
	}
	return public, nil
}

func normalizeX25519Private(private *ecdh.PrivateKey) (*ecdh.PrivateKey, error) {
	if private == nil || len(private.Bytes()) != X25519KeyBytes {
		return nil, ErrHandshakeInput
	}
	normalized, err := ecdh.X25519().NewPrivateKey(private.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHandshakeInput, err)
	}
	return normalized, nil
}
