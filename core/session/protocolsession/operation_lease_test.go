package protocolsession

import (
	"sync"
	"testing"
)

func TestOutboundOperationLeaseConcurrentReleaseAndSettlementIsIdempotent(t *testing.T) {
	for iteration := range 128 {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
		operationID := testOperationID(byte(iteration + 1))
		request := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(1)})
		admission, err := table.ObserveInbound(DirectionReceiverToSender, request)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := admission.Outbound.AcquireLease()
		if err != nil {
			t.Fatal(err)
		}
		result := newDeliveryResult()
		result.receipt().ReleaseLeaseOnSettlement(lease)
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			lease.Release()
		}()
		go func() {
			defer wait.Done()
			<-start
			result.complete(SendOutcomeDelivered, OutboundReplayPermit{}, false, nil)
		}()
		close(start)
		wait.Wait()
		table.mu.Lock()
		pins := admission.Generation.authority.pins
		table.mu.Unlock()
		if pins != 0 {
			t.Fatalf("iteration %d retained %d pins", iteration, pins)
		}
	}
}

func TestSendReceiptRetainsAtMostOneSettlementLease(t *testing.T) {
	table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
	operationID := testOperationID(180)
	request := mustMessage(t, MessageRequestBlocks, &operationID, map[uint64]any{0: uint64(1)})
	admission, _ := table.ObserveInbound(DirectionReceiverToSender, request)
	first, _ := admission.Outbound.AcquireLease()
	second, _ := admission.Outbound.AcquireLease()
	result := newDeliveryResult()
	receipt := result.receipt()
	receipt.ReleaseLeaseOnSettlement(first)
	receipt.ReleaseLeaseOnSettlement(second)
	table.mu.Lock()
	pinsBeforeSettlement := admission.Generation.authority.pins
	table.mu.Unlock()
	if pinsBeforeSettlement != 1 {
		t.Fatalf("receipt retained %d settlement leases", pinsBeforeSettlement)
	}
	result.complete(SendOutcomeDropped, OutboundReplayPermit{}, false, nil)
	table.mu.Lock()
	pinsAfterSettlement := admission.Generation.authority.pins
	table.mu.Unlock()
	if pinsAfterSettlement != 0 {
		t.Fatalf("settlement retained %d pins", pinsAfterSettlement)
	}
}
