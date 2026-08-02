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
	registrationRelayBaseURL               = "https://relay.example/performance"
	registrationCapabilityRelayURL         = "wss://relay.example/performance"
	registrationChallengeExpiryUnixSeconds = 1_900_000_000
	registrationWriteCount                 = 3
	registrationReadCount                  = 2
)

type registrationContractRandom struct{ next byte }

func (random *registrationContractRandom) Read(destination []byte) (int, error) {
	for index := range destination {
		random.next++
		if random.next == 0 {
			random.next = 1
		}
		destination[index] = random.next
	}
	return len(destination), nil
}

type registrationContractFixture struct {
	config        SenderConfig
	endpoint      v2.RelayEndpoint
	transcript    []registrationContractEvent
	expectedStats RegistrationStats
}

type registrationContractOperation uint8

const (
	registrationContractWrite registrationContractOperation = iota + 1
	registrationContractRead
)

type registrationContractEvent struct {
	operation registrationContractOperation
	name      string
	encoded   []byte
}

func newRegistrationContractFixture(tb testing.TB) (registrationContractFixture, error) {
	// Preparing once outside the timed loop keeps this a transport measurement,
	// while using the real descriptor prevents a toy payload from understating
	// the production registration transcript.
	root := tb.TempDir()
	prepared, err := liveshare.PrepareSender(context.Background(), liveshare.SenderConfig{
		Paths: []string{root}, Relays: []string{registrationCapabilityRelayURL},
		ChunkSize: catalog.DefaultChunkSize, Random: &registrationContractRandom{},
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
	})
	if err != nil {
		return registrationContractFixture{}, err
	}
	if err := prepared.AuthorizeRegistration(); err != nil {
		return registrationContractFixture{}, errors.Join(err, prepared.Close())
	}
	material := prepared.Registration()
	if err := prepared.Close(); err != nil {
		return registrationContractFixture{}, err
	}
	privateKey := material.SenderPrivateKey
	var pkHash v2.PKHash
	if len(material.PKHash) != len(pkHash) {
		return registrationContractFixture{}, fmt.Errorf("production PK hash bytes = %d, want %d", len(material.PKHash), len(pkHash))
	}
	copy(pkHash[:], material.PKHash)
	var shareID v2.ShareID
	if len(material.ShareID) != len(shareID) {
		return registrationContractFixture{}, fmt.Errorf("production share ID bytes = %d, want %d", len(material.ShareID), len(shareID))
	}
	copy(shareID[:], material.ShareID)
	var shareInstance v2.ShareInstance
	if len(material.ShareInstance) != len(shareInstance) {
		return registrationContractFixture{}, fmt.Errorf("production share instance bytes = %d, want %d", len(material.ShareInstance), len(shareInstance))
	}
	copy(shareInstance[:], material.ShareInstance)
	var resumeToken v2.ResumeToken
	for index := range resumeToken {
		resumeToken[index] = byte(index + 33)
	}
	descriptor := material.Descriptor
	init, err := NewFreshRegisterInit(shareID, shareInstance, pkHash, descriptor, resumeToken)
	if err != nil {
		return registrationContractFixture{}, err
	}
	endpoint, err := v2.NormalizeRelayEndpoint(registrationRelayBaseURL)
	if err != nil {
		return registrationContractFixture{}, err
	}
	challenge := v2.Challenge{Purpose: v2.ChallengeRegister, ExpiresAtUnixSeconds: registrationChallengeExpiryUnixSeconds}
	for index := range challenge.ID {
		challenge.ID[index] = byte(index + 65)
	}
	for index := range challenge.Nonce {
		challenge.Nonce[index] = byte(index + 97)
	}
	proof, err := v2.NewRegisterProof(init, challenge, endpoint.Identity, privateKey)
	if err != nil {
		return registrationContractFixture{}, err
	}
	registered := v2.Registered{
		ShareID: init.ShareID, ShareInstance: init.ShareInstance, DescriptorDigest: init.DescriptorDigest,
	}
	initBytes, err := init.MarshalBinary()
	if err != nil {
		return registrationContractFixture{}, err
	}
	challengeBytes, err := challenge.MarshalBinary()
	if err != nil {
		return registrationContractFixture{}, err
	}
	proofBytes, err := proof.MarshalBinary()
	if err != nil {
		return registrationContractFixture{}, err
	}
	uploadBytes, err := (v2.DescriptorUpload{Object: descriptor}).MarshalBinary()
	if err != nil {
		return registrationContractFixture{}, err
	}
	registeredBytes, err := registered.MarshalBinary()
	if err != nil {
		return registrationContractFixture{}, err
	}
	return registrationContractFixture{
		config: SenderConfig{
			RelayBaseURL: registrationRelayBaseURL, Init: init, SenderPrivateKey: privateKey, Descriptor: descriptor,
		},
		endpoint: endpoint,
		transcript: []registrationContractEvent{
			{operation: registrationContractWrite, name: "REGISTER_INIT", encoded: initBytes},
			{operation: registrationContractRead, name: "CHALLENGE", encoded: challengeBytes},
			{operation: registrationContractWrite, name: "REGISTER_PROOF", encoded: proofBytes},
			{operation: registrationContractWrite, name: "DESCRIPTOR_UPLOAD", encoded: uploadBytes},
			{operation: registrationContractRead, name: "REGISTERED", encoded: registeredBytes},
		},
		expectedStats: RegistrationStats{
			BytesSent:     uint64(len(initBytes) + len(proofBytes) + len(uploadBytes)),
			BytesReceived: uint64(len(challengeBytes) + len(registeredBytes)),
		},
	}, nil
}

type registrationContractSocket struct {
	mu         sync.Mutex
	transcript []registrationContractEvent
	eventIndex int
	done       chan struct{}
	closeOnce  sync.Once
	readLimit  atomic.Int64
}

func newRegistrationContractSocket(fixture registrationContractFixture) *registrationContractSocket {
	return &registrationContractSocket{
		transcript: fixture.transcript,
		done:       make(chan struct{}),
	}
}

func (socket *registrationContractSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	socket.mu.Lock()
	if socket.eventIndex < len(socket.transcript) {
		event := socket.transcript[socket.eventIndex]
		if event.operation != registrationContractRead {
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

func (socket *registrationContractSocket) Write(
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
	if event.operation != registrationContractWrite {
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

func (socket *registrationContractSocket) Close(websocket.StatusCode, string) error {
	socket.closeOnce.Do(func() { close(socket.done) })
	return nil
}

func (socket *registrationContractSocket) SetReadLimit(limit int64) {
	socket.readLimit.Store(limit)
}

func (socket *registrationContractSocket) assertComplete() error {
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

func runSenderRegistrationContract(fixture registrationContractFixture) (RegistrationStats, error) {
	socket := newRegistrationContractSocket(fixture)
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

func BenchmarkRelaySenderRegistration(b *testing.B) {
	fixture, err := newRegistrationContractFixture(b)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	var stats RegistrationStats
	b.ResetTimer()
	for b.Loop() {
		stats, err = runSenderRegistrationContract(fixture)
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
	b.ReportMetric(registrationWriteCount, "registration-writes/op")
	b.ReportMetric(registrationReadCount, "registration-reads/op")
}
