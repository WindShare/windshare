package contentflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func TestResultValidationEntryPointsEnforceCanonicalAuthenticatedBodies(t *testing.T) {
	file := flowID[catalog.FileID](231)
	lease := flowID[content.LeaseID](232)
	openBody := mustContentBody(t, map[uint64]any{
		0: uint64(controlSchemaVersion),
		1: []any{[]any{file.Bytes(), uint64(1), uint64(RevisionCodeStale), false, nil}},
	})
	if err := ValidateOpenResults(openBody); err != nil {
		t.Fatalf("valid open results failed validation: %v", err)
	}
	if err := ValidateOpenResults(nil); !errors.Is(err, ErrNonCanonicalBody) {
		t.Fatalf("empty open results validation = %v", err)
	}

	leaseBody := mustContentBody(t, map[uint64]any{
		0: uint64(controlSchemaVersion),
		1: lease.Bytes(),
		2: uint64(RevisionLeaseTTL / time.Millisecond),
		3: uint64(RevisionLeaseRenewAfter / time.Millisecond),
	})
	if err := ValidateLeaseResult(leaseBody); err != nil {
		t.Fatalf("valid lease result failed validation: %v", err)
	}
	if err := ValidateLeaseResult(nil); !errors.Is(err, ErrNonCanonicalBody) {
		t.Fatalf("empty lease result validation = %v", err)
	}
	if _, err := DecodeOperationComplete(nil); !errors.Is(err, ErrNonCanonicalBody) {
		t.Fatalf("empty completion result = %v", err)
	}
}

func TestRequestDecodersRejectAggregateAndAlternateEncodingAuthority(t *testing.T) {
	file := flowID[catalog.FileID](233)
	lease := flowID[content.LeaseID](234)

	ranges := make([]any, MaxInitialRangesPerFile)
	for index := range ranges {
		offset := uint64(index * 2)
		ranges[index] = []any{offset, offset + 1}
	}
	items := make([]any, MaxInitialRangesPerOpen/MaxInitialRangesPerFile+1)
	for index := range items {
		items[index] = []any{flowID[catalog.FileID](byte(index + 1)).Bytes(), ranges}
	}
	if _, err := DecodeOpenRequest(mustContentBody(t, items)); !errors.Is(err, ErrInvalidOpenRequest) {
		t.Fatalf("aggregate range budget error = %v", err)
	}

	for name, value := range map[string]any{
		"ranges type": []any{[]any{file.Bytes(), "ranges"}},
		"range arity": []any{[]any{file.Bytes(), []any{[]any{uint64(1)}}}},
		"file type":   []any{[]any{"file", []any{}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeOpenRequest(mustContentBody(t, value)); !errors.Is(err, ErrInvalidOpenRequest) {
				t.Fatalf("open request error = %v", err)
			}
		})
	}

	request, err := NewOpenRequest([]OpenItem{{FileID: file}})
	if err != nil {
		t.Fatal(err)
	}
	canonicalOpen, err := EncodeOpenRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	nonCanonicalOpen := append([]byte{0x98, 0x01}, canonicalOpen[1:]...)
	if _, err := DecodeOpenRequest(nonCanonicalOpen); !errors.Is(err, ErrNonCanonicalBody) {
		t.Fatalf("alternate open encoding = %v", err)
	}

	block, err := NewBlockRequest(lease, []uint64{1})
	if err != nil {
		t.Fatal(err)
	}
	canonicalBlock, err := EncodeBlockRequest(block)
	if err != nil {
		t.Fatal(err)
	}
	nonCanonicalBlock := append([]byte{0x98, 0x02}, canonicalBlock[1:]...)
	if _, err := DecodeBlockRequest(nonCanonicalBlock); !errors.Is(err, ErrNonCanonicalBody) {
		t.Fatalf("alternate block encoding = %v", err)
	}
	if _, err := DecodeBlockRequest(mustContentBody(t, []any{"lease", []uint64{1}})); !errors.Is(err, ErrInvalidBlockRequest) {
		t.Fatalf("typed lease confusion = %v", err)
	}

	canonicalLease, err := EncodeLeaseRequest(lease)
	if err != nil {
		t.Fatal(err)
	}
	nonCanonicalLease := append([]byte{0x98, 0x01}, canonicalLease[1:]...)
	if _, err := DecodeLeaseRequest(nonCanonicalLease); !errors.Is(err, ErrNonCanonicalBody) {
		t.Fatalf("alternate lease encoding = %v", err)
	}
}

func TestOperationResultDecodersRejectTypedItemAndEnvelopeConfusion(t *testing.T) {
	file := flowID[catalog.FileID](235)
	lease := flowID[content.LeaseID](236)
	ttl := uint64(RevisionLeaseTTL / time.Millisecond)
	renew := uint64(RevisionLeaseRenewAfter / time.Millisecond)

	for name, item := range map[string]any{
		"wrong arity": []any{file.Bytes(), uint64(1), uint64(RevisionCodeStale), false},
		"file type":   []any{"file", uint64(1), uint64(RevisionCodeStale), false, nil},
		"status type": []any{file.Bytes(), "success", []byte{1}, lease.Bytes(), ttl, renew},
		"object type": []any{file.Bytes(), uint64(0), "object", lease.Bytes(), ttl, renew},
		"code overflow": []any{
			file.Bytes(), uint64(1), ^uint64(0), false, nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			encoded := mustContentBody(t, map[uint64]any{
				0: uint64(controlSchemaVersion),
				1: []any{item},
			})
			if _, err := DecodeOpenResults(encoded, []catalog.FileID{file}); err == nil {
				t.Fatal("typed open-result confusion was accepted")
			}
		})
	}

	shortLeaseEnvelope := mustContentBody(t, map[uint64]any{
		0: uint64(controlSchemaVersion),
		1: lease.Bytes(),
		2: ttl,
	})
	if _, err := DecodeLeaseResult(shortLeaseEnvelope, lease); !errors.Is(err, ErrInvalidOperationResult) {
		t.Fatalf("short lease envelope = %v", err)
	}
}

func TestOpenResultsConstructorRejectsWireIncompatibleLeaseAndFailureShapes(t *testing.T) {
	descriptor := flowDescriptor(t, 1)
	file := descriptor.FileID()
	wrongTiming, err := content.NewRevisionLease(
		flowID[content.LeaseID](237),
		descriptor,
		RevisionLeaseTTL-time.Second,
		RevisionLeaseRenewAfter-time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewOpenResults([]OpenResult{{
		FileID: file, Lease: wrongTiming, RevisionObject: []byte{1},
	}}); !errors.Is(err, ErrInvalidOpenResults) {
		t.Fatalf("wire-incompatible lease timing = %v", err)
	}

	invalidFailure := RevisionFailure{Code: RevisionCodeStale - 1}
	if _, err := NewOpenResults([]OpenResult{{FileID: file, Failure: &invalidFailure}}); err == nil {
		t.Fatal("wire-incompatible revision failure was accepted")
	}
}

func TestSenderHandlerLifecycleSnapshotReportsOnlyBoundedOwnership(t *testing.T) {
	var absent *SenderHandler
	if snapshot := absent.LifecycleSnapshot(); snapshot != (SenderHandlerLifecycle{}) {
		t.Fatalf("nil handler snapshot = %+v", snapshot)
	}

	handler := &SenderHandler{
		queue:   make(chan queuedOperation, 2),
		workers: make(chan struct{}, 2),
		active: map[handlerOperation]context.CancelFunc{
			{}: func() {},
		},
	}
	handler.queue <- queuedOperation{}
	handler.workers <- struct{}{}
	if snapshot := handler.LifecycleSnapshot(); snapshot != (SenderHandlerLifecycle{
		ActiveOperations: 1,
		RunningWorkers:   1,
		QueuedOperations: 1,
	}) {
		t.Fatalf("handler ownership snapshot = %+v", snapshot)
	}
}

func mustContentBody(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := bodyEncMode.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
