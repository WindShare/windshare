package resumecommand

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs"
)

func TestFilesystemRunnerWiresResumeFacadeAndHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	runner := NewFilesystemRunner(FilesystemConfig{
		Input:                    strings.NewReader(""),
		Output:                   &stdout,
		RawTerminalOutput:        &stderr,
		SerializedTerminalOutput: &stderr,
		Logf: func(format string, args ...any) {
			t.Errorf("unexpected diagnostic: "+format, args...)
		},
	})
	if result := runner.Run(context.Background(), []string{"help"}); result != ResultOK {
		t.Fatalf("result=%d stderr=%q", result, stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "windshare resume discard") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if _, err := (filesystemResumeStateInventoryOpener{}).OpenResumeStateInventory(
		context.Background(),
		"relative-root",
	); err == nil {
		t.Fatal("relative native root was accepted")
	}
}

func TestFilesystemProjectionFailsClosedForDetachedValues(t *testing.T) {
	if _, err := projectResumeStateSummary(osfs.ResumeStateSummary{}); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("zero summary error=%v", err)
	}
	if _, err := projectResumeDiscardSummary(osfs.ResumeStateSummary{}); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("zero discard result error=%v", err)
	}
	if _, err := projectResumeStateInventory(osfs.ResumeStateInventory{}); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("zero inventory error=%v", err)
	}
	if _, err := projectResumeAttention([]osfs.ResumeStateAttention{{}}); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("zero attention error=%v", err)
	}

	var detached *filesystemResumeStateInventory
	if _, err := detached.Items(); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("detached items error=%v", err)
	}
	if _, err := detached.Discard(context.Background(), 0); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("detached discard error=%v", err)
	}
}

func TestResumeRendererUsesClosedLegacyEnums(t *testing.T) {
	report := osfs.CheckpointCleanupReport{
		Status:   osfs.CheckpointCleanupStatusComplete,
		Complete: true,
		Entries: []osfs.CheckpointCleanupEntry{
			{RelativePath: "kept", Disposition: osfs.CheckpointCleanupSkip},
			{RelativePath: "removed", Disposition: osfs.CheckpointCleanupRemove},
			{RelativePath: "quarantined", Disposition: osfs.CheckpointCleanupQuarantine},
		},
	}
	rendered, err := (textRenderer{}).LegacyCleanup(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"path=\"kept\" disposition=\"skip\"",
		"path=\"removed\" disposition=\"remove\"",
		"path=\"quarantined\" disposition=\"quarantine\"",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered=%q missing=%q", rendered, want)
		}
	}

	report.Status = osfs.CheckpointCleanupStatus(255)
	if _, err := (textRenderer{}).LegacyCleanup(report); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("unknown status error=%v", err)
	}
	report.Status = osfs.CheckpointCleanupStatusComplete
	report.Entries[0].Disposition = osfs.CheckpointCleanupDisposition(255)
	if _, err := (textRenderer{}).LegacyCleanup(report); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("unknown disposition error=%v", err)
	}
}
