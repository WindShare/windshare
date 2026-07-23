package relayv2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/liveshare"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	r8RelayBaseURL               = "https://relay.example/r8"
	r8CapabilityRelayURL         = "wss://relay.example/r8"
	r8ChallengeExpiryUnixSeconds = 1_900_000_000
	r8RegistrationWriteCount     = 3
	r8RegistrationReadCount      = 2
)

type r8RegistrationRandom struct{ next byte }

func (random *r8RegistrationRandom) Read(destination []byte) (int, error) {
	for index := range destination {
		random.next++
		if random.next == 0 {
			random.next = 1
		}
		destination[index] = random.next
	}
	return len(destination), nil
}

type r8RegistrationFixture struct {
	config        SenderConfig
	endpoint      v2.RelayEndpoint
	transcript    []r8RegistrationEvent
	expectedStats RegistrationStats
}

type r8RegistrationOperation uint8

const (
	r8RegistrationWrite r8RegistrationOperation = iota + 1
	r8RegistrationRead
)

type r8RegistrationEvent struct {
	operation r8RegistrationOperation
	name      string
	encoded   []byte
}

func newR8RegistrationFixture(tb testing.TB) (r8RegistrationFixture, error) {
	// Preparing once outside the timed loop keeps this a transport measurement,
	// while using the real descriptor prevents a toy payload from understating
	// the production registration transcript.
	root := tb.TempDir()
	prepared, err := liveshare.PrepareSender(context.Background(), liveshare.SenderConfig{
		Paths: []string{root}, Relays: []string{r8CapabilityRelayURL},
		ChunkSize: catalog.DefaultChunkSize, Random: &r8RegistrationRandom{},
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
	})
	if err != nil {
		return r8RegistrationFixture{}, err
	}
	if err := prepared.AuthorizeRegistration(); err != nil {
		return r8RegistrationFixture{}, errors.Join(err, prepared.Close())
	}
	material := prepared.Registration()
	if err := prepared.Close(); err != nil {
		return r8RegistrationFixture{}, err
	}
	privateKey := material.SenderPrivateKey
	var pkHash v2.PKHash
	if len(material.PKHash) != len(pkHash) {
		return r8RegistrationFixture{}, fmt.Errorf("production PK hash bytes = %d, want %d", len(material.PKHash), len(pkHash))
	}
	copy(pkHash[:], material.PKHash)
	var shareID v2.ShareID
	if len(material.ShareID) != len(shareID) {
		return r8RegistrationFixture{}, fmt.Errorf("production share ID bytes = %d, want %d", len(material.ShareID), len(shareID))
	}
	copy(shareID[:], material.ShareID)
	var shareInstance v2.ShareInstance
	if len(material.ShareInstance) != len(shareInstance) {
		return r8RegistrationFixture{}, fmt.Errorf("production share instance bytes = %d, want %d", len(material.ShareInstance), len(shareInstance))
	}
	copy(shareInstance[:], material.ShareInstance)
	var resumeToken v2.ResumeToken
	for index := range resumeToken {
		resumeToken[index] = byte(index + 33)
	}
	descriptor := material.Descriptor
	init, err := NewFreshRegisterInit(shareID, shareInstance, pkHash, descriptor, resumeToken)
	if err != nil {
		return r8RegistrationFixture{}, err
	}
	endpoint, err := v2.NormalizeRelayEndpoint(r8RelayBaseURL)
	if err != nil {
		return r8RegistrationFixture{}, err
	}
	challenge := v2.Challenge{Purpose: v2.ChallengeRegister, ExpiresAtUnixSeconds: r8ChallengeExpiryUnixSeconds}
	for index := range challenge.ID {
		challenge.ID[index] = byte(index + 65)
	}
	for index := range challenge.Nonce {
		challenge.Nonce[index] = byte(index + 97)
	}
	proof, err := v2.NewRegisterProof(init, challenge, endpoint.Identity, privateKey)
	if err != nil {
		return r8RegistrationFixture{}, err
	}
	registered := v2.Registered{
		ShareID: init.ShareID, ShareInstance: init.ShareInstance, DescriptorDigest: init.DescriptorDigest,
	}
	initBytes, err := init.MarshalBinary()
	if err != nil {
		return r8RegistrationFixture{}, err
	}
	challengeBytes, err := challenge.MarshalBinary()
	if err != nil {
		return r8RegistrationFixture{}, err
	}
	proofBytes, err := proof.MarshalBinary()
	if err != nil {
		return r8RegistrationFixture{}, err
	}
	uploadBytes, err := (v2.DescriptorUpload{Object: descriptor}).MarshalBinary()
	if err != nil {
		return r8RegistrationFixture{}, err
	}
	registeredBytes, err := registered.MarshalBinary()
	if err != nil {
		return r8RegistrationFixture{}, err
	}
	return r8RegistrationFixture{
		config: SenderConfig{
			RelayBaseURL: r8RelayBaseURL, Init: init, SenderPrivateKey: privateKey, Descriptor: descriptor,
		},
		endpoint: endpoint,
		transcript: []r8RegistrationEvent{
			{operation: r8RegistrationWrite, name: "REGISTER_INIT", encoded: initBytes},
			{operation: r8RegistrationRead, name: "CHALLENGE", encoded: challengeBytes},
			{operation: r8RegistrationWrite, name: "REGISTER_PROOF", encoded: proofBytes},
			{operation: r8RegistrationWrite, name: "DESCRIPTOR_UPLOAD", encoded: uploadBytes},
			{operation: r8RegistrationRead, name: "REGISTERED", encoded: registeredBytes},
		},
		expectedStats: RegistrationStats{
			BytesSent:     uint64(len(initBytes) + len(proofBytes) + len(uploadBytes)),
			BytesReceived: uint64(len(challengeBytes) + len(registeredBytes)),
		},
	}, nil
}

type r8RegistrationSocket struct {
	mu         sync.Mutex
	transcript []r8RegistrationEvent
	eventIndex int
	done       chan struct{}
	closeOnce  sync.Once
	readLimit  atomic.Int64
}

func newR8RegistrationSocket(fixture r8RegistrationFixture) *r8RegistrationSocket {
	return &r8RegistrationSocket{
		transcript: fixture.transcript,
		done:       make(chan struct{}),
	}
}

func (socket *r8RegistrationSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	socket.mu.Lock()
	if socket.eventIndex < len(socket.transcript) {
		event := socket.transcript[socket.eventIndex]
		if event.operation != r8RegistrationRead {
			socket.mu.Unlock()
			return 0, nil, fmt.Errorf(
				"registration read at transcript event %d, want %s write",
				socket.eventIndex,
				event.name,
			)
		}
		response := bytes.Clone(event.encoded)
		socket.eventIndex++
		socket.mu.Unlock()
		return websocket.MessageBinary, response, nil
	}
	socket.mu.Unlock()
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-socket.done:
		return 0, nil, websocket.CloseError{Code: websocket.StatusNormalClosure}
	}
}

func (socket *r8RegistrationSocket) Write(
	_ context.Context,
	messageType websocket.MessageType,
	encoded []byte,
) error {
	socket.mu.Lock()
	defer socket.mu.Unlock()
	if messageType != websocket.MessageBinary {
		return errors.New("registration write was not binary")
	}
	if socket.eventIndex >= len(socket.transcript) {
		return errors.New("registration emitted an unexpected extra write")
	}
	event := socket.transcript[socket.eventIndex]
	if event.operation != r8RegistrationWrite {
		return fmt.Errorf(
			"registration write at transcript event %d, want %s read",
			socket.eventIndex,
			event.name,
		)
	}
	// Byte equality proves that DialSender reached every production handshake
	// stage; the shared cursor also rejects any cross-direction reordering.
	if !bytes.Equal(encoded, event.encoded) {
		return fmt.Errorf("registration %s differs from the exact protocol transcript", event.name)
	}
	socket.eventIndex++
	return nil
}

func (socket *r8RegistrationSocket) Close(websocket.StatusCode, string) error {
	socket.closeOnce.Do(func() { close(socket.done) })
	return nil
}

func (socket *r8RegistrationSocket) SetReadLimit(limit int64) {
	socket.readLimit.Store(limit)
}

func (socket *r8RegistrationSocket) assertComplete() error {
	socket.mu.Lock()
	defer socket.mu.Unlock()
	if socket.eventIndex != len(socket.transcript) {
		return fmt.Errorf("registration transcript stopped at event %d of %d", socket.eventIndex, len(socket.transcript))
	}
	wantReadLimit := int64(v2.OpaqueRouteHeaderBytes + v2.MaxOpaqueCiphertextBytes)
	if got := socket.readLimit.Load(); got != wantReadLimit {
		return fmt.Errorf("post-registration read limit = %d, want %d", got, wantReadLimit)
	}
	return nil
}

func r8RunSenderRegistration(fixture r8RegistrationFixture) (RegistrationStats, error) {
	socket := newR8RegistrationSocket(fixture)
	config := fixture.config
	config.Dial = DialOptions{SocketDialer: func(
		_ context.Context,
		target string,
		_ http.Header,
	) (BinarySocket, error) {
		if target != fixture.endpoint.DialURL {
			return nil, fmt.Errorf("registration dial target = %q, want %q", target, fixture.endpoint.DialURL)
		}
		return socket, nil
	}}
	sender, err := DialSender(context.Background(), config)
	if err != nil {
		return RegistrationStats{}, err
	}
	stats := sender.RegistrationStats()
	if stats != fixture.expectedStats {
		_ = sender.Close()
		return RegistrationStats{}, fmt.Errorf("registration stats = %+v, want %+v", stats, fixture.expectedStats)
	}
	if err := socket.assertComplete(); err != nil {
		_ = sender.Close()
		return RegistrationStats{}, err
	}
	if err := sender.Close(); err != nil {
		return RegistrationStats{}, err
	}
	<-sender.Done()
	return stats, nil
}

func TestR8RelaySenderRegistrationWireContract(t *testing.T) {
	fixture, err := newR8RegistrationFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := r8RunSenderRegistration(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if stats.BytesSent == 0 || stats.BytesReceived == 0 {
		t.Fatalf("registration stats are empty: %+v", stats)
	}
}

func TestR8RegistrationSocketRejectsCrossDirectionReordering(t *testing.T) {
	fixture, err := newR8RegistrationFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("challenge before init", func(t *testing.T) {
		socket := newR8RegistrationSocket(fixture)
		t.Cleanup(func() { _ = socket.Close(websocket.StatusNormalClosure, "") })
		if _, _, err := socket.Read(context.Background()); err == nil ||
			!bytes.Contains([]byte(err.Error()), []byte("want REGISTER_INIT write")) {
			t.Fatalf("out-of-order challenge error = %v", err)
		}
	})
	t.Run("proof before challenge", func(t *testing.T) {
		socket := newR8RegistrationSocket(fixture)
		t.Cleanup(func() { _ = socket.Close(websocket.StatusNormalClosure, "") })
		if err := socket.Write(context.Background(), websocket.MessageBinary, fixture.transcript[0].encoded); err != nil {
			t.Fatal(err)
		}
		if err := socket.Write(context.Background(), websocket.MessageBinary, fixture.transcript[2].encoded); err == nil ||
			!bytes.Contains([]byte(err.Error()), []byte("want CHALLENGE read")) {
			t.Fatalf("out-of-order proof error = %v", err)
		}
	})
	t.Run("registered before upload", func(t *testing.T) {
		socket := newR8RegistrationSocket(fixture)
		t.Cleanup(func() { _ = socket.Close(websocket.StatusNormalClosure, "") })
		if err := socket.Write(context.Background(), websocket.MessageBinary, fixture.transcript[0].encoded); err != nil {
			t.Fatal(err)
		}
		if _, _, err := socket.Read(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := socket.Write(context.Background(), websocket.MessageBinary, fixture.transcript[2].encoded); err != nil {
			t.Fatal(err)
		}
		if _, _, err := socket.Read(context.Background()); err == nil ||
			!bytes.Contains([]byte(err.Error()), []byte("want DESCRIPTOR_UPLOAD write")) {
			t.Fatalf("out-of-order registered error = %v", err)
		}
	})
}

func BenchmarkR8RelaySenderRegistration(b *testing.B) {
	fixture, err := newR8RegistrationFixture(b)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	var stats RegistrationStats
	b.ResetTimer()
	for b.Loop() {
		stats, err = r8RunSenderRegistration(fixture)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if stats != fixture.expectedStats {
		b.Fatalf("registration stats = %+v, want %+v", stats, fixture.expectedStats)
	}
	b.ReportMetric(float64(stats.BytesSent), "registration-wire-sent-B/op")
	b.ReportMetric(float64(stats.BytesReceived), "registration-wire-received-B/op")
	b.ReportMetric(float64(len(fixture.config.Descriptor)), "descriptor-bytes/op")
	b.ReportMetric(r8RegistrationWriteCount, "registration-writes/op")
	b.ReportMetric(r8RegistrationReadCount, "registration-reads/op")
}
