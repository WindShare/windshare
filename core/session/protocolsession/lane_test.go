package protocolsession

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func laneB64(t *testing.T, value string) []byte {
	t.Helper()
	result, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func laneSequence(first byte, length int) []byte {
	result := make([]byte, length)
	for index := range result {
		result[index] = first + byte(index)
	}
	return result
}

func TestLaneHelloAndAcceptMatchFrozenVector(t *testing.T) {
	share, _ := catalog.ShareInstanceFromBytes(laneB64(t, "QEFCQ0RFRkdISUpLTE1OTw=="))
	session, _ := ProtocolSessionIDFromBytes(laneB64(t, "bx6dUla4+OmakbGs5HxWMA=="))
	operation, _ := OperationIDFromBytes(laneB64(t, "0dLT1NXW19jZ2tvc3d7f4A=="))
	traffic, _ := TrafficKeyFromBytes(laneB64(t, "NkHzTW7/u+4V6KLR1bszmC3HFKREKe/0bLjVtMhrNPk="), DirectionReceiverToSender)
	hello, err := NewLaneHello(share, session, 0x05060708, 1, operation, laneB64(t, "YWJjZGVmZ2hpamtsbW5vcA=="), traffic)
	if err != nil {
		t.Fatal(err)
	}
	wantHello := laneB64(t, "V1MyQQJAQUJDREVGR0hJSktMTU5Pbx6dUla4+OmakbGs5HxWMAUGBwgAAAAB0dLT1NXW19jZ2tvc3d7f4GFiY2RlZmdoaWprbG1ub3CK5vKIALOUUri9tV3K20/JjkjmqjEFvayxXsEg8tM24w==")
	if !bytes.Equal(hello.Encoded(), wantHello) {
		t.Fatal("lane hello diverged from frozen vector")
	}
	routeShare, routeSession, err := UntrustedLaneHelloRoute(wantHello)
	if err != nil || routeShare != share || routeSession != session {
		t.Fatalf("untrusted route = %x/%x, %v", routeShare, routeSession, err)
	}
	forgedProof := bytes.Clone(wantHello)
	forgedProof[len(forgedProof)-1] ^= 1
	if routedShare, routedSession, err := UntrustedLaneHelloRoute(forgedProof); err != nil || routedShare != share || routedSession != session {
		t.Fatalf("untrusted selector attempted proof authority: %x/%x, %v", routedShare, routedSession, err)
	}
	zeroShare := bytes.Clone(wantHello)
	clear(zeroShare[5 : 5+catalog.IdentityBytes])
	if _, _, err := UntrustedLaneHelloRoute(zeroShare); !errors.Is(err, ErrLaneMalformed) {
		t.Fatalf("zero route identity error = %v", err)
	}
	parsed, err := ParseLaneHello(wantHello, traffic)
	if err != nil || parsed.LaneID() != 0x05060708 || parsed.LaneEpoch() != 1 || parsed.OperationID() != operation {
		t.Fatalf("parse lane hello = %+v, %v", parsed, err)
	}
	privateKey := ed25519.NewKeyFromSeed(laneSequence(0x20, ed25519.SeedSize))
	accept, err := NewLaneAccept(hello, laneSequence(0x81, LaneSenderNonceBytes), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	wantAccept := laneB64(t, "V1MyQgJ0h+5q1A0tcXynT84Il+UT8GYbLIQqVL9FQLRvsb0snIGCg4SFhoeIiYqLjI2Oj5B2MCOk1ovq9SZHx6VGzLovaRUq3bzz9bAMeP8OoApS/Ad+V/6Rq9WYTphtTJio0O3YJEKGy9HNNqP7uoetwjoD")
	if !bytes.Equal(accept, wantAccept) {
		t.Fatal("lane accept diverged from frozen vector")
	}
	nonce, err := ParseLaneAccept(accept, hello, privateKey.Public().(ed25519.PublicKey))
	if err != nil || !bytes.Equal(nonce, laneSequence(0x81, LaneSenderNonceBytes)) {
		t.Fatalf("parse lane accept: %v", err)
	}
	reject, err := NewLaneReject(hello, LaneRejection{Code: LaneRejectAdmissionLimited, RetryAfter: time.Second}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	wantReject := laneB64(t, "V1MyTgIFAAB0h+5q1A0tcXynT84Il+UT8GYbLIQqVL9FQLRvsb0snAAAA+jpb7fjwnH++p2jDca5KXyWzMcUfQQvLPukK7OUNYtsH+YEkgeUQRJGovGazCKa0gXlW3LhRGhtIxWnZxA3ZXgF")
	if !bytes.Equal(reject, wantReject) {
		t.Fatal("lane rejection diverged from frozen vector")
	}
}

func TestLaneRejectIsSenderAuthenticatedAndHelloBound(t *testing.T) {
	share, _ := catalog.ShareInstanceFromBytes(laneSequence(1, 16))
	session, _ := ProtocolSessionIDFromBytes(laneSequence(21, 16))
	operation, _ := OperationIDFromBytes(laneSequence(41, 16))
	traffic, _ := TrafficKeyFromBytes(bytes.Repeat([]byte{0x51}, 32), DirectionReceiverToSender)
	hello, _ := NewLaneHello(share, session, 9, 3, operation, laneSequence(61, 16), traffic)
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x71}, 32))
	encoded, err := NewLaneReject(hello, LaneRejection{Code: LaneRejectAdmissionLimited, RetryAfter: time.Second}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != LaneRejectBytes {
		t.Fatalf("reject length = %d", len(encoded))
	}
	got, err := ParseLaneReject(encoded, hello, privateKey.Public().(ed25519.PublicKey))
	if err != nil || got.Code != LaneRejectAdmissionLimited || got.RetryAfter != time.Second {
		t.Fatalf("reject = %+v, %v", got, err)
	}
	other, _ := NewLaneHello(share, session, 9, 4, operation, laneSequence(61, 16), traffic)
	if _, err := ParseLaneReject(encoded, other, privateKey.Public().(ed25519.PublicKey)); !errors.Is(err, ErrLaneSignature) {
		t.Fatalf("hello substitution error = %v", err)
	}
	tampered := bytes.Clone(encoded)
	tampered[len(tampered)-1] ^= 1
	if _, err := ParseLaneReject(tampered, hello, privateKey.Public().(ed25519.PublicKey)); !errors.Is(err, ErrLaneSignature) {
		t.Fatalf("signature substitution error = %v", err)
	}
	if _, err := NewLaneHello(share, session, 9, 3, operation, make([]byte, LaneAttachNonceBytes), traffic); !errors.Is(err, ErrLaneInput) {
		t.Fatalf("zero attach nonce error = %v", err)
	}
	if _, err := NewLaneAccept(hello, make([]byte, LaneSenderNonceBytes), privateKey); !errors.Is(err, ErrLaneInput) {
		t.Fatalf("zero sender nonce error = %v", err)
	}
}

func TestLaneRegistryOwnsGrantEpochAdmissionAndStop(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	share, _ := catalog.ShareInstanceFromBytes(laneSequence(1, 16))
	session, _ := ProtocolSessionIDFromBytes(laneSequence(21, 16))
	traffic, _ := TrafficKeyFromBytes(bytes.Repeat([]byte{0x31}, 32), DirectionReceiverToSender)
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x41}, 32))
	registry, err := NewLaneRegistry(LaneRegistryConfig{
		ShareInstance: share, ProtocolSessionID: session, ReceiverToSender: traffic, SenderSigningKey: privateKey,
		InitialLaneID: 7, MaxLogicalLanes: 2, MaxPendingGrants: 4, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, _ := OperationIDFromBytes(laneSequence(61, 16))
	grant, err := registry.IssueGrant(0, operation, bytes.Repeat([]byte{0x51}, 16))
	if err != nil || grant.LaneID == 0 || grant.LaneEpoch != 1 {
		t.Fatalf("grant = %+v, %v", grant, err)
	}
	hello, _ := NewLaneHello(share, session, grant.LaneID, grant.LaneEpoch, operation, grant.AttachNonce[:], traffic)
	accepted, err := registry.AdmitCandidate(hello.Encoded(), bytes.Repeat([]byte{0x61}, 16))
	if err != nil || accepted.Disposition != LaneAdmissionAccepted {
		t.Fatalf("admission = %+v, %v", accepted, err)
	}
	if _, err := ParseLaneAccept(accepted.Response, hello, privateKey.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	duplicate, err := registry.AdmitCandidate(hello.Encoded(), bytes.Repeat([]byte{0x61}, 16))
	if err != nil || duplicate.Disposition != LaneAdmissionRejected || duplicate.Rejection != LaneRejectGrantConsumed {
		t.Fatalf("duplicate = %+v, %v", duplicate, err)
	}
	if rejection, err := ParseLaneReject(duplicate.Response, hello, privateKey.Public().(ed25519.PublicKey)); err != nil || rejection.Code != LaneRejectGrantConsumed {
		t.Fatalf("duplicate rejection = %+v, %v", rejection, err)
	}

	malformed := hello.Encoded()
	malformed[len(malformed)-1] ^= 1
	silent, err := registry.AdmitCandidate(malformed, bytes.Repeat([]byte{0x61}, 16))
	if !errors.Is(err, ErrLaneProof) || silent.Disposition != LaneAdmissionSilentClose || len(silent.Response) != 0 {
		t.Fatalf("unauthenticated disposition = %+v, %v", silent, err)
	}

	if !registry.Release(grant.LaneID, grant.LaneEpoch) || registry.Release(grant.LaneID, grant.LaneEpoch) {
		t.Fatal("lane release was not epoch-owned and idempotent")
	}
	operation2, _ := OperationIDFromBytes(laneSequence(81, 16))
	grant2, err := registry.IssueGrant(grant.LaneID, operation2, bytes.Repeat([]byte{0x71}, 16))
	if err != nil || grant2.LaneEpoch != 2 {
		t.Fatalf("reattach grant = %+v, %v", grant2, err)
	}
	registry.Stop()
	hello2, _ := NewLaneHello(share, session, grant2.LaneID, grant2.LaneEpoch, operation2, grant2.AttachNonce[:], traffic)
	stopped, err := registry.AdmitCandidate(hello2.Encoded(), bytes.Repeat([]byte{0x61}, 16))
	if err != nil || stopped.Rejection != LaneRejectStopping {
		t.Fatalf("stopping admission = %+v, %v", stopped, err)
	}
	if _, err := registry.IssueGrant(grant.LaneID, operation2, grant2.AttachNonce[:]); !errors.Is(err, ErrLaneStopping) {
		t.Fatalf("stopping grant error = %v", err)
	}
}

func TestLaneRegistryExpiresGrantsAndRejectsWrongSessionAfterProof(t *testing.T) {
	now := time.Unix(10, 0)
	share, _ := catalog.ShareInstanceFromBytes(laneSequence(1, 16))
	session, _ := ProtocolSessionIDFromBytes(laneSequence(21, 16))
	otherSession, _ := ProtocolSessionIDFromBytes(laneSequence(22, 16))
	traffic, _ := TrafficKeyFromBytes(bytes.Repeat([]byte{0x31}, 32), DirectionReceiverToSender)
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x41}, 32))
	registry, _ := NewLaneRegistry(LaneRegistryConfig{ShareInstance: share, ProtocolSessionID: session, ReceiverToSender: traffic, SenderSigningKey: privateKey, InitialLaneID: 7, Now: func() time.Time { return now }})
	operation, _ := OperationIDFromBytes(laneSequence(61, 16))
	grant, _ := registry.IssueGrant(0, operation, bytes.Repeat([]byte{0x51}, 16))
	now = now.Add(LaneGrantTTL)
	hello, _ := NewLaneHello(share, session, grant.LaneID, grant.LaneEpoch, operation, grant.AttachNonce[:], traffic)
	expired, err := registry.AdmitCandidate(hello.Encoded(), bytes.Repeat([]byte{0x61}, 16))
	if err != nil || expired.Rejection != LaneRejectGrantExpired {
		t.Fatalf("expired admission = %+v, %v", expired, err)
	}
	wrongSession, _ := NewLaneHello(share, otherSession, grant.LaneID, grant.LaneEpoch, operation, grant.AttachNonce[:], traffic)
	wrong, err := registry.AdmitCandidate(wrongSession.Encoded(), bytes.Repeat([]byte{0x61}, 16))
	if err != nil || wrong.Rejection != LaneRejectUnknownSession {
		t.Fatalf("wrong-session admission = %+v, %v", wrong, err)
	}
}

func TestLaneParsersRejectEveryHostileWireAxis(t *testing.T) {
	share, _ := catalog.ShareInstanceFromBytes(laneSequence(1, 16))
	session, _ := ProtocolSessionIDFromBytes(laneSequence(21, 16))
	operation, _ := OperationIDFromBytes(laneSequence(41, 16))
	traffic, _ := TrafficKeyFromBytes(bytes.Repeat([]byte{0x51}, 32), DirectionReceiverToSender)
	hello, _ := NewLaneHello(share, session, 9, 3, operation, laneSequence(61, 16), traffic)
	if hello.ShareInstance() != share || hello.ProtocolSessionID() != session ||
		!bytes.Equal(hello.AttachNonce(), laneSequence(61, 16)) {
		t.Fatal("lane hello getters lost authenticated identity")
	}
	for name, mutate := range map[string]func([]byte){
		"magic":   func(value []byte) { value[0] ^= 1 },
		"version": func(value []byte) { value[4]++ },
		"share":   func(value []byte) { clear(value[5 : 5+catalog.IdentityBytes]); refreshLaneProof(value, traffic) },
		"session": func(value []byte) { clear(value[21:37]); refreshLaneProof(value, traffic) },
		"lane":    func(value []byte) { clear(value[37:41]); refreshLaneProof(value, traffic) },
		"epoch":   func(value []byte) { clear(value[41:45]); refreshLaneProof(value, traffic) },
		"operation": func(value []byte) {
			clear(value[45:61])
			refreshLaneProof(value, traffic)
		},
	} {
		t.Run("hello-"+name, func(t *testing.T) {
			hostile := hello.Encoded()
			mutate(hostile)
			if _, err := ParseLaneHello(hostile, traffic); !errors.Is(err, ErrLaneMalformed) {
				t.Fatalf("hostile hello error = %v", err)
			}
		})
	}
	if _, err := ParseLaneHello(hello.Encoded()[:LaneHelloBytes-1], traffic); !errors.Is(err, ErrLaneMalformed) {
		t.Fatalf("short hello error = %v", err)
	}
	wrongDirection, _ := TrafficKeyFromBytes(traffic.Bytes(), DirectionSenderToReceiver)
	if _, err := ParseLaneHello(hello.Encoded(), wrongDirection); !errors.Is(err, ErrLaneMalformed) {
		t.Fatalf("wrong traffic direction error = %v", err)
	}

	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x71}, 32))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	accept, _ := NewLaneAccept(hello, laneSequence(81, LaneSenderNonceBytes), privateKey)
	for name, mutate := range map[string]func([]byte){
		"magic":      func(value []byte) { value[0] ^= 1 },
		"version":    func(value []byte) { value[4]++ },
		"hello-hash": func(value []byte) { value[5] ^= 1 },
		"signature":  func(value []byte) { value[len(value)-1] ^= 1 },
	} {
		t.Run("accept-"+name, func(t *testing.T) {
			hostile := bytes.Clone(accept)
			mutate(hostile)
			if _, err := ParseLaneAccept(hostile, hello, publicKey); err == nil {
				t.Fatal("hostile lane accept was accepted")
			}
		})
	}
	if _, err := ParseLaneAccept(accept, LaneHello{}, publicKey); !errors.Is(err, ErrLaneMalformed) {
		t.Fatalf("unbound accept error = %v", err)
	}

	reject, _ := NewLaneReject(hello, LaneRejection{Code: LaneRejectAdmissionLimited, RetryAfter: time.Second}, privateKey)
	for name, mutate := range map[string]func([]byte){
		"code":       func(value []byte) { value[5] = 99 },
		"reserved":   func(value []byte) { value[6] = 1 },
		"hello-hash": func(value []byte) { value[8] ^= 1 },
		"retry":      func(value []byte) { value[5] = byte(LaneRejectStopping); value[43] = 1 },
		"signature":  func(value []byte) { value[len(value)-1] ^= 1 },
	} {
		t.Run("reject-"+name, func(t *testing.T) {
			hostile := bytes.Clone(reject)
			mutate(hostile)
			if _, err := ParseLaneReject(hostile, hello, publicKey); err == nil {
				t.Fatal("hostile lane rejection was accepted")
			}
		})
	}
	if _, err := NewLaneReject(hello, LaneRejection{Code: 99}, privateKey); !errors.Is(err, ErrLaneInput) {
		t.Fatalf("unknown rejection code error = %v", err)
	}
	if _, err := NewLaneReject(hello, LaneRejection{Code: LaneRejectStopping, RetryAfter: time.Second}, privateKey); !errors.Is(err, ErrLaneInput) {
		t.Fatalf("illegal retry error = %v", err)
	}
}

func refreshLaneProof(encoded []byte, traffic TrafficKey) {
	proof := laneProof(traffic.value[:], encoded[:LaneHelloBodyBytes])
	copy(encoded[LaneHelloBodyBytes:], proof)
}

func TestLaneRegistryRejectsBudgetEpochAndGrantSubstitution(t *testing.T) {
	now := time.Unix(100, 0)
	share, _ := catalog.ShareInstanceFromBytes(laneSequence(1, 16))
	session, _ := ProtocolSessionIDFromBytes(laneSequence(21, 16))
	traffic, _ := TrafficKeyFromBytes(bytes.Repeat([]byte{0x31}, 32), DirectionReceiverToSender)
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x41}, 32))
	base := LaneRegistryConfig{
		ShareInstance: share, ProtocolSessionID: session, ReceiverToSender: traffic,
		SenderSigningKey: privateKey, InitialLaneID: 7, Now: func() time.Time { return now },
	}
	if _, err := NewLaneRegistry(LaneRegistryConfig{}); !errors.Is(err, ErrLaneInput) {
		t.Fatalf("zero registry config error = %v", err)
	}
	invalid := base
	invalid.MaxLogicalLanes = -1
	if _, err := NewLaneRegistry(invalid); !errors.Is(err, ErrLaneInput) {
		t.Fatalf("negative logical budget error = %v", err)
	}
	registry, _ := NewLaneRegistry(base)
	operation, _ := OperationIDFromBytes(laneSequence(61, 16))
	if _, err := registry.IssueGrant(99, operation, laneSequence(81, 16)); !errors.Is(err, ErrLaneUnknown) {
		t.Fatalf("unknown logical lane error = %v", err)
	}
	limited := base
	limited.MaxLogicalLanes = 1
	limitedRegistry, _ := NewLaneRegistry(limited)
	if _, err := limitedRegistry.IssueGrant(0, operation, laneSequence(81, 16)); !errors.Is(err, ErrLaneGrantBudget) {
		t.Fatalf("logical lane budget error = %v", err)
	}
	pending := base
	pending.MaxPendingGrants = 1
	pendingRegistry, _ := NewLaneRegistry(pending)
	if _, err := pendingRegistry.IssueGrant(7, operation, laneSequence(81, 16)); err != nil {
		t.Fatal(err)
	}
	operation2, _ := OperationIDFromBytes(laneSequence(91, 16))
	if _, err := pendingRegistry.IssueGrant(7, operation2, laneSequence(101, 16)); !errors.Is(err, ErrLaneGrantBudget) {
		t.Fatalf("pending grant budget error = %v", err)
	}
	registry.nextEpoch = ^uint32(0)
	if _, err := registry.IssueGrant(7, operation, laneSequence(81, 16)); !errors.Is(err, ErrLaneEpoch) {
		t.Fatalf("epoch exhaustion error = %v", err)
	}

	staleRegistry, _ := NewLaneRegistry(base)
	first, _ := staleRegistry.IssueGrant(7, operation, laneSequence(81, 16))
	second, _ := staleRegistry.IssueGrant(7, operation2, laneSequence(101, 16))
	if !staleRegistry.Release(7, 0) {
		t.Fatal("initial lane release failed")
	}
	secondHello, _ := NewLaneHello(share, session, 7, second.LaneEpoch, operation2, second.AttachNonce[:], traffic)
	if result, err := staleRegistry.AdmitCandidate(secondHello.Encoded(), laneSequence(111, 16)); err != nil || result.Disposition != LaneAdmissionAccepted {
		t.Fatalf("newer epoch admission = %+v, %v", result, err)
	}
	staleRegistry.Release(7, second.LaneEpoch)
	firstHello, _ := NewLaneHello(share, session, 7, first.LaneEpoch, operation, first.AttachNonce[:], traffic)
	if result, err := staleRegistry.AdmitCandidate(firstHello.Encoded(), laneSequence(121, 16)); err != nil || result.Rejection != LaneRejectStaleEpoch {
		t.Fatalf("stale epoch admission = %+v, %v", result, err)
	}

	mismatchRegistry, _ := NewLaneRegistry(base)
	grant, _ := mismatchRegistry.IssueGrant(7, operation, laneSequence(81, 16))
	mismatchNonce := grant.AttachNonce
	mismatchNonce[0] ^= 1
	mismatchHello, _ := NewLaneHello(share, session, 7, grant.LaneEpoch, operation, mismatchNonce[:], traffic)
	if result, err := mismatchRegistry.AdmitCandidate(mismatchHello.Encoded(), laneSequence(111, 16)); err != nil || result.Rejection != LaneRejectGrantMismatch {
		t.Fatalf("grant mismatch admission = %+v, %v", result, err)
	}
	if (*LaneRegistry)(nil).Release(7, 0) {
		t.Fatal("nil registry released a lane")
	}
	if _, err := (*LaneRegistry)(nil).IssueGrant(7, operation, laneSequence(81, 16)); !errors.Is(err, ErrLaneInput) {
		t.Fatalf("nil registry grant error = %v", err)
	}
	(*LaneRegistry)(nil).Stop()
}
