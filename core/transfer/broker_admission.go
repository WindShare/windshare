package transfer

import (
	"github.com/windshare/windshare/core/transfer/bufferbudget"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

func (b *BlockBroker) ObserveRemainingWork(snapshot func() transferlanes.RemainingWork) func() {
	if observer, ok := b.blocks.(interface {
		ObserveRemainingWork(func() transferlanes.RemainingWork) func()
	}); ok {
		return observer.ObserveRemainingWork(snapshot)
	}
	return func() {}
}

func (b *BlockBroker) callOrder(call *blockCall) transferlanes.WorkOrder {
	b.mu.Lock()
	defer b.mu.Unlock()
	var best transferlanes.WorkOrder
	found := false
	for consumer := range call.consumers {
		var order transferlanes.WorkOrder
		if consumer.order != nil {
			order = consumer.order()
		}
		if !found || order.Distance < best.Distance || (order.Distance == best.Distance && order.LastServed < best.LastServed) {
			best, found = order, true
		}
	}
	return best
}

func (b *BlockBroker) callReady(call *blockCall) transferlanes.CapacityReadiness {
	changed := b.process.Changes()
	b.mu.Lock()
	defer b.mu.Unlock()
	// Claim carries terminal errors. A readiness observation may defer work but
	// cannot hide a closed owner behind an unsignalled capacity wait.
	if b.closed || call.ctx.Err() != nil {
		return transferlanes.CapacityReadiness{Kind: transferlanes.CapacityReady}
	}
	limit := b.maxConcurrentBlocks
	if call.activeAttempts > 0 {
		limit += lanescheduling.MaximumSupplements
	}
	bytes := call.expected
	if call.reserved == (bufferbudget.Reservation{}) {
		ready := false
		for consumer := range call.consumers {
			if consumer.ready() {
				ready = true
				break
			}
		}
		if !ready {
			return transferlanes.CapacityReadiness{Kind: transferlanes.CapacityConsumer, Changed: changed}
		}
		bytes += 2 * call.expected
	}
	kind := transferlanes.CapacityReady
	if !b.account.Available(bytes) {
		kind = transferlanes.CapacityConsumer
	} else if b.activeAttempts >= limit {
		kind = transferlanes.CapacityNetwork
	}
	return transferlanes.CapacityReadiness{Kind: kind, Changed: changed}
}
