package commandprojection

import (
	"encoding/base64"
	"fmt"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/revisioncapacity"
)

func ProjectSenderCapacity(value revisioncapacity.TraceEvent) (clievent.SenderCapacityObserved, error) {
	stage, ok := projectSenderCapacityStage(value.Stage())
	if !ok {
		return clievent.SenderCapacityObserved{}, ErrInvalidProjection
	}
	process, ok := projectCapacityScope(value.Snapshot().Process(), revisioncapacity.CapacityScopeProcess)
	if !ok {
		return clievent.SenderCapacityObserved{}, ErrInvalidProjection
	}
	spec := clievent.SenderCapacitySpec{Stage: stage, Process: process}
	share := value.Snapshot().Share()
	if share.Scope() != 0 {
		spec.Share, ok = projectCapacityScope(share, revisioncapacity.CapacityScopeShare)
		if !ok {
			return clievent.SenderCapacityObserved{}, ErrInvalidProjection
		}
		spec.HasShare = true
	}
	if value.DecisionID() != "" {
		spec.Decision, _ = clievent.NewCapacityDecisionID(string(value.DecisionID()))
	}
	if value.RevisionID() != "" {
		spec.Revision, _ = clievent.NewSenderRevisionID([]byte(value.RevisionID()))
	}
	if value.SessionID() != "" {
		spec.Session, ok = projectRevisionCapacitySession(value.SessionID())
		if !ok {
			return clievent.SenderCapacityObserved{}, ErrInvalidProjection
		}
		for _, snapshot := range value.Snapshot().Sessions() {
			if snapshot.Identity() != string(value.SessionID()) {
				continue
			}
			spec.SessionScope, ok = projectCapacityScope(snapshot, revisioncapacity.CapacityScopeSession)
			spec.HasSessionScope = ok
			break
		}
		if !spec.HasSessionScope {
			return clievent.SenderCapacityObserved{}, ErrInvalidProjection
		}
	}
	event, err := clievent.NewSenderCapacityObserved(spec)
	if err != nil {
		return clievent.SenderCapacityObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func projectCapacityScope(
	value revisioncapacity.ScopeSnapshot,
	want revisioncapacity.CapacityScope,
) (clievent.CapacityScopeSnapshot, bool) {
	if value.Scope() != want {
		return clievent.CapacityScopeSnapshot{}, false
	}
	limits := value.Limits()
	used := value.Used()
	result := clievent.CapacityScopeSnapshot{
		StableHandles: used.StableHandles, ActiveLeases: used.ActiveLeases,
		StableHandleLimit: limits.StableHandles, ActiveLeaseLimit: limits.ActiveLeases,
		ReclaimableStableHandles: value.ReclaimableStableHandles(),
		QuarantinedStableHandles: value.QuarantinedStableHandles(),
		PendingAdmissions:        value.PendingAdmissions(), ActiveReclaims: value.ActiveReclaims(),
	}
	return result, result.Valid()
}

func ProjectSenderRevision(value content.RevisionTrace) (clievent.SenderRevisionObserved, error) {
	stage, ok := projectSenderRevisionStage(value.Stage())
	if !ok {
		return clievent.SenderRevisionObserved{}, ErrInvalidProjection
	}
	cause, ok := projectSenderRevisionCause(value.Cause())
	if !ok {
		return clievent.SenderRevisionObserved{}, ErrInvalidProjection
	}
	if value.ShareInstance().IsZero() || value.FileID().IsZero() || value.FileRevision().IsZero() {
		return clievent.SenderRevisionObserved{}, ErrInvalidProjection
	}
	// Capacity ownership uses this exact transport-neutral tuple. Hashing the
	// same canonical form keeps coordinator and revision-store traces joinable.
	revisionMaterial := []byte(fmt.Sprintf("%x:%x", value.FileID().Bytes(), value.FileRevision().Bytes()))
	revision, err := clievent.NewSenderRevisionID(revisionMaterial)
	if err != nil {
		return clievent.SenderRevisionObserved{}, ErrInvalidProjection
	}
	var lease clievent.RevisionLeaseID
	var session clievent.ProtocolSessionID
	if !value.LeaseID().IsZero() || value.SessionID() != "" {
		lease, err = clievent.NewRevisionLeaseID(value.LeaseID().Bytes())
		if err != nil {
			return clievent.SenderRevisionObserved{}, ErrInvalidProjection
		}
		session, ok = projectRevisionCapacitySession(value.SessionID())
		if !ok {
			return clievent.SenderRevisionObserved{}, ErrInvalidProjection
		}
	}
	event, err := clievent.NewSenderRevisionObserved(stage, cause, revision, lease, session)
	if err != nil {
		return clievent.SenderRevisionObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func projectRevisionCapacitySession(value revisioncapacity.SessionID) (clievent.ProtocolSessionID, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(string(value))
	if err != nil {
		return clievent.ProtocolSessionID{}, false
	}
	session, err := clievent.NewProtocolSessionID(raw)
	return session, err == nil
}

func projectSenderCapacityStage(value revisioncapacity.TraceStage) (clievent.SenderCapacityStage, bool) {
	stages := map[revisioncapacity.TraceStage]clievent.SenderCapacityStage{
		revisioncapacity.TraceAdmissionGranted:     clievent.SenderCapacityAdmissionGranted,
		revisioncapacity.TraceAdmissionDenied:      clievent.SenderCapacityAdmissionDenied,
		revisioncapacity.TraceAdmissionCancelled:   clievent.SenderCapacityAdmissionCanceled,
		revisioncapacity.TraceReclaimClaimed:       clievent.SenderCapacityReclaimClaimed,
		revisioncapacity.TraceReclaimDeclined:      clievent.SenderCapacityReclaimDeclined,
		revisioncapacity.TraceReclaimCompleted:     clievent.SenderCapacityReclaimCompleted,
		revisioncapacity.TraceReclaimQuarantined:   clievent.SenderCapacityReclaimQuarantined,
		revisioncapacity.TraceIdlePublished:        clievent.SenderCapacityIdlePublished,
		revisioncapacity.TraceIdleWithdrawn:        clievent.SenderCapacityIdleWithdrawn,
		revisioncapacity.TraceOwnershipQuarantined: clievent.SenderCapacityOwnershipQuarantined,
	}
	stage, ok := stages[value]
	return stage, ok
}

func projectSenderRevisionStage(value content.RevisionTraceStage) (clievent.SenderRevisionStage, bool) {
	stages := map[content.RevisionTraceStage]clievent.SenderRevisionStage{
		content.RevisionTraceStageActiveReuse:           clievent.SenderRevisionActiveReuse,
		content.RevisionTraceStageCleanRelease:          clievent.SenderRevisionCleanRelease,
		content.RevisionTraceStageReopenMatch:           clievent.SenderRevisionReopenMatch,
		content.RevisionTraceStageMismatchInvalidation:  clievent.SenderRevisionMismatchInvalidation,
		content.RevisionTraceStageUnavailableRetry:      clievent.SenderRevisionUnavailableRetry,
		content.RevisionTraceStageInvalidationRejection: clievent.SenderRevisionInvalidationRejection,
		content.RevisionTraceStageMetadataBudgetStop:    clievent.SenderRevisionMetadataBudgetStop,
		content.RevisionTraceStageLeaseSettlement:       clievent.SenderRevisionLeaseSettlement,
	}
	stage, ok := stages[value]
	return stage, ok
}

func projectSenderRevisionCause(value content.RevisionTraceCause) (clievent.SenderRevisionCause, bool) {
	causes := map[content.RevisionTraceCause]clievent.SenderRevisionCause{
		content.RevisionTraceCauseUnknown:           clievent.SenderRevisionCauseNone,
		content.RevisionTraceCauseCatalog:           clievent.SenderRevisionCauseCatalog,
		content.RevisionTraceCauseSourceOpen:        clievent.SenderRevisionCauseSourceOpen,
		content.RevisionTraceCauseVerification:      clievent.SenderRevisionCauseVerification,
		content.RevisionTraceCauseGeometry:          clievent.SenderRevisionCauseGeometry,
		content.RevisionTraceCauseModifiedTime:      clievent.SenderRevisionCauseModifiedTime,
		content.RevisionTraceCauseActiveRead:        clievent.SenderRevisionCauseActiveRead,
		content.RevisionTraceCausePanic:             clievent.SenderRevisionCausePanic,
		content.RevisionTraceCauseKnownInvalidation: clievent.SenderRevisionCauseKnownInvalidation,
		content.RevisionTraceCauseMetadataBudget:    clievent.SenderRevisionCauseMetadataBudget,
		content.RevisionTraceCauseRelinquished:      clievent.SenderRevisionCauseRelinquished,
		content.RevisionTraceCauseUndelivered:       clievent.SenderRevisionCauseUndelivered,
		content.RevisionTraceCauseDetached:          clievent.SenderRevisionCauseDetached,
		content.RevisionTraceCauseCapacity:          clievent.SenderRevisionCauseCapacity,
	}
	cause, ok := causes[value]
	return cause, ok
}
