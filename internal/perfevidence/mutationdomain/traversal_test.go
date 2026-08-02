package mutationdomain

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMutationTraversalBudgetRejectsAggregateNPlusOneAcrossCandidates(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "first"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "second"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	budget := newMutationTraversalBudget(mutationTraversalLimits{
		objects: 3, contentBytes: 2, metadataBytes: 1 << 20, depth: 8,
	})
	if err := budget.admitCandidate("first"); err != nil {
		t.Fatal(err)
	}
	if _, err := treeSHA256WithBudget(first, budget); err != nil {
		t.Fatal(err)
	}
	if err := budget.admitCandidate("second"); err != nil {
		t.Fatal(err)
	}
	if _, err := treeSHA256WithBudget(second, budget); err == nil || !strings.Contains(err.Error(), "object count") {
		t.Fatalf("aggregate N+1 traversal error = %v, want object count bound", err)
	}
}

func TestMutationTraversalBudgetRejectsDeepEmptyTree(t *testing.T) {
	root := t.TempDir()
	path := root
	for _, leaf := range []string{"one", "two", "three"} {
		path = filepath.Join(path, leaf)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	budget := newMutationTraversalBudget(mutationTraversalLimits{
		objects: 16, contentBytes: 0, metadataBytes: 1 << 20, depth: 2,
	})
	if err := budget.admitCandidate("deep"); err != nil {
		t.Fatal(err)
	}
	if _, err := treeSHA256WithBudget(root, budget); err == nil || !strings.Contains(err.Error(), "directory depth") {
		t.Fatalf("deep empty traversal error = %v, want depth bound", err)
	}
}

func TestMutationTraversalBudgetSeparatelyBoundsContentAndMetadata(t *testing.T) {
	content := newMutationTraversalBudget(mutationTraversalLimits{
		objects: 2, contentBytes: 1, metadataBytes: 1 << 20, depth: 1,
	})
	if err := content.admitCandidate("content"); err != nil {
		t.Fatal(err)
	}
	if err := content.admitObject("file", 1, 2); err == nil || !strings.Contains(err.Error(), "byte bound") {
		t.Fatalf("content budget error = %v, want byte bound", err)
	}

	metadata := newMutationTraversalBudget(mutationTraversalLimits{
		objects: 2, contentBytes: 0,
		metadataBytes: mutationObjectMetadataOverhead + int64(len("metadata")), depth: 1,
	})
	if err := metadata.admitCandidate("metadata"); err != nil {
		t.Fatal(err)
	}
	if err := metadata.admitObject("x", 1, 0); err == nil || !strings.Contains(err.Error(), "metadata byte") {
		t.Fatalf("metadata budget error = %v, want metadata byte bound", err)
	}
}

func TestPathTraversalRejectsSymlinkWhenSupported(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skip("symlink creation is unavailable")
		}
		t.Fatal(err)
	}
	if _, err := treeSHA256(root); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink traversal error = %v, want explicit rejection", err)
	}
}
