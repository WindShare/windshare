package v2route

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

var routeTestConnections sync.Map

func routeTestConnection(id ConnectionID) ConnectionRef {
	if existing, ok := routeTestConnections.Load(id); ok {
		return existing.(ConnectionRef)
	}
	reference, err := NewConnectionRef(id)
	if err != nil {
		panic(err)
	}
	actual, _ := routeTestConnections.LoadOrStore(id, reference)
	return actual.(ConnectionRef)
}

type memoryTombstones struct {
	mu      sync.Mutex
	records map[v2.ShareID]Tombstone
	load    []Tombstone
	loadErr error
	failPut bool
	puts    int
}

func (s *memoryTombstones) Load(context.Context) ([]Tombstone, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	if s.load != nil {
		return append([]Tombstone(nil), s.load...), nil
	}
	result := make([]Tombstone, 0, len(s.records))
	for _, record := range s.records {
		result = append(result, record)
	}
	return result, nil
}

func (s *memoryTombstones) Commit(_ context.Context, record Tombstone) (CommitOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPut {
		return CommitNotCommitted, errors.New("durability failure")
	}
	if s.records == nil {
		s.records = make(map[v2.ShareID]Tombstone)
	}
	if existing, exists := s.records[record.ShareID]; exists {
		if existing == record {
			return CommitCommitted, nil
		}
		return CommitUnknown, ErrTombstoneConflict
	}
	s.records[record.ShareID] = record
	s.puts++
	return CommitCommitted, nil
}

type routeFixture struct {
	init       v2.RegisterInit
	descriptor []byte
	token      v2.ResumeToken
	stop       v2.StopInit
	verified   v2.VerifiedDescriptor
	authority  v2.SenderAuthority
	stopAuth   v2.StopAuthority
	privateKey ed25519.PrivateKey
}

func makeFixture(t *testing.T, identityByte byte) routeFixture {
	t.Helper()
	seed := bytes.Repeat([]byte{identityByte}, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	var fixture routeFixture
	fixture.init.Mode = v2.RegistrationFresh
	publicDigest := sha256.Sum256(append([]byte("windshare/v2 sender-key\x00"), publicKey...))
	copy(fixture.init.PKHash[:], publicDigest[:v2.PKHashBytes])
	shareDigest := sha256.Sum256(append([]byte("windshare/v2 share-id\x00"), fixture.init.PKHash[:]...))
	copy(fixture.init.ShareID[:], shareDigest[:v2.ShareIDBytes])
	copy(fixture.init.ShareInstance[:], testBytes(identityByte+1, v2.ShareInstanceBytes))
	info := append([]byte("windshare/v2 descriptor"), 0)
	info = append(info, fixture.init.PKHash[:]...)
	descriptorKey, err := hkdf.Key(sha256.New, bytes.Repeat([]byte{0x11}, 16), nil, string(info), 32)
	if err != nil {
		t.Fatal(err)
	}
	fixture.descriptor, err = sealTestDescriptor(
		fixture.init, descriptorKey, privateKey, bytes.Repeat([]byte{identityByte + 2}, 12), []byte{0xa1, 0x00, 0x01},
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.init.DescriptorDigest = sha256.Sum256(fixture.descriptor)
	copy(fixture.token[:], testBytes(identityByte+3, v2.ResumeTokenBytes))
	fixture.init.ResumeTokenHash = sha256.Sum256(fixture.token[:])
	fixture.stop.ShareID = fixture.init.ShareID
	fixture.stop.ShareInstance = fixture.init.ShareInstance
	fixture.stop.PKHash = fixture.init.PKHash
	copy(fixture.stop.RelayIdentity[:], testBytes(identityByte+4, v2.RelayIdentityBytes))
	copy(fixture.stop.StopID[:], testBytes(identityByte+5, v2.StopIDBytes))
	fixture.privateKey = privateKey
	fixture.authority = registrationAuthority(t, fixture, fixture.init)
	fixture.verified, err = v2.VerifyDescriptorUpload(fixture.init, fixture.authority, v2.DescriptorUpload{Object: fixture.descriptor})
	if err != nil {
		t.Fatal(err)
	}
	fixture.stopAuth = stopAuthority(t, fixture, fixture.stop)
	return fixture
}

func sealTestDescriptor(init v2.RegisterInit, key []byte, privateKey ed25519.PrivateKey, nonce, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 8)
	header[0] = v2.WireVersion
	binary.BigEndian.PutUint32(header[4:], uint32(len(plaintext)+aead.Overhead()))
	context := append([]byte{v2.Suite}, init.PKHash[:]...)
	context = append(context, init.ShareID[:]...)
	contextHash := sha256.Sum256(context)
	aad := append([]byte("windshare/v2 object/descriptor\x00"), contextHash[:]...)
	aad = append(aad, header...)
	prefix := append(bytes.Clone(header), nonce...)
	prefix = aead.Seal(prefix, nonce, plaintext, aad)
	preimage := append([]byte("windshare/v2 object/descriptor\x00"), contextHash[:]...)
	preimage = append(preimage, prefix...)
	return append(prefix, ed25519.Sign(privateKey, preimage)...), nil
}

func challengeLedger(t *testing.T) *v2.ChallengeLedger {
	t.Helper()
	random := append(testBytes(1, v2.ChallengeIDBytes), testBytes(21, v2.ChallengeNonceBytes)...)
	ledger, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{
		Capacity: 1, Random: bytes.NewReader(random), Now: func() time.Time { return time.Unix(1, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func registrationAuthority(t *testing.T, fixture routeFixture, init v2.RegisterInit) v2.SenderAuthority {
	t.Helper()
	ledger := challengeLedger(t)
	binding, err := v2.RegistrationChallengeBinding(init, fixture.stop.RelayIdentity)
	if err != nil {
		t.Fatal(err)
	}
	purpose := v2.ChallengeRegister
	if init.Mode == v2.RegistrationResume {
		purpose = v2.ChallengeResume
	}
	challenge, err := ledger.Issue(purpose, binding)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := v2.NewRegisterProof(init, challenge, fixture.stop.RelayIdentity, fixture.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := ledger.AuthenticateRegistration(challenge.ID, init, fixture.stop.RelayIdentity, proof)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func resumeAuthority(t *testing.T, fixture routeFixture, init v2.RegisterInit) v2.SenderAuthority {
	t.Helper()
	return registrationAuthority(t, fixture, init)
}

func stopAuthority(t *testing.T, fixture routeFixture, stop v2.StopInit) v2.StopAuthority {
	t.Helper()
	ledger := challengeLedger(t)
	binding, err := v2.StopChallengeBinding(stop)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := ledger.Issue(v2.ChallengeStop, binding)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := v2.NewStopProof(stop, challenge, fixture.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := ledger.AuthenticateStop(challenge.ID, stop, proof)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func testBytes(first byte, length int) []byte {
	result := make([]byte, length)
	for index := range result {
		result[index] = first + byte(index)
	}
	return result
}

func newRegistry(t *testing.T, now *time.Time, store TombstoneStore, maxRoutes int) *Registry {
	t.Helper()
	randomBytes := make([]byte, 0, v2.RelaySessionIDBytes*128)
	for value := byte(1); value < 129; value++ {
		id := make([]byte, v2.RelaySessionIDBytes)
		id[0] = value
		randomBytes = append(randomBytes, id...)
	}
	random := bytes.NewReader(randomBytes)
	registry, err := New(context.Background(), Config{
		MaxRoutes: maxRoutes, MaxSessions: 16, MaxSessionsPerShare: 8,
		Random: random, Now: func() time.Time { return *now }, Tombstones: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestCrashGraceResumeAndRoleDerivedRouting(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &memoryTombstones{records: make(map[v2.ShareID]Tombstone)}
	registry := newRegistry(t, &now, store, 4)
	fixture := makeFixture(t, 0x20)
	senderA := routeTestConnection("sender-a")
	receiverA := routeTestConnection("receiver-a")
	if err := registry.BeginRegistration(fixture.init, senderA); err != nil {
		t.Fatal(err)
	}
	starting, err := registry.Join(fixture.init.ShareID, receiverA)
	if err != nil || starting.Status != JoinStarting || starting.RetryAfter == 0 {
		t.Fatalf("starting join = %+v, %v", starting, err)
	}
	if err := registry.Publish(fixture.init.ShareID, senderA, fixture.verified); err != nil {
		t.Fatal(err)
	}
	joined, err := registry.Join(fixture.init.ShareID, receiverA)
	if err != nil || joined.Status != JoinReady || !bytes.Equal(joined.Descriptor, fixture.descriptor) {
		t.Fatalf("ready join = %+v, %v", joined, err)
	}
	joined.Descriptor[0] ^= 1
	again, _ := registry.Join(fixture.init.ShareID, routeTestConnection("receiver-b"))
	if !bytes.Equal(again.Descriptor, fixture.descriptor) {
		t.Fatal("registry exposed descriptor backing storage")
	}
	receiverRoute, err := registry.ResolveSession(joined.RelaySessionID, receiverA)
	if err != nil || receiverRoute.Disposition != SessionForward || receiverRoute.Destination != senderA {
		t.Fatalf("receiver route = %+v, %v", receiverRoute, err)
	}
	senderRoute, err := registry.ResolveSession(joined.RelaySessionID, senderA)
	if err != nil || senderRoute.Disposition != SessionForward || senderRoute.Destination != receiverA {
		t.Fatalf("sender route = %+v, %v", senderRoute, err)
	}
	if _, err := registry.ResolveSession(joined.RelaySessionID, routeTestConnection("outsider")); !errors.Is(err, ErrOwner) {
		t.Fatalf("outsider route error = %v", err)
	}
	if _, transitioned := registry.UnexpectedDisconnect(fixture.init.ShareID, senderA); !transitioned {
		t.Fatal("sender crash was not admitted")
	}
	for _, formerOwner := range []ConnectionRef{senderA, receiverA} {
		resolution, resolveErr := registry.ResolveSession(joined.RelaySessionID, formerOwner)
		if resolveErr != nil || resolution.Disposition != SessionRetired || resolution.Destination.Valid() {
			t.Fatalf("retired route for %q = %+v, %v", formerOwner.ConnectionID(), resolution, resolveErr)
		}
	}
	if _, err := registry.ResolveSession(joined.RelaySessionID, routeTestConnection("outsider")); !errors.Is(err, ErrOwner) {
		t.Fatalf("retired outsider route error = %v", err)
	}
	duringGrace, _ := registry.Join(fixture.init.ShareID, receiverA)
	if duringGrace.Status != JoinStarting {
		t.Fatalf("grace join = %+v", duringGrace)
	}
	resume := fixture.init
	resume.Mode = v2.RegistrationResume
	resumeAuth := resumeAuthority(t, fixture, resume)
	wrong := fixture.token
	wrong[0] ^= 1
	senderB := routeTestConnection("sender-b")
	if err := registry.Resume(resume, resumeAuth, senderB, wrong); !errors.Is(err, ErrResume) {
		t.Fatalf("wrong token error = %v", err)
	}
	if err := registry.Resume(resume, resumeAuth, senderB, fixture.token); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveSession(joined.RelaySessionID, senderB); !errors.Is(err, ErrOwner) {
		t.Fatalf("resumed sender inherited retired session authority: %v", err)
	}
	rejoined, err := registry.Join(fixture.init.ShareID, receiverA)
	if err != nil || rejoined.Status != JoinReady {
		t.Fatalf("resumed join = %+v, %v", rejoined, err)
	}
	if resolution, _ := registry.ResolveSession(rejoined.RelaySessionID, receiverA); resolution.Disposition != SessionForward ||
		resolution.Destination != senderB {
		t.Fatalf("resumed route retained old owner: %+v", resolution)
	}

	registry.UnexpectedDisconnect(fixture.init.ShareID, senderB)
	now = now.Add(SenderCrashGrace)
	expired, _ := registry.Join(fixture.init.ShareID, receiverA)
	if expired.Status != JoinNotFound {
		t.Fatalf("expired grace join = %+v", expired)
	}
	if err := registry.BeginRegistration(fixture.init, routeTestConnection("sender-c")); err != nil {
		t.Fatalf("crash expiry did not release admission: %v", err)
	}
}

func TestConnectionGenerationIsRouteAndSessionAuthority(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	registry := newRegistry(t, &now, &memoryTombstones{records: make(map[v2.ShareID]Tombstone)}, 2)
	fixture := makeFixture(t, 0x2a)
	ownerA, err := NewConnectionRef("reused-wire-id")
	if err != nil {
		t.Fatal(err)
	}
	ownerB, err := NewConnectionRef("reused-wire-id")
	if err != nil {
		t.Fatal(err)
	}
	if ownerA == ownerB || ownerA.ConnectionID() != ownerB.ConnectionID() ||
		ownerA.LocalGeneration() == ownerB.LocalGeneration() {
		t.Fatalf("connection generations are not exact: A=%+v B=%+v", ownerA, ownerB)
	}
	if err := registry.BeginRegistration(fixture.init, ownerA); err != nil {
		t.Fatal(err)
	}
	if registry.AbortRegistration(fixture.init.ShareID, ownerB) {
		t.Fatal("same-ID replacement aborted the old generation")
	}
	if err := registry.Publish(fixture.init.ShareID, ownerB, fixture.verified); !errors.Is(err, ErrOwner) {
		t.Fatalf("same-ID replacement publish error = %v", err)
	}
	if err := registry.Publish(fixture.init.ShareID, ownerA, fixture.verified); err != nil {
		t.Fatal(err)
	}
	receiver := routeTestConnection("generation-receiver")
	joined, err := registry.Join(fixture.init.ShareID, receiver)
	if err != nil || joined.Status != JoinReady || joined.Sender != ownerA {
		t.Fatalf("join = %+v, %v", joined, err)
	}
	if _, err := registry.ResolveSession(joined.RelaySessionID, ownerB); !errors.Is(err, ErrOwner) {
		t.Fatalf("same-ID replacement resolved old session: %v", err)
	}
	if _, ended := registry.EndSession(joined.RelaySessionID, ownerB); ended {
		t.Fatal("same-ID replacement ended old session")
	}
	if _, transitioned := registry.UnexpectedDisconnect(fixture.init.ShareID, ownerB); transitioned {
		t.Fatal("same-ID replacement disconnected old route")
	}
	retirement, transitioned := registry.UnexpectedDisconnect(fixture.init.ShareID, ownerA)
	if !transitioned || retirement.Owner != ownerA || len(retirement.Sessions) != 1 ||
		retirement.Sessions[0].Sender != ownerA || retirement.Sessions[0].Receiver != receiver {
		t.Fatalf("exact owner retirement = %+v, transitioned=%t", retirement, transitioned)
	}
	if resolution, resolveErr := registry.ResolveSession(joined.RelaySessionID, ownerA); resolveErr != nil ||
		resolution.Disposition != SessionRetired {
		t.Fatalf("old exact generation lost retirement authority: %+v, %v", resolution, resolveErr)
	}
	if _, resolveErr := registry.ResolveSession(joined.RelaySessionID, ownerB); !errors.Is(resolveErr, ErrOwner) {
		t.Fatalf("replacement inherited retirement authority: %v", resolveErr)
	}
}

func TestRetiredSessionAuthorityIsExactAndExpires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &memoryTombstones{records: make(map[v2.ShareID]Tombstone)}
	registry := newRegistry(t, &now, store, 4)
	fixture := makeFixture(t, 0x2f)
	sender := routeTestConnection("sender")
	receiver := routeTestConnection("receiver")
	if err := registry.BeginRegistration(fixture.init, sender); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(fixture.init.ShareID, sender, fixture.verified); err != nil {
		t.Fatal(err)
	}
	joined, err := registry.Join(fixture.init.ShareID, receiver)
	if err != nil || joined.Status != JoinReady {
		t.Fatalf("join = %+v, %v", joined, err)
	}
	if _, ended := registry.EndSession(joined.RelaySessionID, receiver); !ended {
		t.Fatal("receiver did not retire its exact session")
	}
	expiresAt := now.Add(SessionTombstoneTTL)
	for _, formerOwner := range []ConnectionRef{sender, receiver} {
		resolution, resolveErr := registry.ResolveSession(joined.RelaySessionID, formerOwner)
		if resolveErr != nil || resolution.Disposition != SessionRetired || resolution.Destination.Valid() {
			t.Fatalf("retired route for %q = %+v, %v", formerOwner.ConnectionID(), resolution, resolveErr)
		}
	}
	if _, err := registry.ResolveSession(joined.RelaySessionID, routeTestConnection("outsider")); !errors.Is(err, ErrOwner) {
		t.Fatalf("retired outsider error = %v", err)
	}
	unknown := joined.RelaySessionID
	unknown[0] ^= 0xff
	if _, err := registry.ResolveSession(unknown, sender); !errors.Is(err, ErrSession) {
		t.Fatalf("never-authorized session error = %v", err)
	}

	now = now.Add(time.Second)
	if _, ended := registry.EndSession(joined.RelaySessionID, sender); ended {
		t.Fatal("duplicate retirement changed lifecycle state")
	}
	registry.mu.Lock()
	retainedExpiry := registry.sessionTombstones[joined.RelaySessionID].expiresAt
	registry.mu.Unlock()
	if !retainedExpiry.Equal(expiresAt) {
		t.Fatalf("duplicate retirement extended expiry to %v, want %v", retainedExpiry, expiresAt)
	}

	now = expiresAt.Add(-time.Nanosecond)
	if resolution, err := registry.ResolveSession(joined.RelaySessionID, sender); err != nil ||
		resolution.Disposition != SessionRetired {
		t.Fatalf("pre-expiry retirement authority = %+v, %v", resolution, err)
	}
	now = now.Add(time.Nanosecond)
	if _, err := registry.ResolveSession(joined.RelaySessionID, sender); !errors.Is(err, ErrSession) {
		t.Fatalf("expired retirement authority error = %v", err)
	}
}

func TestExplicitStopIsDurablePermanentAndNeverGrace(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &memoryTombstones{records: make(map[v2.ShareID]Tombstone), failPut: true}
	registry := newRegistry(t, &now, store, 1)
	fixture := makeFixture(t, 0x30)
	sender := routeTestConnection("sender")
	receiver := routeTestConnection("receiver")
	if err := registry.BeginRegistration(fixture.init, sender); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(fixture.init.ShareID, sender, fixture.verified); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Stop(context.Background(), fixture.stop, fixture.stopAuth); err == nil {
		t.Fatal("stop acknowledged before durable tombstone")
	}
	if result, _ := registry.Join(fixture.init.ShareID, receiver); result.Status != JoinReady {
		t.Fatalf("failed persistence changed live route: %+v", result)
	}
	store.failPut = false
	if _, err := registry.Stop(context.Background(), fixture.stop, fixture.stopAuth); err != nil {
		t.Fatal(err)
	}
	stopped, err := registry.Join(fixture.init.ShareID, receiver)
	if err != nil || stopped.Status != JoinStopped || stopped.RetryAfter != 0 {
		t.Fatalf("stopped join = %+v, %v", stopped, err)
	}
	if _, err := registry.Stop(context.Background(), fixture.stop, fixture.stopAuth); err != nil || store.puts != 1 {
		t.Fatalf("idempotent stop = %v, puts=%d", err, store.puts)
	}
	different := fixture.stop
	different.StopID[0] ^= 1
	if _, err := registry.Stop(context.Background(), different, stopAuthority(t, fixture, different)); !errors.Is(err, ErrStopped) {
		t.Fatalf("different stop ID error = %v", err)
	}
	senderNew := routeTestConnection("sender-new")
	if err := registry.BeginRegistration(fixture.init, senderNew); !errors.Is(err, ErrStopped) {
		t.Fatalf("stopped registration error = %v", err)
	}
	resume := fixture.init
	resume.Mode = v2.RegistrationResume
	if err := registry.ValidateResumeCredential(resume, fixture.token); !errors.Is(err, ErrStopped) {
		t.Fatalf("stopped resume precheck error = %v", err)
	}
	if err := registry.Resume(resume, resumeAuthority(t, fixture, resume), senderNew, fixture.token); !errors.Is(err, ErrStopped) {
		t.Fatalf("stopped resume error = %v", err)
	}

	restored := newRegistry(t, &now, store, 1)
	if result, _ := restored.Join(fixture.init.ShareID, receiver); result.Status != JoinStopped {
		t.Fatalf("restored tombstone join = %+v", result)
	}
	other := makeFixture(t, 0x50)
	if err := restored.BeginRegistration(other.init, routeTestConnection("other")); !errors.Is(err, ErrAdmission) {
		t.Fatalf("permanent tombstone did not consume bounded slot: %v", err)
	}
}

func TestStartingDeadlineAndAtomicStopJoin(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &memoryTombstones{records: make(map[v2.ShareID]Tombstone)}
	registry := newRegistry(t, &now, store, 4)
	fixture := makeFixture(t, 0x60)
	sender := routeTestConnection("sender")
	if err := registry.BeginRegistration(fixture.init, sender); err != nil {
		t.Fatal(err)
	}
	now = now.Add(JoinStartingGrace)
	if result, _ := registry.Join(fixture.init.ShareID, routeTestConnection("receiver")); result.Status != JoinNotFound {
		t.Fatalf("expired starting route = %+v", result)
	}
	if err := registry.BeginRegistration(fixture.init, sender); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(fixture.init.ShareID, sender, fixture.verified); err != nil {
		t.Fatal(err)
	}

	const joiners = 32
	results := make(chan JoinStatus, joiners)
	var wait sync.WaitGroup
	for index := range joiners {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			result, err := registry.Join(fixture.init.ShareID, routeTestConnection(ConnectionID("receiver-"+string(rune('A'+index)))))
			if err == nil {
				results <- result.Status
			}
		}(index)
	}
	if _, err := registry.Stop(context.Background(), fixture.stop, fixture.stopAuth); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	close(results)
	for status := range results {
		if status != JoinReady && status != JoinStarting && status != JoinStopped {
			t.Fatalf("stop/join race exposed intermediate status %d", status)
		}
	}
	if result, _ := registry.Join(fixture.init.ShareID, routeTestConnection("final")); result.Status != JoinStopped {
		t.Fatalf("post-stop join = %+v", result)
	}
}

func TestRegistrationAbortAndOpaqueAuthorityBoundaries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &memoryTombstones{records: make(map[v2.ShareID]Tombstone)}
	registry := newRegistry(t, &now, store, 2)
	fixture := makeFixture(t, 0x70)
	sender := routeTestConnection("sender")
	outsider := routeTestConnection("outsider")
	if err := registry.BeginRegistration(fixture.init, sender); err != nil {
		t.Fatal(err)
	}
	if registry.AbortRegistration(fixture.init.ShareID, outsider) {
		t.Fatal("outsider aborted registration")
	}
	if !registry.AbortRegistration(fixture.init.ShareID, sender) ||
		registry.AbortRegistration(fixture.init.ShareID, sender) ||
		(*Registry)(nil).AbortRegistration(fixture.init.ShareID, sender) {
		t.Fatal("registration abort lifecycle is not owner-bound")
	}
	if err := registry.BeginRegistration(fixture.init, sender); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(fixture.init.ShareID, outsider, fixture.verified); !errors.Is(err, ErrOwner) {
		t.Fatalf("outsider publish error = %v", err)
	}
	if err := registry.Publish(fixture.init.ShareID, sender, v2.VerifiedDescriptor{}); !errors.Is(err, ErrOwner) {
		t.Fatalf("unverified descriptor publish error = %v", err)
	}
	other := makeFixture(t, 0x72)
	if err := registry.Publish(fixture.init.ShareID, sender, other.verified); !errors.Is(err, ErrOwner) {
		t.Fatalf("cross-registration descriptor error = %v", err)
	}
	if err := registry.Publish(fixture.init.ShareID, sender, fixture.verified); err != nil {
		t.Fatal(err)
	}
	replacement := routeTestConnection("replacement")
	if err := registry.BeginRegistration(fixture.init, replacement); !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("live replacement error = %v", err)
	}
	if err := registry.BeginRegistration(other.init, routeTestConnection("other")); err != nil {
		t.Fatal(err)
	}
	third := makeFixture(t, 0x74)
	thirdConnection := routeTestConnection("third")
	if err := registry.BeginRegistration(third.init, thirdConnection); !errors.Is(err, ErrAdmission) {
		t.Fatalf("route capacity error = %v", err)
	}
	resume := third.init
	resume.Mode = v2.RegistrationResume
	if err := registry.BeginRegistration(resume, thirdConnection); !errors.Is(err, ErrConfig) {
		t.Fatalf("resume used fresh registration path: %v", err)
	}
	if _, err := registry.Stop(context.Background(), fixture.stop, v2.StopAuthority{}); !errors.Is(err, ErrConfig) {
		t.Fatalf("zero stop authority error = %v", err)
	}

	// A valid incoming identity must not overwrite a corrupted existing route.
	registry.mu.Lock()
	registry.routes[fixture.init.ShareID].init.PKHash[0] ^= 1
	registry.mu.Unlock()
	if err := registry.BeginRegistration(fixture.init, replacement); !errors.Is(err, ErrCollision) {
		t.Fatalf("collision error = %v", err)
	}
}

func TestSessionEndBudgetsAndRandomIdentityFailures(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &memoryTombstones{records: make(map[v2.ShareID]Tombstone)}
	randomBytes := append(make([]byte, v2.RelaySessionIDBytes), testBytes(1, v2.RelaySessionIDBytes)...)
	randomBytes = append(randomBytes, testBytes(1, v2.RelaySessionIDBytes)...)
	randomBytes = append(randomBytes, testBytes(21, v2.RelaySessionIDBytes)...)
	registry, err := New(context.Background(), Config{
		MaxRoutes: 2, MaxSessions: 2, MaxSessionsPerShare: 1,
		Random: bytes.NewReader(randomBytes), Now: func() time.Time { return now }, Tombstones: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := makeFixture(t, 0x40)
	sender := routeTestConnection("sender")
	receiverA := routeTestConnection("receiver-a")
	receiverB := routeTestConnection("receiver-b")
	if err := registry.BeginRegistration(fixture.init, sender); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(fixture.init.ShareID, sender, fixture.verified); err != nil {
		t.Fatal(err)
	}
	first, err := registry.Join(fixture.init.ShareID, receiverA)
	if err != nil || first.Status != JoinReady {
		t.Fatalf("first join = %+v, %v", first, err)
	}
	if _, err := registry.Join(fixture.init.ShareID, receiverB); !errors.Is(err, ErrAdmission) {
		t.Fatalf("per-share session budget error = %v", err)
	}
	_, outsiderEnded := registry.EndSession(first.RelaySessionID, routeTestConnection("outsider"))
	_, senderEnded := registry.EndSession(first.RelaySessionID, sender)
	_, duplicateEnded := registry.EndSession(first.RelaySessionID, sender)
	_, nilEnded := (*Registry)(nil).EndSession(first.RelaySessionID, sender)
	if outsiderEnded || !senderEnded || duplicateEnded || nilEnded {
		t.Fatal("session ending is not role-bound and idempotent")
	}
	if _, err := registry.Join(fixture.init.ShareID, receiverB); !errors.Is(err, ErrAdmission) {
		t.Fatalf("ended-ID tombstone bypassed per-share budget: %v", err)
	}
	now = now.Add(SessionTombstoneTTL)
	second, err := registry.Join(fixture.init.ShareID, receiverB)
	if err != nil || second.Status != JoinReady {
		t.Fatalf("second join = %+v, %v", second, err)
	}

	broken, _ := New(context.Background(), Config{
		MaxRoutes: 1, MaxSessions: 1, MaxSessionsPerShare: 1,
		Random: bytes.NewReader([]byte{1}), Now: func() time.Time { return now }, Tombstones: store,
	})
	other := makeFixture(t, 0x50)
	if err := broken.BeginRegistration(other.init, sender); err != nil {
		t.Fatal(err)
	}
	if err := broken.Publish(other.init.ShareID, sender, other.verified); err != nil {
		t.Fatal(err)
	}
	if _, err := broken.Join(other.init.ShareID, routeTestConnection("receiver")); err == nil {
		t.Fatal("short relay-session random source was accepted")
	}
}

func TestRegistryRejectsInvalidPersistentAndBudgetConfiguration(t *testing.T) {
	now := time.Unix(1, 0)
	valid := makeFixture(t, 0x30)
	tombstone := Tombstone{
		ShareID: valid.stop.ShareID, ShareInstance: valid.stop.ShareInstance,
		PKHash: valid.stop.PKHash, StopID: valid.stop.StopID,
	}
	tests := []Config{
		{},
		{MaxRoutes: 1, MaxSessions: 1, MaxSessionsPerShare: 2, Random: bytes.NewReader(nil), Now: func() time.Time { return now }, Tombstones: &memoryTombstones{}},
		{MaxRoutes: 1, MaxSessions: 1, MaxSessionsPerShare: 1, Random: bytes.NewReader(nil), Now: func() time.Time { return now }},
	}
	for index, config := range tests {
		if _, err := New(context.Background(), config); !errors.Is(err, ErrConfig) {
			t.Fatalf("invalid config %d error = %v", index, err)
		}
	}
	loadFailure := errors.New("load failed")
	if _, err := New(context.Background(), Config{
		MaxRoutes: 1, MaxSessions: 1, MaxSessionsPerShare: 1, Random: bytes.NewReader(nil),
		Tombstones: &memoryTombstones{loadErr: loadFailure},
	}); !errors.Is(err, loadFailure) {
		t.Fatalf("load failure = %v", err)
	}
	if _, err := New(context.Background(), Config{
		MaxRoutes: 1, MaxSessions: 1, MaxSessionsPerShare: 1, Random: bytes.NewReader(nil),
		Tombstones: &memoryTombstones{load: []Tombstone{tombstone, tombstone}},
	}); !errors.Is(err, ErrAdmission) {
		t.Fatalf("loaded capacity error = %v", err)
	}
	invalid := tombstone
	invalid.ShareInstance = v2.ShareInstance{}
	if _, err := New(context.Background(), Config{
		MaxRoutes: 2, MaxSessions: 1, MaxSessionsPerShare: 1, Random: bytes.NewReader(nil),
		Tombstones: &memoryTombstones{load: []Tombstone{invalid}},
	}); !errors.Is(err, ErrConfig) {
		t.Fatalf("invalid tombstone error = %v", err)
	}
	if _, err := New(context.Background(), Config{
		MaxRoutes: 2, MaxSessions: 1, MaxSessionsPerShare: 1, Random: bytes.NewReader(nil),
		Tombstones: &memoryTombstones{load: []Tombstone{tombstone, tombstone}},
	}); !errors.Is(err, ErrConfig) {
		t.Fatalf("duplicate tombstone error = %v", err)
	}
}
