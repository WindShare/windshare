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
	outcome protocolsession.SendOutcome
	err     error
}

type fragmentFailureOutbound struct {
	fragmentErr    error
	fragmentCalls  int
	operationError int
}

func (*fragmentFailureOutbound) SendControl(
	context.Context,
	protocolsession.MessageKind,
	protocolsession.OperationID,
	[]byte,
) (protocolsession.SendOutcome, error) {
	return protocolsession.SendOutcomeDropped, errors.New("final control was not expected")
}

func (outbound *fragmentFailureOutbound) SendFragment(context.Context, protocolsession.Message) error {
	outbound.fragmentCalls++
	return outbound.fragmentErr
}

func (outbound *fragmentFailureOutbound) SendOperationError(
	context.Context,
	protocolsession.OperationID,
	OperationFailure,
) error {
	outbound.operationError++
	return nil
}

func (outbound fixedOutcomeOutbound) SendControl(
	context.Context,
	protocolsession.MessageKind,
	protocolsession.OperationID,
	[]byte,
) (protocolsession.SendOutcome, error) {
	return outbound.outcome, outbound.err
}

func (fixedOutcomeOutbound) SendFragment(context.Context, protocolsession.Message) error {
	return nil
}

func (fixedOutcomeOutbound) SendOperationError(context.Context, protocolsession.OperationID, OperationFailure) error {
	return nil
}

func TestOpenResultsReleaseLeasesOnlyWhenReceiptProvesNoSend(t *testing.T) {
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
		outcome          protocolsession.SendOutcome
		sendErr          error
		wantReleased     bool
		wantProcessError error
	}{
		{name: "cancel before send", outcome: protocolsession.SendOutcomeDropped, wantReleased: true},
		{name: "send wins cancel", outcome: protocolsession.SendOutcomeDelivered},
		{name: "writer transport failure", outcome: protocolsession.SendOutcomeUnknown, sendErr: transportErr, wantProcessError: transportErr},
		{name: "caller timeout", outcome: protocolsession.SendOutcomeUnknown, sendErr: context.DeadlineExceeded, wantProcessError: context.DeadlineExceeded},
		{name: "session close before send", outcome: protocolsession.SendOutcomeDropped, sendErr: protocolsession.ErrWriterStopped, wantReleased: true, wantProcessError: protocolsession.ErrWriterStopped},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &stubRevisionStore{results: []content.OpenRevisionResult{{FileID: base.file, Lease: lease}}}
			service, cache := stubService(t, lease.Descriptor(), store, stubRecordSealer{revision: []byte("revision")})
			defer cache.Close()
			handler, err := NewSenderHandler(SenderHandlerConfig{
				Service:  service,
				Outbound: fixedOutcomeOutbound{outcome: test.outcome, err: test.sendErr},
			})
			if err != nil {
				t.Fatal(err)
			}
			body, err := EncodeOpenRequest(mustOpenRequest(t, base.file))
			if err != nil {
				t.Fatal(err)
			}
			processErr := handler.processOpen(context.Background(), flowID[protocolsession.OperationID](219), body)
			if !errors.Is(processErr, test.wantProcessError) || (test.wantProcessError == nil && processErr != nil) {
				t.Fatalf("process error=%v want=%v", processErr, test.wantProcessError)
			}

			_, ownedErr := service.ownedDescriptor(lease.ID())
			if test.wantReleased && !errors.Is(ownedErr, ErrLeaseNotOwned) {
				t.Fatalf("definitively dropped result retained lease: %v", ownedErr)
			}
			if !test.wantReleased && ownedErr != nil {
				t.Fatalf("uncertain or delivered result released lease: %v", ownedErr)
			}
			store.mu.Lock()
			releasedBeforeClose := len(store.released)
			store.mu.Unlock()
			if (releasedBeforeClose == 1) != test.wantReleased {
				t.Fatalf("release count before close=%d wantReleased=%v", releasedBeforeClose, test.wantReleased)
			}
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			releasedAfterClose := len(store.released)
			store.mu.Unlock()
			if releasedAfterClose != 1 {
				t.Fatalf("lease release count after close=%d", releasedAfterClose)
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
	operationID := flowID[protocolsession.OperationID](218)
	message, _ := protocolsession.NewMessage(protocolsession.MessageRequestBlocks, &operationID, body)
	outbound := &fragmentFailureOutbound{fragmentErr: errors.New("fragment path failed")}
	handler, err := NewSenderHandler(SenderHandlerConfig{Service: fixture.service, Outbound: outbound})
	if err != nil {
		t.Fatal(err)
	}
	handler.process(context.Background(), message)
	if outbound.fragmentCalls != 1 || outbound.operationError != 1 {
		t.Fatalf("fragment calls=%d operation errors=%d", outbound.fragmentCalls, outbound.operationError)
	}
}

func TestRenewResultRetiresLeaseOnlyWhenReceiptProvesNoSend(t *testing.T) {
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
		outcome      protocolsession.SendOutcome
		sendErr      error
		wantReleased bool
	}{
		{name: "definitive drop", outcome: protocolsession.SendOutcomeDropped, wantReleased: true},
		{name: "delivered", outcome: protocolsession.SendOutcomeDelivered},
		{name: "transport uncertainty", outcome: protocolsession.SendOutcomeUnknown, sendErr: transportErr},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &stubRevisionStore{
				results: []content.OpenRevisionResult{{FileID: base.file, Lease: lease}},
				renew:   lease,
			}
			service, cache := stubService(t, lease.Descriptor(), store, stubRecordSealer{revision: []byte("revision")})
			defer cache.Close()
			defer service.Close()
			if _, err := service.Open(context.Background(), mustOpenRequest(t, base.file)); err != nil {
				t.Fatal(err)
			}
			handler, err := NewSenderHandler(SenderHandlerConfig{
				Service: service, Outbound: fixedOutcomeOutbound{outcome: test.outcome, err: test.sendErr},
			})
			if err != nil {
				t.Fatal(err)
			}
			body, err := EncodeLeaseRequest(lease.ID())
			if err != nil {
				t.Fatal(err)
			}
			processErr := handler.processRenew(context.Background(), flowID[protocolsession.OperationID](220), body)
			if !errors.Is(processErr, test.sendErr) || (test.sendErr == nil && processErr != nil) {
				t.Fatalf("renew process error=%v want=%v", processErr, test.sendErr)
			}
			_, ownedErr := service.ownedDescriptor(lease.ID())
			if test.wantReleased != errors.Is(ownedErr, ErrLeaseNotOwned) {
				t.Fatalf("renew ownership error=%v wantReleased=%v", ownedErr, test.wantReleased)
			}
			store.mu.Lock()
			released := len(store.released)
			store.mu.Unlock()
			if (released == 1) != test.wantReleased {
				t.Fatalf("renew release count=%d wantReleased=%v", released, test.wantReleased)
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
	service, cache := stubService(t, first.Descriptor(), store, stubRecordSealer{revision: []byte("revision")})
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
	released := len(store.released)
	store.mu.Unlock()
	if released != 1 {
		t.Fatalf("malformed renewal release count=%d", released)
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
	service, cache := stubService(t, driftLease.Descriptor(), store, stubRecordSealer{revision: []byte("revision")})
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

func TestOpenRejectsAReleasedLeaseIdentityReusedByTheStore(t *testing.T) {
	base := newRuntimeFixture(t, 1)
	defer base.close(t)
	opened, err := base.service.Open(context.Background(), mustOpenRequest(t, base.file))
	if err != nil {
		t.Fatal(err)
	}
	lease := opened.Items()[0].Lease
	store := &stubRevisionStore{results: []content.OpenRevisionResult{{FileID: base.file, Lease: lease}}}
	service, cache := stubService(t, lease.Descriptor(), store, stubRecordSealer{revision: []byte("revision")})
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
	service, cache := stubService(t, descriptor, store, stubRecordSealer{})
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
	released := len(store.released)
	store.mu.Unlock()
	if released != 0 {
		t.Fatalf("unknown replay reached the share-scoped store: releases=%d", released)
	}
}
