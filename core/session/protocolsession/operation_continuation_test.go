package protocolsession

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

var errTestContinuationBinding = errors.New("test continuation changed binding")

type testContinuationClassifier struct{ maximum int }

func (classifier testContinuationClassifier) ClassifyUnboundOperationContinuation(
	kind MessageKind,
	canonicalBody []byte,
) (OperationContinuationScope, bool, error) {
	if kind != MessagePeerCandidate {
		return OperationContinuationScope{}, false, nil
	}
	binding, _, err := decodeTestContinuationBody(canonicalBody, true)
	if err != nil {
		return OperationContinuationScope{}, true, err
	}
	return testContinuationScope(binding), true, nil
}

func (classifier testContinuationClassifier) BeginOperationContinuation(
	requestKind MessageKind,
	canonicalRequestBody []byte,
) (OperationContinuationAuthority, bool, error) {
	if requestKind != MessagePeerOffer {
		return nil, false, nil
	}
	binding, _, err := decodeTestContinuationBody(canonicalRequestBody, false)
	if err != nil {
		return nil, true, err
	}
	return testContinuationAuthority{binding: binding, maximum: classifier.maximum}, true, nil
}

type testContinuationAuthority struct {
	binding byte
	maximum int
}

func (authority testContinuationAuthority) ClassifyOperationContinuation(
	kind MessageKind,
	canonicalBody []byte,
) ([sha256.Size]byte, bool, error) {
	if kind != MessagePeerCandidate {
		return [sha256.Size]byte{}, false, nil
	}
	binding, _, err := decodeTestContinuationBody(canonicalBody, true)
	if err != nil {
		return [sha256.Size]byte{}, true, err
	}
	if binding != authority.binding {
		return [sha256.Size]byte{}, true, errTestContinuationBinding
	}
	return sha256.Sum256(canonicalBody), true, nil
}

func (authority testContinuationAuthority) MaximumContinuations() int { return authority.maximum }

func (authority testContinuationAuthority) OperationContinuationScope() OperationContinuationScope {
	return testContinuationScope(authority.binding)
}

func testContinuationScope(binding byte) OperationContinuationScope {
	return OperationContinuationScope(sha256.Sum256([]byte{'t', binding}))
}

func decodeTestContinuationBody(body []byte, candidate bool) (byte, string, error) {
	var fields map[uint64]any
	if err := messageDecMode.Unmarshal(body, &fields); err != nil {
		return 0, "", err
	}
	want := 2
	if candidate {
		want = 3
	}
	version, versionOK := fields[0].(uint64)
	binding, bindingOK := fields[1].([]byte)
	if len(fields) != want || !versionOK || version != 1 || !bindingOK || len(binding) != 1 || binding[0] == 0 {
		return 0, "", ErrInvalidMessage
	}
	if !candidate {
		return binding[0], "", nil
	}
	value, valueOK := fields[2].(string)
	if !valueOK || value == "" {
		return 0, "", ErrInvalidMessage
	}
	return binding[0], value, nil
}

func testContinuationOffer(t *testing.T, operationID OperationID, binding byte) Message {
	t.Helper()
	body, err := EncodeBody(map[uint64]any{0: uint64(1), 1: []byte{binding}})
	if err != nil {
		t.Fatal(err)
	}
	message, err := NewMessage(MessagePeerOffer, &operationID, body)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func testContinuationCandidate(
	t *testing.T,
	operationID OperationID,
	binding byte,
	value string,
) Message {
	t.Helper()
	body, err := EncodeBody(map[uint64]any{0: uint64(1), 1: []byte{binding}, 2: value})
	if err != nil {
		t.Fatal(err)
	}
	message, err := NewMessage(MessagePeerCandidate, &operationID, body)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func testSignedContinuationCandidate(
	t *testing.T,
	operationID OperationID,
	binding byte,
	value string,
) Message {
	t.Helper()
	semantic := testContinuationCandidate(t, operationID, binding, value).Body()
	_, _, envelopeBinding := loadEnvelopeVector(t, "sender-signed-operation-error")
	prepared, err := PrepareSenderControl(
		vectorSenderSigningKey(),
		ControlBinding{
			ShareInstance: envelopeBinding.ShareInstance, ProtocolSessionID: envelopeBinding.ProtocolSessionID,
			LaneID: envelopeBinding.LaneID, LaneEpoch: envelopeBinding.LaneEpoch,
			Direction: DirectionSenderToReceiver,
		},
		MessagePeerCandidate,
		&operationID,
		semantic,
	)
	if err != nil {
		t.Fatal(err)
	}
	return prepared.intent
}

func newTestContinuationTable(
	t *testing.T,
	now func() time.Time,
	maximum int,
) *OperationTable {
	t.Helper()
	table, err := NewOperationTableWithContinuations(
		OperationLimits{MaxActive: 8, MaxTombstones: 8},
		now,
		testContinuationClassifier{maximum: maximum},
	)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func TestRoleRouterCoalescesConcurrentExactCandidateBeforeControlCapacity(t *testing.T) {
	operationID := testOperationID(221)
	table := newTestContinuationTable(t, nil, 64)
	router, err := NewRoleRouterWithLimits(RoleSender, table, RouterLimits{ControlFrames: 2, DataFrames: 1})
	if err != nil {
		t.Fatal(err)
	}
	if disposition, err := router.RouteInbound(
		context.Background(), testContinuationOffer(t, operationID, 7),
	); err != nil || disposition != OperationDeliver {
		t.Fatalf("offer admission = %d, %v", disposition, err)
	}

	candidate := testContinuationCandidate(t, operationID, 7, "same")
	const replays = RouterControlQueueLimit * 2
	start := make(chan struct{})
	results := make(chan struct {
		disposition OperationDisposition
		err         error
	}, replays)
	var work sync.WaitGroup
	work.Add(replays)
	for range replays {
		go func() {
			defer work.Done()
			<-start
			disposition, err := router.RouteInbound(context.Background(), candidate)
			results <- struct {
				disposition OperationDisposition
				err         error
			}{disposition: disposition, err: err}
		}()
	}
	close(start)
	work.Wait()
	close(results)
	delivered := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("exact replay escaped pre-router authority: %v", result.err)
		}
		if result.disposition == OperationDeliver {
			delivered++
		} else if result.disposition != OperationDrop {
			t.Fatalf("exact replay disposition = %d", result.disposition)
		}
	}
	if delivered != 1 {
		t.Fatalf("semantic candidate publications = %d, want 1", delivered)
	}
	if got := len(router.control); got != 2 {
		t.Fatalf("control backlog = %d, want offer + one candidate", got)
	}
}

func TestRoleRouterRollsBackCandidateWhenControlPublicationFails(t *testing.T) {
	operationID := testOperationID(222)
	table := newTestContinuationTable(t, nil, 64)
	router, err := NewRoleRouterWithLimits(RoleSender, table, RouterLimits{ControlFrames: 2, DataFrames: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []Message{
		testContinuationOffer(t, operationID, 8),
		testContinuationCandidate(t, operationID, 8, "first"),
	} {
		if disposition, err := router.RouteInbound(context.Background(), message); err != nil || disposition != OperationDeliver {
			t.Fatalf("fill control backlog = %d, %v", disposition, err)
		}
	}
	retry := testContinuationCandidate(t, operationID, 8, "retry")
	if disposition, err := router.RouteInbound(context.Background(), retry); disposition != OperationDrop ||
		!errors.Is(err, ErrRouterControlFull) {
		t.Fatalf("full admission = %d, %v", disposition, err)
	}
	if _, err := router.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if disposition, err := router.RouteInbound(context.Background(), retry); err != nil || disposition != OperationDeliver {
		t.Fatalf("retry after rollback = %d, %v", disposition, err)
	}
}

func TestOperationContinuationTombstoneDropsBindingCompatibleCrossLaneRace(t *testing.T) {
	operationID := testOperationID(223)
	table := newTestContinuationTable(t, nil, 4)
	offer := testContinuationOffer(t, operationID, 9)
	admission, err := table.ObserveInbound(DirectionReceiverToSender, offer)
	if err != nil {
		t.Fatal(err)
	}
	exact := testContinuationCandidate(t, operationID, 9, "known")
	candidateAdmission, err := table.ObserveInbound(DirectionReceiverToSender, exact)
	if err != nil || candidateAdmission.continuation == nil {
		t.Fatalf("candidate reservation = %+v, %v", candidateAdmission, err)
	}
	candidateAdmission.continuation.commit()
	outboundExact := testSignedContinuationCandidate(t, operationID, 9, "known-outbound")
	outboundAdmission, err := table.AdmitOutbound(
		DirectionSenderToReceiver, outboundExact, admission.Outbound,
	)
	if err != nil || outboundAdmission.Disposition != OperationDeliver || outboundAdmission.continuation == nil {
		t.Fatalf("outbound candidate reservation = %+v, %v", outboundAdmission, err)
	}
	outboundAdmission.continuation.commit()
	outboundAdmission.pin.release()
	if err := table.CancelGeneration(admission.Generation); err != nil {
		t.Fatal(err)
	}
	if disposition, err := table.Observe(DirectionReceiverToSender, exact); err != nil || disposition != OperationDrop {
		t.Fatalf("late exact = %d, %v", disposition, err)
	}
	distinct := testContinuationCandidate(t, operationID, 9, "not-admitted")
	if disposition, err := table.Observe(DirectionReceiverToSender, distinct); disposition != OperationDrop || err != nil {
		t.Fatalf("late distinct = %d, %v", disposition, err)
	}
	outboundDistinct := testSignedContinuationCandidate(t, operationID, 9, "late-outbound")
	if disposition, err := table.Observe(DirectionSenderToReceiver, outboundDistinct); disposition != OperationDrop || err != nil {
		t.Fatalf("late outbound distinct = %d, %v", disposition, err)
	}
	wrongBinding := testContinuationCandidate(t, operationID, 10, "known")
	if disposition, err := table.Observe(DirectionReceiverToSender, wrongBinding); disposition != OperationDrop ||
		!errors.Is(err, errTestContinuationBinding) {
		t.Fatalf("late binding conflict = %d, %v", disposition, err)
	}
}

func TestOperationContinuationRetireRollbackAndSameIDRolloverAreABAContained(t *testing.T) {
	for _, settle := range []struct {
		name string
		run  func(*operationContinuationReservation)
	}{
		{name: "rollback", run: func(reservation *operationContinuationReservation) { reservation.rollback() }},
		{name: "commit", run: func(reservation *operationContinuationReservation) { reservation.commit() }},
	} {
		t.Run(settle.name, func(t *testing.T) {
			now := time.Unix(1_700_000_000, 0)
			table := newTestContinuationTable(t, func() time.Time { return now }, 4)
			operationID := testOperationID(224)
			offer := testContinuationOffer(t, operationID, 11)
			first, err := table.ObserveInbound(DirectionReceiverToSender, offer)
			if err != nil {
				t.Fatal(err)
			}
			candidate := testContinuationCandidate(t, operationID, 11, "pending-a")
			pending, err := table.ObserveInbound(DirectionReceiverToSender, candidate)
			if err != nil || pending.continuation == nil {
				t.Fatalf("reserve generation A: %+v, %v", pending, err)
			}
			if err := table.CancelGeneration(first.Generation); err != nil {
				t.Fatal(err)
			}
			if disposition, err := table.Observe(DirectionReceiverToSender, candidate); err != nil ||
				disposition != OperationDrop {
				t.Fatalf("pending exact during retirement = %d, %v", disposition, err)
			}

			now = now.Add(OperationTombstoneLifetime + time.Nanosecond)
			second, err := table.ObserveInbound(DirectionReceiverToSender, offer)
			if err != nil || !second.Generation.IsActive() || second.Generation.Same(first.Generation) {
				t.Fatalf("generation B admission = %+v, %v", second, err)
			}
			settle.run(pending.continuation)
			secondCandidate, err := table.ObserveInbound(DirectionReceiverToSender, candidate)
			if err != nil || secondCandidate.Disposition != OperationDeliver || secondCandidate.continuation == nil {
				t.Fatalf("generation B candidate = %+v, %v", secondCandidate, err)
			}
			secondCandidate.continuation.commit()
		})
	}
}

func TestOperationContinuationPreemptiveCancelLearnsRacedBindingWithoutQueueState(t *testing.T) {
	operationID := testOperationID(228)
	table := newTestContinuationTable(t, nil, 4)
	cancelBody, _ := EncodeBody(map[uint64]any{0: uint64(1)})
	cancelMessage, _ := NewMessage(MessageCancel, &operationID, cancelBody)
	if disposition, err := table.Observe(DirectionReceiverToSender, cancelMessage); err != nil ||
		disposition != OperationDeliver {
		t.Fatalf("preemptive cancel = %d, %v", disposition, err)
	}
	beforeOffer := testContinuationCandidate(t, operationID, 16, "crossed-before-offer")
	if disposition, err := table.Observe(DirectionReceiverToSender, beforeOffer); err != nil ||
		disposition != OperationDrop {
		t.Fatalf("candidate crossed cancel = %d, %v", disposition, err)
	}
	if disposition, err := table.Observe(
		DirectionReceiverToSender,
		testContinuationOffer(t, operationID, 16),
	); err != nil || disposition != OperationDrop {
		t.Fatalf("offer crossed cancel = %d, %v", disposition, err)
	}
	afterOffer := testContinuationCandidate(t, operationID, 16, "crossed-after-offer")
	if disposition, err := table.Observe(DirectionReceiverToSender, afterOffer); err != nil ||
		disposition != OperationDrop {
		t.Fatalf("late compatible candidate = %d, %v", disposition, err)
	}
	wrong := testContinuationCandidate(t, operationID, 17, "wrong-binding")
	if disposition, err := table.Observe(DirectionReceiverToSender, wrong); disposition != OperationDrop ||
		!errors.Is(err, errTestContinuationBinding) {
		t.Fatalf("late wrong binding = %d, %v", disposition, err)
	}

	conflictingID := testOperationID(229)
	conflictingCancel, _ := NewMessage(MessageCancel, &conflictingID, cancelBody)
	_, _ = table.Observe(DirectionReceiverToSender, conflictingCancel)
	_, _ = table.Observe(
		DirectionReceiverToSender,
		testContinuationCandidate(t, conflictingID, 18, "first-scope"),
	)
	if disposition, err := table.Observe(
		DirectionReceiverToSender,
		testContinuationOffer(t, conflictingID, 19),
	); disposition != OperationDrop || !errors.Is(err, ErrConflictingContinuation) {
		t.Fatalf("raced offer binding conflict = %d, %v", disposition, err)
	}
}

func TestOperationContinuationReplayPinFailureDoesNotLeakReservation(t *testing.T) {
	operationID := testOperationID(225)
	table := newTestContinuationTable(t, nil, 4)
	offerAdmission, err := table.ObserveInbound(
		DirectionReceiverToSender,
		testContinuationOffer(t, operationID, 12),
	)
	if err != nil {
		t.Fatal(err)
	}
	candidate := testContinuationCandidate(t, operationID, 12, "pin-budget")
	authority := offerAdmission.Generation.authority
	permit := OutboundReplayPermit{
		table: table, authority: authority, direction: DirectionReceiverToSender,
		kind: MessagePeerCandidate, operationID: operationID,
		fingerprint: candidate.operationFingerprint(DirectionReceiverToSender),
	}
	table.mu.Lock()
	authority.pins = MaximumOperationPins
	table.mu.Unlock()
	if _, err := table.AcceptOutboundReplay(DirectionReceiverToSender, candidate, permit); !errors.Is(err, ErrOperationPinBudget) {
		t.Fatalf("pin budget error = %v", err)
	}
	table.mu.Lock()
	authority.pins = 0
	table.mu.Unlock()
	admission, err := table.ObserveInbound(DirectionReceiverToSender, candidate)
	if err != nil || admission.Disposition != OperationDeliver || admission.continuation == nil {
		t.Fatalf("candidate after failed replay = %+v, %v", admission, err)
	}
	admission.continuation.rollback()
}

func TestOperationContinuationClassifierFailuresAreSessionFatalInputs(t *testing.T) {
	table := newTestContinuationTable(t, nil, 4)
	operationID := testOperationID(226)
	malformedBody, _ := EncodeBody(map[uint64]any{0: uint64(2), 1: []byte{byte(1)}})
	malformedOffer, _ := NewMessage(MessagePeerOffer, &operationID, malformedBody)
	if disposition, err := table.Observe(DirectionReceiverToSender, malformedOffer); disposition != OperationDrop || err == nil {
		t.Fatalf("malformed offer = %d, %v", disposition, err)
	}

	validOffer := testContinuationOffer(t, operationID, 13)
	if disposition, err := table.Observe(DirectionReceiverToSender, validOffer); err != nil || disposition != OperationDeliver {
		t.Fatalf("valid offer after rejected malformed input = %d, %v", disposition, err)
	}
	wrong := testContinuationCandidate(t, operationID, 14, "wrong-binding")
	if disposition, err := table.Observe(DirectionReceiverToSender, wrong); disposition != OperationDrop ||
		!errors.Is(err, errTestContinuationBinding) {
		t.Fatalf("binding conflict = %d, %v", disposition, err)
	}
}

func TestOperationContinuationOverflowIsOneBoundedLeafOutcome(t *testing.T) {
	operationID := testOperationID(227)
	table := newTestContinuationTable(t, nil, 2)
	if disposition, err := table.Observe(
		DirectionReceiverToSender,
		testContinuationOffer(t, operationID, 15),
	); err != nil || disposition != OperationDeliver {
		t.Fatal(fmt.Errorf("offer = %d: %w", disposition, err))
	}
	for index, want := range []OperationDisposition{OperationDeliver, OperationDeliver, OperationDeliver, OperationDrop} {
		candidate := testContinuationCandidate(t, operationID, 15, fmt.Sprintf("candidate-%d", index))
		disposition, err := table.Observe(DirectionReceiverToSender, candidate)
		if err != nil || disposition != want {
			t.Fatalf("candidate %d = %d, %v; want %d", index, disposition, err, want)
		}
	}
	table.mu.Lock()
	active := table.active[operationID]
	direction := active.authority.continuations.directions[DirectionReceiverToSender]
	records, overflow := len(direction.records), direction.overflow != nil
	table.mu.Unlock()
	if records != 2 || !overflow {
		t.Fatalf("bounded replay state = %d records, overflow=%v", records, overflow)
	}
}

func TestOperationContinuationPendingAdmissionRejectsUntilOwnerSettles(t *testing.T) {
	operationID := testOperationID(230)
	table := newTestContinuationTable(t, nil, 2)
	if disposition, err := table.Observe(
		DirectionReceiverToSender,
		testContinuationOffer(t, operationID, 20),
	); err != nil || disposition != OperationDeliver {
		t.Fatalf("offer = %d, %v", disposition, err)
	}

	candidate := testContinuationCandidate(t, operationID, 20, "pending")
	owner, err := table.ObserveInbound(DirectionReceiverToSender, candidate)
	if err != nil || owner.Disposition != OperationDeliver || owner.continuation == nil {
		t.Fatalf("owner reservation = %+v, %v", owner, err)
	}
	duplicate, err := table.ObserveInbound(DirectionReceiverToSender, candidate)
	if duplicate.Disposition != OperationDrop || !errors.Is(err, ErrContinuationPending) ||
		duplicate.continuation != nil {
		t.Fatalf("pending duplicate = %+v, %v", duplicate, err)
	}

	owner.continuation.rollback()
	retry, err := table.ObserveInbound(DirectionReceiverToSender, candidate)
	if err != nil || retry.Disposition != OperationDeliver || retry.continuation == nil {
		t.Fatalf("retry after rollback = %+v, %v", retry, err)
	}
	retry.continuation.commit()

	// Observe commits synchronously; a following distinct candidate proves that
	// the public convenience path does not strand the direction's pending gate.
	if disposition, err := table.Observe(
		DirectionReceiverToSender,
		testContinuationCandidate(t, operationID, 20, "after-commit"),
	); err != nil || disposition != OperationDeliver {
		t.Fatalf("distinct after public commit = %d, %v", disposition, err)
	}
}

func TestOperationContinuationPendingBaseSlotCannotCreatePrematureOverflow(t *testing.T) {
	operationID := testOperationID(231)
	table := newTestContinuationTable(t, nil, 2)
	if disposition, err := table.Observe(
		DirectionReceiverToSender,
		testContinuationOffer(t, operationID, 21),
	); err != nil || disposition != OperationDeliver {
		t.Fatalf("offer = %d, %v", disposition, err)
	}
	if disposition, err := table.Observe(
		DirectionReceiverToSender,
		testContinuationCandidate(t, operationID, 21, "committed-base"),
	); err != nil || disposition != OperationDeliver {
		t.Fatalf("first base = %d, %v", disposition, err)
	}

	pendingBase, err := table.ObserveInbound(
		DirectionReceiverToSender,
		testContinuationCandidate(t, operationID, 21, "pending-base"),
	)
	if err != nil || pendingBase.Disposition != OperationDeliver || pendingBase.continuation == nil {
		t.Fatalf("pending base = %+v, %v", pendingBase, err)
	}
	nextBase := testContinuationCandidate(t, operationID, 21, "replacement-base")
	blocked, err := table.ObserveInbound(DirectionReceiverToSender, nextBase)
	if blocked.Disposition != OperationDrop || !errors.Is(err, ErrContinuationPending) ||
		blocked.continuation != nil {
		t.Fatalf("candidate behind pending base = %+v, %v", blocked, err)
	}

	pendingBase.continuation.rollback()
	replacement, err := table.ObserveInbound(DirectionReceiverToSender, nextBase)
	if err != nil || replacement.Disposition != OperationDeliver || replacement.continuation == nil ||
		replacement.continuation.overflow {
		t.Fatalf("replacement base = %+v, %v", replacement, err)
	}
	replacement.continuation.commit()
	overflow, err := table.ObserveInbound(
		DirectionReceiverToSender,
		testContinuationCandidate(t, operationID, 21, "real-overflow"),
	)
	if err != nil || overflow.Disposition != OperationDeliver || overflow.continuation == nil ||
		!overflow.continuation.overflow {
		t.Fatalf("first real overflow = %+v, %v", overflow, err)
	}
	overflow.continuation.commit()
	if disposition, err := table.Observe(
		DirectionReceiverToSender,
		testContinuationCandidate(t, operationID, 21, "coalesced-overflow"),
	); err != nil || disposition != OperationDrop {
		t.Fatalf("later overflow = %d, %v", disposition, err)
	}
}

type blockedContinuationSealer struct {
	entered   chan struct{}
	release   chan struct{}
	nextErr   error
	sealErr   error
	sealCalls int
	sequence  uint64
}

type postSealBarrierSealer struct {
	delegate  OutboundEnvelopeSealer
	entered   chan struct{}
	release   chan struct{}
	returnErr error
	once      sync.Once
}

func (sealer *postSealBarrierSealer) NextSequence() (uint64, error) {
	return sealer.delegate.NextSequence()
}

func (sealer *postSealBarrierSealer) Seal(plaintext []byte) (SealedEnvelope, error) {
	sealed, err := sealer.delegate.Seal(plaintext)
	if err != nil {
		return SealedEnvelope{}, err
	}
	blocked := false
	sealer.once.Do(func() {
		blocked = true
		close(sealer.entered)
	})
	if blocked {
		<-sealer.release
	}
	if sealer.returnErr != nil {
		return SealedEnvelope{}, sealer.returnErr
	}
	return sealed, nil
}

func newContinuationEnvelopePair(t *testing.T) (*EnvelopeSealer, *EnvelopeOpener) {
	t.Helper()
	_, vectorKey, binding := loadEnvelopeVector(t, "sender-signed-operation-error")
	rawKey := vectorKey.Bytes()
	vectorKey.Destroy()
	defer clear(rawKey)
	binding.Direction = DirectionReceiverToSender
	sealerKey, err := TrafficKeyFromBytes(rawKey, binding.Direction)
	if err != nil {
		t.Fatal(err)
	}
	openerKey, err := TrafficKeyFromBytes(rawKey, binding.Direction)
	if err != nil {
		t.Fatal(err)
	}
	defer sealerKey.Destroy()
	defer openerKey.Destroy()
	nonces := make([]byte, 2*EnvelopeNonceBytes)
	for index := range nonces {
		nonces[index] = byte(index + 1)
	}
	sealer, err := NewEnvelopeSealer(sealerKey, binding, bytes.NewReader(nonces))
	if err != nil {
		t.Fatal(err)
	}
	opener, err := NewEnvelopeOpener(openerKey, binding)
	if err != nil {
		t.Fatal(err)
	}
	return sealer, opener
}

func (sealer *blockedContinuationSealer) NextSequence() (uint64, error) {
	if sealer.entered != nil {
		close(sealer.entered)
		<-sealer.release
	}
	return sealer.sequence, sealer.nextErr
}

func (sealer *blockedContinuationSealer) Seal(plaintext []byte) (SealedEnvelope, error) {
	sealer.sealCalls++
	if sealer.sealErr != nil {
		return SealedEnvelope{}, sealer.sealErr
	}
	return SealedEnvelope{Sequence: sealer.sequence, Frame: append([]byte(nil), plaintext...)}, nil
}

func queuedContinuationWriter(
	t *testing.T,
	channel FrameChannel,
	sealer OutboundEnvelopeSealer,
) (*OperationTable, *SessionWriter, SendReceipt, OperationID, OutboundOperationPermit) {
	t.Helper()
	operationID := testOperationID(230)
	table := newTestContinuationTable(t, nil, 4)
	offerAdmission, err := table.AdmitOutbound(
		DirectionReceiverToSender,
		testContinuationOffer(t, operationID, 21),
		OutboundOperationPermit{},
	)
	if err != nil || offerAdmission.Disposition != OperationDeliver || offerAdmission.Operation.IsZero() {
		t.Fatalf("offer admission = %+v, %v", offerAdmission, err)
	}
	offerAdmission.pin.release()
	router, err := NewRoleRouter(RoleReceiver, table)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewSessionWriter(channel, sealer, router)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := writer.TryAuthorizedControl(
		testContinuationCandidate(t, operationID, 21, "reserved"),
		offerAdmission.Operation,
	)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Admitted() {
		t.Fatal("prequeue continuation reservation reported physical admission")
	}
	return table, writer, receipt, operationID, offerAdmission.Operation
}

func continuationRecordCount(
	table *OperationTable,
	operationID OperationID,
	direction Direction,
) int {
	table.mu.Lock()
	defer table.mu.Unlock()
	active := table.active[operationID]
	if active.authority == nil || active.authority.continuations == nil {
		return 0
	}
	current := active.authority.continuations.directions[direction]
	if current == nil {
		return 0
	}
	count := len(current.records)
	if current.overflow != nil {
		count++
	}
	return count
}

func TestContinuationReservationTracksIrreversibleSequenceOwnership(t *testing.T) {
	t.Run("unstarted caller cancellation", func(t *testing.T) {
		channel := newRuntimeChannel(0)
		table, writer, receipt, operationID, _ := queuedContinuationWriter(
			t, channel, &passthroughSealer{},
		)
		waitContext, cancelWait := context.WithCancel(context.Background())
		cancelWait()
		completion := receipt.Await(waitContext)
		if !completion.Settled || completion.Admitted || completion.Outcome != SendOutcomeDropped ||
			!errors.Is(completion.Err, context.Canceled) {
			t.Fatalf("unstarted cancellation = %+v", completion)
		}
		if count := continuationRecordCount(table, operationID, DirectionReceiverToSender); count != 0 {
			t.Fatalf("unstarted cancellation retained %d continuation records", count)
		}
		closed, closeWriter := context.WithCancel(context.Background())
		closeWriter()
		if err := writer.Run(closed); !errors.Is(err, context.Canceled) {
			t.Fatalf("writer stop = %v", err)
		}
	})

	t.Run("cancel after claim and before materialization", func(t *testing.T) {
		channel := newRuntimeChannel(0)
		sealer := &blockedContinuationSealer{entered: make(chan struct{}), release: make(chan struct{})}
		table, writer, receipt, operationID, _ := queuedContinuationWriter(t, channel, sealer)
		runContext, stop := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- writer.Run(runContext) }()
		<-sealer.entered
		waitContext, cancelWait := context.WithCancel(context.Background())
		cancelWait()
		completion := receipt.Await(waitContext)
		if !completion.Settled || completion.Admitted || completion.Outcome != SendOutcomeDropped ||
			!errors.Is(completion.Err, context.Canceled) {
			t.Fatalf("claimed reservation cancellation = %+v", completion)
		}
		if count := continuationRecordCount(table, operationID, DirectionReceiverToSender); count != 0 {
			t.Fatalf("claimed cancellation retained %d continuation records", count)
		}
		close(sealer.release)
		stop()
		if err := <-runDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("writer stop = %v", err)
		}
		if sealer.sealCalls != 0 {
			t.Fatalf("lost reservation reached Seal %d times", sealer.sealCalls)
		}
		channel.mu.Lock()
		sent := len(channel.sent)
		channel.mu.Unlock()
		if sent != 0 {
			t.Fatalf("lost reservation reached Send %d times", sent)
		}
	})

	t.Run("cancel during Seal cannot retract consumed sequence", func(t *testing.T) {
		channel := newRuntimeChannel(0)
		delegate, opener := newContinuationEnvelopePair(t)
		sealer := &postSealBarrierSealer{
			delegate: delegate, entered: make(chan struct{}), release: make(chan struct{}),
		}
		table, writer, receipt, operationID, authority := queuedContinuationWriter(t, channel, sealer)
		runContext, stop := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- writer.Run(runContext) }()
		released := false
		joined := false
		defer func() {
			if !released {
				close(sealer.release)
			}
			stop()
			if !joined {
				<-runDone
			}
		}()
		<-sealer.entered

		waitContext, cancelWait := context.WithCancel(context.Background())
		cancelWait()
		unsettled := receipt.Await(waitContext)
		if unsettled.Settled || unsettled.Admitted || unsettled.Outcome != SendOutcomeUnknown ||
			unsettled.Generation.IsZero() || unsettled.Operation.IsZero() || !unsettled.Replay.IsZero() ||
			!unsettled.Generation.Same(unsettled.Operation.Generation()) ||
			!errors.Is(unsettled.Err, context.Canceled) {
			t.Fatalf("cancellation during Seal = %+v", unsettled)
		}
		if receipt.Admitted() {
			t.Fatal("in-flight Seal committed continuation before success")
		}
		table.mu.Lock()
		active := table.active[operationID]
		direction := active.authority.continuations.directions[DirectionReceiverToSender]
		pins := active.authority.pins
		pending := direction != nil && direction.pending != nil
		pendingCommitted := pending && direction.pending.committed
		table.mu.Unlock()
		if pins != 1 || !pending || pendingCommitted {
			t.Fatalf("sealing authority: pins=%d pending=%v committed=%v", pins, pending, pendingCommitted)
		}

		second := testContinuationCandidate(t, operationID, 21, "behind-seal")
		if _, err := writer.TryAuthorizedControl(second, authority); !errors.Is(err, ErrContinuationPending) {
			t.Fatalf("continuation behind in-flight Seal error = %v", err)
		}
		close(sealer.release)
		released = true
		if outcome, err := receipt.Wait(context.Background()); err != nil || outcome != SendOutcomeDelivered {
			t.Fatalf("sealed continuation completion = %d, %v", outcome, err)
		}
		secondReceipt, err := writer.TryAuthorizedControl(second, authority)
		if err != nil {
			t.Fatal(err)
		}
		if outcome, err := secondReceipt.Wait(context.Background()); err != nil || outcome != SendOutcomeDelivered {
			t.Fatalf("following continuation completion = %d, %v", outcome, err)
		}
		stop()
		if err := <-runDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("writer stop = %v", err)
		}
		joined = true

		channel.mu.Lock()
		frames := make([][]byte, len(channel.sent))
		for index := range channel.sent {
			frames[index] = append([]byte(nil), channel.sent[index]...)
		}
		channel.mu.Unlock()
		if len(frames) != 2 {
			t.Fatalf("sent frames = %d, want 2", len(frames))
		}
		for index, frame := range frames {
			opened, err := opener.Open(frame)
			if err != nil {
				t.Fatalf("open frame %d: %v", index, err)
			}
			if opened.Sequence != uint64(index) {
				t.Fatalf("frame %d sequence = %d", index, opened.Sequence)
			}
		}
	})

	t.Run("Seal error after cancellation rolls back and stops writer", func(t *testing.T) {
		sealErr := errors.New("blocked Seal failed after consuming sequence")
		delegate := &passthroughSealer{}
		sealer := &postSealBarrierSealer{
			delegate: delegate, entered: make(chan struct{}), release: make(chan struct{}),
			returnErr: sealErr,
		}
		channel := newRuntimeChannel(0)
		table, writer, receipt, operationID, _ := queuedContinuationWriter(t, channel, sealer)
		runDone := make(chan error, 1)
		go func() { runDone <- writer.Run(context.Background()) }()
		released := false
		joined := false
		defer func() {
			if !released {
				close(sealer.release)
			}
			if !joined {
				<-runDone
			}
		}()
		<-sealer.entered
		waitContext, cancelWait := context.WithCancel(context.Background())
		cancelWait()
		if unsettled := receipt.Await(waitContext); unsettled.Settled || unsettled.Admitted ||
			unsettled.Outcome != SendOutcomeUnknown || !errors.Is(unsettled.Err, context.Canceled) {
			t.Fatalf("cancellation before Seal error = %+v", unsettled)
		}
		close(sealer.release)
		released = true
		if err := <-runDone; !errors.Is(err, sealErr) {
			t.Fatalf("writer Seal error = %v", err)
		}
		joined = true
		completion := receipt.Await(context.Background())
		if !completion.Settled || completion.Admitted || completion.Outcome != SendOutcomeDropped ||
			!completion.RetryableAcrossLane || !errors.Is(completion.Err, sealErr) {
			t.Fatalf("settled Seal failure = %+v", completion)
		}
		if count := continuationRecordCount(table, operationID, DirectionReceiverToSender); count != 0 {
			t.Fatalf("Seal failure retained %d continuation records", count)
		}
		channel.mu.Lock()
		sent := len(channel.sent)
		channel.mu.Unlock()
		if delegate.next != 1 || sent != 0 || writer.Accepting() {
			t.Fatalf("failed Seal state: next=%d sent=%d accepting=%v", delegate.next, sent, writer.Accepting())
		}
	})

	t.Run("lost post-Seal authority fails writer closed", func(t *testing.T) {
		delegate := &passthroughSealer{}
		sealer := &postSealBarrierSealer{
			delegate: delegate, entered: make(chan struct{}), release: make(chan struct{}),
		}
		channel := newRuntimeChannel(0)
		table, writer, receipt, operationID, authority := queuedContinuationWriter(t, channel, sealer)
		runDone := make(chan error, 1)
		go func() { runDone <- writer.Run(context.Background()) }()
		released := false
		joined := false
		defer func() {
			if !released {
				close(sealer.release)
			}
			if !joined {
				<-runDone
			}
		}()
		<-sealer.entered
		receipt.result.mu.Lock()
		phase := receipt.result.phase
		if phase != deliveryReservedSealing {
			receipt.result.mu.Unlock()
			t.Fatalf("phase at post-Seal barrier = %d", phase)
		}
		receipt.result.phase = deliveryReservedClaimed
		receipt.result.mu.Unlock()
		close(sealer.release)
		released = true
		if err := <-runDone; !errors.Is(err, ErrSealingReservation) {
			t.Fatalf("writer invariant error = %v", err)
		}
		joined = true
		completion := receipt.Await(context.Background())
		if !completion.Settled || completion.Admitted || completion.Outcome != SendOutcomeDropped ||
			!completion.RetryableAcrossLane || !errors.Is(completion.Err, ErrSealingReservation) {
			t.Fatalf("invariant completion = %+v", completion)
		}
		if count := continuationRecordCount(table, operationID, DirectionReceiverToSender); count != 0 {
			t.Fatalf("invariant failure retained %d continuation records", count)
		}
		if _, err := writer.TryAuthorizedControl(
			testContinuationCandidate(t, operationID, 21, "after-invariant"), authority,
		); !errors.Is(err, ErrWriterStopped) {
			t.Fatalf("writer accepted after invariant failure: %v", err)
		}
		channel.mu.Lock()
		sent := len(channel.sent)
		channel.mu.Unlock()
		if delegate.next != 1 || sent != 0 {
			t.Fatalf("invariant failure state: next=%d sent=%d", delegate.next, sent)
		}
	})

	for _, failure := range []struct {
		name     string
		nextErr  error
		sealErr  error
		wantSeal int
	}{
		{name: "next sequence failure", nextErr: errors.New("next failed")},
		{name: "seal failure", sealErr: errors.New("seal failed"), wantSeal: 1},
	} {
		t.Run(failure.name, func(t *testing.T) {
			sealer := &blockedContinuationSealer{nextErr: failure.nextErr, sealErr: failure.sealErr}
			table, writer, receipt, operationID, _ := queuedContinuationWriter(t, newRuntimeChannel(0), sealer)
			wantErr := failure.nextErr
			if wantErr == nil {
				wantErr = failure.sealErr
			}
			if err := writer.Run(context.Background()); !errors.Is(err, wantErr) {
				t.Fatalf("writer failure = %v", err)
			}
			completion := receipt.Await(context.Background())
			if !completion.Settled || completion.Admitted || completion.Outcome != SendOutcomeDropped ||
				!completion.RetryableAcrossLane || !errors.Is(completion.Err, wantErr) {
				t.Fatalf("pretransport failure = %+v", completion)
			}
			if count := continuationRecordCount(table, operationID, DirectionReceiverToSender); count != 0 {
				t.Fatalf("pretransport failure retained %d continuation records", count)
			}
			if sealer.sealCalls != failure.wantSeal {
				t.Fatalf("Seal calls = %d, want %d", sealer.sealCalls, failure.wantSeal)
			}
		})
	}

	t.Run("sealed commitment immediately precedes transport", func(t *testing.T) {
		channel := newGatedWriterChannel()
		table, writer, receipt, operationID, _ := queuedContinuationWriter(
			t, channel, &passthroughSealer{},
		)
		runContext, stop := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- writer.Run(runContext) }()
		<-channel.firstStarted
		waitContext, cancelWait := context.WithCancel(context.Background())
		cancelWait()
		unsettled := receipt.Await(waitContext)
		if unsettled.Settled || !unsettled.Admitted || unsettled.Outcome != SendOutcomeUnknown ||
			!errors.Is(unsettled.Err, context.Canceled) {
			t.Fatalf("post-commit cancellation = %+v", unsettled)
		}
		if count := continuationRecordCount(table, operationID, DirectionReceiverToSender); count != 1 {
			t.Fatalf("committed reservation records = %d, want 1", count)
		}
		close(channel.releaseFirst)
		if outcome, err := receipt.Wait(context.Background()); err != nil || outcome != SendOutcomeDelivered {
			t.Fatalf("physical completion = %d, %v", outcome, err)
		}
		stop()
		if err := <-runDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("writer stop = %v", err)
		}
	})
}
