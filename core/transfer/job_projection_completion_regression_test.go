package transfer

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestSelectedAnchorDriftFinalizesAsTypedPartialAndLeavesNoArtifactAuthority(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xc1)
	root := transferID[catalog.DirectoryID](0xc2)
	frozenFile := transferID[catalog.FileID](0xc3)
	driftedFile := transferID[catalog.FileID](0xc4)
	const sourcePath = "file.bin"
	const exactSize uint64 = 17

	tests := []struct {
		name        string
		catalogFile catalog.FileID
		catalogPath string
		rules       func(*testing.T) SelectionRules
	}{
		{
			name: "same path different identity", catalogFile: driftedFile, catalogPath: sourcePath,
			rules: func(t *testing.T) SelectionRules {
				t.Helper()
				rules, err := NewPathSelectionRules([]string{sourcePath})
				if err != nil {
					t.Fatal(err)
				}
				return rules
			},
		},
		{
			name: "same identity different path", catalogFile: frozenFile, catalogPath: "moved.bin",
			rules: func(t *testing.T) SelectionRules {
				t.Helper()
				rules, err := NewSelectionRules(false, []SelectionOverride{{FileID: frozenFile, Selected: true}})
				if err != nil {
					t.Fatal(err)
				}
				return rules
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			intent := projectionDriftIntent(t, share, root, frozenFile, sourcePath, test.rules(t))
			output := newJobOutput(share)
			revisions := &jobRevisionClient{
				opened: make(map[catalog.FileID]OpenedRevision), failures: make(map[catalog.FileID]error),
			}
			job, err := newTestTransferJob(t, testTransferJobConfig{
				ReceiveIntent: intent,
				Catalog: failingCatalog{
					snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
						root: jobSnapshot(t, share, root, 0xc5, jobEntry(t, test.catalogFile, test.catalogPath, exactSize)),
					},
					failures: make(map[catalog.DirectoryID]error),
				},
				Revisions: revisions, Blocks: scriptedRangeReader{}, Materializer: output,
			})
			if err != nil {
				t.Fatal(err)
			}

			result := job.Run(context.Background())
			if result.Outcome != DirectTreeOutcomePartial ||
				result.Settlement.Kind() != DirectTreeSettlementPartial {
				t.Fatalf("anchor drift terminal result = (outcome %d, settlement %d)", result.Outcome, result.Settlement.Kind())
			}
			if result.SourceDriftFault != mustCatalogFault(fault.ScopeDirectoryLocal, fault.CatalogDirectoryStale) {
				t.Fatalf("anchor drift fault = %v", result.SourceDriftFault)
			}
			drift, admitted := admitLifecycleFailure(result.SourceDriftFailure)
			if !admitted || !errors.Is(drift.diagnostic, ErrFrozenSourceDrift) {
				t.Fatalf("anchor drift diagnostic = %v", result.SourceDriftFailure)
			}
			if result.Progress.Discovery != DiscoveryFailed ||
				result.Progress.DiscoveredFiles != 1 || result.Progress.DiscoveredBytes != exactSize ||
				result.Progress.PublishedFiles != 0 || result.Progress.PublishedBytes != 0 {
				t.Fatalf("anchor drift measure = %+v", result.Progress)
			}
			if len(result.Directories) != 0 || len(result.Files) != 0 ||
				result.SelectionResolutionFailure != nil || result.SucceededFiles != 0 ||
				result.Progress.FileOutcomes != (FileOutcomeSummary{}) {
				t.Fatalf("anchor drift manufactured artifact failure coordinates: %+v", result)
			}

			revisions.mu.Lock()
			opened := len(revisions.order)
			revisions.mu.Unlock()
			output.mu.Lock()
			finished, completeCalls, pauseCalls := output.finished, output.completeCalls, output.pauseCalls
			transactions := len(output.transactions)
			output.mu.Unlock()
			if opened != 0 || transactions != 0 {
				t.Fatalf("rejected anchor reached content/output: revisions=%d transactions=%d", opened, transactions)
			}
			if finished != DirectTreeOutcomePartial || completeCalls != 1 || pauseCalls != 0 {
				t.Fatalf("terminal settlement = (outcome %d, finalize %d, pause %d)", finished, completeCalls, pauseCalls)
			}
		})
	}
}

func TestSelectedDirectoryAnchorDriftIsRejectedBeforeOutputAdmission(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xf1)
	root := transferID[catalog.DirectoryID](0xf2)
	frozenDirectory := transferID[catalog.DirectoryID](0xf3)
	driftedDirectory := transferID[catalog.DirectoryID](0xf4)
	const sourcePath = "photos"
	rules, err := NewPathSelectionRules([]string{sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	intent := directoryProjectionDriftIntent(t, share, root, frozenDirectory, sourcePath, rules)
	tree, directoryTree := intent.ArtifactSpec().DirectoryTree()
	resultRoot, resultRootArtifact := tree.ResultRoot()
	scope, scopeErr := NewDirectoryAdmissionScope(intent)
	rootExpectation := scope.RootExpectation()
	if !directoryTree || !resultRootArtifact || scopeErr != nil ||
		resultRoot.SourcePath() != sourcePath ||
		rootExpectation.Kind() != DirectoryAdmissionDirectoryAnchor ||
		rootExpectation.DirectoryID() != frozenDirectory || rootExpectation.Path() != "" {
		t.Fatalf("source/materialization coordinates = (source %q, root %q, scope error %v)",
			resultRoot.SourcePath(), rootExpectation.Path(), scopeErr)
	}
	catalogClient := &countingJobCatalog{
		snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
			root:             jobSnapshot(t, share, root, 0xf5, jobDirectoryEntry(t, driftedDirectory, sourcePath)),
			driftedDirectory: jobSnapshot(t, share, driftedDirectory, 0xf6),
		},
		loads: make(map[catalog.DirectoryID]int),
	}
	output := newJobOutput(share)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ReceiveIntent: intent, Catalog: catalogClient,
		Revisions: &jobRevisionClient{
			opened: make(map[catalog.FileID]OpenedRevision), failures: make(map[catalog.FileID]error),
		},
		Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartial ||
		result.Settlement.Kind() != DirectTreeSettlementPartial {
		t.Fatalf("directory anchor drift terminal result = (outcome %d, settlement %d)",
			result.Outcome, result.Settlement.Kind())
	}
	if result.SourceDriftFault != mustCatalogFault(fault.ScopeDirectoryLocal, fault.CatalogDirectoryStale) {
		t.Fatalf("directory anchor drift fault = %v", result.SourceDriftFault)
	}
	drift, admitted := admitLifecycleFailure(result.SourceDriftFailure)
	if !admitted || !errors.Is(drift.diagnostic, ErrFrozenSourceDrift) {
		t.Fatalf("directory anchor drift diagnostic = %v", result.SourceDriftFailure)
	}
	if result.Progress.Discovery != DiscoveryFailed || len(result.Directories) != 0 || len(result.Files) != 0 {
		t.Fatalf("directory anchor drift manufactured artifact coordinates: %+v", result)
	}
	if catalogClient.loadCount(driftedDirectory) != 1 {
		t.Fatalf("selected empty directory discovery count = %d", catalogClient.loadCount(driftedDirectory))
	}
	output.mu.Lock()
	defer output.mu.Unlock()
	if len(output.directories) != 0 || len(output.finalized) != 0 || len(output.transactions) != 0 {
		t.Fatalf("rejected directory reached output: admitted=%v finalized=%v transactions=%d",
			output.directories, output.finalized, len(output.transactions))
	}
	if output.finished != DirectTreeOutcomePartial || output.completeCalls != 1 || output.pauseCalls != 0 {
		t.Fatalf("directory anchor settlement = outcome %d complete %d pause %d",
			output.finished, output.completeCalls, output.pauseCalls)
	}
}

func TestTransferJobSingleFileUsesReferenceParentWithoutDirectoryLifecycle(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xe1)
	root := transferID[catalog.DirectoryID](0xe2)
	file := transferID[catalog.FileID](0xe3)
	const sourcePath = "file.bin"
	const exactSize uint64 = 17
	rules, err := NewSelectionRules(false, []SelectionOverride{{FileID: file, Selected: true}})
	if err != nil {
		t.Fatal(err)
	}
	intent := projectionDriftIntent(t, share, root, file, sourcePath, rules)
	descriptor := jobDescriptor(t, share, file, 0xe4, exactSize)
	opened, err := NewOpenedRevision(transferID[content.LeaseID](0xe5), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	output := newJobOutput(share)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ReceiveIntent: intent,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 0xe6, jobEntry(t, file, sourcePath, exactSize)),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		},
		Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeSuccess || result.Settlement.Kind() != DirectTreeSettlementSuccess ||
		result.SucceededFiles != 1 {
		t.Fatalf("single-file terminal result = %+v", result)
	}
	output.mu.Lock()
	defer output.mu.Unlock()
	if len(output.directories) != 0 || len(output.finalized) != 0 || len(output.transactions) != 1 {
		t.Fatalf("single-file directory/file lifecycle = admitted %v finalized %v transactions %d",
			output.directories, output.finalized, len(output.transactions))
	}
	if output.finished != DirectTreeOutcomeSuccess || output.completeCalls != 1 || output.pauseCalls != 0 {
		t.Fatalf("single-file settlement = outcome %d complete %d pause %d",
			output.finished, output.completeCalls, output.pauseCalls)
	}
}

func projectionDriftIntent(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	file catalog.FileID,
	sourcePath string,
	rules SelectionRules,
) ReceiveIntent {
	t.Helper()
	selection, err := NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewSingleFileDirectoryTree(file, sourcePath, "file.bin")
	if err != nil {
		t.Fatal(err)
	}
	operation := transferID[receivecontract.OperationID](0xd1)
	reservationID := transferID[receivecontract.DestinationReservationID](0xd2)
	authority, err := receivecontract.AuthorityRefFromBytes(
		bytes.Repeat([]byte{0xd3}, receivecontract.AuthorityRefBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeNamedEntryReservation(
		operation, reservationID, artifact, authority, "file.bin", 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func directoryProjectionDriftIntent(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	directory catalog.DirectoryID,
	sourcePath string,
	rules SelectionRules,
) ReceiveIntent {
	t.Helper()
	selection, err := NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	resultRoot, err := receivecontract.NewCompleteDirectoryResultRoot(directory, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(resultRoot)
	if err != nil {
		t.Fatal(err)
	}
	operation := transferID[receivecontract.OperationID](0xfa)
	reservationID := transferID[receivecontract.DestinationReservationID](0xfb)
	authority, err := receivecontract.AuthorityRefFromBytes(
		bytes.Repeat([]byte{0xfc}, receivecontract.AuthorityRefBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewFSANamedEntryReservation(
		operation, reservationID, artifact, authority, resultRoot.Name(), resultRoot.Name(), 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}
