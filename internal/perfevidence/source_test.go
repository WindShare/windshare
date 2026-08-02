package perfevidence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureSourceDetectsTrackedAndUntrackedChanges(t *testing.T) {
	root := t.TempDir()
	tracked := filepath.Join(root, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := ProcessRunner{}
	for _, arguments := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "performance@example.invalid"},
		{"config", "user.name", "Performance Evidence"},
		{"add", ".gitignore", "tracked.txt"},
		{"commit", "--quiet", "-m", "fixture"},
	} {
		result, err := runner.Run(context.Background(), Command{Executable: "git", Arguments: arguments, Directory: root})
		if err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, result.Output)
		}
	}
	start, err := CaptureSource(context.Background(), runner, root)
	if err != nil {
		t.Fatal(err)
	}
	if start.WorktreeDirty || start.Commit == "" || len(start.Files) != 2 {
		t.Fatalf("clean source = %+v", start)
	}
	if err := os.MkdirAll(filepath.Join(root, "ignored"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored", "artifact"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	ignored, err := CaptureSource(context.Background(), runner, root)
	if err != nil {
		t.Fatal(err)
	}
	if !SameSource(start, ignored) {
		t.Fatal("ignored artifact changed source identity")
	}
	if err := os.WriteFile(tracked, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := CaptureSource(context.Background(), runner, root)
	if err != nil {
		t.Fatal(err)
	}
	if !changed.WorktreeDirty || SameSource(start, changed) || len(changed.Files) != 3 {
		t.Fatalf("changed source = %+v", changed)
	}
}

func TestSourceHelpersRejectUnsafeOrMalformedInputs(t *testing.T) {
	if _, err := nulPaths([]byte("not-terminated")); err == nil {
		t.Fatal("non-NUL path list was accepted")
	}
	if _, err := nulPaths([]byte("same\x00same\x00")); err == nil {
		t.Fatal("duplicate path was accepted")
	}
	if _, err := snapshotSourceFile(t.TempDir(), "../escape"); err == nil {
		t.Fatal("escaping source path was accepted")
	}
	root := t.TempDir()
	record, err := snapshotSourceFile(root, "deleted.txt")
	if err != nil || !record.Missing || record.Kind != "missing" {
		t.Fatalf("missing record = %+v, err = %v", record, err)
	}
	failing := commandFunc(func(context.Context, Command) (CommandResult, error) {
		return CommandResult{ExitCode: 2, Output: []byte("bad repository")}, os.ErrNotExist
	})
	if _, err := CaptureSource(context.Background(), failing, root); err == nil || !strings.Contains(err.Error(), "bad repository") {
		t.Fatalf("git failure = %v", err)
	}
}

func TestStableSourceObservationRejectsCommitTransition(t *testing.T) {
	commits := []string{strings.Repeat("a", 40), strings.Repeat("b", 40)}
	revision := 0
	runner := commandFunc(func(_ context.Context, command Command) (CommandResult, error) {
		switch command.Arguments[2] {
		case "rev-parse":
			commit := commits[revision]
			revision++
			return CommandResult{ExitCode: 0, Output: []byte(commit + "\n")}, nil
		case "status", "ls-files":
			return CommandResult{ExitCode: 0}, nil
		default:
			return CommandResult{}, errors.New("unexpected Git command")
		}
	})
	initial, err := CaptureSource(context.Background(), runner, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = requireStableSourceObservation(
		context.Background(), runner, t.TempDir(), initial, "adversarial ref transition",
	)
	if err == nil || !strings.Contains(err.Error(), commits[1]) {
		t.Fatalf("A-to-B ref transition remained baseline-eligible: %v", err)
	}
}

type commandFunc func(context.Context, Command) (CommandResult, error)

func (function commandFunc) Run(ctx context.Context, command Command) (CommandResult, error) {
	return function(ctx, command)
}
