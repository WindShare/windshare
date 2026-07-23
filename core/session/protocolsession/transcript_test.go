package protocolsession

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/internal/testvec"
)

type transcriptVector struct {
	Name                   string `json:"name"`
	ClientBodyB64          string `json:"clientBodyB64"`
	ClientHelloB64         string `json:"clientHelloB64"`
	ClientProofB64         string `json:"clientProofB64"`
	HandshakeSecretB64     string `json:"handshakeSecretB64"`
	InitialLaneEpoch       uint32 `json:"initialLaneEpoch"`
	InitialLaneID          uint32 `json:"initialLaneId"`
	ProtocolSessionIDB64   string `json:"protocolSessionIdB64"`
	ReceiverPrivateB64     string `json:"receiverPrivateB64"`
	ReceiverPublicB64      string `json:"receiverPublicB64"`
	ReceiverToSenderKeyB64 string `json:"receiverToSenderKeyB64"`
	SenderPrivateB64       string `json:"senderPrivateB64"`
	SenderPublicB64        string `json:"senderPublicB64"`
	SenderToReceiverKeyB64 string `json:"senderToReceiverKeyB64"`
	ServerBodyB64          string `json:"serverBodyB64"`
	ServerHelloB64         string `json:"serverHelloB64"`
	ServerSignatureB64     string `json:"serverSignatureB64"`
	SessionAuthKeyB64      string `json:"sessionAuthKeyB64"`
	SharedSecretB64        string `json:"sharedSecretB64"`
	TranscriptHashB64      string `json:"transcriptHashB64"`
}

type relayProofVector struct {
	Name             string `json:"name"`
	RegisterProofB64 string `json:"registerProofB64"`
}

func TestTranscriptMatchesGoldenSessionVector(t *testing.T) {
	var vector transcriptVector
	decodeSessionCase(t, "sender-authenticated-x25519-transcript", &vector)
	clientBody := decodeB64(t, vector.ClientBodyB64)
	clientEncoded := decodeB64(t, vector.ClientHelloB64)
	sessionAuthKey := decodeB64(t, vector.SessionAuthKeyB64)

	share, err := catalog.ShareInstanceFromBytes(clientBody[5 : 5+catalog.IdentityBytes])
	if err != nil {
		t.Fatalf("decode vector share identity: %v", err)
	}
	receiver, err := ReceiverInstanceIDFromBytes(clientBody[21:37])
	if err != nil {
		t.Fatalf("decode vector receiver identity: %v", err)
	}
	receiverPublic := newX25519Public(t, decodeB64(t, vector.ReceiverPublicB64))
	client, err := NewClientHello(share, receiver, clientBody[37:69], receiverPublic, sessionAuthKey)
	if err != nil {
		t.Fatalf("build client hello: %v", err)
	}
	assertBytes(t, "client body", client.Body(), clientBody)
	assertBytes(t, "client proof", client.Encoded()[ClientHelloBodySize:], decodeB64(t, vector.ClientProofB64))
	assertBytes(t, "client hello", client.Encoded(), clientEncoded)

	replayGuard, err := NewClientHelloReplayGuard(8, nil)
	if err != nil {
		t.Fatal(err)
	}
	parsedClient, err := replayGuard.AcceptClientHello(clientEncoded, share, sessionAuthKey)
	if err != nil {
		t.Fatalf("parse client hello: %v", err)
	}
	if !parsedClient.ReceiverInstanceID().Equal(receiver) || !bytes.Equal(parsedClient.ReceiverNonce(), clientBody[37:69]) {
		t.Fatal("parsed client identity or nonce differs from vector")
	}

	senderVerificationKey := vectorSenderVerificationKey(t)
	serverEncoded := decodeB64(t, vector.ServerHelloB64)
	server, err := ParseServerHello(serverEncoded, parsedClient, senderVerificationKey)
	if err != nil {
		t.Fatalf("parse server hello: %v", err)
	}
	assertBytes(t, "server body", server.Body(), decodeB64(t, vector.ServerBodyB64))
	assertBytes(t, "server signature", server.Encoded()[ServerHelloBodySize:], decodeB64(t, vector.ServerSignatureB64))
	if server.InitialLaneID() != vector.InitialLaneID || server.InitialLaneEpoch() != vector.InitialLaneEpoch {
		t.Fatalf("lane identity = (%d,%d), want (%d,%d)", server.InitialLaneID(), server.InitialLaneEpoch(), vector.InitialLaneID, vector.InitialLaneEpoch)
	}

	receiverPrivate := newX25519Private(t, decodeB64(t, vector.ReceiverPrivateB64))
	senderPrivate := newX25519Private(t, decodeB64(t, vector.SenderPrivateB64))
	receiverKeys, err := DeriveReceiverSession(receiverPrivate, sessionAuthKey, parsedClient, server)
	if err != nil {
		t.Fatalf("derive receiver session: %v", err)
	}
	senderKeys, err := DeriveSenderSession(senderPrivate, sessionAuthKey, parsedClient, server)
	if err != nil {
		t.Fatalf("derive sender session: %v", err)
	}
	wantSession, err := ProtocolSessionIDFromBytes(decodeB64(t, vector.ProtocolSessionIDB64))
	if err != nil {
		t.Fatalf("decode vector protocol session identity: %v", err)
	}
	if !receiverKeys.ProtocolSessionID().Equal(wantSession) || !senderKeys.ProtocolSessionID().Equal(wantSession) {
		t.Fatal("derived protocol session identity differs from vector")
	}
	assertBytes(t, "transcript hash", receiverKeys.TranscriptHash().Bytes(), decodeB64(t, vector.TranscriptHashB64))
	assertBytes(t, "receiver-to-sender key", receiverKeys.ReceiverToSender().Bytes(), decodeB64(t, vector.ReceiverToSenderKeyB64))
	assertBytes(t, "sender-to-receiver key", receiverKeys.SenderToReceiver().Bytes(), decodeB64(t, vector.SenderToReceiverKeyB64))
	if !receiverKeys.ReceiverToSender().Equal(senderKeys.ReceiverToSender()) || !receiverKeys.SenderToReceiver().Equal(senderKeys.SenderToReceiver()) {
		t.Fatal("protocol roles derived different directional keys")
	}

	shared, err := receiverPrivate.ECDH(server.SenderPublicKey())
	if err != nil {
		t.Fatalf("derive vector shared secret: %v", err)
	}
	assertBytes(t, "shared secret", shared, decodeB64(t, vector.SharedSecretB64))
	handshake, err := hkdf.Key(sha256.New, shared, sessionAuthKey, string(domainInfo(handshakeDomain, receiverKeys.TranscriptHash().Bytes())), SessionAuthKeyBytes)
	if err != nil {
		t.Fatalf("derive vector handshake secret: %v", err)
	}
	assertBytes(t, "handshake secret", handshake, decodeB64(t, vector.HandshakeSecretB64))
}

func TestServerHelloBuilderAndParserBindClientAndSigningKey(t *testing.T) {
	share := mustShare(t, sequentialBytes(0x40, IdentityBytes))
	receiver := mustReceiver(t, sequentialBytes(0xc0, IdentityBytes))
	receiverPrivate := newX25519Private(t, sequentialBytes(0x11, X25519KeyBytes))
	senderPrivate := newX25519Private(t, sequentialBytes(0x51, X25519KeyBytes))
	authKey := sequentialBytes(0xa0, SessionAuthKeyBytes)
	client, err := NewClientHello(share, receiver, sequentialBytes(0x80, HandshakeNonceBytes), receiverPrivate.PublicKey(), authKey)
	if err != nil {
		t.Fatalf("build client hello: %v", err)
	}
	if _, err := NewServerHello(
		client,
		sequentialBytes(0x70, HandshakeNonceBytes),
		senderPrivate.PublicKey(),
		7,
		ed25519.NewKeyFromSeed(sequentialBytes(0x20, ed25519.SeedSize)),
	); !errors.Is(err, ErrHandshakeInput) {
		t.Fatalf("server hello accepted a client without replay admission: %v", err)
	}
	client = admitBuiltClient(t, client, share, authKey)
	signingKey := ed25519.NewKeyFromSeed(sequentialBytes(0x20, ed25519.SeedSize))
	server, err := NewServerHello(client, sequentialBytes(0x70, HandshakeNonceBytes), senderPrivate.PublicKey(), 7, signingKey)
	if err != nil {
		t.Fatalf("build server hello: %v", err)
	}
	parsed, err := ParseServerHello(server.Encoded(), client, signingKey.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("parse server hello: %v", err)
	}
	if parsed.InitialLaneID() != 7 || !bytes.Equal(parsed.SenderNonce(), sequentialBytes(0x70, HandshakeNonceBytes)) {
		t.Fatal("parsed server hello lost authenticated fields")
	}

	wrongClientBytes := client.Encoded()
	wrongClientBytes[40] ^= 1
	wrongClient, err := parseClientHello(wrongClientBytes, share, authKey)
	if !errors.Is(err, ErrClientHelloProof) || wrongClient.valid {
		t.Fatalf("tampered client = (%v,%v), want proof rejection", wrongClient.valid, err)
	}
	wrongSigningKey := ed25519.NewKeyFromSeed(sequentialBytes(0x21, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	if _, err := ParseServerHello(server.Encoded(), client, wrongSigningKey); !errors.Is(err, ErrServerHelloSignature) {
		t.Fatalf("wrong signing key: got %v", err)
	}
	otherClient, err := NewClientHello(share, receiver, sequentialBytes(0x81, HandshakeNonceBytes), receiverPrivate.PublicKey(), authKey)
	if err != nil {
		t.Fatalf("build other client: %v", err)
	}
	if _, err := ParseServerHello(server.Encoded(), otherClient, signingKey.Public().(ed25519.PublicKey)); !errors.Is(err, ErrServerHelloMalformed) {
		t.Fatalf("server/client transcript mismatch: got %v", err)
	}
	if _, err := DeriveReceiverSession(receiverPrivate, authKey, otherClient, server); !errors.Is(err, ErrHandshakeInput) {
		t.Fatalf("derive mismatched transcript pair: got %v", err)
	}
}

func TestTranscriptRejectsMalformedAndUnboundInputs(t *testing.T) {
	share := mustShare(t, sequentialBytes(0x10, IdentityBytes))
	receiver := mustReceiver(t, sequentialBytes(0x20, IdentityBytes))
	receiverPrivate := newX25519Private(t, sequentialBytes(0x30, X25519KeyBytes))
	senderPrivate := newX25519Private(t, sequentialBytes(0x50, X25519KeyBytes))
	authKey := sequentialBytes(0x70, SessionAuthKeyBytes)
	client, err := NewClientHello(share, receiver, make([]byte, HandshakeNonceBytes), receiverPrivate.PublicKey(), authKey)
	if err != nil {
		t.Fatalf("build client hello: %v", err)
	}
	client = admitBuiltClient(t, client, share, authKey)
	signingKey := ed25519.NewKeyFromSeed(sequentialBytes(0x90, ed25519.SeedSize))
	server, err := NewServerHello(client, make([]byte, HandshakeNonceBytes), senderPrivate.PublicKey(), 1, signingKey)
	if err != nil {
		t.Fatalf("build server hello: %v", err)
	}
	unadmittedClient := client
	unadmittedClient.admitted = false

	tests := []struct {
		name string
		run  func() error
		want error
	}{
		{"short client", func() error {
			_, err := parseClientHello(client.Encoded()[:ClientHelloSize-1], share, authKey)
			return err
		}, ErrClientHelloMalformed},
		{"wrong client magic", func() error {
			raw := client.Encoded()
			raw[0] ^= 1
			_, err := parseClientHello(raw, share, authKey)
			return err
		}, ErrClientHelloMalformed},
		{"wrong client version", func() error {
			raw := client.Encoded()
			raw[4]++
			_, err := parseClientHello(raw, share, authKey)
			return err
		}, ErrUnsupportedVersion},
		{"zero expected share", func() error {
			_, err := parseClientHello(client.Encoded(), catalog.ShareInstance{}, authKey)
			return err
		}, ErrHandshakeInput},
		{"wrong share", func() error {
			_, err := parseClientHello(client.Encoded(), mustShare(t, sequentialBytes(0x11, IdentityBytes)), authKey)
			return err
		}, ErrUnexpectedShare},
		{"zero receiver identity", func() error {
			raw := client.Encoded()
			receiverOffset := len(clientHelloMagic) + 1 + catalog.IdentityBytes
			clear(raw[receiverOffset : receiverOffset+IdentityBytes])
			copy(raw[ClientHelloBodySize:], clientProof(authKey, raw[:ClientHelloBodySize]))
			_, err := parseClientHello(raw, share, authKey)
			return err
		}, ErrClientHelloMalformed},
		{"short auth key", func() error { _, err := parseClientHello(client.Encoded(), share, authKey[:31]); return err }, ErrKeyLength},
		{"short server", func() error {
			_, err := ParseServerHello(server.Encoded()[:ServerHelloSize-1], client, signingKey.Public().(ed25519.PublicKey))
			return err
		}, ErrServerHelloMalformed},
		{"short sender verification key", func() error {
			_, err := ParseServerHello(server.Encoded(), client, signingKey.Public().(ed25519.PublicKey)[:ed25519.PublicKeySize-1])
			return err
		}, ErrHandshakeInput},
		{"wrong server magic", func() error {
			raw := server.Encoded()
			raw[0] ^= 1
			_, err := ParseServerHello(raw, client, signingKey.Public().(ed25519.PublicKey))
			return err
		}, ErrServerHelloMalformed},
		{"wrong server version", func() error {
			raw := server.Encoded()
			raw[len(serverHelloMagic)]++
			_, err := ParseServerHello(raw, client, signingKey.Public().(ed25519.PublicKey))
			return err
		}, ErrUnsupportedVersion},
		{"signed zero lane", func() error {
			raw := server.Encoded()
			laneOffset := len(serverHelloMagic) + 1 + sha256.Size + HandshakeNonceBytes + X25519KeyBytes
			clear(raw[laneOffset : laneOffset+4])
			digest := sha256.Sum256(raw[:ServerHelloBodySize])
			signature := ed25519.Sign(signingKey, append([]byte(serverHelloDomain), digest[:]...))
			copy(raw[ServerHelloBodySize:], signature)
			_, err := ParseServerHello(raw, client, signingKey.Public().(ed25519.PublicKey))
			return err
		}, ErrServerHelloMalformed},
		{"zero lane", func() error {
			_, err := NewServerHello(client, make([]byte, HandshakeNonceBytes), senderPrivate.PublicKey(), 0, signingKey)
			return err
		}, ErrHandshakeInput},
		{"sender derivation without replay admission", func() error {
			_, err := DeriveSenderSession(senderPrivate, authKey, unadmittedClient, server)
			return err
		}, ErrClientHelloReplay},
		{"invalid client derivation", func() error {
			_, err := DeriveReceiverSession(receiverPrivate, authKey, ClientHello{}, server)
			return err
		}, ErrHandshakeInput},
		{"short derivation auth key", func() error {
			_, err := DeriveReceiverSession(receiverPrivate, authKey[:SessionAuthKeyBytes-1], client, server)
			return err
		}, ErrKeyLength},
		{"nil derivation private key", func() error {
			_, err := DeriveReceiverSession(nil, authKey, client, server)
			return err
		}, ErrHandshakeInput},
		{"unbound private", func() error { _, err := DeriveReceiverSession(senderPrivate, authKey, client, server); return err }, ErrKeyAgreement},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}

	keys, err := DeriveSenderSession(senderPrivate, authKey, client, server)
	if err != nil {
		t.Fatalf("derive valid sender session: %v", err)
	}
	keys.Destroy()
	if !keys.ReceiverToSender().Equal(TrafficKey{}) || !keys.SenderToReceiver().Equal(TrafficKey{}) {
		t.Fatal("destroy did not erase traffic keys")
	}
}

func TestTranscriptValuesAreFixedSizeAndCallerIndependent(t *testing.T) {
	raw := sequentialBytes(0x30, TrafficKeyBytes)
	key, err := TrafficKeyFromBytes(raw, DirectionReceiverToSender)
	if err != nil {
		t.Fatalf("create traffic key: %v", err)
	}
	raw[0] ^= 0xff
	if key.Bytes()[0] != 0x30 {
		t.Fatal("traffic key retained caller-owned bytes")
	}
	copyOut := key.Bytes()
	copyOut[0] ^= 0xff
	if key.Bytes()[0] != 0x30 {
		t.Fatal("traffic key exposed mutable storage")
	}
	if _, err := TrafficKeyFromBytes(make([]byte, TrafficKeyBytes-1), DirectionReceiverToSender); !errors.Is(err, ErrKeyLength) {
		t.Fatalf("short traffic key: got %v", err)
	}
	if _, err := TrafficKeyFromBytes(make([]byte, TrafficKeyBytes), Direction(99)); !errors.Is(err, ErrTrafficKeyDirection) {
		t.Fatalf("invalid traffic key direction: got %v", err)
	}
	key.Destroy()
	if !key.Equal(TrafficKey{}) {
		t.Fatal("traffic key destroy did not clear storage")
	}
	var nilKey *TrafficKey
	nilKey.Destroy()

	var first TranscriptHash
	copy(first[:], sequentialBytes(0x60, TranscriptHashBytes))
	second := first
	if !first.Equal(second) {
		t.Fatal("equal transcript hashes differ")
	}
	second[0] ^= 1
	if first.Equal(second) {
		t.Fatal("different transcript hashes compare equal")
	}

	share := mustShare(t, sequentialBytes(0x10, IdentityBytes))
	receiver := mustReceiver(t, sequentialBytes(0x20, IdentityBytes))
	private := newX25519Private(t, sequentialBytes(0x40, X25519KeyBytes))
	authKey := sequentialBytes(0x80, SessionAuthKeyBytes)
	client, err := NewClientHello(share, receiver, make([]byte, HandshakeNonceBytes), private.PublicKey(), authKey)
	if err != nil {
		t.Fatalf("build client hello: %v", err)
	}
	if !client.ShareInstance().Equal(share) || !bytes.Equal(client.ReceiverPublicKey().Bytes(), private.PublicKey().Bytes()) {
		t.Fatal("client hello accessors lost transcript fields")
	}
	if _, err := NewClientHello(share, receiver, make([]byte, HandshakeNonceBytes), nil, authKey); !errors.Is(err, ErrHandshakeInput) {
		t.Fatalf("nil X25519 public key: got %v", err)
	}
}

func TestClientHelloReplayGuardIsAtomicAndExpiresAtFiveMinutes(t *testing.T) {
	share := mustShare(t, sequentialBytes(0x10, IdentityBytes))
	receiver := mustReceiver(t, sequentialBytes(0x20, IdentityBytes))
	private := newX25519Private(t, sequentialBytes(0x40, X25519KeyBytes))
	authKey := sequentialBytes(0x80, SessionAuthKeyBytes)
	client, err := NewClientHello(share, receiver, sequentialBytes(0xa0, HandshakeNonceBytes), private.PublicKey(), authKey)
	if err != nil {
		t.Fatalf("build client hello: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	guard, err := NewClientHelloReplayGuard(4, func() time.Time { return now })
	if err != nil {
		t.Fatalf("create replay guard: %v", err)
	}

	var admitted atomic.Int32
	var replays atomic.Int32
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			err := guard.admit(client)
			switch {
			case err == nil:
				admitted.Add(1)
			case errors.Is(err, ErrClientHelloReplay):
				replays.Add(1)
			default:
				t.Errorf("admit client hello: %v", err)
			}
		}()
	}
	wait.Wait()
	if admitted.Load() != 1 || replays.Load() != 31 {
		t.Fatalf("atomic admission accepted=%d replayed=%d", admitted.Load(), replays.Load())
	}

	now = now.Add(ClientHelloReplayTTL - time.Nanosecond)
	if err := guard.admit(client); !errors.Is(err, ErrClientHelloReplay) {
		t.Fatalf("replay before expiry: got %v", err)
	}
	now = now.Add(time.Nanosecond)
	if err := guard.admit(client); err != nil {
		t.Fatalf("admission at exact expiry: %v", err)
	}
	if err := (*ClientHelloReplayGuard)(nil).admit(client); !errors.Is(err, ErrHandshakeInput) {
		t.Fatalf("nil guard: got %v", err)
	}
}

func TestAcceptClientHelloAtomicallyAuthenticatesAndTombstones(t *testing.T) {
	share := mustShare(t, sequentialBytes(0xb0, IdentityBytes))
	receiver := mustReceiver(t, sequentialBytes(0xc0, IdentityBytes))
	private := newX25519Private(t, sequentialBytes(0xd0, X25519KeyBytes))
	authKey := sequentialBytes(0xe0, SessionAuthKeyBytes)
	client, err := NewClientHello(
		share,
		receiver,
		sequentialBytes(0xf0, HandshakeNonceBytes),
		private.PublicKey(),
		authKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := NewClientHelloReplayGuard(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	tampered := client.Encoded()
	tampered[len(tampered)-1] ^= 1
	if _, err := guard.AcceptClientHello(tampered, share, authKey); !errors.Is(err, ErrClientHelloProof) {
		t.Fatalf("invalid proof admission = %v", err)
	}
	accepted, err := guard.AcceptClientHello(client.Encoded(), share, authKey)
	if err != nil || !accepted.admitted {
		t.Fatalf("first acceptance = admitted %v, err %v", accepted.admitted, err)
	}
	if _, err := guard.AcceptClientHello(client.Encoded(), share, authKey); !errors.Is(err, ErrClientHelloReplay) {
		t.Fatalf("atomic replay acceptance = %v", err)
	}
	if _, err := (*ClientHelloReplayGuard)(nil).AcceptClientHello(client.Encoded(), share, authKey); !errors.Is(err, ErrHandshakeInput) {
		t.Fatalf("nil replay guard = %v", err)
	}
}

func TestClientHelloReplayTupleExcludesDiagnosticReceiverIdentity(t *testing.T) {
	share := mustShare(t, sequentialBytes(0x10, IdentityBytes))
	private := newX25519Private(t, sequentialBytes(0x40, X25519KeyBytes))
	authKey := sequentialBytes(0x80, SessionAuthKeyBytes)
	nonce := sequentialBytes(0xa0, HandshakeNonceBytes)
	first, err := NewClientHello(share, mustReceiver(t, sequentialBytes(0x20, IdentityBytes)), nonce, private.PublicKey(), authKey)
	if err != nil {
		t.Fatalf("build first client hello: %v", err)
	}
	second, err := NewClientHello(share, mustReceiver(t, sequentialBytes(0x21, IdentityBytes)), nonce, private.PublicKey(), authKey)
	if err != nil {
		t.Fatalf("build second client hello: %v", err)
	}
	guard, err := NewClientHelloReplayGuard(2, nil)
	if err != nil {
		t.Fatalf("create replay guard: %v", err)
	}
	if err := guard.admit(first); err != nil {
		t.Fatalf("admit first client hello: %v", err)
	}
	if err := guard.admit(second); !errors.Is(err, ErrClientHelloReplay) {
		t.Fatalf("receiver identity bypass: got %v", err)
	}

	otherNonce, err := NewClientHello(share, second.ReceiverInstanceID(), sequentialBytes(0xa1, HandshakeNonceBytes), private.PublicKey(), authKey)
	if err != nil {
		t.Fatalf("build new nonce client hello: %v", err)
	}
	if err := guard.admit(otherNonce); err != nil {
		t.Fatalf("distinct replay tuple: %v", err)
	}
	var zeroGuard ClientHelloReplayGuard
	if err := zeroGuard.admit(otherNonce); !errors.Is(err, ErrClientHelloReplayBudget) {
		t.Fatalf("zero-value replay guard: got %v", err)
	}
}

func TestClientHelloReplayGuardHasAnExplicitBoundedBudget(t *testing.T) {
	if _, err := NewClientHelloReplayGuard(0, nil); !errors.Is(err, ErrClientHelloReplayBudget) {
		t.Fatalf("zero replay capacity: got %v", err)
	}
	share := mustShare(t, sequentialBytes(0x10, IdentityBytes))
	receiver := mustReceiver(t, sequentialBytes(0x20, IdentityBytes))
	private := newX25519Private(t, sequentialBytes(0x40, X25519KeyBytes))
	authKey := sequentialBytes(0x80, SessionAuthKeyBytes)
	build := func(nonceFirst byte) ClientHello {
		hello, err := NewClientHello(share, receiver, sequentialBytes(nonceFirst, HandshakeNonceBytes), private.PublicKey(), authKey)
		if err != nil {
			t.Fatalf("build client hello: %v", err)
		}
		return hello
	}
	now := time.Unix(1_700_000_000, 0)
	guard, err := NewClientHelloReplayGuard(2, func() time.Time { return now })
	if err != nil {
		t.Fatalf("create replay guard: %v", err)
	}
	first, second, third := build(0xa0), build(0xb0), build(0xc0)
	if err := guard.admit(first); err != nil {
		t.Fatalf("admit first hello: %v", err)
	}
	if err := guard.admit(second); err != nil {
		t.Fatalf("admit second hello: %v", err)
	}
	if err := guard.admit(third); !errors.Is(err, ErrClientHelloReplayBudget) {
		t.Fatalf("over-budget hello: got %v", err)
	}
	if err := guard.admit(first); !errors.Is(err, ErrClientHelloReplay) {
		t.Fatalf("duplicate at capacity: got %v", err)
	}
	now = now.Add(ClientHelloReplayTTL)
	if err := guard.admit(third); err != nil {
		t.Fatalf("expired tombstones did not free budget: %v", err)
	}
}

func admitBuiltClient(
	t *testing.T,
	client ClientHello,
	share catalog.ShareInstance,
	sessionAuthKey []byte,
) ClientHello {
	t.Helper()
	guard, err := NewClientHelloReplayGuard(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := guard.AcceptClientHello(client.Encoded(), share, sessionAuthKey)
	if err != nil {
		t.Fatalf("admit client hello: %v", err)
	}
	return admitted
}

func decodeSessionCase(t *testing.T, name string, destination any) {
	t.Helper()
	file, err := testvec.Load(filepath.Join("..", "..", "testvectors", "v2-session.json"))
	if err != nil {
		t.Fatalf("load session vectors: %v", err)
	}
	if file.Kind != "v2-session" {
		t.Fatalf("vector kind = %q, want v2-session", file.Kind)
	}
	for i := range file.Cases {
		if file.Cases[i].Name == name {
			if err := file.Cases[i].Decode(destination); err != nil {
				t.Fatalf("decode session vector %q: %v", name, err)
			}
			return
		}
	}
	t.Fatalf("session vector %q not found", name)
}

func vectorSenderVerificationKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	var vector relayProofVector
	decodeSessionCase(t, "fresh-relay-registration-proof", &vector)
	proof := decodeB64(t, vector.RegisterProofB64)
	const relayProofHeaderBytes = 8
	if len(proof) < relayProofHeaderBytes+ed25519.PublicKeySize || string(proof[:4]) != "WS2P" {
		t.Fatal("relay proof vector does not contain a sender verification key")
	}
	return append(ed25519.PublicKey(nil), proof[relayProofHeaderBytes:relayProofHeaderBytes+ed25519.PublicKeySize]...)
}

func mustShare(t *testing.T, raw []byte) catalog.ShareInstance {
	t.Helper()
	share, err := catalog.ShareInstanceFromBytes(raw)
	if err != nil {
		t.Fatalf("create share identity: %v", err)
	}
	return share
}

func mustReceiver(t *testing.T, raw []byte) ReceiverInstanceID {
	t.Helper()
	receiver, err := ReceiverInstanceIDFromBytes(raw)
	if err != nil {
		t.Fatalf("create receiver identity: %v", err)
	}
	return receiver
}

func newX25519Private(t *testing.T, raw []byte) *ecdh.PrivateKey {
	t.Helper()
	private, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		t.Fatalf("create X25519 private key: %v", err)
	}
	return private
}

func newX25519Public(t *testing.T, raw []byte) *ecdh.PublicKey {
	t.Helper()
	public, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		t.Fatalf("create X25519 public key: %v", err)
	}
	return public
}

func decodeB64(t *testing.T, encoded string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	return decoded
}

func assertBytes(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs\n got: %x\nwant: %x", name, got, want)
	}
}

func sequentialBytes(first byte, size int) []byte {
	result := make([]byte, size)
	for i := range result {
		result[i] = first + byte(i)
	}
	return result
}
