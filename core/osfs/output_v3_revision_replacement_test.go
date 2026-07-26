package osfs

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3RevisionOnlyMismatchRetiresOldAuthorityWithoutRangeReuse(t *testing.T) {
	payload := []byte("old-revision-checkpoint")
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	sessionIDs := &v3RecoverySessionIDs{}
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	oldFile := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	oldTransaction := v3RecoveryBeginTransaction(t, opened.Session, oldFile).(*filesystemFileTransaction)
	if err := oldTransaction.WriteRange(context.Background(), 0, payload[:8]); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := oldTransaction.Checkpoint(context.Background())
	if err != nil || checkpoint.Ranges().IsEmpty() {
		t.Fatalf("old checkpoint = (%v, %v)", checkpoint.Ranges().Ranges(), err)
	}
	oldRecord := oldTransaction.resumable.Bound().Record()
	outputV3AbandonTransaction(t, oldTransaction)
	v3RecoveryCloseSession(t, opened.Session)

	reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, reopened.Session) })
	current := outputV3NextRevisionFile(t, reopened.Session, oldFile)
	start, err := reopened.Session.BeginFile(context.Background(), current)
	if err != nil {
		t.Fatal(err)
	}
	transaction, durable, ok := start.Transaction()
	if !ok || !durable.Ranges().IsEmpty() {
		t.Fatalf("replacement start = (transaction=%t, ranges=%v)", ok, durable.Ranges().Ranges())
	}
	replacement := transaction.(*filesystemFileTransaction)
	newRecord := replacement.resumable.Bound().Record()
	if newRecord.Revision() != current.Descriptor.FileRevision() ||
		newRecord.OutputObject() == oldRecord.OutputObject() || newRecord.CheckpointGeneration() != 0 {
		t.Fatalf(
			"replacement record = (revision=%v, object=%v, checkpoint=%d), old object=%v",
			newRecord.Revision(), newRecord.OutputObject(), newRecord.CheckpointGeneration(), oldRecord.OutputObject(),
		)
	}
	outputV3AssertObjectNamesAbsent(t, root, selection, reopened.Session.SessionID(), oldRecord)
	outputV3AbandonTransaction(t, replacement)
}

func TestOutputV3PublishingRevisionMismatchUsesPhaseEvidence(t *testing.T) {
	payload := []byte("complete-old-revision")
	for _, finalState := range []string{"missing", "matching", "foreign"} {
		t.Run(finalState, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, true, uint64(len(payload)))
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			oldFile := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
			oldRecord := v3RecoveryPreparePublishingCut(t, opened.Session, oldFile, payload, finalState)
			v3RecoveryCloseSession(t, opened.Session)

			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			t.Cleanup(func() { v3RecoveryCloseSession(t, reopened.Session) })
			current := outputV3NextRevisionFile(t, reopened.Session, oldFile)
			start, err := reopened.Session.BeginFile(context.Background(), current)
			if err != nil {
				t.Fatal(err)
			}

			switch finalState {
			case "missing":
				transaction, durable, ok := start.Transaction()
				if !ok || !durable.Ranges().IsEmpty() {
					t.Fatalf("missing-final replacement = (transaction=%t, ranges=%v)", ok, durable.Ranges().Ranges())
				}
				outputV3AbandonTransaction(t, transaction.(*filesystemFileTransaction))
				outputV3AssertObjectNamesAbsent(t, root, selection, reopened.Session.SessionID(), oldRecord)
			case "matching":
				settlement, immediate := start.ImmediateSettlement()
				if !immediate || settlement.Kind() != transfer.FileCollision {
					t.Fatalf("matching-final replacement settlement = (kind=%v, immediate=%t)", settlement.Kind(), immediate)
				}
				actual, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(oldRecord.CanonicalLocator())))
				if readErr != nil || !bytes.Equal(actual, payload) {
					t.Fatalf("matching final = %q, %v, want %q", actual, readErr, payload)
				}
				outputV3AssertObjectNamesAbsent(t, root, selection, reopened.Session.SessionID(), oldRecord)
			case "foreign":
				settlement, immediate := start.ImmediateSettlement()
				reference, reason, quarantined := settlement.Quarantine()
				if !immediate || settlement.Kind() != transfer.FileQuarantined || !quarantined ||
					reason != transfer.QuarantinePublicationAmbiguous || reference.IsZero() {
					t.Fatalf(
						"foreign-final replacement = (kind=%v, immediate=%t, quarantine=%t/%v)",
						settlement.Kind(), immediate, quarantined, reason,
					)
				}
				record := readOutputV3PublicationAuthorityRecord(
					t, root, selection, reopened.Session.SessionID(), oldRecord,
				)
				if record.Phase() != resumestate.FileQuarantined ||
					record.QuarantineReason() != resumestate.QuarantinePublicationHistory {
					t.Fatalf("foreign-final record = (phase=%v, reason=%v)", record.Phase(), record.QuarantineReason())
				}
			}
		})
	}
}

func TestOutputV3PublishingRevisionMismatchClassifiesFinalParentReopenFailure(t *testing.T) {
	payload := []byte("old-revision-final-parent")
	for _, test := range []struct {
		name       string
		cause      error
		quarantine bool
	}{
		{name: "raw not-exist pauses", cause: fs.ErrNotExist},
		{name: "raw permission pauses", cause: fs.ErrPermission},
		{name: "raw EACCES pauses", cause: syscall.EACCES},
		{name: "raw EPERM pauses", cause: syscall.EPERM},
		{name: "unsafe identity quarantines", cause: errors.Join(errOutputV3Unsafe, errors.New("identity mismatch")), quarantine: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, true, uint64(len(payload)))
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
			t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
			oldFile := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
			oldRecord := v3RecoveryPreparePublishingCut(t, opened.Session, oldFile, payload, "matching")
			current := outputV3NextRevisionFile(t, opened.Session, oldFile)
			originalPlatform := opened.Session.platform
			faults := &outputV3PublicationDirectoryFaults{syncErr: test.cause}
			opened.Session.platform = &outputV3PublicationPlatform{
				outputV3Platform: originalPlatform,
				root:             &outputV3PublicationDirectory{outputV3Directory: originalPlatform.Root(), faults: faults},
			}

			start, beginErr := opened.Session.BeginFile(context.Background(), current)
			opened.Session.platform = originalPlatform
			persisted := readOutputV3PublicationAuthorityRecord(
				t, root, selection, opened.Session.SessionID(), oldRecord,
			)
			if test.quarantine {
				settlement, immediate := start.ImmediateSettlement()
				if beginErr != nil || !immediate || settlement.Kind() != transfer.FileQuarantined ||
					persisted.Phase() != resumestate.FileQuarantined ||
					persisted.QuarantineReason() != resumestate.QuarantineFinalUnsafe {
					t.Fatalf("unsafe stale sync-parent = (kind=%v/%t, phase=%v, reason=%v, err=%v)",
						settlement.Kind(), immediate, persisted.Phase(), persisted.QuarantineReason(), beginErr)
				}
				return
			}

			if !errors.Is(beginErr, test.cause) || !outputV3FailureRequiresJobPause(beginErr) ||
				persisted.Phase() != resumestate.FilePublishing ||
				persisted.StateGeneration() != oldRecord.StateGeneration() {
				t.Fatalf("denied stale sync-parent = (phase=%v, err=%v)", persisted.Phase(), beginErr)
			}
			retry, retryErr := opened.Session.BeginFile(context.Background(), current)
			settlement, immediate := retry.ImmediateSettlement()
			if retryErr != nil || !immediate || settlement.Kind() != transfer.FileCollision {
				t.Fatalf("stale sync-parent retry = (kind=%v/%t, err=%v)", settlement.Kind(), immediate, retryErr)
			}
		})
	}
}

func TestOutputV3InvalidatedPublishedCleanupHoldPreservesAuthority(t *testing.T) {
	payload := []byte("invalidated-published-hold")
	foreignStage := []byte("foreign-invalidated-stage")
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	oldFile := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	transaction := v3RecoveryBeginTransaction(t, opened.Session, oldFile)
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("publish old revision = (kind=%v, err=%v)", settlement.Kind(), err)
	}
	published := outputV3PersistedFileRecord(t, opened.Session, oldFile.Path)
	stage := resumestate.StageName(published.OutputObject())
	stagePath := filepath.Join(
		v3RecoverySessionPath(root, selection, opened.Session.SessionID()),
		resumestate.StagesDirectoryName, stage.Shard(), stage.Name(),
	)
	if err := os.WriteFile(stagePath, foreignStage, 0o600); err != nil {
		t.Fatal(err)
	}

	start, err := opened.Session.BeginFile(
		context.Background(), outputV3NextRevisionFile(t, opened.Session, oldFile),
	)
	if _, immediate := start.ImmediateSettlement(); immediate || !outputV3FailureRequiresJobPause(err) ||
		!errors.Is(err, errOutputV3InternalCleanupNeedsAttention) {
		t.Fatalf("invalidated published hold = (immediate=%t, err=%v), want preservation pause", immediate, err)
	}
	outputV3SemanticRequireFault(t, err, transfer.OutputFaultFile, transfer.OutputFaultOwnership)
	persisted := outputV3PersistedFileRecord(t, opened.Session, oldFile.Path)
	if persisted.Phase() != resumestate.FilePublished || persisted.QuarantineReason().Valid() ||
		persisted.StateGeneration() != published.StateGeneration() ||
		persisted.OutputObject() != published.OutputObject() {
		t.Fatalf(
			"invalidated published authority = (phase=%v, quarantine=%v, generation=%d, object=%v)",
			persisted.Phase(), persisted.QuarantineReason(), persisted.StateGeneration(), persisted.OutputObject(),
		)
	}
	if actual, readErr := os.ReadFile(stagePath); readErr != nil || !bytes.Equal(actual, foreignStage) {
		t.Fatalf("foreign invalidated stage changed = %q, %v", actual, readErr)
	}
	if actual, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(oldFile.Path))); readErr != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("published final changed = %q, %v", actual, readErr)
	}
	if _, statErr := os.Stat(outputV3PublicationAuthorityAnchorPath(
		root, selection, opened.Session.SessionID(), published,
	)); statErr != nil {
		t.Fatalf("published anchor changed: %v", statErr)
	}
}

func TestOutputV3InvalidatedRetiringCleanupHoldPreservesPublishedEvidence(t *testing.T) {
	payload := []byte("invalidated-retiring-hold")
	foreignStage := []byte("foreign-retiring-stage")
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	oldFile := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	transaction := v3RecoveryBeginTransaction(t, opened.Session, oldFile)
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("publish old revision = (kind=%v, err=%v)", settlement.Kind(), err)
	}
	bound := outputV3OpenBoundRecord(t, opened.Session, oldFile.Path)
	retiring, err := resumestate.PrepareInvalidatedRevisionRetirement(bound)
	if err != nil {
		t.Fatal(err)
	}
	recordName := resumestate.FileRecordName(bound.Record().LocatorDigest())
	recordDir, present, err := openOutputShard(opened.Session.filesDir, recordName.Shard(), false)
	if err != nil || !present {
		t.Fatalf("open retiring record shard = (present=%t, err=%v)", present, err)
	}
	if err := opened.Session.installFileRecord(recordDir, recordName.Name(), bound, retiring); err != nil {
		_ = recordDir.Close()
		t.Fatal(err)
	}
	if err := recordDir.Close(); err != nil {
		t.Fatal(err)
	}
	retiringRecord := retiring.Record()
	stage := resumestate.StageName(retiringRecord.OutputObject())
	stagePath := filepath.Join(
		v3RecoverySessionPath(root, selection, opened.Session.SessionID()),
		resumestate.StagesDirectoryName, stage.Shard(), stage.Name(),
	)
	if err := os.WriteFile(stagePath, foreignStage, 0o600); err != nil {
		t.Fatal(err)
	}

	start, err := opened.Session.BeginFile(
		context.Background(), outputV3NextRevisionFile(t, opened.Session, oldFile),
	)
	if _, immediate := start.ImmediateSettlement(); immediate || !outputV3FailureRequiresJobPause(err) ||
		!errors.Is(err, errOutputV3InternalCleanupNeedsAttention) {
		t.Fatalf("invalidated retiring hold = (immediate=%t, err=%v), want preservation pause", immediate, err)
	}
	outputV3SemanticRequireFault(t, err, transfer.OutputFaultFile, transfer.OutputFaultOwnership)
	persisted := outputV3PersistedFileRecord(t, opened.Session, oldFile.Path)
	if persisted.Phase() != resumestate.FileRetiring ||
		persisted.RetirementReason() != resumestate.RetirementInvalidatedRevision ||
		persisted.QuarantineReason().Valid() ||
		persisted.StateGeneration() != retiringRecord.StateGeneration() ||
		persisted.OutputObject() != retiringRecord.OutputObject() {
		t.Fatalf(
			"invalidated retiring authority = (phase=%v, reason=%v, quarantine=%v, generation=%d, object=%v)",
			persisted.Phase(), persisted.RetirementReason(), persisted.QuarantineReason(),
			persisted.StateGeneration(), persisted.OutputObject(),
		)
	}
	if actual, readErr := os.ReadFile(stagePath); readErr != nil || !bytes.Equal(actual, foreignStage) {
		t.Fatalf("foreign retiring stage changed = %q, %v", actual, readErr)
	}
	if actual, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(oldFile.Path))); readErr != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("retiring final changed = %q, %v", actual, readErr)
	}
	if _, statErr := os.Stat(outputV3PublicationAuthorityAnchorPath(
		root, selection, opened.Session.SessionID(), retiringRecord,
	)); statErr != nil {
		t.Fatalf("retiring anchor changed: %v", statErr)
	}
}

func TestOutputV3InvalidatedPublishedRevalidationPersistsFinalMismatch(t *testing.T) {
	payload := []byte("published-before-revalidation")
	replacement := append([]byte(nil), payload...)
	replacement[0] ^= 0xff
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	oldFile := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	transaction := v3RecoveryBeginTransaction(t, opened.Session, oldFile)
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("publish old revision = (kind=%v, err=%v)", settlement.Kind(), err)
	}
	published := outputV3PersistedFileRecord(t, opened.Session, oldFile.Path)
	gate := &outputV3InvalidatedFinalReplacementGate{
		target:      oldFile.Path,
		finalPath:   filepath.Join(root, filepath.FromSlash(oldFile.Path)),
		replacement: replacement,
	}
	originalPlatform := opened.Session.platform
	opened.Session.platform = &outputV3InvalidatedFinalReplacementPlatform{
		outputV3Platform: originalPlatform,
		gate:             gate,
	}

	start, beginErr := opened.Session.BeginFile(
		context.Background(), outputV3NextRevisionFile(t, opened.Session, oldFile),
	)
	opened.Session.platform = originalPlatform
	if beginErr != nil {
		t.Fatal(beginErr)
	}
	if !gate.replaced || gate.observations != 2 {
		t.Fatalf("final replacement gate = (replaced=%t, observations=%d)", gate.replaced, gate.observations)
	}
	settlement, immediate := start.ImmediateSettlement()
	reference, reason, quarantined := settlement.Quarantine()
	if !immediate || settlement.Kind() != transfer.FileQuarantined || !quarantined ||
		reason != transfer.QuarantinePublicationAmbiguous || reference.IsZero() {
		t.Fatalf(
			"revalidated mismatch = (kind=%v, immediate=%t, quarantine=%t/%v, reference=%v)",
			settlement.Kind(), immediate, quarantined, reason, reference,
		)
	}
	persisted := outputV3PersistedFileRecord(t, opened.Session, oldFile.Path)
	if persisted.Phase() != resumestate.FileQuarantined ||
		persisted.PhaseBeforeQuarantine() != resumestate.FilePublished ||
		persisted.QuarantineReason() != resumestate.QuarantineFinalMismatch ||
		persisted.OutputObject() != published.OutputObject() {
		t.Fatalf(
			"revalidated mismatch authority = (phase=%v, before=%v, reason=%v, object=%v)",
			persisted.Phase(), persisted.PhaseBeforeQuarantine(),
			persisted.QuarantineReason(), persisted.OutputObject(),
		)
	}
	if actual, readErr := os.ReadFile(gate.finalPath); readErr != nil || !bytes.Equal(actual, replacement) {
		t.Fatalf("replacement final changed = %q, %v", actual, readErr)
	}
	if _, statErr := os.Stat(outputV3PublicationAuthorityAnchorPath(
		root, selection, opened.Session.SessionID(), published,
	)); statErr != nil {
		t.Fatalf("quarantined publication anchor changed: %v", statErr)
	}
}

type outputV3InvalidatedFinalReplacementGate struct {
	target       string
	finalPath    string
	replacement  []byte
	observations int
	replaced     bool
}

func (gate *outputV3InvalidatedFinalReplacementGate) beforeObserve(name string) error {
	if name != gate.target {
		return nil
	}
	gate.observations++
	if gate.observations != 2 {
		return nil
	}
	// The second observation is the retirement authorization boundary. Replacing
	// the final here proves that a mismatch discovered at that boundary is made
	// durable instead of being mistaken for cleanup authority.
	if err := os.Remove(gate.finalPath); err != nil {
		return err
	}
	if err := os.WriteFile(gate.finalPath, gate.replacement, 0o600); err != nil {
		return err
	}
	gate.replaced = true
	return nil
}

type outputV3InvalidatedFinalReplacementPlatform struct {
	outputV3Platform
	gate *outputV3InvalidatedFinalReplacementGate
}

func (platform *outputV3InvalidatedFinalReplacementPlatform) Root() outputV3Directory {
	return outputV3WrapInvalidatedFinalReplacementDirectory(platform.outputV3Platform.Root(), platform.gate)
}

func (platform *outputV3InvalidatedFinalReplacementPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	return acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return outputV3WrapInvalidatedFinalReplacementDirectory(root, platform.gate)
		},
	)
}

type outputV3InvalidatedFinalReplacementDirectory struct {
	outputV3Directory
	gate *outputV3InvalidatedFinalReplacementGate
}

func outputV3WrapInvalidatedFinalReplacementDirectory(
	directory outputV3Directory,
	gate *outputV3InvalidatedFinalReplacementGate,
) outputV3Directory {
	if directory == nil {
		return nil
	}
	return &outputV3InvalidatedFinalReplacementDirectory{outputV3Directory: directory, gate: gate}
}

func outputV3UnwrapInvalidatedFinalReplacementDirectory(directory outputV3Directory) outputV3Directory {
	if wrapped, ok := directory.(*outputV3InvalidatedFinalReplacementDirectory); ok {
		return wrapped.outputV3Directory
	}
	return directory
}

func (directory *outputV3InvalidatedFinalReplacementDirectory) Duplicate() (outputV3Directory, error) {
	duplicate, err := directory.outputV3Directory.Duplicate()
	return outputV3WrapInvalidatedFinalReplacementDirectory(duplicate, directory.gate), err
}

func (directory *outputV3InvalidatedFinalReplacementDirectory) SameDirectory(
	other outputV3Directory,
) (bool, error) {
	return directory.outputV3Directory.SameDirectory(
		outputV3UnwrapInvalidatedFinalReplacementDirectory(other),
	)
}

func (directory *outputV3InvalidatedFinalReplacementDirectory) ObserveEntry(
	name string,
) (outputV3EntryKind, error) {
	if err := directory.gate.beforeObserve(name); err != nil {
		return 0, err
	}
	return directory.outputV3Directory.ObserveEntry(name)
}

func (directory *outputV3InvalidatedFinalReplacementDirectory) ValidateCreateAuthority() error {
	return validateOutputCreateAuthority(directory.outputV3Directory)
}

func (directory *outputV3InvalidatedFinalReplacementDirectory) ValidateMetadataAuthority() error {
	return validateOutputMetadataAuthority(directory.outputV3Directory)
}

func TestOutputV3InvalidatedRevisionRetirementCutRetriesIdempotently(t *testing.T) {
	payload := []byte("retry-invalidated-retirement")
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	sessionIDs := &v3RecoverySessionIDs{}
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	oldFile := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	oldTransaction := v3RecoveryBeginTransaction(t, opened.Session, oldFile).(*filesystemFileTransaction)
	oldRecord := oldTransaction.resumable.Bound().Record()
	outputV3AbandonTransaction(t, oldTransaction)

	failure := errors.New("injected invalidated retirement cut")
	faults := outputV3RetirementDirectoryFaults{
		injected: failure,
		child:    outputV3RetirementChildFaults{removeFileErrAt: 1, injected: failure},
	}
	originalStages := opened.Session.stagesDir
	opened.Session.stagesDir = &outputV3RetirementDirectory{
		outputV3Directory: originalStages,
		faults:            &faults,
	}
	current := outputV3NextRevisionFile(t, opened.Session, oldFile)
	if _, err := opened.Session.BeginFile(context.Background(), current); !errors.Is(err, failure) {
		t.Fatalf("injected retirement error = %v", err)
	}
	record := readOutputV3PublicationAuthorityRecord(
		t, root, selection, opened.Session.SessionID(), oldRecord,
	)
	if record.Phase() != resumestate.FileRetiring ||
		record.RetirementReason() != resumestate.RetirementInvalidatedRevision {
		t.Fatalf("retirement cut = (phase=%v, reason=%v)", record.Phase(), record.RetirementReason())
	}
	opened.Session.stagesDir = originalStages
	start, err := opened.Session.BeginFile(context.Background(), current)
	if err != nil {
		t.Fatal(err)
	}
	transaction, durable, ok := start.Transaction()
	if !ok || !durable.Ranges().IsEmpty() {
		t.Fatalf("retried replacement = (transaction=%t, ranges=%v)", ok, durable.Ranges().Ranges())
	}
	outputV3AssertObjectNamesAbsent(t, root, selection, opened.Session.SessionID(), oldRecord)
	outputV3AbandonTransaction(t, transaction.(*filesystemFileTransaction))
}

func TestOutputV3PublishBlockedRevisionMismatchUsesCurrentFinalEvidence(t *testing.T) {
	payload := []byte("blocked-old-revision")
	for _, finalState := range []string{"missing", "different", "matching"} {
		t.Run(finalState, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, true, uint64(len(payload)))
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			oldFile := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
			oldRecord := outputV3PreparePublishBlocked(t, opened.Session, oldFile, payload)
			finalPath := filepath.Join(root, filepath.FromSlash(oldRecord.CanonicalLocator()))
			switch finalState {
			case "missing":
				if err := os.Remove(finalPath); err != nil {
					t.Fatal(err)
				}
			case "matching":
				if err := os.Remove(finalPath); err != nil {
					t.Fatal(err)
				}
				anchorPath := outputV3PublicationAuthorityAnchorPath(
					root, selection, opened.Session.SessionID(), oldRecord,
				)
				if err := os.Link(anchorPath, finalPath); err != nil {
					t.Fatal(err)
				}
			}
			v3RecoveryCloseSession(t, opened.Session)

			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			t.Cleanup(func() { v3RecoveryCloseSession(t, reopened.Session) })
			current := outputV3NextRevisionFile(t, reopened.Session, oldFile)
			start, err := reopened.Session.BeginFile(context.Background(), current)
			if err != nil {
				t.Fatal(err)
			}
			switch finalState {
			case "missing":
				transaction, durable, ok := start.Transaction()
				if !ok || !durable.Ranges().IsEmpty() {
					t.Fatalf("missing blocked final = (transaction=%t, ranges=%v)", ok, durable.Ranges().Ranges())
				}
				outputV3AbandonTransaction(t, transaction.(*filesystemFileTransaction))
				outputV3AssertObjectNamesAbsent(t, root, selection, reopened.Session.SessionID(), oldRecord)
			case "different":
				settlement, immediate := start.ImmediateSettlement()
				if !immediate || settlement.Kind() != transfer.FileCollision {
					t.Fatalf("different blocked final = (kind=%v, immediate=%t)", settlement.Kind(), immediate)
				}
				outputV3AssertObjectNamesAbsent(t, root, selection, reopened.Session.SessionID(), oldRecord)
			case "matching":
				settlement, immediate := start.ImmediateSettlement()
				_, reason, quarantined := settlement.Quarantine()
				if !immediate || settlement.Kind() != transfer.FileQuarantined || !quarantined ||
					reason != transfer.QuarantinePublicationAmbiguous {
					t.Fatalf(
						"matching blocked final = (kind=%v, immediate=%t, quarantine=%t/%v)",
						settlement.Kind(), immediate, quarantined, reason,
					)
				}
			}
		})
	}
}

func TestOutputV3RevisionMismatchCompletesEveryDurableRetiringReason(t *testing.T) {
	payload := []byte("durable-retirement")
	for _, reason := range []resumestate.RetirementReason{
		resumestate.RetirementPublished,
		resumestate.RetirementIsolatedFailure,
		resumestate.RetirementPreObjectCollision,
		resumestate.RetirementInvalidatedRevision,
	} {
		t.Run(outputV3RetirementReasonName(reason), func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, true, uint64(len(payload)))
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			oldFile := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
			oldRecord := outputV3InstallRetiringReason(t, opened.Session, oldFile, payload, reason)
			v3RecoveryCloseSession(t, opened.Session)

			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			t.Cleanup(func() { v3RecoveryCloseSession(t, reopened.Session) })
			current := outputV3NextRevisionFile(t, reopened.Session, oldFile)
			start, err := reopened.Session.BeginFile(context.Background(), current)
			if err != nil {
				t.Fatal(err)
			}
			if reason == resumestate.RetirementPublished || reason == resumestate.RetirementPreObjectCollision {
				settlement, immediate := start.ImmediateSettlement()
				if !immediate || settlement.Kind() != transfer.FileCollision {
					t.Fatalf("retired final settlement = (kind=%v, immediate=%t)", settlement.Kind(), immediate)
				}
				outputV3AssertFileRecordAbsent(t, root, selection, reopened.Session.SessionID(), oldRecord)
			} else {
				transaction, durable, ok := start.Transaction()
				if !ok || !durable.Ranges().IsEmpty() {
					t.Fatalf("retired replacement = (transaction=%t, ranges=%v)", ok, durable.Ranges().Ranges())
				}
				replacement := transaction.(*filesystemFileTransaction)
				if replacement.resumable.Bound().Record().Revision() != current.Descriptor.FileRevision() ||
					replacement.resumable.Bound().Record().OutputObject() == oldRecord.OutputObject() {
					t.Fatalf("old retiring record was not replaced by current revision")
				}
				outputV3AbandonTransaction(t, replacement)
			}
			outputV3AssertObjectNamesAbsent(t, root, selection, reopened.Session.SessionID(), oldRecord)
		})
	}
}

func outputV3NextRevisionFile(
	t *testing.T,
	session *filesystemOutputSession,
	previous transfer.OutputFile,
) transfer.OutputFile {
	t.Helper()
	descriptor, err := content.NewFileRevisionDescriptor(
		previous.Descriptor.ShareInstance(), previous.Descriptor.FileID(),
		v3RecoveryIdentity16[content.FileRevision](0x71), previous.Descriptor.Geometry(),
		previous.Descriptor.ModifiedTime(),
	)
	if err != nil {
		t.Fatal(err)
	}
	target, err := outputTargetForDescriptor(session.SessionID(), descriptor, previous.Path)
	if err != nil {
		t.Fatal(err)
	}
	return transfer.OutputFile{
		Path: previous.Path, ExpectedSize: previous.ExpectedSize, Descriptor: descriptor, Target: target,
	}
}

func outputV3PreparePublishBlocked(
	t *testing.T,
	session *filesystemOutputSession,
	file transfer.OutputFile,
	payload []byte,
) resumestate.FileRecord {
	t.Helper()
	transaction := v3RecoveryBeginTransaction(t, session, file).(*filesystemFileTransaction)
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(session.owner.rootPath, filepath.FromSlash(file.Path))
	foreign := append([]byte(nil), payload...)
	foreign[0] ^= 0xff
	if err := os.WriteFile(finalPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublishBlocked {
		t.Fatalf("prepare publish-blocked cut = (kind=%v, err=%v)", settlement.Kind(), err)
	}
	record := transaction.resumable.Bound().Record()
	if record.Phase() != resumestate.FilePublishBlocked {
		t.Fatalf("publish-blocked record phase = %v", record.Phase())
	}
	return record
}

func outputV3InstallRetiringReason(
	t *testing.T,
	session *filesystemOutputSession,
	file transfer.OutputFile,
	payload []byte,
	reason resumestate.RetirementReason,
) resumestate.FileRecord {
	t.Helper()
	if reason == resumestate.RetirementPreObjectCollision {
		return outputV3InstallPreObjectCollisionRetirement(t, session, file, payload)
	}
	transaction := v3RecoveryBeginTransaction(t, session, file).(*filesystemFileTransaction)
	bound := transaction.resumable.Bound()
	if reason == resumestate.RetirementPublished {
		if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
			t.Fatal(err)
		}
		settlement, err := transaction.Commit(context.Background())
		if err != nil || settlement.Kind() != transfer.FilePublished {
			t.Fatalf("prepare published retirement = (kind=%v, err=%v)", settlement.Kind(), err)
		}
		bound = outputV3OpenBoundRecord(t, session, file.Path)
	} else {
		outputV3AbandonTransaction(t, transaction)
	}
	var retiring resumestate.BoundFileRecord
	var err error
	switch reason {
	case resumestate.RetirementPublished:
		retiring, err = resumestate.PreparePublishedRetirement(bound)
	case resumestate.RetirementIsolatedFailure:
		retiring, err = resumestate.PrepareIsolatedRetirement(bound)
	case resumestate.RetirementInvalidatedRevision:
		retiring, err = resumestate.PrepareInvalidatedRevisionRetirement(bound)
	default:
		t.Fatalf("unsupported retirement reason %v", reason)
	}
	if err != nil {
		t.Fatal(err)
	}
	name := resumestate.FileRecordName(bound.Record().LocatorDigest())
	shard, present, err := openOutputShard(session.filesDir, name.Shard(), false)
	if err != nil || !present {
		t.Fatalf("open retiring record shard = (present=%t, err=%v)", present, err)
	}
	defer shard.Close()
	if err := session.installFileRecord(shard, name.Name(), bound, retiring); err != nil {
		t.Fatal(err)
	}
	return retiring.Record()
}

func outputV3InstallPreObjectCollisionRetirement(
	t *testing.T,
	session *filesystemOutputSession,
	file transfer.OutputFile,
	payload []byte,
) resumestate.FileRecord {
	t.Helper()
	digest := resumestate.DigestCanonicalLocator(file.Path)
	name := resumestate.FileRecordName(digest)
	shard, _, err := openOutputShard(session.filesDir, name.Shard(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer shard.Close()
	objectID, err := session.allocateOutputObjectID(digest)
	if err != nil {
		t.Fatal(err)
	}
	resumable, err := resumestate.NewFileRecord(resumestate.FileRecordSpec{
		Session: session.stateSnapshot(), Descriptor: file.Descriptor,
		CanonicalLocator: file.Path, OutputObject: objectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := resumestate.EncodeFileRecord(resumable.Bound())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.ensureInitialFileRecord(shard, name.Name(), encoded); err != nil {
		t.Fatal(err)
	}
	foreign := append([]byte(nil), payload...)
	foreign[0] ^= 0xff
	if err := os.WriteFile(filepath.Join(session.owner.rootPath, filepath.FromSlash(file.Path)), foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	decision, err := resumestate.ReduceFileRecovery(resumable.Bound(), resumestate.FileObservation{
		Anchor: resumestate.AnchorMissing, Stage: resumestate.EntryMissing,
		Final: resumestate.EntryPresentUnresolved, Metadata: resumestate.MetadataNotObserved,
		FinalParent: resumestate.FinalParentNotObserved,
	})
	if err != nil || decision.Action() != resumestate.RecoveryInstallRetiring {
		t.Fatalf("pre-object collision decision = (action=%v, err=%v)", decision.Action(), err)
	}
	retiring, err := resumestate.ApplyRecoveryDecision(resumable.Bound(), decision)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.installFileRecord(shard, name.Name(), resumable.Bound(), retiring); err != nil {
		t.Fatal(err)
	}
	return retiring.Record()
}

func outputV3OpenBoundRecord(
	t *testing.T,
	session *filesystemOutputSession,
	locator string,
) resumestate.BoundFileRecord {
	t.Helper()
	name := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(locator))
	shard, present, err := openOutputShard(session.filesDir, name.Shard(), false)
	if err != nil || !present {
		t.Fatalf("open bound record shard = (present=%t, err=%v)", present, err)
	}
	defer shard.Close()
	bound, closeErr, err := session.openBoundFileRecord(shard, name)
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func outputV3RetirementReasonName(reason resumestate.RetirementReason) string {
	return map[resumestate.RetirementReason]string{
		resumestate.RetirementPublished:           "published",
		resumestate.RetirementIsolatedFailure:     "isolated",
		resumestate.RetirementPreObjectCollision:  "pre-object-collision",
		resumestate.RetirementInvalidatedRevision: "invalidated-revision",
	}[reason]
}

func outputV3AbandonTransaction(t *testing.T, transaction *filesystemFileTransaction) {
	t.Helper()
	transaction.lifecycle = filesystemFileTransactionClosed
	if err := transaction.closeHandles(); err != nil {
		t.Fatal(err)
	}
	transaction.session.finishFile(transaction.resumable.Bound().Record().LocatorDigest(), transaction)
}

func outputV3AssertObjectNamesAbsent(
	t *testing.T,
	root string,
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
	record resumestate.FileRecord,
) {
	t.Helper()
	sessionPath := v3RecoverySessionPath(root, selection, sessionID)
	for _, name := range []resumestate.ShardedName{
		resumestate.StageName(record.OutputObject()), resumestate.AnchorName(record.OutputObject()),
	} {
		parent := resumestate.StagesDirectoryName
		if name == resumestate.AnchorName(record.OutputObject()) {
			parent = resumestate.AnchorsDirectoryName
		}
		path := filepath.Join(sessionPath, parent, name.Shard(), name.Name())
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retired object name still exists at %s: %v", path, err)
		}
	}
}

func outputV3AssertFileRecordAbsent(
	t *testing.T,
	root string,
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
	record resumestate.FileRecord,
) {
	t.Helper()
	name := resumestate.FileRecordName(record.LocatorDigest())
	path := filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID), resumestate.FilesDirectoryName,
		name.Shard(), name.Name(),
	)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired file record still exists at %s: %v", path, err)
	}
}
