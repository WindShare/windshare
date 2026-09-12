package runtrace

import "github.com/windshare/windshare/cmd/wind/internal/clievent"

func (visitor *encodeVisitorV4) VisitSenderCapacityObserved(event clievent.SenderCapacityObserved) error {
	stage, err := nameOf(event.Stage())
	if err != nil {
		return err
	}
	payload := senderCapacityPayloadV4{Stage: stage, Process: projectCapacityScopeV4(event.ProcessSnapshot())}
	if decision, ok := event.DecisionID(); ok {
		encoded := decision.Hex()
		payload.DecisionID = &encoded
	}
	if revision, ok := event.RevisionID(); ok {
		encoded := revision.Hex()
		payload.RevisionID = &encoded
	}
	if share, ok := event.ShareSnapshot(); ok {
		projected := projectCapacityScopeV4(share)
		payload.Share = &projected
	}
	if sessionScope, ok := event.SessionSnapshot(); ok {
		projected := projectCapacityScopeV4(sessionScope)
		payload.Session = &projected
	}
	var correlation *CorrelationV1
	if session, ok := event.ProtocolSessionID(); ok {
		correlation, err = projectSessionCorrelation(session, clievent.LaneIdentity{}, false)
		if err != nil {
			return err
		}
	}
	visitor.set("sender_capacity", correlation, payload)
	return nil
}

func projectCapacityScopeV4(value clievent.CapacityScopeSnapshot) capacityScopeV4 {
	return capacityScopeV4{
		StableHandles: decimal(value.StableHandles), ActiveLeases: decimal(value.ActiveLeases),
		StableHandleLimit: decimal(value.StableHandleLimit), ActiveLeaseLimit: decimal(value.ActiveLeaseLimit),
		ReclaimableStableHandles: decimal(value.ReclaimableStableHandles),
		QuarantinedStableHandles: decimal(value.QuarantinedStableHandles),
		PendingAdmissions:        decimal(value.PendingAdmissions), ActiveReclaims: decimal(value.ActiveReclaims),
	}
}

func (visitor *encodeVisitorV4) VisitSenderRevisionObserved(event clievent.SenderRevisionObserved) error {
	stage, err := nameOf(event.Stage())
	if err != nil {
		return err
	}
	cause, err := nameOf(event.Cause())
	if err != nil {
		return err
	}
	payload := senderRevisionPayloadV4{Stage: stage, Cause: cause, RevisionID: event.RevisionID().Hex()}
	var correlation *CorrelationV1
	if lease, ok := event.LeaseID(); ok {
		encoded := lease.Hex()
		payload.LeaseID = &encoded
		session, sessionOK := event.ProtocolSessionID()
		if !sessionOK {
			return errInvalidSchemaEvent
		}
		correlation, err = projectSessionCorrelation(session, clievent.LaneIdentity{}, false)
		if err != nil {
			return err
		}
	}
	visitor.set("sender_revision", correlation, payload)
	return nil
}
