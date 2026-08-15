package transfer

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
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
			if result.Measure.Discovery != DiscoveryFailed || result.Measure.DiscoveryTerminalSuccess ||
				result.Measure.DiscoveredFiles != 1 || result.Measure.DiscoveredBytes != exactSize ||
				result.Measure.CompletedFiles != 0 || result.Measure.CompletedBytes != 0 {
				t.Fatalf("anchor drift measure = %+v", result.Measure)
			}
			if len(result.Directories) != 0 || len(result.Files) != 0 ||
				result.SelectionResolutionFailure != nil || result.SucceededFiles != 0 ||
				result.FileOutcomes != (FileOutcomeSummary{}) {
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
