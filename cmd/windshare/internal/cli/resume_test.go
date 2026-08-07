package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs"
)

func TestResumeCleanupRequestUsesAnExplicitAbsoluteRoot(t *testing.T) {
	root := t.TempDir()
	app, _, _ := newResumeTestApp(nil)
	request, code := app.parseResumeCleanupRequest([]string{"-o", root})
	if code != ExitOK || request.rootPath != root {
		t.Fatalf("request=%+v code=%d", request, code)
	}
	for name, args := range map[string][]string{
		"missing root": nil,
		"positional":   {"-o", root, "extra"},
		"numeric item": {"-o", root, "--item", "1"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, code := app.parseResumeCleanupRequest(args); code != ExitUsage {
				t.Fatalf("code=%d", code)
			}
		})
	}
}

func TestResumeRenderingEscapesCleanupAttentionAndEntries(t *testing.T) {
	report := osfs.CheckpointCleanupReport{
		Status:    osfs.CheckpointCleanupStatusNeedsAttention,
		Attention: []string{"ownership\nmarker\""},
		Entries: []osfs.CheckpointCleanupEntry{{
			RelativePath: "legacy\tentry",
			Disposition:  osfs.CheckpointCleanupQuarantine,
			Detail:       "quarantine\"path",
		}},
	}
	rendered, err := renderCheckpointCleanupReport(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`cleanup_status="needs-attention"`,
		`attention="ownership\nmarker\""`,
		`path="legacy\tentry"`,
		`disposition="quarantine"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered=%q missing=%q", rendered, want)
		}
	}
	if strings.ContainsRune(rendered, '\x1b') || strings.Contains(rendered, "ownership\nmarker") {
		t.Fatalf("unescaped control data in report=%q", rendered)
	}
}

func TestFilesystemCheckpointCleanerForwardsTheOwnedRoot(t *testing.T) {
	root := t.TempDir()
	var observed osfs.FilesystemResumeRoot
	cleaner := filesystemCheckpointCleaner{cleanup: func(
		_ context.Context,
		requested osfs.FilesystemResumeRoot,
	) (osfs.CheckpointCleanupReport, error) {
		observed = requested
		return osfs.CheckpointCleanupReport{
			Status: osfs.CheckpointCleanupStatusComplete, Complete: true,
		}, nil
	}}
	report, err := cleaner.Cleanup(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if observed.RootPath != root || !report.Complete {
		t.Fatalf("observed=%+v report=%+v", observed, report)
	}
}

func TestResumeCleanupPublishesACompleteOneShotReport(t *testing.T) {
	root := t.TempDir()
	cleaner := &fakeCheckpointCleaner{report: osfs.CheckpointCleanupReport{
		Status: osfs.CheckpointCleanupStatusComplete, Complete: true,
		Scanned: 2, Removed: 1, Quarantined: 1,
		Entries: []osfs.CheckpointCleanupEntry{{
			RelativePath: "legacy.state", Disposition: osfs.CheckpointCleanupRemove,
		}},
	}}
	app, stdout, stderr := newResumeTestApp(cleaner)
	if code := app.Run(context.Background(), []string{"resume", "cleanup", "-o", root}); code != ExitOK {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if cleaner.calls != 1 || cleaner.rootPath != root {
		t.Fatalf("calls=%d root=%q", cleaner.calls, cleaner.rootPath)
	}
	for _, want := range []string{`cleanup_status="complete"`, `removed=1`, `quarantined=1`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout=%q missing=%q", stdout.String(), want)
		}
	}
}

func TestResumeCleanupReturnsFailureWhenOwnershipNeedsAttention(t *testing.T) {
	root := t.TempDir()
	cleaner := &fakeCheckpointCleaner{report: osfs.CheckpointCleanupReport{
		Status:    osfs.CheckpointCleanupStatusNeedsAttention,
		Attention: []string{"ownership marker is absent"},
	}}
	app, stdout, stderr := newResumeTestApp(cleaner)
	if code := app.Run(context.Background(), []string{"resume", "cleanup", "-o", root}); code != ExitFailure {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(stdout.String(), `attention="ownership marker is absent"`) ||
		!strings.Contains(stderr.String(), "still needs attention") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestResumeCleanupReportsBackendAndOutputFailures(t *testing.T) {
	root := t.TempDir()
	t.Run("backend", func(t *testing.T) {
		app, stdout, stderr := newResumeTestApp(&fakeCheckpointCleaner{err: errors.New("cleanup lock busy")})
		if code := app.Run(context.Background(), []string{"resume", "cleanup", "-o", root}); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "cleanup lock busy") {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})
	t.Run("empty report", func(t *testing.T) {
		app, _, stderr := newResumeTestApp(&fakeCheckpointCleaner{})
		if code := app.Run(context.Background(), []string{"resume", "cleanup", "-o", root}); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
		if !strings.Contains(stderr.String(), "report is empty") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})
	if err := (&App{Stdout: shortResumeWriter{}}).writeResumeOutput("checkpoint"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error=%v", err)
	}
}

func TestResumeSurfaceRetiresLegacyInventoryCommands(t *testing.T) {
	for _, action := range []string{"list", "discard", "status"} {
		app, _, stderr := newResumeTestApp(nil)
		if code := app.Run(context.Background(), []string{"resume", action}); code != ExitUsage {
			t.Fatalf("action=%q code=%d", action, code)
		}
		if !strings.Contains(stderr.String(), "unknown action") {
			t.Fatalf("action=%q stderr=%q", action, stderr.String())
		}
	}
	app, _, stderr := newResumeTestApp(nil)
	if code := app.Run(context.Background(), []string{"resume", "help"}); code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	help := stderr.String()
	if !strings.Contains(help, "resume cleanup") || strings.Contains(help, "--item") ||
		strings.Contains(help, "resume list") || strings.Contains(help, "resume discard") {
		t.Fatalf("help=%q", help)
	}
}

func newResumeTestApp(cleaner checkpointCleanupRunner) (*App, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	return &App{
		Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader(""), checkpointCleaner: cleaner,
	}, stdout, stderr
}

type fakeCheckpointCleaner struct {
	report   osfs.CheckpointCleanupReport
	err      error
	calls    int
	rootPath string
}

func (cleaner *fakeCheckpointCleaner) Cleanup(
	_ context.Context,
	rootPath string,
) (osfs.CheckpointCleanupReport, error) {
	cleaner.calls++
	cleaner.rootPath = rootPath
	return cleaner.report, cleaner.err
}

type shortResumeWriter struct{}

func (shortResumeWriter) Write(value []byte) (int, error) {
	if len(value) == 0 {
		return 0, nil
	}
	return len(value) - 1, nil
}
