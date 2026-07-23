package contentflow

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func flowID[T ~[16]byte](value byte) T {
	var id T
	id[0] = value
	return id
}

func flowDescriptor(t *testing.T, size uint64) content.FileRevisionDescriptor {
	t.Helper()
	geometry, err := content.NewFileGeometry(size, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		flowID[catalog.ShareInstance](1), flowID[catalog.FileID](2), flowID[content.FileRevision](3),
		geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func flowMessagePlaintext(t *testing.T, message protocolsession.Message) []byte {
	t.Helper()
	plaintext, err := protocolsession.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func reassemblyHierarchy(t *testing.T, limits ReassemblyLimits) (ReassemblyHierarchy, []*ReassemblyAccount) {
	t.Helper()
	accounts := make([]*ReassemblyAccount, 3)
	for index, name := range []string{"process", "share", "session"} {
		account, err := NewReassemblyAccount(name, limits)
		if err != nil {
			t.Fatal(err)
		}
		accounts[index] = account
	}
	return ReassemblyHierarchy{Process: accounts[0], Share: accounts[1], Session: accounts[2]}, accounts
}

func TestAssemblerEnforcesFrozenOperationBudgetIndependentlyOfSessionBudget(t *testing.T) {
	operation := flowID[protocolsession.OperationID](221)
	recordBytes := records.MaxBlockRecordObjectBytes/2 + 1
	first, err := FragmentRecord(operation, bytes.Repeat([]byte{1}, recordBytes))
	if err != nil {
		t.Fatal(err)
	}
	second, err := FragmentRecord(operation, bytes.Repeat([]byte{2}, recordBytes))
	if err != nil {
		t.Fatal(err)
	}
	hierarchy, accounts := reassemblyHierarchy(t, ReassemblyLimits{Bytes: uint64(recordBytes * 2), Records: 2})
	assembler, err := NewAssembler(flowID[protocolsession.ProtocolSessionID](222), hierarchy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer assembler.Close()
	result, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, first[0]))
	if err != nil || result.Status != FragmentAccepted {
		t.Fatalf("first record admission status=%d err=%v", result.Status, err)
	}
	if _, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, second[0])); !errors.Is(err, ErrReassemblyBudget) {
		t.Fatalf("operation budget error=%v", err)
	}
	if assembler.ActiveRecords() != 0 {
		t.Fatalf("active records=%d", assembler.ActiveRecords())
	}
	for _, account := range accounts {
		if usage := account.Usage(); usage != (ReassemblyUsage{}) {
			t.Fatalf("failed operation retained budget=%+v", usage)
		}
	}
	late, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, first[1]))
	if err != nil || late.Status != FragmentTombstoned {
		t.Fatalf("over-budget operation resurrected: status=%d err=%v", late.Status, err)
	}
	independent := flowID[protocolsession.OperationID](223)
	third, err := FragmentRecord(independent, bytes.Repeat([]byte{3}, recordBytes))
	if err != nil {
		t.Fatal(err)
	}
	result, err = assembler.AcceptAuthenticated(flowMessagePlaintext(t, third[0]))
	if err != nil || result.Status != FragmentAccepted {
		t.Fatalf("independent operation lost session budget: status=%d err=%v", result.Status, err)
	}
}

func TestCancelReasonCodecFreezesTheReceiverIntentDomain(t *testing.T) {
	for reason := CancelReasonUser; reason <= CancelReasonLaneRace; reason++ {
		encoded, err := EncodeCancelReason(reason)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeCancelReason(encoded)
		if err != nil || decoded != reason {
			t.Fatalf("reason %d decoded=%d err=%v", reason, decoded, err)
		}
	}
	for _, reason := range []CancelReason{0, CancelReasonLaneRace + 1} {
		if _, err := EncodeCancelReason(reason); !errors.Is(err, ErrInvalidCancelRequest) {
			t.Fatalf("invalid reason %d encode error=%v", reason, err)
		}
	}
	for _, encoded := range [][]byte{
		{0x80},
		{0x81, 0x00},
		{0x81, 0x06},
		{0x81, 0x18, 0x01},
	} {
		if _, err := DecodeCancelReason(encoded); err == nil {
			t.Fatalf("invalid cancel body %x accepted", encoded)
		}
	}
}

func TestFragmentRoundTripOutOfOrderDuplicateAndLate(t *testing.T) {
	operation := flowID[protocolsession.OperationID](4)
	object := make([]byte, MaxFragmentPayloadBytes*2+17)
	for index := range object {
		object[index] = byte(index)
	}
	messages, err := FragmentRecord(operation, object)
	if err != nil || len(messages) != 3 {
		t.Fatalf("fragments=%d err=%v", len(messages), err)
	}
	hierarchy, accounts := reassemblyHierarchy(t, ReassemblyLimits{Bytes: uint64(len(object)) * 2, Records: 4})
	now := time.Unix(100, 0)
	assembler, err := NewAssembler(flowID[protocolsession.ProtocolSessionID](5), hierarchy, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	order := []int{2, 0, 0, 1}
	statuses := []AssemblyStatus{FragmentAccepted, FragmentAccepted, FragmentDuplicate, RecordComplete}
	for index, fragmentIndex := range order {
		result, acceptErr := assembler.AcceptAuthenticated(flowMessagePlaintext(t, messages[fragmentIndex]))
		if acceptErr != nil || result.Status != statuses[index] {
			t.Fatalf("accept %d status=%d err=%v", fragmentIndex, result.Status, acceptErr)
		}
		if result.Status == RecordComplete && !bytes.Equal(result.Object, object) {
			t.Fatal("reassembled object changed")
		}
	}
	late, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, messages[1]))
	if err != nil || late.Status != FragmentTombstoned {
		t.Fatalf("late fragment status=%d err=%v", late.Status, err)
	}
	for _, account := range accounts {
		if usage := account.Usage(); usage != (ReassemblyUsage{}) {
			t.Fatalf("budget leaked: %+v", usage)
		}
	}
	assembler.Close()
	assembler.Close()
	if _, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, messages[0])); !errors.Is(err, ErrAssemblerClosed) {
		t.Fatalf("closed assembler error=%v", err)
	}
}

func TestFragmentConflictTerminatesOnlyItsOperation(t *testing.T) {
	firstOperation := flowID[protocolsession.OperationID](6)
	secondOperation := flowID[protocolsession.OperationID](7)
	object := bytes.Repeat([]byte{0x61}, MaxFragmentPayloadBytes+9)
	first, _ := FragmentRecord(firstOperation, object)
	second, _ := FragmentRecord(secondOperation, []byte("independent"))
	hierarchy, _ := reassemblyHierarchy(t, ReassemblyLimits{Bytes: uint64(len(object)) * 2, Records: 4})
	assembler, _ := NewAssembler(flowID[protocolsession.ProtocolSessionID](8), hierarchy, nil)
	if _, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, first[0])); err != nil {
		t.Fatal(err)
	}
	conflict := flowMessagePlaintext(t, first[0])
	conflict[len(conflict)-1] ^= 1
	if _, err := assembler.AcceptAuthenticated(conflict); !errors.Is(err, ErrFragmentConflict) {
		t.Fatalf("conflict error=%v", err)
	}
	late, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, first[1]))
	if err != nil || late.Status != FragmentTombstoned {
		t.Fatalf("conflicted operation resurrected: %+v %v", late, err)
	}
	completed, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, second[0]))
	if err != nil || completed.Status != RecordComplete {
		t.Fatalf("sibling operation failed: %+v %v", completed, err)
	}

	digestMismatch := flowMessagePlaintext(t, second[0])
	digestMismatch[20] ^= 1
	otherHierarchy, _ := reassemblyHierarchy(t, ReassemblyLimits{Bytes: 1 << 20, Records: 4})
	other, _ := NewAssembler(flowID[protocolsession.ProtocolSessionID](9), otherHierarchy, nil)
	if _, err := other.AcceptAuthenticated(digestMismatch); !errors.Is(err, ErrRecordDigest) {
		t.Fatalf("record digest error=%v", err)
	}
}

func TestAssemblerTimeoutCancelAndThreeTierBudget(t *testing.T) {
	operation := flowID[protocolsession.OperationID](10)
	object := bytes.Repeat([]byte{1}, MaxFragmentPayloadBytes+1)
	messages, _ := FragmentRecord(operation, object)
	now := time.Unix(200, 0)
	hierarchy, accounts := reassemblyHierarchy(t, ReassemblyLimits{Bytes: uint64(len(object)), Records: 1})
	assembler, _ := NewAssembler(flowID[protocolsession.ProtocolSessionID](11), hierarchy, func() time.Time { return now })
	if _, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, messages[0])); err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if usage := account.Usage(); usage.Bytes != uint64(len(object)) || usage.Records != 1 {
			t.Fatalf("reservation missing: %+v", usage)
		}
	}
	now = now.Add(FragmentTimeout)
	timedOut := assembler.SweepTimeouts()
	if !slices.Equal(timedOut, []protocolsession.OperationID{operation}) {
		t.Fatalf("timeouts=%x", timedOut)
	}
	late, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, messages[1]))
	if err != nil || late.Status != FragmentTombstoned {
		t.Fatalf("timeout late result=%+v err=%v", late, err)
	}
	if err := assembler.CancelOperation(flowID[protocolsession.OperationID](12)); err != nil {
		t.Fatal(err)
	}
	cancelledObject, _ := FragmentRecord(flowID[protocolsession.OperationID](12), []byte("cancelled"))
	cancelled, _ := assembler.AcceptAuthenticated(flowMessagePlaintext(t, cancelledObject[0]))
	if cancelled.Status != FragmentTombstoned {
		t.Fatalf("cancelled fragment status=%d", cancelled.Status)
	}
	if err := assembler.CompleteOperation(flowID[protocolsession.OperationID](13)); err != nil {
		t.Fatal(err)
	}
	if err := assembler.CancelOperation(protocolsession.OperationID{}); !errors.Is(err, ErrFragmentMalformed) {
		t.Fatalf("zero cancel error=%v", err)
	}

	tooSmall, _ := NewReassemblyAccount("small", ReassemblyLimits{Bytes: uint64(len(object)) - 1, Records: 1})
	rollbackShare, _ := NewReassemblyAccount("rollback-share", ReassemblyLimits{Bytes: uint64(len(object)), Records: 1})
	rollbackSession, _ := NewReassemblyAccount("rollback-session", ReassemblyLimits{Bytes: uint64(len(object)), Records: 1})
	limited, _ := NewAssembler(flowID[protocolsession.ProtocolSessionID](14), ReassemblyHierarchy{
		Process: rollbackShare, Share: tooSmall, Session: rollbackSession,
	}, nil)
	if _, err := limited.AcceptAuthenticated(flowMessagePlaintext(t, messages[0])); !errors.Is(err, ErrReassemblyBudget) {
		t.Fatalf("budget error=%v", err)
	}
	if usage := rollbackShare.Usage(); usage != (ReassemblyUsage{}) {
		t.Fatalf("hierarchical rollback leaked: %+v", usage)
	}
}

func TestUnrelatedFragmentCannotConsumeAnotherOperationsTimeoutSignal(t *testing.T) {
	now := time.Unix(250, 0)
	timedOutOperation := flowID[protocolsession.OperationID](224)
	activeOperation := flowID[protocolsession.OperationID](225)
	partial, _ := FragmentRecord(timedOutOperation, bytes.Repeat([]byte{1}, MaxFragmentPayloadBytes+1))
	active, _ := FragmentRecord(activeOperation, []byte("active"))
	hierarchy, _ := reassemblyHierarchy(t, ReassemblyLimits{Bytes: 1 << 20, Records: 4})
	assembler, _ := NewAssembler(flowID[protocolsession.ProtocolSessionID](226), hierarchy, func() time.Time { return now })
	if _, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, partial[0])); err != nil {
		t.Fatal(err)
	}
	now = now.Add(FragmentTimeout)
	result, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, active[0]))
	if err != nil || result.Status != RecordComplete {
		t.Fatalf("unrelated operation status=%d err=%v", result.Status, err)
	}
	if timedOut := assembler.SweepTimeouts(); !slices.Equal(timedOut, []protocolsession.OperationID{timedOutOperation}) {
		t.Fatalf("timeout signal was consumed by unrelated traffic: %x", timedOut)
	}

	secondTimedOut := flowID[protocolsession.OperationID](227)
	second, _ := FragmentRecord(secondTimedOut, bytes.Repeat([]byte{2}, MaxFragmentPayloadBytes+1))
	if _, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, second[0])); err != nil {
		t.Fatal(err)
	}
	now = now.Add(FragmentTimeout)
	if _, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, second[1])); !errors.Is(err, ErrFragmentTimeout) {
		t.Fatalf("expired operation accepted a late completing fragment: %v", err)
	}
	late, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, second[0]))
	if err != nil || late.Status != FragmentTombstoned {
		t.Fatalf("timed-out operation resurrected: status=%d err=%v", late.Status, err)
	}
}

func TestFragmentHostileHeadersAllocateNothing(t *testing.T) {
	operation := flowID[protocolsession.OperationID](15)
	message, _ := FragmentRecord(operation, []byte("payload"))
	valid := flowMessagePlaintext(t, message[0])
	hierarchy, accounts := reassemblyHierarchy(t, ReassemblyLimits{Bytes: 1 << 20, Records: 4})
	assembler, _ := NewAssembler(flowID[protocolsession.ProtocolSessionID](16), hierarchy, nil)
	cases := [][]byte{
		nil,
		valid[:FragmentHeaderBytes-1],
		func() []byte { value := bytes.Clone(valid); value[0]++; return value }(),
		func() []byte { value := bytes.Clone(valid); value[2] = 2; return value }(),
		func() []byte { value := bytes.Clone(valid); value[3] = 1; return value }(),
		func() []byte { value := bytes.Clone(valid); clear(value[4:20]); return value }(),
		func() []byte { value := bytes.Clone(valid); clear(value[20:36]); return value }(),
		func() []byte { value := bytes.Clone(valid); value[39] = 1; return value }(),
		func() []byte { value := bytes.Clone(valid); value[43] = 0; return value }(),
		func() []byte { value := bytes.Clone(valid); value[47] = 0; return value }(),
		func() []byte { value := bytes.Clone(valid); value[51]++; return value }(),
	}
	for index, candidate := range cases {
		if _, err := assembler.AcceptAuthenticated(candidate); !errors.Is(err, ErrFragmentMalformed) {
			t.Fatalf("case %d error=%v", index, err)
		}
	}
	for _, account := range accounts {
		if usage := account.Usage(); usage != (ReassemblyUsage{}) {
			t.Fatalf("hostile header allocated: %+v", usage)
		}
	}
	if _, err := FragmentRecord(protocolsession.OperationID{}, []byte{1}); !errors.Is(err, ErrFragmentMalformed) {
		t.Fatalf("zero operation error=%v", err)
	}
	if _, err := FragmentRecord(operation, nil); !errors.Is(err, ErrFragmentMalformed) {
		t.Fatalf("empty object error=%v", err)
	}
}

func TestContentControlCodecsRoundTripAndRejectAlternateForms(t *testing.T) {
	fileA := flowID[catalog.FileID](21)
	fileB := flowID[catalog.FileID](22)
	ranges, _ := content.NewRangeSet([]content.Range{{Offset: 1, End: 7}, {Offset: 9, End: 12}})
	request, err := NewOpenRequest([]OpenItem{{FileID: fileA, InitialRanges: ranges}, {FileID: fileB}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := EncodeOpenRequest(request)
	decoded, err := DecodeOpenRequest(encoded)
	if err != nil || len(decoded.Items()) != 2 || decoded.Items()[0].InitialRanges.Len() != 2 {
		t.Fatalf("open request decode=%+v err=%v", decoded.Items(), err)
	}
	items := decoded.Items()
	items[0].FileID = catalog.FileID{}
	if decoded.Items()[0].FileID != fileA {
		t.Fatal("open request exposed mutable storage")
	}

	lease := flowID[content.LeaseID](23)
	blocks, _ := NewBlockRequest(lease, []uint64{0, 3, 9})
	blockBytes, _ := EncodeBlockRequest(blocks)
	blockDecoded, err := DecodeBlockRequest(blockBytes)
	if err != nil || blockDecoded.LeaseID() != lease || !slices.Equal(blockDecoded.Indices(), []uint64{0, 3, 9}) {
		t.Fatalf("block request=%+v err=%v", blockDecoded, err)
	}
	leaseBytes, _ := EncodeLeaseRequest(lease)
	if parsed, err := DecodeLeaseRequest(leaseBytes); err != nil || parsed != lease {
		t.Fatalf("lease request=%x err=%v", parsed, err)
	}

	nonCanonical, _ := cbor.Marshal([]any{fileA.Bytes(), []any{[]any{uint64(1), uint64(2)}}})
	nonCanonical = append([]byte{0x9f}, nonCanonical[1:]...)
	nonCanonical = append(nonCanonical, 0xff)
	if _, err := DecodeOpenRequest(nonCanonical); err == nil {
		t.Fatal("indefinite open request accepted")
	}
	if _, err := NewBlockRequest(lease, []uint64{2, 2}); !errors.Is(err, ErrInvalidBlockRequest) {
		t.Fatalf("duplicate block error=%v", err)
	}
	if _, err := NewBlockRequest(content.LeaseID{}, []uint64{1}); !errors.Is(err, ErrInvalidBlockRequest) {
		t.Fatalf("zero lease block error=%v", err)
	}
}

func TestOperationResultCodecsValidateIdentityCountAndCanonicalEncoding(t *testing.T) {
	lease := flowID[content.LeaseID](31)
	// These wire decoders intentionally do not require a local RevisionLease;
	// the authenticated sender result is validated against the requested ID.
	leaseBody, _ := bodyEncMode.Marshal(map[uint64]any{0: uint64(1), 1: lease.Bytes(), 2: uint64(120_000), 3: uint64(60_000)})
	remote, err := DecodeLeaseResult(leaseBody, lease)
	if err != nil || remote.ID != lease || remote.TTL != 120*time.Second || remote.RenewAfter != 60*time.Second {
		t.Fatalf("remote lease=%+v err=%v", remote, err)
	}
	if _, err := DecodeLeaseResult(leaseBody, flowID[content.LeaseID](32)); !errors.Is(err, ErrInvalidOperationResult) {
		t.Fatalf("wrong lease identity error=%v", err)
	}
	complete, _ := EncodeOperationComplete(256)
	if count, err := DecodeOperationComplete(complete); err != nil || count != 256 {
		t.Fatalf("complete count=%d err=%v", count, err)
	}
	overflow, _ := bodyEncMode.Marshal(map[uint64]any{0: uint64(1), 1: uint64(1) << 32})
	if _, err := DecodeOperationComplete(overflow); !errors.Is(err, ErrInvalidOperationResult) {
		t.Fatalf("overflow count error=%v", err)
	}
	if _, err := NewRevisionFailure(RevisionCodeStale-1, false, 0); err == nil {
		t.Fatal("out-of-scope revision failure accepted")
	}
	if _, err := NewRevisionFailure(RevisionCodeQuota, true, 0); err == nil {
		t.Fatal("retryable failure without delay accepted")
	}

	descriptor := flowDescriptor(t, 1)
	record, _ := records.NewBlockRecord(descriptor, 0, []byte{1})
	if record.Descriptor() != descriptor {
		t.Fatal("fixture record changed descriptor")
	}
}

func TestRevisionFailureAndLeaseResultFrozenBounds(t *testing.T) {
	for _, test := range []struct {
		name       string
		retryAfter time.Duration
		wantError  bool
	}{
		{name: "zero", wantError: true},
		{name: "minimum", retryAfter: MinRevisionFailureRetryAfter},
		{name: "maximum", retryAfter: MaxRevisionFailureRetryAfter},
		{name: "above maximum", retryAfter: MaxRevisionFailureRetryAfter + time.Millisecond, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRevisionFailure(RevisionCodeQuota, true, test.retryAfter)
			if (err != nil) != test.wantError {
				t.Fatalf("NewRevisionFailure() error = %v, want error %v", err, test.wantError)
			}
		})
	}

	descriptor := flowDescriptor(t, 1)
	leaseID := flowID[content.LeaseID](92)
	valid, err := content.NewRevisionLease(
		leaseID, descriptor, RevisionLeaseTTL, RevisionLeaseRenewAfter,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeLeaseResult(valid); err != nil {
		t.Fatalf("encode frozen lease timing: %v", err)
	}
	for _, timing := range []struct {
		ttl, renewAfter time.Duration
	}{
		{ttl: RevisionLeaseTTL - time.Millisecond, renewAfter: RevisionLeaseRenewAfter},
		{ttl: RevisionLeaseTTL, renewAfter: RevisionLeaseRenewAfter - time.Millisecond},
	} {
		lease, err := content.NewRevisionLease(leaseID, descriptor, timing.ttl, timing.renewAfter)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := EncodeLeaseResult(lease); err == nil {
			t.Fatalf("encoded non-frozen lease timing TTL=%s renew=%s", timing.ttl, timing.renewAfter)
		}
	}
}
