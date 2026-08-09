package transfer

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestTransferJobRejectsCompleteOnlyPlanBeforeMaterialization(t *testing.T) {
	share := transferID[catalog.ShareInstance](181)
	root := transferID[catalog.DirectoryID](182)
	file := transferID[catalog.FileID](183)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewOriginalFileArtifact(file, "file.bin", "file.bin")
	if err != nil {
		t.Fatal(err)
	}
	operation, _ := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{1}, receivecontract.StableIdentityBytes))
	workspaceID, _ := receivecontract.WorkspaceIDFromBytes(bytes.Repeat([]byte{2}, receivecontract.StableIdentityBytes))
	repository, _ := receivecontract.RepositoryRefFromBytes(bytes.Repeat([]byte{3}, receivecontract.AuthorityRefBytes))
	workspace, err := receivecontract.NewWorkspaceBinding(operation, workspaceID, artifact, repository)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewWorkspaceThenPublishPlan(artifact, workspace)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := TransferJobIDFromBytes(bytes.Repeat([]byte{4}, TransferJobIdentityBytes))
	opened := false
	_, err = NewTransferJob(TransferJobConfig{
		ReceiveIntent: intent,
		JobID:         jobID,
		Catalog:       failingCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{}},
		Revisions:     &jobRevisionClient{},
		Blocks:        scriptedRangeReader{},
		Materializer: DirectTreeMaterializerFunc(func(context.Context, ReceiveIntent) (DirectTreeSession, error) {
			opened = true
			return nil, errors.New("must not open")
		}),
	})
	if !errors.Is(err, ErrInvalidTransferJob) {
		t.Fatalf("complete-only plan error = %v", err)
	}
	if opened {
		t.Fatal("complete-only plan reached the progressive DirectTree materializer")
	}
}
