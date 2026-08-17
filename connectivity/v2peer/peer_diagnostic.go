package v2peer

import (
	"math"
	"sync"
)

const (
	DefaultPeerDiagnosticObservationCapacity = 64
	maximumPeerDiagnosticObservationCapacity = 4_096
	peerDiagnosticCounterCount               = 6
)

// PeerDiagnosticCategory identifies the sealed evidence stream that lost
// observability. It never conveys transport or fallback authority.
type PeerDiagnosticCategory string

const (
	PeerDiagnosticSenderAttempt       PeerDiagnosticCategory = "sender_attempt"
	PeerDiagnosticReceiverTermination PeerDiagnosticCategory = "receiver_termination"
)

// PeerDiagnosticReason is deliberately text-free: provider errors remain
// behind the connectivity boundary.
type PeerDiagnosticReason string

const (
	PeerDiagnosticStreamCapacity   PeerDiagnosticReason = "stream_capacity"
	PeerDiagnosticEvidenceCapacity PeerDiagnosticReason = "evidence_capacity"
	PeerDiagnosticCleanupResidue   PeerDiagnosticReason = "cleanup_residue"
)

// PeerDiagnosticObservation reports the cumulative, saturating count for one
// closed category/reason pair. Later observations supersede earlier counts.
type PeerDiagnosticObservation struct {
	Category PeerDiagnosticCategory
	Reason   PeerDiagnosticReason
	Count    uint64
}

// peerDiagnosticReporter keeps cumulative truth in a fixed six-counter table.
// Stream saturation can lose intermediate snapshots, but never the cumulative
// producer state or the authoritative capacity-loss cut.
type peerDiagnosticReporter struct {
	mu        sync.Mutex
	source    *observationSource[PeerDiagnosticObservation]
	totals    [peerDiagnosticCounterCount]uint64
	completed bool
	cut       ObservationCompletion
}

func newPeerDiagnosticReporter(capacity int) (*peerDiagnosticReporter, error) {
	if capacity == 0 {
		return nil, nil
	}
	source, err := newObservationSource[PeerDiagnosticObservation](capacity)
	if err != nil {
		return nil, err
	}
	return &peerDiagnosticReporter{source: source}, nil
}

func (reporter *peerDiagnosticReporter) observations() <-chan PeerDiagnosticObservation {
	if reporter == nil {
		return nil
	}
	return reporter.source.observations()
}

func (reporter *peerDiagnosticReporter) report(
	category PeerDiagnosticCategory,
	reason PeerDiagnosticReason,
) {
	reporter.reportCount(category, reason, 1)
}

func (reporter *peerDiagnosticReporter) reportCount(
	category PeerDiagnosticCategory,
	reason PeerDiagnosticReason,
	increment uint64,
) {
	index, valid := peerDiagnosticIndex(category, reason)
	if reporter == nil || !valid || increment == 0 {
		return
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if reporter.completed {
		return
	}
	reporter.totals[index] = saturatingAdd(reporter.totals[index], increment)
	reporter.source.publish(PeerDiagnosticObservation{
		Category: category,
		Reason:   reason,
		Count:    reporter.totals[index],
	})
}

func (reporter *peerDiagnosticReporter) completeObservations() ObservationCompletion {
	if reporter == nil {
		return ObservationCompletion{}
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if reporter.completed {
		return reporter.cut
	}
	reporter.completed = true
	reporter.cut = reporter.source.completeObservations()
	return reporter.cut
}

func peerDiagnosticIndex(category PeerDiagnosticCategory, reason PeerDiagnosticReason) (int, bool) {
	categoryOffset := 0
	switch category {
	case PeerDiagnosticSenderAttempt:
	case PeerDiagnosticReceiverTermination:
		categoryOffset = peerDiagnosticCounterCount / 2
	default:
		return 0, false
	}
	reasonOffset := 0
	switch reason {
	case PeerDiagnosticStreamCapacity:
	case PeerDiagnosticEvidenceCapacity:
		reasonOffset = 1
	case PeerDiagnosticCleanupResidue:
		reasonOffset = 2
	default:
		return 0, false
	}
	return categoryOffset + reasonOffset, true
}

func saturatingAdd(current, increment uint64) uint64 {
	if math.MaxUint64-current < increment {
		return math.MaxUint64
	}
	return current + increment
}
