package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/windshare/windshare/core/osfs"
)

type resumeCleanupRequest struct {
	rootPath string
}

// checkpointCleanupRunner is the only recovery authority exposed to the CLI.
// It returns an ownership-scoped one-shot report and never exposes checkpoint
// record references that a command could reinterpret as numeric delete handles.
type checkpointCleanupRunner interface {
	Cleanup(context.Context, string) (osfs.CheckpointCleanupReport, error)
}

type checkpointCleanupFunc func(
	context.Context,
	osfs.FilesystemResumeRoot,
) (osfs.CheckpointCleanupReport, error)

type filesystemCheckpointCleaner struct {
	cleanup checkpointCleanupFunc
}

func (cleaner filesystemCheckpointCleaner) Cleanup(
	ctx context.Context,
	rootPath string,
) (osfs.CheckpointCleanupReport, error) {
	cleanup := cleaner.cleanup
	if cleanup == nil {
		cleanup = osfs.CleanLegacyResumeState
	}
	return cleanup(ctx, osfs.FilesystemResumeRoot{RootPath: rootPath})
}

func (a *App) runResume(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.logf("resume: exactly one action is required")
		a.resumeUsage()
		return ExitUsage
	}
	switch args[0] {
	case "cleanup":
		return a.runResumeCleanup(ctx, args[1:])
	case "help", "-h", "--help":
		a.resumeUsage()
		return ExitOK
	default:
		a.logf("resume: unknown action %q", args[0])
		a.resumeUsage()
		return ExitUsage
	}
}

func (a *App) runResumeCleanup(ctx context.Context, args []string) int {
	request, code := a.parseResumeCleanupRequest(args)
	if code != ExitOK {
		return code
	}
	report, err := a.checkpointCleanup().Cleanup(ctx, request.rootPath)
	if err != nil {
		a.logf("resume cleanup: clean owned checkpoint namespace: %v", err)
		return ExitFailure
	}
	rendered, err := renderCheckpointCleanupReport(report)
	if err != nil {
		a.logf("resume cleanup: render checkpoint report: %v", err)
		return ExitFailure
	}
	if err := a.writeResumeOutput(rendered); err != nil {
		a.logf("resume cleanup: write checkpoint report: %v", err)
		return ExitFailure
	}
	if report.Status != osfs.CheckpointCleanupStatusComplete || !report.Complete || report.NeedsAttention() {
		a.logf("resume cleanup: checkpoint namespace still needs attention")
		return ExitFailure
	}
	return ExitOK
}

func (a *App) parseResumeCleanupRequest(args []string) (resumeCleanupRequest, int) {
	flags := a.newFlagSet("resume cleanup")
	rootPath := flags.String("o", "", "output directory (required)")
	positional, err := parseInterleaved(flags, args)
	if err != nil {
		return resumeCleanupRequest{}, ExitUsage
	}
	if len(positional) != 0 {
		a.logf("resume cleanup: positional arguments are not accepted")
		return resumeCleanupRequest{}, ExitUsage
	}
	if *rootPath == "" {
		a.logf("resume cleanup: -o is required")
		return resumeCleanupRequest{}, ExitUsage
	}
	absolute, err := absoluteResumeRoot(*rootPath)
	if err != nil {
		a.logf("resume cleanup: output directory %q is invalid: %v", *rootPath, err)
		return resumeCleanupRequest{}, ExitUsage
	}
	return resumeCleanupRequest{rootPath: absolute}, ExitOK
}

func (a *App) checkpointCleanup() checkpointCleanupRunner {
	if a.checkpointCleaner != nil {
		return a.checkpointCleaner
	}
	return filesystemCheckpointCleaner{}
}

func (a *App) resumeUsage() {
	fmt.Fprint(a.stderrWriter(), `Usage:
	  windshare resume cleanup -o <directory>
	      Run idempotent, ownership-scoped checkpoint cleanup and report retained attention.
`)
}

func (a *App) writeResumeOutput(value string) error {
	if a == nil || a.Stdout == nil {
		return errors.New("standard output is unavailable")
	}
	written, err := io.WriteString(a.Stdout, value)
	if err == nil && written != len(value) {
		return io.ErrShortWrite
	}
	return err
}

func absoluteResumeRoot(rootPath string) (string, error) {
	if rootPath == "" {
		return "", errors.New("output directory is empty")
	}
	return filepath.Abs(rootPath)
}

func renderCheckpointCleanupReport(report osfs.CheckpointCleanupReport) (string, error) {
	var output strings.Builder
	fmt.Fprintf(
		&output,
		"cleanup_status=%q complete=%t resumed=%t scanned=%d removed=%d quarantined=%d skipped=%d\n",
		checkpointCleanupStatusName(report.Status), report.Complete, report.Resumed,
		report.Scanned, report.Removed, report.Quarantined, report.Skipped,
	)
	for _, attention := range report.Attention {
		fmt.Fprintf(&output, "  attention=%q\n", attention)
	}
	for _, entry := range report.Entries {
		fmt.Fprintf(
			&output,
			"  checkpoint_entry path=%q disposition=%q detail=%q\n",
			entry.RelativePath, checkpointCleanupDispositionName(entry.Disposition), entry.Detail,
		)
	}
	if report.Status == 0 && !report.Complete && len(report.Attention) == 0 && len(report.Entries) == 0 {
		return "", errors.New("checkpoint cleanup report is empty")
	}
	return output.String(), nil
}

func checkpointCleanupStatusName(status osfs.CheckpointCleanupStatus) string {
	switch status {
	case osfs.CheckpointCleanupStatusComplete:
		return "complete"
	case osfs.CheckpointCleanupStatusNeedsAttention:
		return "needs-attention"
	case osfs.CheckpointCleanupStatusInProgress:
		return "in-progress"
	default:
		return "unknown"
	}
}

func checkpointCleanupDispositionName(disposition osfs.CheckpointCleanupDisposition) string {
	switch disposition {
	case osfs.CheckpointCleanupSkip:
		return "skip"
	case osfs.CheckpointCleanupRemove:
		return "remove"
	case osfs.CheckpointCleanupQuarantine:
		return "quarantine"
	default:
		return "unknown"
	}
}
