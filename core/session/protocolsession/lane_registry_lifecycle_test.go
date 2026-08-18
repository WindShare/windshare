package protocolsession

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func newLifecycleLaneRegistry(
	t *testing.T,
	now *time.Time,
) (*LaneRegistry, catalog.ShareInstance, ProtocolSessionID, TrafficKey, ed25519.PrivateKey) {
	t.Helper()
	share, _ := catalog.ShareInstanceFromBytes(laneSequence(1, 16))
	session, _ := ProtocolSessionIDFromBytes(laneSequence(21, 16))
	traffic, _ := TrafficKeyFromBytes(bytes.Repeat([]byte{0x31}, TrafficKeyBytes), DirectionReceiverToSender)
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x41}, ed25519.SeedSize))
	registry, err := NewLaneRegistry(LaneRegistryConfig{
		ShareInstance: share, ProtocolSessionID: session, ReceiverToSender: traffic,
		SenderSigningKey: privateKey, InitialLaneID: 7,
		MaxLogicalLanes: 2, MaxPendingGrants: 1, Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry, share, session, traffic, privateKey
}

func TestLaneRegistryExactRevocationReclaimsCanceledGrantLane(t *testing.T) {
	now := time.Unix(100, 0)
	registry, _, _, _, _ := newLifecycleLaneRegistry(t, &now)
	for iteration := range DefaultMaxPendingGrants * 2 {
		operationID := testOperationID(byte(iteration + 1))
		grant, err := registry.IssueGrant(0, operationID, laneSequence(byte(iteration+2), LaneAttachNonceBytes))
		if err != nil {
			t.Fatalf("grant %d: %v", iteration, err)
		}
		if !registry.RevokeGrant(grant) {
			t.Fatalf("grant %d was not revoked", iteration)
		}
	}
	if len(registry.lanes) != 1 || len(registry.grants) != 0 {
		t.Fatalf("revocation retained lanes=%d grants=%d", len(registry.lanes), len(registry.grants))
	}
	if _, err := registry.IssueGrant(0, testOperationID(170), laneSequence(171, LaneAttachNonceBytes)); err != nil {
		t.Fatalf("valid grant after cancellation storm: %v", err)
	}
}

func TestLaneRegistryExpiryCleanupReclaimsNeverAttachedLane(t *testing.T) {
	now := time.Unix(200, 0)
	registry, _, _, _, _ := newLifecycleLaneRegistry(t, &now)
	first, err := registry.IssueGrant(0, testOperationID(171), laneSequence(172, LaneAttachNonceBytes))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(LaneGrantTTL + LaneGrantTombstoneTTL)
	second, err := registry.IssueGrant(0, testOperationID(173), laneSequence(174, LaneAttachNonceBytes))
	if err != nil {
		t.Fatalf("grant after expiry cleanup: %v", err)
	}
	if _, retained := registry.lanes[first.LaneID]; retained && first.LaneID != second.LaneID {
		t.Fatalf("expired lane %d remained allocated", first.LaneID)
	}
	if len(registry.lanes) != 2 || len(registry.grants) != 1 {
		t.Fatalf("expiry cleanup lanes=%d grants=%d", len(registry.lanes), len(registry.grants))
	}
}

func TestLaneRegistryCloseClearsSensitiveAndAdmissionState(t *testing.T) {
	now := time.Unix(300, 0)
	registry, share, session, traffic, _ := newLifecycleLaneRegistry(t, &now)
	operationID := testOperationID(175)
	grant, _ := registry.IssueGrant(0, operationID, laneSequence(176, LaneAttachNonceBytes))
	hello, _ := NewLaneHello(share, session, grant.LaneID, grant.LaneEpoch, operationID, grant.AttachNonce[:], traffic)
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, _ = registry.AdmitCandidate(
			hello.Encoded(), laneSequence(177, LaneSenderNonceBytes), allowLaneSettlement,
		)
	}()
	go func() {
		defer wait.Done()
		<-start
		registry.Close()
	}()
	close(start)
	wait.Wait()
	registry.Close()
	if !registry.stopping || len(registry.grants) != 0 || len(registry.lanes) != 0 ||
		registry.activeCount != 0 || registry.senderSigningKey != nil || registry.receiverToSender.valid {
		t.Fatalf("closed registry retained stopping=%v grants=%d lanes=%d active=%d key=%d trafficValid=%v",
			registry.stopping, len(registry.grants), len(registry.lanes), registry.activeCount,
			len(registry.senderSigningKey), registry.receiverToSender.valid)
	}
	if _, err := registry.IssueGrant(0, testOperationID(178), laneSequence(179, LaneAttachNonceBytes)); !errors.Is(err, ErrLaneStopping) {
		t.Fatalf("post-close grant error=%v", err)
	}
}

func TestLaneRegistryCloseJoinsAuthenticatedSettlementBeforeSecretDestruction(t *testing.T) {
	now := time.Unix(400, 0)
	registry, share, session, traffic, privateKey := newLifecycleLaneRegistry(t, &now)
	operationID := testOperationID(180)
	grant, _ := registry.IssueGrant(0, operationID, laneSequence(181, LaneAttachNonceBytes))
	hello, _ := NewLaneHello(
		share, session, grant.LaneID, grant.LaneEpoch, operationID, grant.AttachNonce[:], traffic,
	)
	settlementEntered := make(chan struct{})
	releaseSettlement := make(chan struct{})
	admissionResult := make(chan struct {
		admission LaneAdmission
		err       error
	}, 1)
	go func() {
		admission, err := registry.AdmitCandidate(
			hello.Encoded(),
			laneSequence(182, LaneSenderNonceBytes),
			LaneAdmissionSettlementGateFunc(func(OperationID, LaneAdmissionIdentity) bool {
				close(settlementEntered)
				<-releaseSettlement
				return true
			}),
		)
		admissionResult <- struct {
			admission LaneAdmission
			err       error
		}{admission: admission, err: err}
	}()
	<-settlementEntered
	closeDone := make(chan struct{})
	go func() {
		registry.Close()
		close(closeDone)
	}()
	for {
		registry.mu.Lock()
		closing := registry.closing
		registry.mu.Unlock()
		if closing {
			break
		}
		runtime.Gosched()
	}
	select {
	case <-closeDone:
		t.Fatal("registry close destroyed authority before settlement joined")
	default:
	}
	close(releaseSettlement)
	settled := <-admissionResult
	if settled.err != nil || !settled.admission.SettlementBegan ||
		settled.admission.Disposition != LaneAdmissionRejected ||
		settled.admission.Rejection.Code != LaneRejectStopping {
		t.Fatalf("close-raced settlement = %+v, %v", settled.admission, settled.err)
	}
	if rejection, err := ParseLaneReject(
		settled.admission.Response, hello, privateKey.Public().(ed25519.PublicKey),
	); err != nil || rejection != settled.admission.Rejection {
		t.Fatalf("close-raced signed rejection = %+v, %v", rejection, err)
	}
	<-closeDone
	if registry.settlementCount != 0 || registry.settlementDone != nil || !registry.closing {
		t.Fatalf("closed settlement lifecycle count=%d done=%v closing=%v",
			registry.settlementCount, registry.settlementDone != nil, registry.closing)
	}
}
