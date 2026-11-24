package contentflow

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type fixedOutcomeOutbound struct {
	evidence protocolsession.SendEvidence
	err      error
}

type fragmentFailureOutbound struct {
	fragmentErr    error
	fragmentCalls  int
	operationError int
}

func (*fragmentFailureOutbound) SendControl(
	context.Context,
	protocolsession.Operation,
	protocolsession.MessageKind,
	[]byte,
) (protocolsession.SendEvidence, error) {
	return protocolsession.SendNotSent, protocolsession.MarkNotSent(errors.New("final control was not expected"))
}

func (outbound *fragmentFailureOutbound) SendFragment(
	context.Context,
	protocolsession.Operation,
	protocolsession.Message,
) error {
	outbound.fragmentCalls++
	return outbound.fragmentErr
}

func (outbound *fragmentFailureOutbound) SendOperationError(
	context.Context,
	protocolsession.Operation,
	OperationFailure,
) error {
	outbound.operationError++
	return nil
}

func (outbound fixedOutcomeOutbound) SendControl(
	context.Context,
	protocolsession.Operation,
	protocolsession.MessageKind,
	[]byte,
) (protocolsession.SendEvidence, error) {
	return outbound.evidence, outbound.err
}

func (fixedOutcomeOutbound) SendFragment(context.Context, protocolsession.Operation, protocolsession.Message) error {
	return nil
}

func (fixedOutcomeOutbound) SendOperationError(context.Context, protocolsession.Operation, OperationFailure) error {
	return nil
}

func TestOpenResultDeliverySettlesLeaseWithExactEvidence(t *testing.T) {
	base := newRuntimeFixture(t, 1)
	opened, err := base.service.Open(context.Background(), mustOpenRequest(t, base.file))
	if err != nil {
		t.Fatal(err)
	}
	lease := opened.Items()[0].Lease
	defer base.close(t)

	transportErr := errors.New("transport result is uncertain")
	tests := []struct {
		name             string
		evidence         protocolsession.SendEvidence
		sendErr          error
		wantEnd          content.LeaseEndKind
		wantProcessError error
	}{
		{name: "cancel before send", evidence: protocolsession.SendNotSent, sendErr: protocolsession.ErrOperationSuppressed, wantEnd: content.LeaseUndelivered, wantProcessError: protocolsession.ErrOperationSuppressed},
		{name: "send wins cancel", evidence: protocolsession.SendDelivered, sendErr: nil},
		{name: "writer transport failure", evidence: protocolsession.SendUncertain, sendErr: transportErr, wantEnd: content.LeaseDetached, wantProcessError: transportErr},
		{name: "caller timeout", evidence: protocolsession.SendUncertain, sendErr: context.DeadlineExceeded, wantEnd: content.LeaseDetached, wantProcessError: context.DeadlineExceeded},
		{name: "session close before send", evidence: protocolsession.SendNotSent, sendErr: protocolsession.MarkNotSent(protocolsession.ErrWriterStopped), wantEnd: content.LeaseUndelivered, wantProcessError: protocolsession.ErrWriterStopped},
		{name: "uncertain evidence ignores joined not-sent error", evidence: protocolsession.SendUncertain, sendErr: errors.Join(transportErr, protocolsession.MarkNotSent(protocolsession.ErrControlQueueFull)), wantEnd: content.LeaseDetached, wantProcessError: transportErr},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &stubRevisionStore{results: []content.OpenRevisionResult{{FileID: base.file, Lease: lease}}}
			service, cache := stubService(t, lease.Descriptor(), store, stubRecordEncoder{revision: []byte("revision")})
			defer cache.Close()
			handler, err := NewSenderHandler(SenderHandlerConfig{
				Service:  service,
				Outbound: fixedOutcomeOutbound{evidence: test.evidence, err: test.sendErr},
			})
			if err != nil {
				t.Fatal(err)
			}
			body, err := EncodeOpenRequest(mustOpenRequest(t, base.file))
			if err != nil {
				t.Fatal(err)
			}
			op := testActiveOperation(protocolsession.OperationID(219))
			processErr := handler.processOpen(context.Background(), op, body)
			if !errors.Is(processErr, test.wantProcessError) || (test.wantProcessError == nil && processErr != nil) {
				t.Fatalf("process error=%v want=%v", processErr, test.wantProcessError)
			}

			_, ownedErr := service.ownedDescriptor(lease.ID())
			if test.wantEnd != 0 && !errors.Is(ownedErr, ErrLeaseNotOwned) {
				t.Fatalf("settled result retained lease: %v", ownedErr)
			}
			if test.wantEnd == 0 && ownedErr != nil {
				t.Fatalf("delivered result ended lease: %v", ownedErr)
			}
			store.mu.Lock()
			endsBeforeClose := append([]recordedLeaseEnd(nil), store.leaseEnds...)
			store.mu.Unlock()
			if test.wantEnd == 0 && len(endsBeforeClose) != 0 ||
				test.wantEnd != 0 && (len(endsBeforeClose) != 1 || endsBeforeClose[0].kind != test.wantEnd) {
				t.Fatalf("lease endings before close=%+v want=%v", endsBeforeClose, test.wantEnd)
			}
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			endsAfterClose := append([]recordedLeaseEnd(nil), store.leaseEnds...)
			store.mu.Unlock()
			if len(endsAfterClose) != 1 {
				t.Fatalf("lease ending count after close=%d", len(endsAfterClose))
			}
			if test.wantEnd == 0 && endsAfterClose[0].kind != content.LeaseDetached {
				t.Fatalf("delivered lease closed as %v", endsAfterClose[0].kind)
			}
		})
	}
}

func TestFragmentOutboundFailureEmitsTheSingleLegalOperationFinal(t *testing.T) {
	fixture := newRuntimeFixture(t, 1)
	defer fixture.close(t)
	opened, err := fixture.service.Open(context.Background(), mustOpenRequest(t, fixture.file))
	if err != nil {
		t.Fatal(err)
	}
	lease := opened.Items()[0].Lease
	request, _ := NewBlockRequest(lease.ID(), []uint64{0})
	body, _ := EncodeBlockRequest(request)
	operationID := protocolsession.OperationID(218)
	message, _ := protocolsession.NewMessage(protocolsession.MessageRequestBlocks, &operationID, body)
	outbound := &fragmentFailureOutbound{fragmentErr: errors.New("fragment path failed")}
	handler, err := NewSenderHandler(SenderHandlerConfig{Service: fixture.service, Outbound: outbound})
	if err != nil {
		t.Fatal(err)
	}
	handler.process(context.Background(), testActiveOperation(operationID), message)
	if outbound.fragmentCalls != 1 || outbound.operationError != 1 {
		t.Fatalf("fragment calls=%d operation errors=%d", outbound.fragmentCalls, outbound.operationError)
	}
}

func TestRenewResultDeliveryDetachesEveryAmbiguousLease(t *testing.T) {
	base := newRuntimeFixture(t, 1)
	opened, err := base.service.Open(context.Background(), mustOpenRequest(t, base.file))
	if err != nil {
		t.Fatal(err)
	}
	lease := opened.Items()[0].Lease
	defer base.close(t)

	transportErr := errors.New("renew delivery is uncertain")
	tests := []struct {
		name         string
		sendErr      error
		wantDetached bool
	}{
		{name: "definitive drop", sendErr: protocolsession.ErrOperationSuppressed, wantDetached: true},
		{name: "delivered", sendErr: nil},
		{name: "transport uncertainty", sendErr: transportErr, wantDetached: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &stubRevisionStore{
				results: []content.OpenRevisionResult{{FileID: base.file, Lease: lease}},
				renew:   lease,
			}
			service, cache := stubService(t, lease.Descriptor(), store, stubRecordEncoder{revision: []byte("revision")})
			defer cache.Close()
			defer service.Close()
			if _, err := service.Open(context.Background(), mustOpenRequest(t, base.file)); err != nil {
				t.Fatal(err)
			}
			handler, err := NewSenderHandler(SenderHandlerConfig{
				Service: service, Outbound: fixedOutcomeOutbound{err: test.sendErr},
			})
			if err != nil {
				t.Fatal(err)
			}
			body, err := EncodeLeaseRequest(lease.ID())
			if err != nil {
				t.Fatal(err)
			}
			processErr := handler.processRenew(context.Background(), testActiveOperation(protocolsession.OperationID(220)), body)
			if !errors.Is(processErr, test.sendErr) || (test.sendErr == nil && processErr != nil) {
				t.Fatalf("renew process error=%v want=%v", processErr, test.sendErr)
			}
			_, ownedErr := service.ownedDescriptor(lease.ID())
			if test.wantDetached != errors.Is(ownedErr, ErrLeaseNotOwned) {
				t.Fatalf("renew ownership error=%v wantDetached=%v", ownedErr, test.wantDetached)
			}
			store.mu.Lock()
			ends := append([]recordedLeaseEnd(nil), store.leaseEnds...)
			store.mu.Unlock()
			if test.wantDetached && (len(ends) != 1 || ends[0].kind != content.LeaseDetached) ||
				!test.wantDetached && len(ends) != 0 {
				t.Fatalf("renew lease endings=%+v wantDetached=%v", ends, test.wantDetached)
			}
		})
	}
}

func TestRenewRetiresLeaseWhenStoreChangesItsIdentity(t *testing.T) {
	base := newRuntimeFixture(t, 1)
	defer base.close(t)
	firstOpen, err := base.service.Open(context.Background(), mustOpenRequest(t, base.file))
	if err != nil {
		t.Fatal(err)
	}
	secondOpen, err := base.service.Open(context.Background(), mustOpenRequest(t, base.file))
	if err != nil {
		t.Fatal(err)
	}
	first := firstOpen.Items()[0].Lease
	second := secondOpen.Items()[0].Lease
	store := &stubRevisionStore{
		results: []content.OpenRevisionResult{{FileID: base.file, Lease: first}},
		renew:   second,
	}
	service, cache := stubService(t, first.Descriptor(), store, stubRecordEncoder{revision: []byte("revision")})
	defer cache.Close()
	defer service.Close()
	if _, err := service.Open(context.Background(), mustOpenRequest(t, base.file)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Renew(first.ID()); !errors.Is(err, ErrRevisionStoreContract) {
		t.Fatalf("malformed renewal error=%v", err)
	}
	if _, err := service.ownedDescriptor(first.ID()); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("malformed renewal retained lease: %v", err)
	}
	store.mu.Lock()
	ends := append([]recordedLeaseEnd(nil), store.leaseEnds...)
	store.mu.Unlock()
	if len(ends) != 1 || ends[0].kind != content.LeaseDetached {
		t.Fatalf("malformed renewal lease endings=%+v", ends)
	}
}

func TestRenewFailureRetiresOnlyTerminalLeaseState(t *testing.T) {
	fixture := newRuntimeFixture(t, 1)
	defer fixture.close(t)
	opened, err := fixture.service.Open(context.Background(), mustOpenRequest(t, fixture.file))
	if err != nil {
		t.Fatal(err)
	}
	lease := opened.Items()[0].Lease
	fixture.clock.Advance(content.LeaseTTL)
	if _, err := fixture.service.Renew(lease.ID()); !errors.Is(err, content.ErrLeaseExpired) {
		t.Fatalf("expired renew error=%v", err)
	}
	if _, err := fixture.service.ownedDescriptor(lease.ID()); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("expired renewal retained session ownership: %v", err)
	}

	base := newRuntimeFixture(t, 1)
	defer base.close(t)
	baseOpen, err := base.service.Open(context.Background(), mustOpenRequest(t, base.file))
	if err != nil {
		t.Fatal(err)
	}
	driftLease := baseOpen.Items()[0].Lease
	store := &stubRevisionStore{
		results:  []content.OpenRevisionResult{{FileID: base.file, Lease: driftLease}},
		renewErr: content.ErrRevisionDrift,
	}
	service, cache := stubService(t, driftLease.Descriptor(), store, stubRecordEncoder{revision: []byte("revision")})
	defer cache.Close()
	defer service.Close()
	if _, err := service.Open(context.Background(), mustOpenRequest(t, base.file)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Renew(driftLease.ID()); !errors.Is(err, content.ErrRevisionDrift) {
		t.Fatalf("drift renew error=%v", err)
	}
	if _, err := service.ownedDescriptor(driftLease.ID()); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("drift renewal retained session ownership: %v", err)
	}
}

func TestSenderServiceUsesRelinquishedDetachedAndGlobalInvalidationBoundaries(t *testing.T) {
	base := newRuntimeFixture(t, 1)
	defer base.close(t)
	firstOpen, err := base.service.Open(context.Background(), mustOpenRequest(t, base.file))
	if err != nil {
		t.Fatal(err)
	}
	secondOpen, err := base.service.Open(context.Background(), mustOpenRequest(t, base.file))
	if err != nil {
		t.Fatal(err)
	}
	first := firstOpen.Items()[0].Lease
	second := secondOpen.Items()[0].Lease
	store := &stubRevisionStore{
		results: []content.OpenRevisionResult{{FileID: base.file, Lease: first}},
		renew:   first,
	}
	service, cache := stubService(t, first.Descriptor(), store, stubRecordEncoder{revision: []byte("revision")})
	defer cache.Close()
	if _, err := service.Open(context.Background(), mustOpenRequest(t, base.file)); err != nil {
		t.Fatal(err)
	}
	if err := service.Release(first.ID()); err != nil {
		t.Fatal(err)
	}

	store.results = []content.OpenRevisionResult{{FileID: base.file, Lease: second}}
	store.renew = second
	if _, err := service.Open(context.Background(), mustOpenRequest(t, base.file)); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	ends := append([]recordedLeaseEnd(nil), store.leaseEnds...)
	store.mu.Unlock()
	if len(ends) != 2 || ends[0].kind != content.LeaseRelinquished || ends[1].kind != content.LeaseDetached {
		t.Fatalf("service lease endings=%+v", ends)
	}

	invalidationStore := &stubRevisionStore{
		results:  []content.OpenRevisionResult{{FileID: base.file, Lease: first}},
		renewErr: content.ErrRevisionDrift,
	}
	invalidationService, invalidationCache := stubService(t, first.Descriptor(), invalidationStore, stubRecordEncoder{revision: []byte("revision")})
	defer invalidationCache.Close()
	defer invalidationService.Close()
	if _, err := invalidationService.Open(context.Background(), mustOpenRequest(t, base.file)); err != nil {
		t.Fatal(err)
	}
	if _, err := invalidationService.Renew(first.ID()); !errors.Is(err, content.ErrRevisionDrift) {
		t.Fatalf("revision invalidation error=%v", err)
	}
	invalidationStore.mu.Lock()
	ordinaryEnds := len(invalidationStore.leaseEnds)
	invalidationStore.mu.Unlock()
	if ordinaryEnds != 0 {
		t.Fatalf("tuple-global invalidation was rewritten as %d ordinary lease endings", ordinaryEnds)
	}
}

func TestOpenRejectsAReleasedLeaseIdentityReusedByTheStore(t *testing.T) {
	base := newRuntimeFixture(t, 1)
	defer base.close(t)
	opened, err := base.service.Open(context.Background(), mustOpenRequest(t, base.file))
	if err != nil {
		t.Fatal(err)
	}
	lease := opened.Items()[0].Lease
	store := &stubRevisionStore{results: []content.OpenRevisionResult{{FileID: base.file, Lease: lease}}}
	service, cache := stubService(t, lease.Descriptor(), store, stubRecordEncoder{revision: []byte("revision")})
	defer cache.Close()
	defer service.Close()
	if _, err := service.Open(context.Background(), mustOpenRequest(t, base.file)); err != nil {
		t.Fatal(err)
	}
	if err := service.Release(lease.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Open(context.Background(), mustOpenRequest(t, base.file)); !errors.Is(err, ErrRevisionStoreContract) {
		t.Fatalf("released lease identity reuse error=%v", err)
	}
	if _, err := service.ownedDescriptor(lease.ID()); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("reused released lease became active: %v", err)
	}
}

func TestReleaseReplayRemainsIdempotentAfterIdentityTombstoneRotation(t *testing.T) {
	descriptor := flowDescriptor(t, 1)
	store := &stubRevisionStore{}
	service, cache := stubService(t, descriptor, store, stubRecordEncoder{})
	defer cache.Close()
	defer service.Close()
	var oldest content.LeaseID
	for index := uint64(1); index <= ReleasedLeaseTombstoneLimit+1; index++ {
		var raw [content.IdentityBytes]byte
		binary.BigEndian.PutUint64(raw[len(raw)-8:], index)
		leaseID, err := content.LeaseIDFromBytes(raw[:])
		if err != nil {
			t.Fatal(err)
		}
		if index == 1 {
			oldest = leaseID
		}
		service.mu.Lock()
		service.rememberReleasedLocked(leaseID)
		service.mu.Unlock()
	}
	if err := service.Release(oldest); err != nil {
		t.Fatalf("rotated release replay was not idempotent: %v", err)
	}
	store.mu.Lock()
	ended := len(store.leaseEnds)
	store.mu.Unlock()
	if ended != 0 {
		t.Fatalf("unknown replay reached the share-scoped store: endings=%d", ended)
	}
}
