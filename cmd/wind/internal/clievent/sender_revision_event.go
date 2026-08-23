package clievent

type SenderCapacityStage uint8

const (
	SenderCapacityAdmissionGranted SenderCapacityStage = iota + 1
	SenderCapacityAdmissionDenied
	SenderCapacityAdmissionCanceled
	SenderCapacityReclaimClaimed
	SenderCapacityReclaimDeclined
	SenderCapacityReclaimCompleted
	SenderCapacityReclaimQuarantined
	SenderCapacityIdlePublished
	SenderCapacityIdleWithdrawn
	SenderCapacityOwnershipQuarantined
)

func (stage SenderCapacityStage) Name() (string, bool) {
	names := [...]string{
		"", "admission_granted", "admission_denied", "admission_canceled",
		"reclaim_claimed", "reclaim_declined", "reclaim_completed", "reclaim_quarantined",
		"idle_published", "idle_withdrawn", "ownership_quarantined",
	}
	if stage == 0 || int(stage) >= len(names) {
		return "", false
	}
	return names[stage], true
}

type CapacityScopeSnapshot struct {
	StableHandles            uint64
	ActiveLeases             uint64
	StableHandleLimit        uint64
	ActiveLeaseLimit         uint64
	ReclaimableStableHandles uint64
	QuarantinedStableHandles uint64
	PendingAdmissions        uint64
	ActiveReclaims           uint64
}

func (snapshot CapacityScopeSnapshot) Valid() bool {
	return snapshot.StableHandleLimit > 0 && snapshot.ActiveLeaseLimit > 0 &&
		snapshot.StableHandles <= snapshot.StableHandleLimit &&
		snapshot.ActiveLeases <= snapshot.ActiveLeaseLimit
}

type SenderCapacitySpec struct {
	Stage           SenderCapacityStage
	Decision        CapacityDecisionID
	Session         ProtocolSessionID
	Revision        SenderRevisionID
	Process         CapacityScopeSnapshot
	Share           CapacityScopeSnapshot
	SessionScope    CapacityScopeSnapshot
	HasShare        bool
	HasSessionScope bool
}

type SenderCapacityObserved struct{ spec SenderCapacitySpec }

func NewSenderCapacityObserved(spec SenderCapacitySpec) (SenderCapacityObserved, error) {
	if !validSenderCapacitySpec(spec) {
		return SenderCapacityObserved{}, ErrInvalidEvent
	}
	return SenderCapacityObserved{spec: spec}, nil
}

func validSenderCapacitySpec(spec SenderCapacitySpec) bool {
	if _, ok := spec.Stage.Name(); !ok || !spec.Process.Valid() {
		return false
	}
	if spec.HasShare != spec.Share.Valid() || spec.HasSessionScope != spec.SessionScope.Valid() {
		return false
	}
	if spec.Session.Valid() != spec.HasSessionScope || spec.Session.Valid() && !spec.HasShare {
		return false
	}
	return true
}

func (SenderCapacityObserved) event()                           {}
func (SenderCapacityObserved) Command() Command                 { return CommandShare }
func (SenderCapacityObserved) Level() Level                     { return LevelDebug }
func (event SenderCapacityObserved) Stage() SenderCapacityStage { return event.spec.Stage }
func (event SenderCapacityObserved) DecisionID() (CapacityDecisionID, bool) {
	return event.spec.Decision, event.spec.Decision.Valid()
}
func (event SenderCapacityObserved) ProtocolSessionID() (ProtocolSessionID, bool) {
	return event.spec.Session, event.spec.Session.Valid()
}
func (event SenderCapacityObserved) RevisionID() (SenderRevisionID, bool) {
	return event.spec.Revision, event.spec.Revision.Valid()
}
func (event SenderCapacityObserved) ProcessSnapshot() CapacityScopeSnapshot {
	return event.spec.Process
}
func (event SenderCapacityObserved) ShareSnapshot() (CapacityScopeSnapshot, bool) {
	return event.spec.Share, event.spec.HasShare
}
func (event SenderCapacityObserved) SessionSnapshot() (CapacityScopeSnapshot, bool) {
	return event.spec.SessionScope, event.spec.HasSessionScope
}
func (event SenderCapacityObserved) Accept(visitor Visitor) error {
	return acceptSenderCapacityObserved(visitor, event)
}

type SenderRevisionStage uint8

const (
	SenderRevisionActiveReuse SenderRevisionStage = iota + 1
	SenderRevisionCleanRelease
	SenderRevisionReopenMatch
	SenderRevisionMismatchInvalidation
	SenderRevisionUnavailableRetry
	SenderRevisionInvalidationRejection
	SenderRevisionMetadataBudgetStop
	SenderRevisionLeaseSettlement
)

func (stage SenderRevisionStage) Name() (string, bool) {
	names := [...]string{
		"", "active_reuse", "clean_release", "reopen_match", "mismatch_invalidation",
		"unavailable_retry", "invalidation_rejection", "metadata_budget_stop", "lease_settlement",
	}
	if stage == 0 || int(stage) >= len(names) {
		return "", false
	}
	return names[stage], true
}

type SenderRevisionCause uint8

const (
	SenderRevisionCauseNone SenderRevisionCause = iota + 1
	SenderRevisionCauseCatalog
	SenderRevisionCauseSourceOpen
	SenderRevisionCauseVerification
	SenderRevisionCauseGeometry
	SenderRevisionCauseModifiedTime
	SenderRevisionCauseActiveRead
	SenderRevisionCausePanic
	SenderRevisionCauseKnownInvalidation
	SenderRevisionCauseMetadataBudget
	SenderRevisionCauseRelinquished
	SenderRevisionCauseUndelivered
	SenderRevisionCauseDetached
	SenderRevisionCauseCapacity
)

func (cause SenderRevisionCause) Name() (string, bool) {
	names := [...]string{
		"", "none", "catalog", "source_open", "verification", "geometry", "modified_time",
		"active_read", "panic", "known_invalidation", "metadata_budget", "relinquished",
		"undelivered", "detached", "capacity",
	}
	if cause == 0 || int(cause) >= len(names) {
		return "", false
	}
	return names[cause], true
}

type SenderRevisionObserved struct {
	stage    SenderRevisionStage
	cause    SenderRevisionCause
	revision SenderRevisionID
	lease    RevisionLeaseID
	session  ProtocolSessionID
}

func NewSenderRevisionObserved(
	stage SenderRevisionStage,
	cause SenderRevisionCause,
	revision SenderRevisionID,
	lease RevisionLeaseID,
	session ProtocolSessionID,
) (SenderRevisionObserved, error) {
	if !validSenderRevisionObserved(stage, cause, revision, lease, session) {
		return SenderRevisionObserved{}, ErrInvalidEvent
	}
	return SenderRevisionObserved{stage: stage, cause: cause, revision: revision, lease: lease, session: session}, nil
}

func validSenderRevisionObserved(
	stage SenderRevisionStage,
	cause SenderRevisionCause,
	revision SenderRevisionID,
	lease RevisionLeaseID,
	session ProtocolSessionID,
) bool {
	_, stageOK := stage.Name()
	_, causeOK := cause.Name()
	leaseCorrelated := lease.Valid() || session.Valid()
	return stageOK && causeOK && revision.Valid() && lease.Valid() == session.Valid() &&
		(stage != SenderRevisionLeaseSettlement || leaseCorrelated)
}

func (SenderRevisionObserved) event()                             {}
func (SenderRevisionObserved) Command() Command                   { return CommandShare }
func (SenderRevisionObserved) Level() Level                       { return LevelDebug }
func (event SenderRevisionObserved) Stage() SenderRevisionStage   { return event.stage }
func (event SenderRevisionObserved) Cause() SenderRevisionCause   { return event.cause }
func (event SenderRevisionObserved) RevisionID() SenderRevisionID { return event.revision }
func (event SenderRevisionObserved) LeaseID() (RevisionLeaseID, bool) {
	return event.lease, event.lease.Valid()
}
func (event SenderRevisionObserved) ProtocolSessionID() (ProtocolSessionID, bool) {
	return event.session, event.session.Valid()
}
func (event SenderRevisionObserved) Accept(visitor Visitor) error {
	return acceptSenderRevisionObserved(visitor, event)
}
