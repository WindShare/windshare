package contentflow

import (
	"errors"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type SenderDecisionStage uint8

const (
	SenderDecisionCapacityBusy SenderDecisionStage = iota + 1
	SenderDecisionLeaseRelinquished
	SenderDecisionLeaseUndelivered
	SenderDecisionLeaseDetached
	SenderDecisionBlockLeaseReleased
	SenderDecisionBlockLeaseNotOwned
	SenderDecisionBlockLeaseExpired
	SenderDecisionBlockLeaseInvalid
)

type SenderDecisionTrace struct {
	Stage              SenderDecisionStage
	OperationID        protocolsession.OperationID
	RequestKind        protocolsession.MessageKind
	CapacityDecisionID revisioncapacity.CapacityDecisionID
	LeaseID            content.LeaseID
}

type SenderDecisionTracer interface {
	TraceSenderDecision(SenderDecisionTrace)
}

type SenderDecisionTraceFunc func(SenderDecisionTrace)

func (function SenderDecisionTraceFunc) TraceSenderDecision(event SenderDecisionTrace) {
	if function != nil {
		function(event)
	}
}

func (h *SenderHandler) traceDecision(event SenderDecisionTrace) {
	if h == nil || h.decisionTracer == nil {
		return
	}
	defer func() { _ = recover() }()
	h.decisionTracer.TraceSenderDecision(event)
}

func (h *SenderHandler) traceCapacityDecisions(
	operation protocolsession.OperationID,
	results OpenResults,
) {
	for _, result := range results.Items() {
		if result.Failure == nil || result.Failure.CapacityDecisionID() == "" {
			continue
		}
		h.traceDecision(SenderDecisionTrace{
			Stage: SenderDecisionCapacityBusy, OperationID: operation,
			RequestKind:        protocolsession.MessageOpenRevisions,
			CapacityDecisionID: result.Failure.CapacityDecisionID(),
		})
	}
}

func (h *SenderHandler) endOpenResults(
	operation protocolsession.OperationID,
	results OpenResults,
	kind content.LeaseEndKind,
) error {
	for _, result := range results.Items() {
		if result.Failure == nil {
			h.traceLeaseDecision(operation, protocolsession.MessageOpenRevisions, result.Lease.ID(), kind)
		}
	}
	return h.service.endOpenResults(results, kind)
}

func (h *SenderHandler) traceLeaseDecision(
	operation protocolsession.OperationID,
	request protocolsession.MessageKind,
	lease content.LeaseID,
	kind content.LeaseEndKind,
) {
	stage := senderLeaseDecisionStage(kind)
	if stage == 0 || lease.IsZero() {
		return
	}
	h.traceDecision(SenderDecisionTrace{
		Stage: stage, OperationID: operation, RequestKind: request, LeaseID: lease,
	})
}

// Only rejected lease authority needs per-block evidence. Successful block traffic
// stays off the trace hot path; the lease ID joins the prior release decision.
func (h *SenderHandler) traceBlockLeaseRejection(operation protocolsession.OperationID, lease content.LeaseID, err error) {
	var stage SenderDecisionStage
	switch {
	case errors.Is(err, errLeaseRelinquished):
		stage = SenderDecisionBlockLeaseReleased
	case errors.Is(err, ErrLeaseNotOwned):
		stage = SenderDecisionBlockLeaseNotOwned
	case errors.Is(err, content.ErrLeaseExpired):
		stage = SenderDecisionBlockLeaseExpired
	case errors.Is(err, content.ErrInvalidLease):
		stage = SenderDecisionBlockLeaseInvalid
	default:
		return
	}
	h.traceDecision(SenderDecisionTrace{
		Stage: stage, OperationID: operation, RequestKind: protocolsession.MessageRequestBlocks, LeaseID: lease,
	})
}

func senderLeaseDecisionStage(kind content.LeaseEndKind) SenderDecisionStage {
	switch kind {
	case content.LeaseRelinquished:
		return SenderDecisionLeaseRelinquished
	case content.LeaseUndelivered:
		return SenderDecisionLeaseUndelivered
	case content.LeaseDetached:
		return SenderDecisionLeaseDetached
	default:
		return 0
	}
}
