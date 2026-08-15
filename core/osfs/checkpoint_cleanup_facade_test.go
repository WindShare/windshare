package osfs

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointcleaner"
)

func TestCheckpointCleanupFacadeProjectsDetachedReport(t *testing.T) {
	inner := checkpointcleaner.CheckpointCleanupReport{
		Status:      checkpointcleaner.CheckpointCleanupStatusNeedsAttention,
		Complete:    false,
		Resumed:     true,
		Scanned:     7,
		Removed:     2,
		Quarantined: 1,
		Skipped:     4,
		Entries: []checkpointcleaner.CheckpointCleanupEntry{
			{RelativePath: "removed", Disposition: checkpointcleaner.CheckpointCleanupRemove, Detail: "owned"},
			{RelativePath: "quarantined", Disposition: checkpointcleaner.CheckpointCleanupQuarantine, Detail: "uncertain"},
		},
		Attention: []string{"review quarantine"},
	}

	projected := projectCleanupReport(inner)
	if projected.Status != CheckpointCleanupStatusNeedsAttention || projected.Complete ||
		!projected.Resumed || projected.Scanned != 7 || projected.Removed != 2 ||
		projected.Quarantined != 1 || projected.Skipped != 4 ||
		len(projected.Entries) != 2 || len(projected.Attention) != 1 {
		t.Fatalf("projected cleanup report = %+v", projected)
	}
	if projected.Entries[0].Disposition != CheckpointCleanupRemove ||
		projected.Entries[1].Disposition != CheckpointCleanupQuarantine ||
		!projected.NeedsAttention() {
		t.Fatalf("projected cleanup semantics = %+v", projected)
	}

	inner.Entries[0].Detail = "mutated"
	inner.Attention[0] = "mutated"
	if projected.Entries[0].Detail != "owned" || projected.Attention[0] != "review quarantine" {
		t.Fatalf("projection retained internal slices: %+v", projected)
	}
	if (CheckpointCleanupReport{Status: CheckpointCleanupStatusComplete}).NeedsAttention() {
		t.Fatal("complete cleanup unexpectedly needs attention")
	}
	if !(CheckpointCleanupReport{Attention: []string{"review"}}).NeedsAttention() {
		t.Fatal("explicit attention was ignored")
	}
}

func TestCheckpointCleanupFacadeNormalizesClosedErrorTaxonomy(t *testing.T) {
	tests := []struct {
		name     string
		internal error
		public   error
	}{
		{name: "busy", internal: checkpointcleaner.ErrCheckpointCleanerBusy, public: ErrCheckpointCleanerBusy},
		{name: "ownership", internal: checkpointcleaner.ErrCheckpointCleanerOwnership, public: ErrCheckpointCleanerOwnership},
		{name: "state", internal: checkpointcleaner.ErrCheckpointCleanerState, public: ErrCheckpointCleanerState},
		{name: "limit", internal: checkpointcleaner.ErrCheckpointCleanerLimit, public: ErrCheckpointCleanerLimit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			internal := errors.Join(errors.New("context"), test.internal)
			got := wrapCleanerError(internal)
			if !errors.Is(got, test.public) || !errors.Is(got, test.internal) {
				t.Fatalf("normalized error = %v", got)
			}
		})
	}

	plain := errors.New("plain failure")
	if got := wrapCleanerError(plain); got != plain {
		t.Fatalf("unclassified error changed: %v", got)
	}
	if got := wrapCleanerError(nil); got != nil {
		t.Fatalf("nil error changed: %v", got)
	}
}

func TestCheckpointCleanupFacadeRejectsNonCanonicalRootsBeforeNativeAccess(t *testing.T) {
	unclean := filepath.Join(t.TempDir(), "child") + string(filepath.Separator) + ".."
	for _, root := range []string{"", "relative", unclean} {
		if _, err := CleanLegacyResumeState(
			context.Background(),
			FilesystemResumeRoot{RootPath: root},
		); !errors.Is(err, ErrCheckpointCleanerOwnership) {
			t.Fatalf("root %q error = %v", root, err)
		}
	}
}

func TestCleanLegacyResumeStateExecutesOnValidRoot(t *testing.T) {
	root := t.TempDir()
	report, err := CleanLegacyResumeState(
		context.Background(),
		FilesystemResumeRoot{RootPath: root},
	)
	if err != nil {
		t.Fatalf("clean legacy resume state on empty root: %v", err)
	}
	if !report.Complete || report.Status != CheckpointCleanupStatusComplete || report.NeedsAttention() {
		t.Fatalf("report semantics on clean root = %+v", report)
	}
}

