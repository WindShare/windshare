package osfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3PublicationWitnessRejectsEveryBrokenAuthorityProof(t *testing.T) {
	failure := errors.New("publication witness fault")

	for _, test := range []struct {
		name       string
		nilStage   bool
		nilAnchor  bool
		stageOpen  error
		anchorOpen error
		stagePlan  outputV3PublicationFileFaults
		anchorPlan outputV3PublicationFileFaults
		expected   bool
		wantUnsafe bool
	}{
		{name: "missing stage directory", nilStage: true, wantUnsafe: true},
		{name: "missing anchor directory", nilAnchor: true, wantUnsafe: true},
		{name: "stage open failure", stageOpen: failure},
		{name: "anchor open failure", anchorOpen: failure},
		{name: "stage size failure", stagePlan: outputV3PublicationFileFaults{sizeErr: failure}},
		{name: "anchor size failure", anchorPlan: outputV3PublicationFileFaults{sizeErr: failure}},
		{name: "stage size mismatch", stagePlan: outputV3PublicationFileFaults{sizeAdjustment: 1}, wantUnsafe: true},
		{name: "anchor size mismatch", anchorPlan: outputV3PublicationFileFaults{sizeAdjustment: 1}, wantUnsafe: true},
		{name: "stage anchor comparison failure", stagePlan: outputV3PublicationFileFaults{sameErrAt: 1, sameErr: failure}},
		{name: "stage anchor identity mismatch", stagePlan: outputV3PublicationFileFaults{differentAt: 1}, wantUnsafe: true},
		{name: "retained stage comparison failure", expected: true, stagePlan: outputV3PublicationFileFaults{sameErrAt: 2, sameErr: failure}},
		{name: "retained anchor comparison failure", expected: true, anchorPlan: outputV3PublicationFileFaults{sameErrAt: 1, sameErr: failure}},
		{name: "retained stage identity mismatch", expected: true, stagePlan: outputV3PublicationFileFaults{differentAt: 2}, wantUnsafe: true},
		{name: "retained anchor identity mismatch", expected: true, anchorPlan: outputV3PublicationFileFaults{differentAt: 1}, wantUnsafe: true},
		{name: "metadata inspection failure", anchorPlan: outputV3PublicationFileFaults{metadataErr: failure}},
		{name: "metadata mismatch", anchorPlan: outputV3PublicationFileFaults{metadataMismatch: true}, wantUnsafe: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, transaction := outputV3PublicationReadyTransaction(t)
			record := transaction.resumable.Bound().Record()
			var stageDir outputV3Directory = &outputV3PublicationOpenDirectory{
				outputV3Directory: transaction.stageDir,
				openErr:           test.stageOpen,
				fileFaults:        test.stagePlan,
			}
			var anchorDir outputV3Directory = &outputV3PublicationOpenDirectory{
				outputV3Directory: transaction.anchorDir,
				openErr:           test.anchorOpen,
				fileFaults:        test.anchorPlan,
			}
			if test.nilStage {
				stageDir = nil
			}
			if test.nilAnchor {
				anchorDir = nil
			}
			var expected outputV3File
			if test.expected {
				expected = transaction.anchor
			}

			witness, err := openPublicationWitnessInDirectories(record, stageDir, anchorDir, expected)
			if witness != nil {
				_ = witness.Close()
				t.Fatal("broken publication proof returned a witness")
			}
			if errors.Is(err, errOutputV3Unsafe) != test.wantUnsafe {
				t.Fatalf("publication proof unsafe=%t, want %t: %v", errors.Is(err, errOutputV3Unsafe), test.wantUnsafe, err)
			}
			if test.stageOpen != nil || test.anchorOpen != nil || test.stagePlan.hasInjectedError() || test.anchorPlan.hasInjectedError() {
				if !errors.Is(err, failure) {
					t.Fatalf("publication proof omitted injected cause: %v", err)
				}
			}
			_ = session
		})
	}
}

func TestOutputV3WitnessCreationCollisionsPersistPartialObjectQuarantine(t *testing.T) {
	for _, test := range []struct {
		name   string
		stage  outputV3PublicationDirectoryFaults
		anchor outputV3PublicationDirectoryFaults
	}{
		{name: "stage already exists", stage: outputV3PublicationDirectoryFaults{createFileErr: errOutputV3Collision}},
		{name: "anchor already exists", anchor: outputV3PublicationDirectoryFaults{linkErr: errOutputV3Collision}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, true, 1)
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
			t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
			file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
			originalStages, originalAnchors := opened.Session.stagesDir, opened.Session.anchorsDir
			opened.Session.stagesDir = &outputV3PublicationDirectory{
				outputV3Directory: originalStages, faults: &test.stage,
			}
			opened.Session.anchorsDir = &outputV3PublicationDirectory{
				outputV3Directory: originalAnchors, faults: &test.anchor,
			}
			t.Cleanup(func() {
				opened.Session.stagesDir, opened.Session.anchorsDir = originalStages, originalAnchors
			})

			start, err := opened.Session.BeginFile(context.Background(), file)
			settlement, immediate := start.ImmediateSettlement()
			if err != nil || !immediate || settlement.Kind() != transfer.FileQuarantined {
				t.Fatalf("witness collision = (kind=%v, immediate=%t, err=%v)", settlement.Kind(), immediate, err)
			}
			_, reason, valid := settlement.Quarantine()
			if !valid || reason != transfer.QuarantineRetirementMismatch {
				t.Fatalf("witness collision quarantine = (reason=%v, valid=%t)", reason, valid)
			}

			digest := resumestate.DigestCanonicalLocator(file.Path)
			name := resumestate.FileRecordName(digest)
			shard := outputV3SemanticOpenShard(t, opened.Session.filesDir, name.Shard(), false)
			defer shard.Close()
			encoded, readErr := readStateRecord(shard, name.Name(), resumestate.MaxFileStateBytes)
			record, decodeErr := resumestate.DecodeFileRecord(encoded)
			if readErr != nil || decodeErr != nil || record.Phase() != resumestate.FileQuarantined ||
				record.PhaseBeforeQuarantine() != resumestate.FileReserved ||
				record.QuarantineReason() != resumestate.QuarantinePartialObjectCreation {
				t.Fatalf("durable witness collision = (phase=%v, before=%v, reason=%v, read=%v, decode=%v)",
					record.Phase(), record.PhaseBeforeQuarantine(), record.QuarantineReason(), readErr, decodeErr)
			}
		})
	}
}

func TestOutputV3ObservationCloseFailuresRemainSeparateFromNamespaceEvidence(t *testing.T) {
	failure := errors.New("observation close fault")
	for _, test := range []struct {
		name        string
		anchor      outputV3RetirementChildFaults
		stage       outputV3RetirementChildFaults
		final       bool
		finalFile   bool
		finalParent bool
		wantAction  resumestate.RecoveryAction
	}{
		{name: "anchor file", anchor: outputV3RetirementChildFaults{fileCloseErrAt: 1}, wantAction: resumestate.RecoveryResumeContent},
		{name: "anchor directory", anchor: outputV3RetirementChildFaults{closeErrAt: 1}, wantAction: resumestate.RecoveryResumeContent},
		{name: "stage file", stage: outputV3RetirementChildFaults{fileCloseErrAt: 1}, wantAction: resumestate.RecoveryResumeContent},
		{name: "stage directory", stage: outputV3RetirementChildFaults{closeErrAt: 1}, wantAction: resumestate.RecoveryResumeContent},
		{name: "final file", final: true, finalFile: true, wantAction: resumestate.RecoveryInstallQuarantine},
		{name: "final parent", final: true, finalParent: true, wantAction: resumestate.RecoveryInstallQuarantine},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, transaction := outputV3PublicationReadyTransaction(t)
			if test.final {
				result, err := session.linkFinalNoReplace(transaction.resumable.Bound(), transaction.anchor)
				if err != nil || result != resumestate.PublishLinkCreated {
					t.Fatalf("create matching final = (result=%v, err=%v)", result, err)
				}
			}

			originalStages, originalAnchors, originalPlatform := session.stagesDir, session.anchorsDir, session.platform
			anchorFaults := outputV3RetirementDirectoryFaults{injected: failure, child: test.anchor}
			stageFaults := outputV3RetirementDirectoryFaults{injected: failure, child: test.stage}
			session.anchorsDir = &outputV3RetirementDirectory{outputV3Directory: originalAnchors, faults: &anchorFaults}
			session.stagesDir = &outputV3RetirementDirectory{outputV3Directory: originalStages, faults: &stageFaults}
			finalFaults := &outputV3PublicationDirectoryFaults{}
			if test.finalFile {
				finalFaults.openedFaults.closeErr = failure
			}
			var guardCloseErr error
			if test.finalParent {
				guardCloseErr = failure
			}
			session.platform = &outputV3PublicationPlatform{
				outputV3Platform: originalPlatform,
				guardCloseErr:    guardCloseErr,
				root: &outputV3PublicationDirectory{
					outputV3Directory: originalPlatform.Root(), faults: finalFaults,
				},
			}

			validation, validationErr := session.validateOutputAncestry(outputAncestryRequirement{})
			if validationErr != nil {
				t.Fatal(validationErr)
			}
			observation, cleanupErr, observationErr := session.observeFile(
				validation, transaction.resumable.Bound().Record(), false,
			)
			validationErr = validation.Revalidate(outputAncestryRequirement{})
			cleanupErr = errors.Join(cleanupErr, closeOutputAncestryValidation(validation))
			session.stagesDir, session.anchorsDir, session.platform = originalStages, originalAnchors, originalPlatform
			if observationErr != nil || validationErr != nil || !errors.Is(cleanupErr, failure) {
				t.Fatalf("observation close split = (observation=%+v, cleanup=%v, err=%v)", observation, cleanupErr, observationErr)
			}
			decision, err := resumestate.ReduceResumableFileRecovery(transaction.resumable, observation)
			if err != nil || decision.Action() != test.wantAction {
				t.Fatalf("preserved observation decision = (action=%v, err=%v), want %v", decision.Action(), err, test.wantAction)
			}
		})
	}
}

func TestOutputV3AmbiguousObservationPersistsQuarantineBeforeCloseFailure(t *testing.T) {
	failure := errors.New("final observation close fault")
	session, transaction := outputV3PublicationReadyTransaction(t)
	result, err := session.linkFinalNoReplace(transaction.resumable.Bound(), transaction.anchor)
	if err != nil || result != resumestate.PublishLinkCreated {
		t.Fatalf("create matching final = (result=%v, err=%v)", result, err)
	}
	originalPlatform := session.platform
	faults := &outputV3PublicationDirectoryFaults{
		openedFaults: outputV3PublicationFileFaults{closeErr: failure},
	}
	session.platform = &outputV3PublicationPlatform{
		outputV3Platform: originalPlatform,
		root:             &outputV3PublicationDirectory{outputV3Directory: originalPlatform.Root(), faults: faults},
	}
	t.Cleanup(func() { session.platform = originalPlatform })

	start, reduceErr := session.reduceFile(
		context.Background(),
		transfer.OutputFile{
			Path: transaction.binding.Locator().CanonicalPath(), ExpectedSize: transaction.binding.ExactSize(),
			Descriptor: transaction.descriptor, Target: transaction.binding.Target(),
		},
		transaction.resumable, transaction.recordDir, transaction.recordName,
	)
	_, _, transactionStarted := start.Transaction()
	settlement, immediate := start.ImmediateSettlement()
	if transactionStarted || immediate || !errors.Is(reduceErr, failure) || !outputV3FailureRequiresJobPause(reduceErr) {
		t.Fatalf("ambiguous observation close = (transaction=%t, settlement=%v/%t, err=%v)",
			transactionStarted, settlement.Kind(), immediate, reduceErr)
	}
	encoded, readErr := readStateRecord(transaction.recordDir, transaction.recordName, resumestate.MaxFileStateBytes)
	record, decodeErr := resumestate.DecodeFileRecord(encoded)
	if readErr != nil || decodeErr != nil || record.Phase() != resumestate.FileQuarantined ||
		record.QuarantineReason() != resumestate.QuarantinePublicationHistory {
		t.Fatalf("persisted close-priority quarantine = (phase=%v, reason=%v, read=%v, decode=%v)",
			record.Phase(), record.QuarantineReason(), readErr, decodeErr)
	}
}

func TestOutputV3SyncFinalParentSeparatesDenialFromIdentityContradiction(t *testing.T) {
	failure := errors.New("final parent reopen denied")
	for _, test := range []struct {
		name       string
		cause      error
		quarantine bool
	}{
		{name: "raw not-exist", cause: fs.ErrNotExist},
		{name: "raw permission", cause: fs.ErrPermission},
		{name: "raw EACCES", cause: syscall.EACCES},
		{name: "raw EPERM", cause: syscall.EPERM},
		{name: "raw denial", cause: failure},
		{name: "unsafe identity", cause: errors.Join(errOutputV3Unsafe, failure), quarantine: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			payload := []byte("sync-parent-cut")
			selection := v3RecoverySelection(t, true, uint64(len(payload)))
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
			t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
			file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
			record := v3RecoveryPreparePublishingCut(t, opened.Session, file, payload, "matching")
			originalPlatform := opened.Session.platform
			faults := &outputV3PublicationDirectoryFaults{syncErr: test.cause}
			opened.Session.platform = &outputV3PublicationPlatform{
				outputV3Platform: originalPlatform,
				root:             &outputV3PublicationDirectory{outputV3Directory: originalPlatform.Root(), faults: faults},
			}

			start, beginErr := opened.Session.BeginFile(context.Background(), file)
			opened.Session.platform = originalPlatform
			name := resumestate.FileRecordName(record.LocatorDigest())
			shard := outputV3SemanticOpenShard(t, opened.Session.filesDir, name.Shard(), false)
			encoded, readErr := readStateRecord(shard, name.Name(), resumestate.MaxFileStateBytes)
			persisted, decodeErr := resumestate.DecodeFileRecord(encoded)
			_ = shard.Close()
			if readErr != nil || decodeErr != nil {
				t.Fatalf("read sync-parent cut: %v", errors.Join(readErr, decodeErr))
			}

			if test.quarantine {
				settlement, immediate := start.ImmediateSettlement()
				if beginErr != nil || !immediate || settlement.Kind() != transfer.FileQuarantined ||
					persisted.Phase() != resumestate.FileQuarantined ||
					persisted.QuarantineReason() != resumestate.QuarantineFinalUnsafe {
					t.Fatalf("unsafe sync-parent cut = (kind=%v/%t, phase=%v, reason=%v, err=%v)",
						settlement.Kind(), immediate, persisted.Phase(), persisted.QuarantineReason(), beginErr)
				}
				return
			}

			if !errors.Is(beginErr, test.cause) || !outputV3FailureRequiresJobPause(beginErr) ||
				persisted.Phase() != resumestate.FilePublishing ||
				persisted.StateGeneration() != record.StateGeneration() {
				t.Fatalf("denied sync-parent cut = (phase=%v, err=%v)", persisted.Phase(), beginErr)
			}
			retry, retryErr := opened.Session.BeginFile(context.Background(), file)
			settlement, immediate := retry.ImmediateSettlement()
			if retryErr != nil || !immediate || settlement.Kind() != transfer.FilePublished {
				t.Fatalf("sync-parent retry = (kind=%v/%t, err=%v)", settlement.Kind(), immediate, retryErr)
			}
		})
	}
}

func TestOutputV3FinalPublicationRequiresPinnedIdentityAtEveryCut(t *testing.T) {
	failure := errors.New("final publication fault")

	for _, test := range []struct {
		name           string
		nilExpected    bool
		guardErr       error
		linkErr        error
		linkReturnsNil bool
		linkedFaults   outputV3PublicationFileFaults
		observeErr     error
		observeKind    outputV3EntryKind
		openErr        error
		openedFaults   outputV3PublicationFileFaults
		parentSyncErr  error
		guardCloseErr  error
		createFinal    outputV3EntryKind
		wantResult     resumestate.PublishResult
		wantInjected   bool
	}{
		{name: "missing retained source", nilExpected: true},
		{name: "public guard acquisition failure", guardErr: failure, wantInjected: true},
		{name: "publication primitive failure", linkErr: failure, wantInjected: true},
		{name: "publication returns no fixed handle", linkReturnsNil: true},
		{name: "published identity comparison failure", linkedFaults: outputV3PublicationFileFaults{sameErrAt: 1, sameErr: failure}, wantInjected: true},
		{name: "published identity mismatch", linkedFaults: outputV3PublicationFileFaults{differentAt: 1}},
		{name: "published metadata inspection failure", linkedFaults: outputV3PublicationFileFaults{metadataErr: failure}, wantInjected: true},
		{name: "published metadata mismatch", linkedFaults: outputV3PublicationFileFaults{metadataMismatch: true}},
		{name: "published parent sync failure", parentSyncErr: failure, wantResult: resumestate.PublishLinkCreated, wantInjected: true},
		{name: "collision observation failure", linkErr: errOutputV3Collision, observeErr: failure, createFinal: outputV3EntryRegularFile, wantResult: resumestate.PublishExistingAmbiguous},
		{name: "collision with directory", linkErr: errOutputV3Collision, createFinal: outputV3EntryDirectory, wantResult: resumestate.PublishAlreadyExistsDifferent},
		{name: "collision final open failure", linkErr: errOutputV3Collision, createFinal: outputV3EntryRegularFile, openErr: failure, wantResult: resumestate.PublishExistingAmbiguous},
		{name: "collision identity comparison failure", linkErr: errOutputV3Collision, createFinal: outputV3EntryRegularFile, openedFaults: outputV3PublicationFileFaults{sameErrAt: 1, sameErr: failure}, wantResult: resumestate.PublishExistingAmbiguous},
		{name: "classified collision final close failure", linkErr: errOutputV3Collision, createFinal: outputV3EntryRegularFile, openedFaults: outputV3PublicationFileFaults{closeErr: failure}, wantResult: resumestate.PublishAlreadyExistsDifferent, wantInjected: true},
		{name: "classified collision parent close failure", linkErr: errOutputV3Collision, createFinal: outputV3EntryRegularFile, guardCloseErr: failure, wantResult: resumestate.PublishAlreadyExistsDifferent, wantInjected: true},
		{name: "collision with different file", linkErr: errOutputV3Collision, createFinal: outputV3EntryRegularFile, wantResult: resumestate.PublishAlreadyExistsDifferent},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, transaction := outputV3PublicationReadyTransaction(t)
			if test.createFinal != outputV3EntryAbsent {
				outputV3CreatePublicCollision(t, session, transaction.binding.Locator().CanonicalPath(), test.createFinal)
			}
			plan := &outputV3PublicationDirectoryFaults{
				linkErr:        test.linkErr,
				linkReturnsNil: test.linkReturnsNil,
				linkedFaults:   test.linkedFaults,
				observeErr:     test.observeErr,
				observeKind:    test.observeKind,
				openErr:        test.openErr,
				openedFaults:   test.openedFaults,
				syncErr:        test.parentSyncErr,
			}
			originalPlatform := session.platform
			session.platform = &outputV3PublicationPlatform{
				outputV3Platform: originalPlatform,
				guardErr:         test.guardErr,
				guardCloseErr:    test.guardCloseErr,
				root: &outputV3PublicationDirectory{
					outputV3Directory: originalPlatform.Root(),
					faults:            plan,
				},
			}
			t.Cleanup(func() { session.platform = originalPlatform })

			expected := transaction.anchor
			if test.nilExpected {
				expected = nil
			}
			result, err := session.linkFinalNoReplace(transaction.resumable.Bound(), expected)
			if result != test.wantResult {
				t.Fatalf("publication result = %v, want %v (err=%v)", result, test.wantResult, err)
			}
			if test.wantResult != 0 && !test.wantInjected {
				if err != nil {
					t.Fatalf("classified collision returned error: %v", err)
				}
				return
			}
			if err == nil || !errors.Is(err, errOutputV3Unsafe) && !test.wantInjected {
				t.Fatalf("unsafe publication cut error = %v", err)
			}
			if test.wantInjected && !errors.Is(err, failure) {
				t.Fatalf("publication error omitted injected cause: %v", err)
			}
		})
	}
}

func TestOutputV3ClassifiedCollisionPersistsDecisionBeforeCloseFailure(t *testing.T) {
	failure := errors.New("classified collision close fault")
	for _, test := range []struct {
		name        string
		finalClose  bool
		parentClose bool
	}{
		{name: "final handle", finalClose: true},
		{name: "parent handle", parentClose: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, transaction := outputV3PublicationReadyTransaction(t)
			if err := transaction.WriteRange(context.Background(), 0, []byte{0x5a}); err != nil {
				t.Fatal(err)
			}
			outputV3CreatePublicCollision(
				t, session, transaction.binding.Locator().CanonicalPath(), outputV3EntryRegularFile,
			)
			originalPlatform := session.platform
			faults := &outputV3PublicationDirectoryFaults{}
			if test.finalClose {
				faults.openedFaults.closeErr = failure
			}
			var guardCloseErr error
			if test.parentClose {
				guardCloseErr = failure
			}
			session.platform = &outputV3PublicationPlatform{
				outputV3Platform: originalPlatform,
				guardCloseErr:    guardCloseErr,
				root:             &outputV3PublicationDirectory{outputV3Directory: originalPlatform.Root(), faults: faults},
			}

			settlement, commitErr := transaction.Commit(context.Background())
			session.platform = originalPlatform
			if settlement.Kind() != 0 || !errors.Is(commitErr, failure) || !outputV3FailureRequiresJobPause(commitErr) {
				t.Fatalf("classified collision close = (kind=%v, err=%v)", settlement.Kind(), commitErr)
			}
			digest := transaction.resumable.Bound().Record().LocatorDigest()
			name := resumestate.FileRecordName(digest)
			shard := outputV3SemanticOpenShard(t, session.filesDir, name.Shard(), false)
			encoded, readErr := readStateRecord(shard, name.Name(), resumestate.MaxFileStateBytes)
			record, decodeErr := resumestate.DecodeFileRecord(encoded)
			_ = shard.Close()
			if readErr != nil || decodeErr != nil || record.Phase() != resumestate.FilePublishBlocked {
				t.Fatalf("persisted collision decision = (phase=%v, read=%v, decode=%v)", record.Phase(), readErr, decodeErr)
			}

			file := transfer.OutputFile{
				Path: transaction.binding.Locator().CanonicalPath(), ExpectedSize: transaction.binding.ExactSize(),
				Descriptor: transaction.descriptor, Target: transaction.binding.Target(),
			}
			retry, retryErr := session.BeginFile(context.Background(), file)
			retried, immediate := retry.ImmediateSettlement()
			if retryErr != nil || !immediate || retried.Kind() != transfer.FilePublishBlocked {
				t.Fatalf("classified collision retry = (kind=%v/%t, err=%v)", retried.Kind(), immediate, retryErr)
			}
		})
	}
}

func TestOutputV3LinkCreatedFailuresRetainPublishingCut(t *testing.T) {
	syncFailure := errors.New("published parent sync fault")
	closeFailure := errors.New("published handle close fault")

	for _, test := range []struct {
		name      string
		syncErr   error
		closeErr  error
		wantCause []error
	}{
		{name: "sync", syncErr: syncFailure, wantCause: []error{syncFailure}},
		{name: "close", closeErr: closeFailure, wantCause: []error{closeFailure}},
		{name: "sync and close", syncErr: syncFailure, closeErr: closeFailure, wantCause: []error{syncFailure, closeFailure}},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, transaction := outputV3PublicationReadyTransaction(t)
			if err := transaction.WriteRange(context.Background(), 0, []byte{0x5a}); err != nil {
				t.Fatal(err)
			}
			originalPlatform := session.platform
			faults := &outputV3PublicationDirectoryFaults{
				syncErr:      test.syncErr,
				linkedFaults: outputV3PublicationFileFaults{closeErr: test.closeErr},
			}
			session.platform = &outputV3PublicationPlatform{
				outputV3Platform: originalPlatform,
				root: &outputV3PublicationDirectory{
					outputV3Directory: originalPlatform.Root(),
					faults:            faults,
				},
			}
			t.Cleanup(func() { session.platform = originalPlatform })

			settlement, commitErr := transaction.Commit(context.Background())
			if settlement.Kind() != 0 || !outputV3FailureRequiresJobPause(commitErr) {
				t.Fatalf("created-link cut = (kind=%v, err=%v)", settlement.Kind(), commitErr)
			}
			for _, cause := range test.wantCause {
				if !errors.Is(commitErr, cause) {
					t.Fatalf("created-link cut omitted %v: %v", cause, commitErr)
				}
			}
			persisted := outputV3PersistedFileRecord(
				t, session, transaction.binding.Locator().CanonicalPath(),
			)
			if persisted.Phase() != resumestate.FilePublishing {
				t.Fatalf("created-link cut phase = %v, want Publishing", persisted.Phase())
			}

			// Once fresh I/O proves the parent sync, the deterministic link cut must
			// converge without retransmitting content or inventing a quarantine.
			session.platform = originalPlatform
			start, retryErr := session.BeginFile(context.Background(), transfer.OutputFile{
				Path:         transaction.binding.Locator().CanonicalPath(),
				ExpectedSize: transaction.binding.ExactSize(),
				Descriptor:   transaction.descriptor,
				Target:       transaction.binding.Target(),
			})
			retried, immediate := start.ImmediateSettlement()
			if retryErr != nil || !immediate || retried.Kind() != transfer.FilePublished {
				t.Fatalf("created-link restart = (kind=%v/%t, err=%v)", retried.Kind(), immediate, retryErr)
			}
		})
	}
}

func TestOutputV3PublishedCleanupMutationFailuresPauseWithoutQuarantine(t *testing.T) {
	failure := errors.New("published cleanup mutation fault")
	for _, test := range []struct {
		name        string
		createStage bool
		faults      outputV3RetirementChildFaults
	}{
		{
			name:        "remove matching stage",
			createStage: true,
			faults:      outputV3RetirementChildFaults{removeFileErrAt: 1, injected: failure},
		},
		{
			name:   "sync absent stage parent",
			faults: outputV3RetirementChildFaults{syncErrAt: 1, injected: failure},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, transaction := outputV3PublicationReadyTransaction(t)
			if err := transaction.WriteRange(context.Background(), 0, []byte{0x5a}); err != nil {
				t.Fatal(err)
			}
			settlement, err := transaction.Commit(context.Background())
			if err != nil || settlement.Kind() != transfer.FilePublished {
				t.Fatalf("initial publication = (kind=%v, err=%v)", settlement.Kind(), err)
			}
			locator := transaction.binding.Locator().CanonicalPath()
			record := outputV3PersistedFileRecord(t, session, locator)
			stage := resumestate.StageName(record.OutputObject())
			stagePath := filepath.Join(
				v3RecoverySessionPath(session.owner.rootPath, session.selection, session.SessionID()),
				resumestate.StagesDirectoryName, stage.Shard(), stage.Name(),
			)
			if test.createStage {
				anchor := resumestate.AnchorName(record.OutputObject())
				anchorPath := filepath.Join(
					v3RecoverySessionPath(session.owner.rootPath, session.selection, session.SessionID()),
					resumestate.AnchorsDirectoryName, anchor.Shard(), anchor.Name(),
				)
				if err := os.Link(anchorPath, stagePath); err != nil {
					t.Fatal(err)
				}
			}

			faults := outputV3RetirementDirectoryFaults{child: test.faults}
			originalStages := session.stagesDir
			session.stagesDir = &outputV3RetirementDirectory{
				outputV3Directory: originalStages,
				faults:            &faults,
			}
			t.Cleanup(func() {
				session.stagesDir = originalStages
			})
			file := transfer.OutputFile{
				Path: locator, ExpectedSize: transaction.binding.ExactSize(),
				Descriptor: transaction.descriptor, Target: transaction.binding.Target(),
			}
			start, beginErr := session.BeginFile(context.Background(), file)
			if _, immediate := start.ImmediateSettlement(); immediate || !errors.Is(beginErr, failure) ||
				!outputV3FailureRequiresJobPause(beginErr) {
				t.Fatalf("published cleanup failure = (immediate=%t, err=%v)", immediate, beginErr)
			}
			persisted := outputV3PersistedFileRecord(t, session, locator)
			if persisted.Phase() != resumestate.FilePublished || persisted.QuarantineReason() != 0 {
				t.Fatalf("published cleanup failure record = (phase=%v, quarantine=%v)",
					persisted.Phase(), persisted.QuarantineReason())
			}

			session.stagesDir = originalStages
			retried, retryErr := session.BeginFile(context.Background(), file)
			retrySettlement, immediate := retried.ImmediateSettlement()
			if retryErr != nil || !immediate || retrySettlement.Kind() != transfer.FilePublished {
				t.Fatalf("published cleanup retry = (kind=%v/%t, err=%v)",
					retrySettlement.Kind(), immediate, retryErr)
			}
		})
	}
}

func TestOutputV3RetiringAnchorPreservesOperationAndCleanupFailuresWithoutQuarantine(t *testing.T) {
	operationFailure := errors.New("retiring anchor operation fault")
	cleanupFailure := errors.New("retiring anchor cleanup fault")

	for _, test := range []struct {
		name        string
		child       outputV3RetirementChildFaults
		wantOpCause bool
	}{
		{
			name: "open ambiguity and cleanup join into pause",
			child: outputV3RetirementChildFaults{
				injected: operationFailure, cleanupInjected: cleanupFailure,
				openFileErrAt: 3, closeErrAt: 3,
			},
			wantOpCause: true,
		},
		{
			name: "raw operation and cleanup join into pause",
			child: outputV3RetirementChildFaults{
				injected: operationFailure, cleanupInjected: cleanupFailure,
				removeFileErrAt: 1, fileCloseErrAt: 3,
			},
			wantOpCause: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, recordDir, recordName, retiring, binding := outputV3PreparedRetirement(t)
			faults := outputV3RetirementDirectoryFaults{child: test.child}
			stageFaults := outputV3RetirementDirectoryFaults{}
			originalStages, originalAnchors := session.stagesDir, session.anchorsDir
			session.stagesDir = &outputV3RetirementDirectory{
				outputV3Directory: originalStages,
				faults:            &stageFaults,
			}
			session.anchorsDir = &outputV3RetirementDirectory{
				outputV3Directory: originalAnchors,
				faults:            &faults,
			}
			settlement, quarantined, err := outputV3RetireBoundFileAsOperation(
				t, session, recordDir, recordName, retiring, binding,
			)
			if settlement.Kind() != 0 || quarantined ||
				!errors.Is(err, cleanupFailure) || !outputV3FailureRequiresJobPause(err) {
				t.Fatalf("retiring anchor coexistence = (kind=%v, quarantined=%t, err=%v, classify=%d, open=%d, remove=%d, sync=%d, close=%d, file-close=%d)",
					settlement.Kind(), quarantined, err, faults.classifyCalls,
					faults.child.openFileCalls, faults.child.removeFileCalls, faults.child.syncCalls,
					faults.child.closeCalls, faults.child.fileCloseCalls)
			}
			if test.wantOpCause && !errors.Is(err, operationFailure) {
				t.Fatalf("retiring anchor pause omitted operation cause: %v", err)
			}
			persisted := outputV3PersistedFileRecord(t, session, retiring.Record().CanonicalLocator())
			if persisted.Phase() != resumestate.FileRetiring {
				t.Fatalf("raw retiring anchor fault phase = %v, want Retiring", persisted.Phase())
			}
			settlement, quarantined, err = outputV3RetireBoundFileAsOperation(
				t, session, recordDir, recordName, retiring, binding,
			)
			if err != nil || quarantined || settlement.Kind() != transfer.FileRetired {
				t.Fatalf("raw retiring anchor retry = (kind=%v, quarantined=%t, err=%v)",
					settlement.Kind(), quarantined, err)
			}
		})
	}
}

func TestOutputV3RetirementPostMutationCleanupCutsRemainRetryable(t *testing.T) {
	cleanupFailure := errors.New("post-mutation cleanup fault")

	for _, test := range []struct {
		name       string
		stage      outputV3RetirementDirectoryFaults
		record     outputV3RetirementRecordFaults
		recordGone bool
	}{
		{
			name: "stage handle after removal",
			stage: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{
				cleanupInjected: cleanupFailure, fileCloseErrAt: 2,
			}},
		},
		{
			name: "stage shard after removal sync",
			stage: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{
				cleanupInjected: cleanupFailure, closeErrAt: 2,
			}},
		},
		{
			name:       "state record after removal sync",
			record:     outputV3RetirementRecordFaults{cleanupInjected: cleanupFailure, fileCloseErrAt: 1},
			recordGone: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, recordDir, recordName, retiring, binding := outputV3PreparedRetirement(t)
			stageFaults := test.stage
			recordFaults := test.record
			originalStages := session.stagesDir
			session.stagesDir = &outputV3RetirementDirectory{
				outputV3Directory: originalStages,
				faults:            &stageFaults,
			}
			faultedRecordDir := &outputV3RetirementRecordDirectory{
				outputV3Directory: recordDir,
				faults:            &recordFaults,
			}
			settlement, quarantined, err := outputV3RetireBoundFileAsOperation(
				t, session, faultedRecordDir, recordName, retiring, binding,
			)
			if settlement.Kind() != 0 || quarantined || !errors.Is(err, cleanupFailure) ||
				!outputV3FailureRequiresJobPause(err) {
				t.Fatalf("post-mutation cleanup = (kind=%v, quarantined=%t, err=%v)",
					settlement.Kind(), quarantined, err)
			}
			if stageFaults.child.removeFileCalls+recordFaults.removeFileCalls == 0 {
				t.Fatal("cleanup fault occurred before its deterministic namespace mutation")
			}
			if test.recordGone {
				kind, observeErr := recordDir.ObserveEntry(recordName)
				if observeErr != nil || kind != outputV3EntryAbsent {
					t.Fatalf("removed record after cleanup fault = (kind=%v, err=%v)", kind, observeErr)
				}
				return
			}
			persisted := outputV3PersistedFileRecord(t, session, retiring.Record().CanonicalLocator())
			if persisted.Phase() != resumestate.FileRetiring {
				t.Fatalf("post-mutation cleanup phase = %v, want Retiring", persisted.Phase())
			}
			settlement, quarantined, err = outputV3RetireBoundFileAsOperation(
				t, session, faultedRecordDir, recordName, retiring, binding,
			)
			if err != nil || quarantined || settlement.Kind() != transfer.FileRetired {
				t.Fatalf("post-mutation cleanup retry = (kind=%v, quarantined=%t, err=%v)",
					settlement.Kind(), quarantined, err)
			}
		})
	}
}

func TestOutputV3RetirementRetriesEveryOrderedCleanupCut(t *testing.T) {
	failure := errors.New("ordered retirement fault")

	for _, test := range []struct {
		name    string
		stage   outputV3RetirementDirectoryFaults
		anchor  outputV3RetirementDirectoryFaults
		record  outputV3RetirementRecordFaults
		cause   error
		code    transfer.OutputFaultCode
		mutates bool
		poisons bool
		noRetry bool
	}{
		{name: "reopen stage shard", stage: outputV3RetirementDirectoryFaults{classifyErrAt: 2, injected: failure}},
		{name: "open stage file", stage: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{openFileErrAt: 2, injected: failure}}},
		{name: "remove stage file", stage: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{removeFileErrAt: 1, injected: failure}}},
		{name: "sync stage removal", stage: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{syncErrAt: 1, injected: failure}}, mutates: true},
		{name: "resync removed stage before anchor", stage: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{syncErrAt: 2, injected: failure}}, mutates: true},
		{name: "reopen anchor shard", anchor: outputV3RetirementDirectoryFaults{classifyErrAt: 3, injected: failure}, cause: fs.ErrNotExist, code: transfer.OutputFaultOwnership, mutates: true},
		{name: "missing reopened anchor shard", anchor: outputV3RetirementDirectoryFaults{absentAt: 3, injected: failure}, cause: fs.ErrNotExist, code: transfer.OutputFaultOwnership, mutates: true},
		{name: "open retiring anchor", anchor: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{openFileErrAt: 3, injected: failure}}, mutates: true},
		{name: "remove retiring anchor", anchor: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{removeFileErrAt: 1, injected: failure}}, mutates: true},
		{name: "sync retiring anchor", anchor: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{syncErrAt: 1, injected: failure}}, mutates: true},
		{name: "close retiring anchor", anchor: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{fileCloseErrAt: 3, injected: failure}}, mutates: true},
		{name: "close retiring anchor shard", anchor: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{closeErrAt: 3, injected: failure}}, mutates: true},
		{name: "final stage shard sync", stage: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{syncErrAt: 3, injected: failure}}, mutates: true},
		{name: "final anchor shard sync", anchor: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{syncErrAt: 2, injected: failure}}, mutates: true},
		{name: "open retiring record", record: outputV3RetirementRecordFaults{openFileErrAt: 1, injected: failure}, mutates: true},
		{name: "read retiring record", record: outputV3RetirementRecordFaults{readErrAt: 1, injected: failure}, code: transfer.OutputFaultOwnership, mutates: true, poisons: true},
		{name: "remove retiring record", record: outputV3RetirementRecordFaults{removeFileErrAt: 1, injected: failure}, mutates: true},
		{name: "close removed retiring record", record: outputV3RetirementRecordFaults{fileCloseErrAt: 1, injected: failure}, mutates: true, noRetry: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, recordDir, recordName, retiring, binding := outputV3PreparedRetirement(t)
			stageFaults := test.stage
			anchorFaults := test.anchor
			recordFaults := test.record
			originalStages, originalAnchors := session.stagesDir, session.anchorsDir
			session.stagesDir = &outputV3RetirementDirectory{
				outputV3Directory: originalStages,
				faults:            &stageFaults,
			}
			session.anchorsDir = &outputV3RetirementDirectory{
				outputV3Directory: originalAnchors,
				faults:            &anchorFaults,
			}
			faultedRecordDir := &outputV3RetirementRecordDirectory{
				outputV3Directory: recordDir,
				faults:            &recordFaults,
			}
			settlement, quarantined, err := outputV3RetireBoundFileAsOperation(
				t, session, faultedRecordDir, recordName, retiring, binding,
			)
			cause := test.cause
			if cause == nil {
				cause = failure
			}
			if settlement.Kind() != 0 || quarantined || !errors.Is(err, cause) {
				t.Fatalf("faulted retirement = (kind=%v, quarantined=%t, err=%v)",
					settlement.Kind(), quarantined, err)
			}
			code := test.code
			if code == 0 {
				code = transfer.OutputFaultStateIO
			}
			outputV3SemanticRequireFault(t, err, transfer.OutputFaultFile, code)
			if !outputV3FailureRequiresJobPause(err) {
				t.Fatalf("retirement cut failure does not require PauseJob: %v", err)
			}
			if test.poisons {
				if !session.operationDisabled() {
					t.Fatal("uncertain state-record authority did not poison the current owner")
				}
				return
			}
			if test.noRetry {
				kind, observeErr := recordDir.ObserveEntry(recordName)
				if observeErr != nil || kind != outputV3EntryAbsent {
					t.Fatalf("terminal retirement cut retained record = (kind=%v, err=%v)", kind, observeErr)
				}
				return
			}

			settlement, _, err = outputV3RetireBoundFileAsOperation(
				t, session, faultedRecordDir, recordName, retiring, binding,
			)
			if err != nil || settlement.Kind() != transfer.FileRetired {
				t.Fatalf("retirement retry = (kind=%v, err=%v)", settlement.Kind(), err)
			}
			if test.mutates && stageFaults.child.removeFileCalls+anchorFaults.child.removeFileCalls == 0 {
				t.Fatal("durable-cut fault did not follow a namespace mutation")
			}
		})
	}
}

func TestOutputV3RetirementRecordSyncFailureLeavesNoDataBearingAuthority(t *testing.T) {
	failure := errors.New("retiring record directory sync failed")
	session, recordDir, recordName, retiring, binding := outputV3PreparedRetirement(t)
	faults := &outputV3RetirementRecordFaults{syncErrAt: 1, injected: failure}
	faultedRecordDir := &outputV3RetirementRecordDirectory{outputV3Directory: recordDir, faults: faults}

	settlement, _, err := outputV3RetireBoundFileAsOperation(
		t, session, faultedRecordDir, recordName, retiring, binding,
	)
	if settlement.Kind() != 0 || !errors.Is(err, failure) {
		t.Fatalf("record sync cut = (kind=%v, err=%v)", settlement.Kind(), err)
	}
	outputV3SemanticRequireFault(t, err, transfer.OutputFaultFile, transfer.OutputFaultStateIO)
	kind, observeErr := recordDir.ObserveEntry(recordName)
	if observeErr != nil || kind != outputV3EntryAbsent {
		t.Fatalf("record after failed removal sync = (kind=%v, err=%v)", kind, observeErr)
	}
	stage := resumestate.StageName(retiring.Record().OutputObject())
	anchor := resumestate.AnchorName(retiring.Record().OutputObject())
	for label, candidate := range map[string]struct {
		parent outputV3Directory
		name   resumestate.ShardedName
	}{
		"stage":  {parent: session.stagesDir, name: stage},
		"anchor": {parent: session.anchorsDir, name: anchor},
	} {
		shard, present, openErr := openOutputShard(candidate.parent, candidate.name.Shard(), false)
		if openErr != nil {
			t.Fatalf("open %s shard: %v", label, openErr)
		}
		if !present {
			continue
		}
		kind, observeErr = shard.ObserveEntry(candidate.name.Name())
		closeErr := shard.Close()
		if observeErr != nil || closeErr != nil || kind != outputV3EntryAbsent {
			t.Fatalf("%s after record sync cut = (kind=%v, err=%v)", label, kind, errors.Join(observeErr, closeErr))
		}
	}
}

func TestOutputV3AnchorObservationClassifiesFixedNamespaceEvidence(t *testing.T) {
	failure := errors.New("anchor observation fault")
	for _, test := range []struct {
		name    string
		faults  outputV3RetirementDirectoryFaults
		want    resumestate.AnchorObservation
		wantErr bool
	}{
		{name: "shard inspection failure", faults: outputV3RetirementDirectoryFaults{classifyErrAt: 1, injected: failure}, wantErr: true},
		{name: "missing shard", faults: outputV3RetirementDirectoryFaults{absentAt: 1}, want: resumestate.AnchorMissing},
		{name: "entry inspection failure", faults: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{observeErrAt: 1, injected: failure}}, wantErr: true},
		{name: "missing entry", faults: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{observeOverrideAt: 1, observeKind: outputV3EntryAbsent}}, want: resumestate.AnchorMissing},
		{name: "wrong entry kind", faults: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{observeOverrideAt: 1, observeKind: outputV3EntryDirectory}}, want: resumestate.AnchorUnsafe},
		{name: "fixed file open failure", faults: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{openFileErrAt: 1, injected: failure}}, want: resumestate.AnchorUnsafe},
		{name: "size inspection failure", faults: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{sizeErrAt: 1, injected: failure}}, want: resumestate.AnchorUnsafe},
		{name: "size mismatch", faults: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{sizeAdjustmentAt: 1, sizeAdjustment: 1}}, want: resumestate.AnchorUnsafe},
		{name: "verified", want: resumestate.AnchorVerified},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, transaction := outputV3PublicationReadyTransaction(t)
			faults := test.faults
			original := session.anchorsDir
			session.anchorsDir = &outputV3RetirementDirectory{outputV3Directory: original, faults: &faults}
			t.Cleanup(func() { session.anchorsDir = original })

			anchor, directory, observation, observeErr := session.observeAnchor(transaction.resumable.Bound().Record())
			if err := errors.Join(closeOutputV3ObservedFile(anchor), closeOutputV3Directory(directory)); err != nil &&
				!errors.Is(err, failure) {
				t.Fatalf("close observed anchor: %v", err)
			}
			if observation != test.want {
				t.Fatalf("anchor observation = %v, want %v", observation, test.want)
			}
			if (observeErr != nil) != test.wantErr || observeErr != nil && !errors.Is(observeErr, failure) {
				t.Fatalf("anchor observation error = %v, want injected=%t", observeErr, test.wantErr)
			}
		})
	}
}

func TestOutputV3StageObservationClassifiesFixedNamespaceEvidence(t *testing.T) {
	failure := errors.New("stage observation fault")
	for _, test := range []struct {
		name        string
		faults      outputV3RetirementDirectoryFaults
		anchorState resumestate.AnchorObservation
		want        resumestate.EntryObservation
		wantErr     bool
	}{
		{name: "shard inspection failure", faults: outputV3RetirementDirectoryFaults{classifyErrAt: 1, injected: failure}, anchorState: resumestate.AnchorVerified, wantErr: true},
		{name: "missing shard", faults: outputV3RetirementDirectoryFaults{absentAt: 1}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryMissing},
		{name: "entry inspection failure", faults: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{observeErrAt: 1, injected: failure}}, anchorState: resumestate.AnchorVerified, wantErr: true},
		{name: "missing entry", faults: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{observeOverrideAt: 1, observeKind: outputV3EntryAbsent}}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryMissing},
		{name: "wrong entry kind with anchor", faults: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{observeOverrideAt: 1, observeKind: outputV3EntryDirectory}}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryDifferentFromAnchor},
		{name: "wrong entry kind without anchor", faults: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{observeOverrideAt: 1, observeKind: outputV3EntryDirectory}}, anchorState: resumestate.AnchorMissing, want: resumestate.EntryPresentUnresolved},
		{name: "fixed file open failure", faults: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{openFileErrAt: 1, injected: failure}}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryUnsafe},
		{name: "unresolved without anchor", anchorState: resumestate.AnchorMissing, want: resumestate.EntryPresentUnresolved},
		{name: "identity comparison failure", faults: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{sameErrAt: 1, injected: failure}}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryUnsafe},
		{name: "different from anchor", faults: outputV3RetirementDirectoryFaults{child: outputV3RetirementChildFaults{differentAt: 1}}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryDifferentFromAnchor},
		{name: "same as anchor", anchorState: resumestate.AnchorVerified, want: resumestate.EntrySameAsAnchor},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, transaction := outputV3PublicationReadyTransaction(t)
			faults := test.faults
			original := session.stagesDir
			session.stagesDir = &outputV3RetirementDirectory{outputV3Directory: original, faults: &faults}
			t.Cleanup(func() { session.stagesDir = original })

			stage, directory, observation, observeErr := session.observeStage(
				transaction.resumable.Bound().Record(), transaction.anchor, test.anchorState,
			)
			if err := errors.Join(closeOutputV3ObservedFile(stage), closeOutputV3Directory(directory)); err != nil &&
				!errors.Is(err, failure) {
				t.Fatalf("close observed stage: %v", err)
			}
			if observation != test.want {
				t.Fatalf("stage observation = %v, want %v", observation, test.want)
			}
			if (observeErr != nil) != test.wantErr || observeErr != nil && !errors.Is(observeErr, failure) {
				t.Fatalf("stage observation error = %v, want injected=%t", observeErr, test.wantErr)
			}
		})
	}
}

type outputV3PublicationFileFaults struct {
	sizeErr          error
	sizeAdjustment   int64
	sameErrAt        int
	sameErr          error
	differentAt      int
	metadataErr      error
	metadataMismatch bool
	closeErr         error
}

func (faults outputV3PublicationFileFaults) hasInjectedError() bool {
	return faults.sizeErr != nil || faults.sameErr != nil || faults.metadataErr != nil || faults.closeErr != nil
}

type outputV3PublicationFile struct {
	outputV3File
	faults    outputV3PublicationFileFaults
	sameCalls int
}

func (file *outputV3PublicationFile) Size() (uint64, error) {
	if file.faults.sizeErr != nil {
		return 0, file.faults.sizeErr
	}
	size, err := file.outputV3File.Size()
	if err != nil || file.faults.sizeAdjustment == 0 {
		return size, err
	}
	if file.faults.sizeAdjustment > 0 {
		return size + uint64(file.faults.sizeAdjustment), nil
	}
	return size - uint64(-file.faults.sizeAdjustment), nil
}

func (file *outputV3PublicationFile) SameFile(other outputV3File) (bool, error) {
	file.sameCalls++
	if file.sameCalls == file.faults.sameErrAt {
		return false, file.faults.sameErr
	}
	if file.sameCalls == file.faults.differentAt {
		return false, nil
	}
	if wrapped, ok := other.(*outputV3PublicationFile); ok {
		other = wrapped.outputV3File
	}
	return file.outputV3File.SameFile(other)
}

func (file *outputV3PublicationFile) MetadataMatches(
	size uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	if file.faults.metadataErr != nil {
		return false, file.faults.metadataErr
	}
	if file.faults.metadataMismatch {
		return false, nil
	}
	return file.outputV3File.MetadataMatches(size, modified)
}

func (file *outputV3PublicationFile) Close() error {
	return errors.Join(file.outputV3File.Close(), file.faults.closeErr)
}

func unwrapOutputV3PublicationFile(file outputV3File) outputV3File {
	if wrapped, ok := file.(*outputV3PublicationFile); ok {
		return wrapped.outputV3File
	}
	return file
}

type outputV3PublicationOpenDirectory struct {
	outputV3Directory
	openErr    error
	fileFaults outputV3PublicationFileFaults
}

func (directory *outputV3PublicationOpenDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputV3File, error) {
	if directory.openErr != nil {
		return nil, directory.openErr
	}
	opened, err := directory.outputV3Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationFile{outputV3File: opened, faults: directory.fileFaults}, nil
}

type outputV3PublicationDirectoryFaults struct {
	duplicateErr       error
	duplicateErrAt     int
	duplicateCalls     int
	prepareIdentityErr error
	identityErr        error
	sameDirectoryAt    int
	sameDirectoryCalls int
	openDirectoryErr   error
	createDirectoryErr error
	createFileErr      error
	createdFaults      *outputV3PublicationFileFaults
	linkErr            error
	linkReturnsNil     bool
	linkedFaults       outputV3PublicationFileFaults
	observeErr         error
	observeKind        outputV3EntryKind
	openErr            error
	openedFaults       outputV3PublicationFileFaults
	syncErr            error
	modifiedErr        error
	closeErr           error
}

type outputV3PublicationDirectory struct {
	outputV3Directory
	faults *outputV3PublicationDirectoryFaults
}

func (directory *outputV3PublicationDirectory) Duplicate() (outputV3Directory, error) {
	directory.faults.duplicateCalls++
	if directory.faults.duplicateErr != nil &&
		(directory.faults.duplicateErrAt == 0 || directory.faults.duplicateCalls == directory.faults.duplicateErrAt) {
		return nil, directory.faults.duplicateErr
	}
	duplicate, err := directory.outputV3Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationDirectory{outputV3Directory: duplicate, faults: directory.faults}, nil
}

func (directory *outputV3PublicationDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	directory.faults.sameDirectoryCalls++
	if directory.faults.sameDirectoryAt != 0 &&
		directory.faults.sameDirectoryCalls == directory.faults.sameDirectoryAt {
		return false, nil
	}
	if wrapped, ok := other.(*outputV3PublicationDirectory); ok {
		other = wrapped.outputV3Directory
	}
	return directory.outputV3Directory.SameDirectory(other)
}

func (directory *outputV3PublicationDirectory) PrepareIdentityClaim() ([]byte, error) {
	if directory.faults.prepareIdentityErr != nil {
		return nil, directory.faults.prepareIdentityErr
	}
	return directory.outputV3Directory.PrepareIdentityClaim()
}

func (directory *outputV3PublicationDirectory) IdentityClaim() ([]byte, error) {
	if directory.faults.identityErr != nil {
		return nil, directory.faults.identityErr
	}
	return directory.outputV3Directory.IdentityClaim()
}

func (directory *outputV3PublicationDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	if directory.faults.openDirectoryErr != nil {
		return nil, directory.faults.openDirectoryErr
	}
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationDirectory{outputV3Directory: opened, faults: directory.faults}, nil
}

func (directory *outputV3PublicationDirectory) CreateDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	if directory.faults.createDirectoryErr != nil {
		return nil, directory.faults.createDirectoryErr
	}
	created, err := directory.outputV3Directory.CreateDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationDirectory{outputV3Directory: created, faults: directory.faults}, nil
}

func (directory *outputV3PublicationDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputV3File, error) {
	if directory.faults.createFileErr != nil {
		return nil, directory.faults.createFileErr
	}
	created, err := directory.outputV3Directory.CreateFile(name, private, size)
	if err != nil {
		return nil, err
	}
	faults := directory.faults.openedFaults
	if directory.faults.createdFaults != nil {
		faults = *directory.faults.createdFaults
	}
	return &outputV3PublicationFile{outputV3File: created, faults: faults}, nil
}

func (directory *outputV3PublicationDirectory) LinkFileNoReplace(
	source outputV3File,
	name string,
) (outputV3File, error) {
	if directory.faults.linkErr != nil {
		return nil, directory.faults.linkErr
	}
	if directory.faults.linkReturnsNil {
		return nil, nil
	}
	linked, err := directory.outputV3Directory.LinkFileNoReplace(unwrapOutputV3PublicationFile(source), name)
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationFile{outputV3File: linked, faults: directory.faults.linkedFaults}, nil
}

func (directory *outputV3PublicationDirectory) ReplacePrivateFile(source outputV3File, name string) error {
	return directory.outputV3Directory.ReplacePrivateFile(unwrapOutputV3PublicationFile(source), name)
}

func (directory *outputV3PublicationDirectory) RemoveFile(name string, expected outputV3File) error {
	return directory.outputV3Directory.RemoveFile(name, unwrapOutputV3PublicationFile(expected))
}

func (directory *outputV3PublicationDirectory) ObserveEntry(name string) (outputV3EntryKind, error) {
	if directory.faults.observeErr != nil {
		return outputV3EntryAbsent, directory.faults.observeErr
	}
	if directory.faults.observeKind != outputV3EntryAbsent {
		return directory.faults.observeKind, nil
	}
	return directory.outputV3Directory.ObserveEntry(name)
}

func (directory *outputV3PublicationDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputV3File, error) {
	if directory.faults.openErr != nil {
		return nil, directory.faults.openErr
	}
	opened, err := directory.outputV3Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationFile{outputV3File: opened, faults: directory.faults.openedFaults}, nil
}

func (directory *outputV3PublicationDirectory) Sync() error {
	if directory.faults.syncErr != nil {
		return directory.faults.syncErr
	}
	return directory.outputV3Directory.Sync()
}

func (directory *outputV3PublicationDirectory) SetModifiedTime(modified catalog.ModifiedTime) error {
	if directory.faults.modifiedErr != nil {
		return directory.faults.modifiedErr
	}
	return directory.outputV3Directory.SetModifiedTime(modified)
}

func (directory *outputV3PublicationDirectory) Close() error {
	return errors.Join(directory.outputV3Directory.Close(), directory.faults.closeErr)
}

type outputV3PublicationPlatform struct {
	outputV3Platform
	root          outputV3Directory
	guardErr      error
	guardCloseErr error
}

func (platform *outputV3PublicationPlatform) Root() outputV3Directory { return platform.root }

func (platform *outputV3PublicationPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	if platform.guardErr != nil {
		return nil, platform.guardErr
	}
	decorated := platform.root.(*outputV3PublicationDirectory)
	guard, err := acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return &outputV3PublicationDirectory{
				outputV3Directory: root,
				faults:            decorated.faults,
			}
		},
	)
	if err != nil || platform.guardCloseErr == nil {
		return guard, err
	}
	return &outputV3PublicationCloseFaultGuard{
		outputV3PublicOperationGuard: guard,
		closeErr:                     platform.guardCloseErr,
	}, nil
}

type outputV3PublicationCloseFaultGuard struct {
	outputV3PublicOperationGuard
	closeErr error
}

func (guard *outputV3PublicationCloseFaultGuard) Close() error {
	if guard == nil {
		return nil
	}
	var nativeErr error
	if guard.outputV3PublicOperationGuard != nil {
		nativeErr = guard.outputV3PublicOperationGuard.Close()
	}
	err := errors.Join(nativeErr, guard.closeErr)
	guard.outputV3PublicOperationGuard = nil
	return err
}

type outputV3RetirementDirectoryFaults struct {
	injected           error
	classifyErrAt      int
	absentAt           int
	classifyCalls      int
	openDirectoryErrAt int
	openDirectoryCalls int
	child              outputV3RetirementChildFaults
}

type outputV3RetirementDirectory struct {
	outputV3Directory
	faults *outputV3RetirementDirectoryFaults
}

func (directory *outputV3RetirementDirectory) ClassifyExactEntry(
	name string,
) (outputV3EntryKind, bool, error) {
	directory.faults.classifyCalls++
	if directory.faults.classifyCalls == directory.faults.classifyErrAt {
		return outputV3EntryAbsent, false, directory.faults.injected
	}
	if directory.faults.classifyCalls == directory.faults.absentAt {
		return outputV3EntryAbsent, true, nil
	}
	return directory.outputV3Directory.ClassifyExactEntry(name)
}

func (directory *outputV3RetirementDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	directory.faults.openDirectoryCalls++
	if directory.faults.openDirectoryCalls == directory.faults.openDirectoryErrAt {
		return nil, directory.faults.injected
	}
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	if directory.faults.child.injected == nil {
		directory.faults.child.injected = directory.faults.injected
	}
	return &outputV3RetirementChildDirectory{
		outputV3Directory: opened,
		faults:            &directory.faults.child,
	}, nil
}

func (directory *outputV3RetirementDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	if wrapped, ok := other.(*outputV3RetirementDirectory); ok {
		other = wrapped.outputV3Directory
	}
	return directory.outputV3Directory.SameDirectory(other)
}

type outputV3RetirementChildFaults struct {
	injected          error
	cleanupInjected   error
	observeErrAt      int
	observeOverrideAt int
	observeKind       outputV3EntryKind
	observeCalls      int
	openFileErrAt     int
	openFileCalls     int
	removeFileErrAt   int
	removeFileCalls   int
	syncErrAt         int
	syncCalls         int
	closeErrAt        int
	closeCalls        int
	fileCloseErrAt    int
	fileCloseCalls    int
	sizeErrAt         int
	sizeAdjustmentAt  int
	sizeAdjustment    int64
	sizeCalls         int
	sameErrAt         int
	differentAt       int
	sameCalls         int
}

type outputV3RetirementChildDirectory struct {
	outputV3Directory
	faults *outputV3RetirementChildFaults
}

func (directory *outputV3RetirementChildDirectory) ObserveEntry(name string) (outputV3EntryKind, error) {
	directory.faults.observeCalls++
	if directory.faults.observeCalls == directory.faults.observeErrAt {
		return outputV3EntryAbsent, directory.faults.injected
	}
	if directory.faults.observeCalls == directory.faults.observeOverrideAt {
		return directory.faults.observeKind, nil
	}
	return directory.outputV3Directory.ObserveEntry(name)
}

func (directory *outputV3RetirementChildDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputV3File, error) {
	directory.faults.openFileCalls++
	if directory.faults.openFileCalls == directory.faults.openFileErrAt {
		return nil, directory.faults.injected
	}
	opened, err := directory.outputV3Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	return &outputV3RetirementFile{outputV3File: opened, faults: directory.faults}, nil
}

func (directory *outputV3RetirementChildDirectory) RemoveFile(
	name string,
	expected outputV3File,
) error {
	directory.faults.removeFileCalls++
	if directory.faults.removeFileCalls == directory.faults.removeFileErrAt {
		return directory.faults.injected
	}
	if wrapped, ok := expected.(*outputV3RetirementFile); ok {
		expected = wrapped.outputV3File
	}
	return directory.outputV3Directory.RemoveFile(name, expected)
}

func (directory *outputV3RetirementChildDirectory) Sync() error {
	directory.faults.syncCalls++
	if directory.faults.syncCalls == directory.faults.syncErrAt {
		return directory.faults.injected
	}
	return directory.outputV3Directory.Sync()
}

func (directory *outputV3RetirementChildDirectory) Close() error {
	directory.faults.closeCalls++
	closeErr := directory.outputV3Directory.Close()
	if directory.faults.closeCalls == directory.faults.closeErrAt {
		return errors.Join(closeErr, directory.faults.cleanupError())
	}
	return closeErr
}

type outputV3RetirementFile struct {
	outputV3File
	faults *outputV3RetirementChildFaults
}

func (file *outputV3RetirementFile) Size() (uint64, error) {
	file.faults.sizeCalls++
	if file.faults.sizeCalls == file.faults.sizeErrAt {
		return 0, file.faults.injected
	}
	size, err := file.outputV3File.Size()
	if err != nil || file.faults.sizeCalls != file.faults.sizeAdjustmentAt {
		return size, err
	}
	if file.faults.sizeAdjustment > 0 {
		return size + uint64(file.faults.sizeAdjustment), nil
	}
	return size - uint64(-file.faults.sizeAdjustment), nil
}

func (file *outputV3RetirementFile) SameFile(other outputV3File) (bool, error) {
	file.faults.sameCalls++
	if file.faults.sameCalls == file.faults.sameErrAt {
		return false, file.faults.injected
	}
	if file.faults.sameCalls == file.faults.differentAt {
		return false, nil
	}
	if wrapped, ok := other.(*outputV3RetirementFile); ok {
		other = wrapped.outputV3File
	}
	return file.outputV3File.SameFile(other)
}

func (file *outputV3RetirementFile) Close() error {
	file.faults.fileCloseCalls++
	closeErr := file.outputV3File.Close()
	if file.faults.fileCloseCalls == file.faults.fileCloseErrAt {
		return errors.Join(closeErr, file.faults.cleanupError())
	}
	return closeErr
}

func (faults *outputV3RetirementChildFaults) cleanupError() error {
	if faults.cleanupInjected != nil {
		return faults.cleanupInjected
	}
	return faults.injected
}

type outputV3RetirementRecordFaults struct {
	injected        error
	cleanupInjected error
	openFileErrAt   int
	openFileCalls   int
	readErrAt       int
	readCalls       int
	fileCloseErrAt  int
	fileCloseCalls  int
	removeFileErrAt int
	removeFileCalls int
	syncErrAt       int
	syncCalls       int
}

type outputV3RetirementRecordDirectory struct {
	outputV3Directory
	faults *outputV3RetirementRecordFaults
}

func (directory *outputV3RetirementRecordDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputV3File, error) {
	directory.faults.openFileCalls++
	if directory.faults.openFileCalls == directory.faults.openFileErrAt {
		return nil, directory.faults.injected
	}
	opened, err := directory.outputV3Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	return &outputV3RetirementRecordFile{outputV3File: opened, faults: directory.faults}, nil
}

func (directory *outputV3RetirementRecordDirectory) RemoveFile(
	name string,
	expected outputV3File,
) error {
	directory.faults.removeFileCalls++
	if directory.faults.removeFileCalls == directory.faults.removeFileErrAt {
		return directory.faults.injected
	}
	if wrapped, ok := expected.(*outputV3RetirementRecordFile); ok {
		expected = wrapped.outputV3File
	}
	return directory.outputV3Directory.RemoveFile(name, expected)
}

func (directory *outputV3RetirementRecordDirectory) Sync() error {
	directory.faults.syncCalls++
	if directory.faults.syncCalls == directory.faults.syncErrAt {
		return directory.faults.injected
	}
	return directory.outputV3Directory.Sync()
}

type outputV3RetirementRecordFile struct {
	outputV3File
	faults *outputV3RetirementRecordFaults
}

func (file *outputV3RetirementRecordFile) Close() error {
	file.faults.fileCloseCalls++
	closeErr := file.outputV3File.Close()
	if file.faults.fileCloseCalls == file.faults.fileCloseErrAt {
		return errors.Join(closeErr, file.faults.cleanupError())
	}
	return closeErr
}

func (faults *outputV3RetirementRecordFaults) cleanupError() error {
	if faults.cleanupInjected != nil {
		return faults.cleanupInjected
	}
	return faults.injected
}

func (file *outputV3RetirementRecordFile) ReadAt(data []byte, offset int64) (int, error) {
	file.faults.readCalls++
	if file.faults.readCalls == file.faults.readErrAt {
		return 0, file.faults.injected
	}
	return file.outputV3File.ReadAt(data, offset)
}

func outputV3PreparedRetirement(
	t *testing.T,
) (
	*filesystemOutputSession,
	outputV3Directory,
	string,
	resumestate.BoundFileRecord,
	transfer.OutputFileBinding,
) {
	t.Helper()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
	record := v3RecoveryPrepareRetiringCut(t, opened.Session, file)
	recordName := resumestate.FileRecordName(record.LocatorDigest())
	recordDir := outputV3SemanticOpenShard(t, opened.Session.filesDir, recordName.Shard(), false)
	t.Cleanup(func() {
		if err := recordDir.Close(); err != nil {
			t.Errorf("close retirement record shard: %v", err)
		}
	})
	retiring, err := resumestate.BindFileRecord(
		opened.Session.stateSnapshot(), recordName.Shard(), recordName.Name(), record,
	)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := outputBindingForRecord(opened.Session.SessionID(), file.Descriptor, record)
	if err != nil {
		t.Fatal(err)
	}
	return opened.Session, recordDir, recordName.Name(), retiring, binding
}

func outputV3RetireBoundFileAsOperation(
	t *testing.T,
	session *filesystemOutputSession,
	recordDir outputV3Directory,
	recordName string,
	retiring resumestate.BoundFileRecord,
	binding transfer.OutputFileBinding,
) (transfer.FileSettlement, bool, error) {
	t.Helper()
	if err := session.beginOperation(); err != nil {
		t.Fatalf("begin retirement operation: %v", err)
	}
	defer session.endOperation()
	return session.retireBoundFile(recordDir, recordName, retiring, binding)
}

func closeOutputV3ObservedFile(file outputV3File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func outputV3PersistedFileRecord(
	t *testing.T,
	session *filesystemOutputSession,
	locator string,
) resumestate.FileRecord {
	t.Helper()
	name := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(locator))
	shard := outputV3SemanticOpenShard(t, session.filesDir, name.Shard(), false)
	encoded, readErr := readStateRecord(shard, name.Name(), resumestate.MaxFileStateBytes)
	closeErr := shard.Close()
	record, decodeErr := resumestate.DecodeFileRecord(encoded)
	if err := errors.Join(readErr, closeErr, decodeErr); err != nil {
		t.Fatalf("read persisted file record: %v", err)
	}
	return record
}

func outputV3PublicationReadyTransaction(
	t *testing.T,
) (*filesystemOutputSession, *filesystemFileTransaction) {
	t.Helper()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)
	t.Cleanup(func() {
		transaction.mu.Lock()
		open := transaction.lifecycle == filesystemFileTransactionOpen
		transaction.mu.Unlock()
		if open {
			outputV3SemanticDetachTransaction(t, opened.Session, transaction)
		}
	})
	if err := transaction.data.SetModifiedTime(transaction.descriptor.ModifiedTime()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.data.Sync(); err != nil {
		t.Fatal(err)
	}
	return opened.Session, transaction
}

func outputV3CreatePublicCollision(
	t *testing.T,
	session *filesystemOutputSession,
	locator string,
	kind outputV3EntryKind,
) {
	t.Helper()
	parentPath, leaf, err := outputLocatorParentAndLeaf(locator)
	if err != nil {
		t.Fatal(err)
	}
	requirement := outputAncestryRequirement{path: parentPath, authority: outputAncestryCreateAuthority}
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cleanupErr := errors.Join(validation.Revalidate(requirement), closeOutputAncestryValidation(validation)); cleanupErr != nil {
			t.Errorf("finish public collision setup: %v", cleanupErr)
		}
	}()
	parent, err := validation.directory(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := validation.revalidateRetainedDirectory(parentPath, outputAncestryCreateAuthority); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case outputV3EntryRegularFile:
		file, createErr := parent.CreateFile(leaf, false, 1)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if err := errors.Join(file.Sync(), file.Close(), parent.Sync()); err != nil {
			t.Fatal(err)
		}
	case outputV3EntryDirectory:
		directory, createErr := parent.CreateDirectory(leaf, false)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if err := errors.Join(directory.Sync(), directory.Close(), parent.Sync()); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported collision kind %d", kind)
	}
}
