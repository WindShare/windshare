// These tests drive a deterministic whole-repo SSA static-analysis gate with
// no concurrency of its own: race instrumentation adds ~4x runtime and zero
// signal, so race builds skip them. The non-race coverage gates remain the
// authoritative execution.
//go:build !race

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompilerDerivedExecutionPlans(t *testing.T) {
	t.Parallel()
	root := writeFixtureModule(t, map[string]string{
		"owner/owner_test.go": `package owner
import (
    "testing"
    "github.com/windshare/windshare/internal/testnetwork"
)
func invoke(callback func()) { callback() }
func pureHelper() { invoke(func() {}) }
func networkHelper() { invoke(testnetwork.AssertOSNetwork) }
func BenchmarkPure(b *testing.B) { pureHelper() }
func BenchmarkNetwork(b *testing.B) { networkHelper() }
func TestPure(*testing.T) {}
func TestSubtests(t *testing.T) {
    t.Run("pure", func(*testing.T) {})
    t.Run("network", func(*testing.T) { networkHelper() })
}
`,
	})
	result, err := analyzeFixtureRoot(root)
	if err != nil {
		t.Fatalf("analyze fixture: %v", err)
	}
	entries := make(map[string]semanticEntry)
	for _, entry := range result.catalog.entries {
		entries[entry.Kind+"/"+entry.Name] = entry
	}
	for _, name := range []string{"benchmark/BenchmarkPure", "test/TestPure"} {
		entry, ok := entries[name]
		if !ok || entry.RequiresNetwork {
			t.Errorf("entry %s = %+v, want compiler-resolved non-network entry", name, entry)
		}
	}
	for _, name := range []string{"benchmark/BenchmarkNetwork", "test/TestSubtests"} {
		entry, ok := entries[name]
		if !ok || !entry.RequiresNetwork {
			t.Errorf("entry %s = %+v, want compiler-resolved network entry", name, entry)
		}
	}

	program := currentTestProgram(t)
	source := executionSourceIdentity{
		IdentityKind:  "workspace-manifest",
		Commit:        "0123456789abcdef0123456789abcdef01234567",
		WorktreeClean: false,
		SourceDigest:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	request := executionPlanRequest{
		SchemaVersion: executionPlanSchemaVersion,
		RunID:         "semantic-plan-regression",
		Source:        source,
		Operations: []executionOperationRequest{
			{
				RequestID:        "pure-benchmark",
				PackagePath:      "owner",
				Executable:       program,
				WorkingDirectory: filepath.Join(root, "owner"),
				Arguments:        []string{"-test.run=^$", "-test.bench=^BenchmarkPure$"},
			},
			{
				RequestID:        "network-benchmark",
				PackagePath:      "owner",
				Executable:       program,
				WorkingDirectory: filepath.Join(root, "owner"),
				Arguments:        []string{"-test.run=^$", "-test.bench=^BenchmarkNetwork$"},
			},
			{
				RequestID:        "mixed-benchmark",
				PackagePath:      "owner",
				Executable:       program,
				WorkingDirectory: filepath.Join(root, "owner"),
				Arguments:        []string{"-test.run=^$", "-test.bench=^Benchmark(Pure|Network)$"},
			},
			{
				RequestID:        "network-subtest",
				PackagePath:      "owner",
				Executable:       program,
				WorkingDirectory: filepath.Join(root, "owner"),
				Arguments:        []string{"-test.run=^TestSubtests$/pure$"},
			},
			{
				RequestID:        "path-alternation",
				PackagePath:      "owner",
				Executable:       program,
				WorkingDirectory: filepath.Join(root, "owner"),
				Arguments:        []string{"-test.run=^TestPure$/never$|^TestSubtests$"},
			},
			{
				RequestID:        "top-level-skip",
				PackagePath:      "owner",
				Executable:       program,
				WorkingDirectory: filepath.Join(root, "owner"),
				Arguments: []string{
					"-test.run=^Test(Pure|Subtests)$",
					"-test.skip=^TestSubtests$",
				},
			},
		},
	}
	document, err := buildExecutionPlanDocument(
		root,
		map[string]bool{"owner": true},
		nil,
		result.catalog,
		request,
	)
	if err != nil {
		t.Fatalf("build execution plans: %v", err)
	}
	plans := make(map[string]testExecutionPlan)
	for _, plan := range document.Plans {
		plans[plan.RequestID] = plan
		if got := executionPlanSHA256(plan); got != plan.PlanSHA256 {
			t.Errorf("plan %s digest = %s, want %s", plan.RequestID, plan.PlanSHA256, got)
		}
	}
	assertPlanClass(t, plans["pure-benchmark"], networkAccessNone, "non-network")
	assertPlanClass(t, plans["network-benchmark"], networkAccessParentPipe, "network")
	assertPlanClass(t, plans["mixed-benchmark"], networkAccessParentPipe, "mixed-network")
	assertPlanClass(t, plans["network-subtest"], networkAccessParentPipe, "network")
	assertPlanClass(t, plans["path-alternation"], networkAccessParentPipe, "mixed-network")
	assertPlanClass(t, plans["top-level-skip"], networkAccessNone, "non-network")
}

func TestExecutionPlanSelectionRejectsAmbiguity(t *testing.T) {
	cases := []struct {
		name      string
		arguments []string
	}{
		{name: "duplicate test regex", arguments: []string{"-test.run=TestA", "-test.run=TestB"}},
		{name: "invalid regex", arguments: []string{"-test.run=["}},
		{name: "listing", arguments: []string{"-test.list=Test"}},
		{name: "fuzzing", arguments: []string{"-test.fuzz=Fuzz"}},
	}
	entries := []semanticEntry{{Kind: "test", Name: "TestA"}}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := selectSemanticEntries(entries, test.arguments); err == nil {
				t.Fatalf("selectSemanticEntries(%q) succeeded, want fail-closed error", test.arguments)
			}
		})
	}
}

func TestExecutionPlanRejectsRetiredExecutableTombstone(t *testing.T) {
	t.Parallel()
	root := writeFixtureModule(t, map[string]string{
		"owner/owner_test.go": `package owner
import "testing"
func TestPure(*testing.T) {}
`,
	})
	program := currentTestProgram(t)
	raw, err := os.ReadFile(program.Path)
	if err != nil {
		t.Fatal(err)
	}
	program.Path = filepath.Join(t.TempDir(), retiredConnectivityProgram)
	if err := os.WriteFile(program.Path, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	request := executionPlanRequest{
		SchemaVersion: executionPlanSchemaVersion,
		RunID:         "retired-program-regression",
		Source: executionSourceIdentity{
			IdentityKind: "workspace-manifest",
			Commit:       "0123456789abcdef0123456789abcdef01234567",
			SourceDigest: strings.Repeat("a", 64),
		},
		Operations: []executionOperationRequest{{
			RequestID:        "retired-program",
			PackagePath:      "owner",
			Executable:       program,
			WorkingDirectory: filepath.Join(root, "owner"),
			Arguments:        []string{"-test.run=^TestPure$"},
		}},
	}
	_, err = buildExecutionPlanDocument(
		root,
		map[string]bool{"owner": true},
		map[string]bool{retiredConnectivityProgram: true},
		semanticCatalog{entries: []semanticEntry{{
			PackagePath: "owner",
			Kind:        "test",
			Name:        "TestPure",
		}}},
		request,
	)
	if err == nil || !strings.Contains(err.Error(), "retired executable tombstone") {
		t.Fatalf("buildExecutionPlanDocument error = %v, want retired tombstone rejection", err)
	}
}

func TestCompilerDerivedLifecycleGate(t *testing.T) {
	t.Parallel()
	root := writeFixtureModule(t, map[string]string{
		"owner/owner_test.go": `package owner
import (
    "testing"
    "github.com/windshare/windshare/internal/testnetwork"
)
func TestPure(*testing.T) {}
func TestMain(*testing.M) { testnetwork.AssertOSNetwork() }
`,
	})
	result, err := analyzeFixtureRoot(root)
	if err != nil {
		t.Fatalf("analyze fixture: %v", err)
	}
	if !result.catalog.lifecycle["owner"] {
		t.Fatal("compiler catalog omitted TestMain network lifecycle")
	}
	entries, err := selectSemanticEntries(
		result.catalog.entries,
		[]string{"-test.run=^TestPure$"},
	)
	if err != nil {
		t.Fatal(err)
	}
	access, class := classifySelection(entries, result.catalog.lifecycle["owner"])
	if access != networkAccessParentPipe || class != "network-lifecycle" {
		t.Fatalf("lifecycle selection = %q, %q", access, class)
	}
}

func currentTestProgram(t *testing.T) executionProgram {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return executionProgram{
		Path:   path,
		Bytes:  int64(len(raw)),
		SHA256: hex.EncodeToString(digest[:]),
	}
}

func assertPlanClass(t *testing.T, plan testExecutionPlan, access, class string) {
	t.Helper()
	if plan.NetworkAccess != access || plan.SelectionClass != class {
		t.Errorf(
			"plan %s class = %q, %q; want %q, %q",
			plan.RequestID,
			plan.NetworkAccess,
			plan.SelectionClass,
			access,
			class,
		)
	}
}
