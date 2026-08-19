package content

import "github.com/windshare/windshare/core/catalog"

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
)

type RevisionTrace struct {
	stage         RevisionTraceStage
	cause         RevisionTraceCause
	shareInstance catalog.ShareInstance
	fileID        catalog.FileID
	fileRevision  FileRevision
}

func (t RevisionTrace) Stage() RevisionTraceStage            { return t.stage }
func (t RevisionTrace) Cause() RevisionTraceCause            { return t.cause }
func (t RevisionTrace) ShareInstance() catalog.ShareInstance { return t.shareInstance }
func (t RevisionTrace) FileID() catalog.FileID               { return t.fileID }
func (t RevisionTrace) FileRevision() FileRevision           { return t.fileRevision }

type RevisionTracer interface {
	TraceRevision(RevisionTrace)
}

type RevisionTracerFunc func(RevisionTrace)

func (f RevisionTracerFunc) TraceRevision(event RevisionTrace) { f(event) }

func (s *RevisionStore) traceRevision(stage RevisionTraceStage, cause RevisionTraceCause, file catalog.FileID, revision FileRevision) {
	if s == nil || s.tracer == nil {
		return
	}
	defer func() { _ = recover() }()
	s.tracer.TraceRevision(RevisionTrace{
		stage: stage, cause: cause, shareInstance: s.shareInstance, fileID: file, fileRevision: revision,
	})
}
