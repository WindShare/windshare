package eventproject

import (
	"encoding/base64"
	"fmt"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	appobs "github.com/windshare/windshare/engine/observation"
)

func ProjectSenderCapacity(value revisioncapacity.TraceEvent) (appobs.SenderCapacityObserved, error) {
	stage, ok := projectSenderCapacityStage(value.Stage())
	if !ok {
		return appobs.SenderCapacityObserved{}, ErrInvalidProjection
	}
	process, ok := projectCapacityScope(value.Snapshot().Process(), revisioncapacity.CapacityScopeProcess)
	if !ok {
		return appobs.SenderCapacityObserved{}, ErrInvalidProjection
	}
	spec := appobs.SenderCapacitySpec{Stage: stage, Process: process}
	share := value.Snapshot().Share()
	if share.Scope() != 0 {
		spec.Share, ok = projectCapacityScope(share, revisioncapacity.CapacityScopeShare)
		if !ok {
			return appobs.SenderCapacityObserved{}, ErrInvalidProjection
		}
		spec.HasShare = true
	}
	if value.DecisionID() != "" {
		spec.Decision, _ = appobs.NewCapacityDecisionID(string(value.DecisionID()))
	}
	if value.RevisionID() != "" {
		spec.Revision, _ = appobs.NewSenderRevisionID([]byte(value.RevisionID()))
	}
	if value.SessionID() != "" {
		spec.Session, ok = projectRevisionCapacitySession(value.SessionID())
		if !ok {
			return appobs.SenderCapacityObserved{}, ErrInvalidProjection
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
			return appobs.SenderCapacityObserved{}, ErrInvalidProjection
		}
	}
	event, err := appobs.NewSenderCapacityObserved(spec)
	if err != nil {
		return appobs.SenderCapacityObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func projectCapacityScope(
	value revisioncapacity.ScopeSnapshot,
	want revisioncapacity.CapacityScope,
) (appobs.CapacityScopeSnapshot, bool) {
	if value.Scope() != want {
		return appobs.CapacityScopeSnapshot{}, false
	}
	limits := value.Limits()
	used := value.Used()
	result := appobs.CapacityScopeSnapshot{
		StableHandles: used.StableHandles, ActiveLeases: used.ActiveLeases,
		StableHandleLimit: limits.StableHandles, ActiveLeaseLimit: limits.ActiveLeases,
		ReclaimableStableHandles: value.ReclaimableStableHandles(),
		QuarantinedStableHandles: value.QuarantinedStableHandles(),
		PendingAdmissions:        value.PendingAdmissions(), ActiveReclaims: value.ActiveReclaims(),
	}
	return result, result.Valid()
}

func ProjectSenderRevision(value content.RevisionTrace) (appobs.SenderRevisionObserved, error) {
	event, err := projectSenderRevision(value)
	if err == nil {
		return event, nil
	}
	context := appobs.ObservationRejection{
		Event: "sender_revision", Source: "eventproject.ProjectSenderRevision", Stage: "unknown",
		Session:  string(value.SessionID()),
		Revision: fmt.Sprintf("%x:%x", value.FileID().Bytes(), value.FileRevision().Bytes()),
	}
	if stage, ok := projectSenderRevisionStage(value.Stage()); ok {
		context.Stage, _ = stage.Name()
	}
	context = appobs.CaptureObservationRejection(context,
		appobs.RejectedEnum("stage", uint64(value.Stage())),
		appobs.RejectedEnum("cause", uint64(value.Cause())),
		appobs.RejectedIdentity("share_instance", value.ShareInstance().Bytes()),
		appobs.RejectedIdentity("file_id", value.FileID().Bytes()),
		appobs.RejectedIdentity("file_revision", value.FileRevision().Bytes()),
		appobs.RejectedIdentity("lease_id", value.LeaseID().Bytes()),
		appobs.RejectedString("protocol_session_id", string(value.SessionID())),
	)
	return event, withRejectionContext(err, context)
}

func projectSenderRevision(value content.RevisionTrace) (appobs.SenderRevisionObserved, error) {
	stage, ok := projectSenderRevisionStage(value.Stage())
	if !ok {
		return appobs.SenderRevisionObserved{}, rejectedProjection(ProjectionUnknownEnum, "stage", "known_enum")
	}
	cause, ok := projectSenderRevisionCause(value.Cause())
	if !ok {
		return appobs.SenderRevisionObserved{}, rejectedProjection(ProjectionUnknownEnum, "cause", "known_enum")
	}
	switch {
	case value.ShareInstance().IsZero():
		return appobs.SenderRevisionObserved{}, rejectedProjection(ProjectionInvalidIdentity, "share_instance", "nonzero")
	case value.FileID().IsZero():
		return appobs.SenderRevisionObserved{}, rejectedProjection(ProjectionInvalidIdentity, "file_id", "nonzero")
	case value.FileRevision().IsZero():
		return appobs.SenderRevisionObserved{}, rejectedProjection(ProjectionInvalidIdentity, "file_revision", "nonzero")
	}
	// Capacity ownership uses this exact transport-neutral tuple. Hashing the
	// same canonical form keeps coordinator and revision-store traces joinable.
	revisionMaterial := fmt.Appendf(nil, "%x:%x", value.FileID().Bytes(), value.FileRevision().Bytes())
	revision, err := appobs.NewSenderRevisionID(revisionMaterial)
	if err != nil {
		return appobs.SenderRevisionObserved{}, rejectedProjection(ProjectionInvalidIdentity, "revision_id", "valid_revision_material")
	}
	var lease appobs.RevisionLeaseID
	var session appobs.ProtocolSessionID
	if !value.LeaseID().IsZero() || value.SessionID() != "" {
		lease, err = appobs.NewRevisionLeaseID(value.LeaseID().Bytes())
		if err != nil {
			return appobs.SenderRevisionObserved{}, rejectedProjection(ProjectionInvalidIdentity, "lease_id", "nonzero_16_bytes")
		}
		session, ok = projectRevisionCapacitySession(value.SessionID())
		if !ok {
			return appobs.SenderRevisionObserved{}, rejectedProjection(ProjectionInvalidIdentity, "protocol_session_id", "base64url_nonzero_16_bytes")
		}
	}
	event, err := appobs.NewSenderRevisionObserved(stage, cause, revision, lease, session)
	if err != nil {
		return appobs.SenderRevisionObserved{}, err
	}
	return event, nil
}

func projectRevisionCapacitySession(value revisioncapacity.SessionID) (appobs.ProtocolSessionID, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(string(value))
	if err != nil {
		return appobs.ProtocolSessionID{}, false
	}
	session, err := appobs.NewProtocolSessionID(raw)
	return session, err == nil
}

func projectSenderCapacityStage(value revisioncapacity.TraceStage) (appobs.SenderCapacityStage, bool) {
	stages := map[revisioncapacity.TraceStage]appobs.SenderCapacityStage{
		revisioncapacity.TraceAdmissionGranted:     appobs.SenderCapacityAdmissionGranted,
		revisioncapacity.TraceAdmissionDenied:      appobs.SenderCapacityAdmissionDenied,
		revisioncapacity.TraceAdmissionCancelled:   appobs.SenderCapacityAdmissionCanceled,
		revisioncapacity.TraceReclaimClaimed:       appobs.SenderCapacityReclaimClaimed,
		revisioncapacity.TraceReclaimDeclined:      appobs.SenderCapacityReclaimDeclined,
		revisioncapacity.TraceReclaimCompleted:     appobs.SenderCapacityReclaimCompleted,
		revisioncapacity.TraceReclaimQuarantined:   appobs.SenderCapacityReclaimQuarantined,
		revisioncapacity.TraceIdlePublished:        appobs.SenderCapacityIdlePublished,
		revisioncapacity.TraceIdleWithdrawn:        appobs.SenderCapacityIdleWithdrawn,
		revisioncapacity.TraceOwnershipQuarantined: appobs.SenderCapacityOwnershipQuarantined,
	}
	stage, ok := stages[value]
	return stage, ok
}

func projectSenderRevisionStage(value content.RevisionTraceStage) (appobs.SenderRevisionStage, bool) {
	stages := map[content.RevisionTraceStage]appobs.SenderRevisionStage{
		content.RevisionTraceStageActiveReuse:           appobs.SenderRevisionActiveReuse,
		content.RevisionTraceStageCleanRelease:          appobs.SenderRevisionCleanRelease,
		content.RevisionTraceStageReopenMatch:           appobs.SenderRevisionReopenMatch,
		content.RevisionTraceStageMismatchInvalidation:  appobs.SenderRevisionMismatchInvalidation,
		content.RevisionTraceStageUnavailableRetry:      appobs.SenderRevisionUnavailableRetry,
		content.RevisionTraceStageInvalidationRejection: appobs.SenderRevisionInvalidationRejection,
		content.RevisionTraceStageMetadataBudgetStop:    appobs.SenderRevisionMetadataBudgetStop,
		content.RevisionTraceStageLeaseSettlement:       appobs.SenderRevisionLeaseSettlement,
		content.RevisionTraceStageOpenHandleBound:       appobs.SenderRevisionOpenHandleBound,
		content.RevisionTraceStageOpenRejected:          appobs.SenderRevisionOpenRejected,
	}
	stage, ok := stages[value]
	return stage, ok
}

func projectSenderRevisionCause(value content.RevisionTraceCause) (appobs.SenderRevisionCause, bool) {
	causes := map[content.RevisionTraceCause]appobs.SenderRevisionCause{
		content.RevisionTraceCauseUnknown:           appobs.SenderRevisionCauseNone,
		content.RevisionTraceCauseCatalog:           appobs.SenderRevisionCauseCatalog,
		content.RevisionTraceCauseSourceOpen:        appobs.SenderRevisionCauseSourceOpen,
		content.RevisionTraceCauseVerification:      appobs.SenderRevisionCauseVerification,
		content.RevisionTraceCauseGeometry:          appobs.SenderRevisionCauseGeometry,
		content.RevisionTraceCauseModifiedTime:      appobs.SenderRevisionCauseModifiedTime,
		content.RevisionTraceCauseActiveRead:        appobs.SenderRevisionCauseActiveRead,
		content.RevisionTraceCausePanic:             appobs.SenderRevisionCausePanic,
		content.RevisionTraceCauseKnownInvalidation: appobs.SenderRevisionCauseKnownInvalidation,
		content.RevisionTraceCauseMetadataBudget:    appobs.SenderRevisionCauseMetadataBudget,
		content.RevisionTraceCauseRelinquished:      appobs.SenderRevisionCauseRelinquished,
		content.RevisionTraceCauseUndelivered:       appobs.SenderRevisionCauseUndelivered,
		content.RevisionTraceCauseDetached:          appobs.SenderRevisionCauseDetached,
		content.RevisionTraceCauseCapacity:          appobs.SenderRevisionCauseCapacity,
	}
	cause, ok := causes[value]
	return cause, ok
}
