package transfer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type backendIDAdmissionOutput struct {
	*jobOutput
	backend                OutputBackendID
	openSelectionCalls     int
	beginFileCalls         int
	finalizeDirectoryCalls int
	pauseJobCalls          int
	completeJobCalls       int
}

func (output *backendIDAdmissionOutput) BackendID() OutputBackendID {
	return output.backend
}

func (output *backendIDAdmissionOutput) OpenSelection(
	context.Context,
	OutputSelection,
) (OutputSession, error) {
	output.openSelectionCalls++
	return output, nil
}

func (output *backendIDAdmissionOutput) BeginFile(
	ctx context.Context,
	file OutputFile,
) (FileStart, error) {
	output.beginFileCalls++
	return output.jobOutput.BeginFile(ctx, file)
}

func (output *backendIDAdmissionOutput) FinalizeDirectory(
	ctx context.Context,
	directory OutputDirectory,
) error {
	output.finalizeDirectoryCalls++
	return output.jobOutput.FinalizeDirectory(ctx, directory)
}

func (output *backendIDAdmissionOutput) PauseJob(
	ctx context.Context,
	reason JobPauseReason,
) (JobSettlement, error) {
	output.pauseJobCalls++
	return output.jobOutput.PauseJob(ctx, reason)
}

func (output *backendIDAdmissionOutput) CompleteJob(
	ctx context.Context,
	outcome JobOutcome,
) (JobSettlement, error) {
	output.completeJobCalls++
	return output.jobOutput.CompleteJob(ctx, outcome)
}

type backendIDAdmissionRangeReader struct {
	calls int
}

func (reader *backendIDAdmissionRangeReader) ReadRange(
	ctx context.Context,
	_ content.LeaseID,
	_ content.FileRevisionDescriptor,
	requested content.Range,
	sink RangeSink,
) error {
	reader.calls++
	return sink.WriteRange(ctx, requested.Offset, make([]byte, requested.Length()))
}

type backendIDAdmissionFixture struct {
	job       *TransferJob
	output    *backendIDAdmissionOutput
	revisions *jobRevisionClient
	blocks    *backendIDAdmissionRangeReader
	file      catalog.FileID
	lease     content.LeaseID
}

func newBackendIDAdmissionFixture(
	t *testing.T,
	backend OutputBackendID,
) backendIDAdmissionFixture {
	t.Helper()
	share := transferID[catalog.ShareInstance](181)
	root := transferID[catalog.DirectoryID](182)
	file := transferID[catalog.FileID](183)
	lease := transferID[content.LeaseID](184)
	const fileSize = uint64(1)

	descriptor := jobDescriptor(t, share, file, 185, fileSize)
	opened, err := NewOpenedRevision(lease, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	revisions := &jobRevisionClient{
		opened:   map[catalog.FileID]OpenedRevision{file: opened},
		failures: make(map[catalog.FileID]error),
	}
	blocks := &backendIDAdmissionRangeReader{}
	output := &backendIDAdmissionOutput{
		jobOutput: newJobOutput(share),
		backend:   backend,
	}
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := NewTransferJob(TransferJobConfig{
		ShareInstance: share,
		SyntheticRoot: root,
		Rules:         rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 186, jobEntry(t, file, "file.bin", fileSize)),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: revisions,
		Blocks:    blocks,
		Output:    output,
	})
	if err != nil {
		t.Fatal(err)
	}
	return backendIDAdmissionFixture{
		job: job, output: output, revisions: revisions, blocks: blocks, file: file, lease: lease,
	}
}

func TestTransferJobRejectsMalformedBackendIDBeforeDownstreamWork(t *testing.T) {
	tests := []struct {
		name    string
		backend string
	}{
		{name: "empty", backend: ""},
		{name: "whitespace only", backend: " \t\r\n"},
		{name: "leading whitespace", backend: " backend"},
		{name: "trailing whitespace", backend: "backend "},
		{name: "oversize", backend: strings.Repeat("x", MaxOutputBackendIDBytes+1)},
		{name: "invalid UTF-8", backend: string([]byte{'b', 0xff})},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBackendIDAdmissionFixture(t, OutputBackendID(test.backend))
			result := fixture.job.Run(context.Background())

			if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, ErrOutputContract) {
				t.Fatalf("result = %+v", result)
			}
			if fixture.output.openSelectionCalls != 1 {
				t.Fatalf("OpenSelection calls = %d", fixture.output.openSelectionCalls)
			}
			if fixture.output.beginFileCalls != 0 || fixture.output.finalizeDirectoryCalls != 0 ||
				fixture.output.pauseJobCalls != 0 || fixture.output.completeJobCalls != 0 {
				t.Fatalf(
					"downstream output calls: begin=%d finalize=%d pause=%d complete=%d",
					fixture.output.beginFileCalls,
					fixture.output.finalizeDirectoryCalls,
					fixture.output.pauseJobCalls,
					fixture.output.completeJobCalls,
				)
			}
			if len(fixture.revisions.order) != 0 || len(fixture.revisions.released) != 0 || fixture.blocks.calls != 0 {
				t.Fatalf(
					"downstream file work: opened=%v released=%v range reads=%d",
					fixture.revisions.order,
					fixture.revisions.released,
					fixture.blocks.calls,
				)
			}
			if len(fixture.output.transactions) != 0 || result.SucceededFiles != 0 || len(result.Files) != 0 {
				t.Fatalf(
					"downstream transaction work: transactions=%d succeeded=%d failures=%d",
					len(fixture.output.transactions),
					result.SucceededFiles,
					len(result.Files),
				)
			}
		})
	}
}

func TestTransferJobAcceptsCanonicalBackendIDWithoutChangingWorkflow(t *testing.T) {
	backend, err := NewOutputBackendID("test/backend-id-admission")
	if err != nil {
		t.Fatal(err)
	}
	fixture := newBackendIDAdmissionFixture(t, backend)
	result := fixture.job.Run(context.Background())

	if result.Outcome != JobSucceeded || result.TerminationCause != nil || result.SucceededFiles != 1 {
		t.Fatalf("result = %+v", result)
	}
	if fixture.output.openSelectionCalls != 1 || fixture.output.beginFileCalls != 1 ||
		fixture.output.completeJobCalls != 1 || fixture.output.pauseJobCalls != 0 {
		t.Fatalf(
			"output calls: open=%d begin=%d complete=%d pause=%d",
			fixture.output.openSelectionCalls,
			fixture.output.beginFileCalls,
			fixture.output.completeJobCalls,
			fixture.output.pauseJobCalls,
		)
	}
	if len(fixture.revisions.order) != 1 || fixture.revisions.order[0] != fixture.file ||
		len(fixture.revisions.released) != 1 || fixture.revisions.released[0] != fixture.lease ||
		fixture.blocks.calls != 1 {
		t.Fatalf(
			"file work: opened=%v released=%v range reads=%d",
			fixture.revisions.order,
			fixture.revisions.released,
			fixture.blocks.calls,
		)
	}
}
