package content

import (
	"strconv"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
)

type RevisionTraceStage uint8

const (
	RevisionTraceStageUnknown RevisionTraceStage = iota
	RevisionTraceStageActiveReuse
	RevisionTraceStageCleanRelease
	RevisionTraceStageReopenMatch
	RevisionTraceStageMismatchInvalidation
	RevisionTraceStageUnavailableRetry
	RevisionTraceStageInvalidationRejection
	RevisionTraceStageMetadataBudgetStop
	RevisionTraceStageLeaseSettlement
	RevisionTraceStageOpenHandleBound
	RevisionTraceStageOpenRejected
)

type RevisionTraceCause uint8

const (
	RevisionTraceCauseUnknown RevisionTraceCause = iota
	RevisionTraceCauseCatalog
	RevisionTraceCauseSourceOpen
	RevisionTraceCauseVerification
	RevisionTraceCauseGeometry
	RevisionTraceCauseModifiedTime
	RevisionTraceCauseActiveRead
	RevisionTraceCausePanic
	RevisionTraceCauseKnownInvalidation
	RevisionTraceCauseMetadataBudget
	RevisionTraceCauseRelinquished
	RevisionTraceCauseUndelivered
	RevisionTraceCauseDetached
	RevisionTraceCauseCapacity
)

type RevisionTrace struct {
	stage         RevisionTraceStage
	cause         RevisionTraceCause
	shareInstance catalog.ShareInstance
	fileID        catalog.FileID
	fileRevision  FileRevision
	leaseID       LeaseID
	sessionID     revisioncapacity.SessionID
}

func (t RevisionTrace) Stage() RevisionTraceStage             { return t.stage }
func (t RevisionTrace) Cause() RevisionTraceCause             { return t.cause }
func (t RevisionTrace) ShareInstance() catalog.ShareInstance  { return t.shareInstance }
func (t RevisionTrace) FileID() catalog.FileID                { return t.fileID }
func (t RevisionTrace) FileRevision() FileRevision            { return t.fileRevision }
func (t RevisionTrace) LeaseID() LeaseID                      { return t.leaseID }
func (t RevisionTrace) SessionID() revisioncapacity.SessionID { return t.sessionID }

type RevisionTracer interface {
	TraceRevision(RevisionTrace)
}

type RevisionTracerFunc func(RevisionTrace)

func (f RevisionTracerFunc) TraceRevision(event RevisionTrace) { f(event) }

func (s *RevisionStore) traceRevision(stage RevisionTraceStage, cause RevisionTraceCause, file catalog.FileID, revision FileRevision) {
	if s == nil {
		return
	}
	s.traceRevisionEvent(RevisionTrace{
		stage: stage, cause: cause, shareInstance: s.shareInstance, fileID: file, fileRevision: revision,
	})
}

func (s *RevisionStore) traceLeaseRevision(
	stage RevisionTraceStage,
	cause RevisionTraceCause,
	file catalog.FileID,
	revision FileRevision,
	lease LeaseID,
	session revisioncapacity.SessionID,
) {
	if s == nil {
		return
	}
	s.traceRevisionEvent(RevisionTrace{
		stage: stage, cause: cause, shareInstance: s.shareInstance, fileID: file, fileRevision: revision,
		leaseID: lease, sessionID: session,
	})
}

func (s *RevisionStore) traceRevisionEvent(event RevisionTrace) {
	if s == nil || s.tracer == nil {
		return
	}
	defer func() { _ = recover() }()
	s.tracer.TraceRevision(event)
}

// DiagnosticName belongs to the source vocabulary; recorders accept new values.
func (value RevisionTraceStage) DiagnosticName() string {
	switch value {
	case RevisionTraceStageActiveReuse:
		return "active_reuse"
	case RevisionTraceStageCleanRelease:
		return "clean_release"
	case RevisionTraceStageReopenMatch:
		return "reopen_match"
	case RevisionTraceStageMismatchInvalidation:
		return "mismatch_invalidation"
	case RevisionTraceStageUnavailableRetry:
		return "unavailable_retry"
	case RevisionTraceStageInvalidationRejection:
		return "invalidation_rejection"
	case RevisionTraceStageMetadataBudgetStop:
		return "metadata_budget_stop"
	case RevisionTraceStageLeaseSettlement:
		return "lease_settlement"
	case RevisionTraceStageOpenHandleBound:
		return "open_handle_bound"
	case RevisionTraceStageOpenRejected:
		return "open_rejected"
	default:
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
}

// DiagnosticName belongs to the source vocabulary; recorders accept new values.
func (value RevisionTraceCause) DiagnosticName() string {
	switch value {
	case RevisionTraceCauseUnknown:
		return "none"
	case RevisionTraceCauseCatalog:
		return "catalog"
	case RevisionTraceCauseSourceOpen:
		return "source_open"
	case RevisionTraceCauseVerification:
		return "verification"
	case RevisionTraceCauseGeometry:
		return "geometry"
	case RevisionTraceCauseModifiedTime:
		return "modified_time"
	case RevisionTraceCauseActiveRead:
		return "active_read"
	case RevisionTraceCausePanic:
		return "panic"
	case RevisionTraceCauseKnownInvalidation:
		return "known_invalidation"
	case RevisionTraceCauseMetadataBudget:
		return "metadata_budget"
	case RevisionTraceCauseRelinquished:
		return "relinquished"
	case RevisionTraceCauseUndelivered:
		return "undelivered"
	case RevisionTraceCauseDetached:
		return "detached"
	case RevisionTraceCauseCapacity:
		return "capacity"
	default:
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
}
