package senderrelay

import "github.com/windshare/windshare/transport/relayv2"

func (lifecycle *Lifecycle) trackObservationConnection(connection Connection) Connection {
	if lifecycle != nil && connection.valid() && lifecycle.config.ObserveConnection != nil {
		connection.finishObservations = lifecycle.config.ObserveConnection(connection)
	}
	return connection
}

func (lifecycle *Lifecycle) retireConnection(connection Connection) error {
	closeErr := connection.Close()
	// Close can encounter a transport already retiring on another goroutine.
	// Its Done follows the terminal observation, so never cut diagnostics early.
	if terminal, ok := connection.endpoint.(interface{ Done() <-chan struct{} }); ok {
		<-terminal.Done()
	}
	completion := connection.CompleteObservations()
	if connection.finishObservations != nil {
		connection.finishObservations()
	}
	lifecycle.mu.Lock()
	mergeRelayCompletion(&lifecycle.retiredCompletion, completion)
	lifecycle.mu.Unlock()
	return closeErr
}

func (lifecycle *Lifecycle) CompleteObservations() relayv2.LifecycleObservationCompletion {
	if lifecycle == nil {
		return relayv2.LifecycleObservationCompletion{}
	}
	// The command joins recovery and cleanup before its final observation cut.
	// Retired transports have already contributed immutable counters and no
	// longer need to remain reachable from this lifecycle.
	lifecycle.mu.Lock()
	completion, connection := lifecycle.retiredCompletion, lifecycle.connection
	lifecycle.mu.Unlock()
	mergeRelayCompletion(&completion, connection.CompleteObservations())
	return completion
}

func mergeRelayCompletion(total *relayv2.LifecycleObservationCompletion, next relayv2.LifecycleObservationCompletion) {
	total.Enqueued = saturatingAdd(total.Enqueued, next.Enqueued)
	total.Loss.CapacityDropped = saturatingAdd(total.Loss.CapacityDropped, next.Loss.CapacityDropped)
}
func saturatingAdd(a, b uint64) uint64 {
	if ^uint64(0)-a < b {
		return ^uint64(0)
	}
	return a + b
}
