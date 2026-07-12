package main

import (
	"flag"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"sync"
	"testing"
)

type testingSelectionScenario struct {
	name           string
	run            string
	benchmark      string
	skip           string
	networkAccess  string
	selectionClass string
}

type testingSelectionOracle struct {
	mu       sync.Mutex
	selected map[string]bool
}

func TestTestingSelectionMatchesGo126(t *testing.T) {
	entries := testingSelectionCatalog()
	scenarios := []testingSelectionScenario{
		{
			name:           "pure test",
			run:            `^TestPure$`,
			networkAccess:  networkAccessNone,
			selectionClass: "non-network",
		},
		{
			name:           "network test",
			run:            `^TestNetwork$`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "network",
		},
		{
			name:           "mixed tests",
			run:            `^Test(Pure|Network)$`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "mixed-network",
		},
		{
			name:           "pure subtest keeps conservative network owner",
			run:            `^TestSubtests$/pure$`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "network",
		},
		{
			name:           "complete path alternation regression",
			run:            `^TestPure$/never$|^TestNetwork$`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "mixed-network",
		},
		{
			name:           "review rejection exact regexp",
			run:            `^TestSharedForwardQueueChunkPolicy$/never$|^TestRegisterJoinManifestRoundtrip$`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "mixed-network",
		},
		{
			name:           "subtest path alternates with top level",
			run:            `^TestSubtests$/pure$|^TestNetwork$`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "network",
		},
		{
			name:           "top level skip",
			run:            `^Test(Pure|Network)$`,
			skip:           `^TestNetwork$`,
			networkAccess:  networkAccessNone,
			selectionClass: "non-network",
		},
		{
			name:           "partial subtest skip retains owner",
			run:            `^TestSubtests$`,
			skip:           `^TestSubtests$/network$`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "network",
		},
		{
			name:           "ordered partial skip wins like testing",
			run:            `^Test(Pure|Subtests)$`,
			skip:           `^TestSubtests$/network$|^TestSubtests$`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "mixed-network",
		},
		{
			name:           "ordered complete skip wins like testing",
			run:            `^Test(Pure|Subtests)$`,
			skip:           `^TestSubtests$|^TestSubtests$/network$`,
			networkAccess:  networkAccessNone,
			selectionClass: "non-network",
		},
		{
			name:           "empty alternative matches every test",
			run:            `^TestPure$|`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "mixed-network",
		},
		{
			name:           "space rewriting reaches named subtest",
			run:            `^TestSubtests$/space name$`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "network",
		},
		{
			name:           "escaped pipe stays inside component",
			run:            `^TestSubtests$/literal\|pipe$`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "network",
		},
		{
			name:           "slash in character class stays inside component",
			run:            `^TestSubtests$/pure$|^No[/]Match$`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "network",
		},
		{
			name:           "pure benchmark",
			run:            `^$`,
			benchmark:      `^BenchmarkPure$`,
			networkAccess:  networkAccessNone,
			selectionClass: "non-network",
		},
		{
			name:           "mixed benchmark path alternatives",
			run:            `^$`,
			benchmark:      `^BenchmarkPure$/never$|^BenchmarkNetwork$`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "mixed-network",
		},
		{
			name:           "benchmark top level skip",
			run:            `^$`,
			benchmark:      `^Benchmark(Pure|Network)$`,
			skip:           `^BenchmarkNetwork$`,
			networkAccess:  networkAccessNone,
			selectionClass: "non-network",
		},
		{
			name:           "benchmark subname",
			run:            `^$`,
			benchmark:      `^BenchmarkSubtests$/pure$`,
			networkAccess:  networkAccessParentPipe,
			selectionClass: "network",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			assertTestingSelectionMatchesOracle(t, entries, scenario)
		})
	}
}

func TestTestingSelectionGeneratedPatternsMatchGo126(t *testing.T) {
	entries := testingSelectionCatalog()
	topLevelPatterns := []string{
		`^TestPure$`,
		`^TestNetwork$`,
		`^TestSubtests$`,
		`^Test(Pure|Network)$`,
		`^Test[PNS][[:alpha:]]+$`,
	}
	tails := []string{"", `/pure$`, `/never$`}
	alternatives := []string{"", `|^TestNetwork$`, `|^TestPure$/never$`}
	for patternIndex, topLevel := range topLevelPatterns {
		for tailIndex, tail := range tails {
			alternative := alternatives[(patternIndex+tailIndex)%len(alternatives)]
			pattern := topLevel + tail + alternative
			t.Run(fmt.Sprintf("pattern_%d_%d", patternIndex, tailIndex), func(t *testing.T) {
				assertTestingSelectionMatchesOracle(t, entries, testingSelectionScenario{run: pattern})
			})
		}
	}
}

func TestMalformedTestingSelectionRejectsEveryGo126Path(t *testing.T) {
	cases := []testingSelectionScenario{
		{name: "run component", run: `^TestPure$/[`},
		{name: "later run alternative", run: `^TestPure$|[`},
		{name: "benchmark component", run: `^$`, benchmark: `^BenchmarkPure$/(`},
		{name: "skip alternative", run: `^TestPure$`, skip: `^TestNetwork$|[`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			arguments := testingSelectionArguments(test)
			if _, err := selectSemanticEntries(testingSelectionCatalog(), arguments); err == nil {
				t.Fatalf("selectSemanticEntries(%q) succeeded, want malformed-regexp error", arguments)
			}
		})
	}
}

func TestSplitTestingRegexpGo126Fixtures(t *testing.T) {
	cases := []struct {
		pattern string
		want    [][]string
	}{
		{pattern: "", want: [][]string{{""}}},
		{pattern: "/", want: [][]string{{"", ""}}},
		{pattern: "//", want: [][]string{{"", "", ""}}},
		{pattern: "A/B", want: [][]string{{"A", "B"}}},
		{pattern: "[/]/[:/]", want: [][]string{{"[/]", "[:/]"}}},
		{pattern: `([)/][(])`, want: [][]string{{`([)/][(])`}}},
		{pattern: "A/B|C/D", want: [][]string{{"A", "B"}, {"C", "D"}}},
		{pattern: "A/|/B", want: [][]string{{"A", ""}, {"", "B"}}},
		{pattern: ")/", want: [][]string{{")/"}}},
		{pattern: ")/(/)", want: [][]string{{")/(", ")"}}},
		{pattern: `\p{/}`, want: [][]string{{`\p{`, "}"}}},
		{pattern: `\p/`, want: [][]string{{`\p`, ""}}},
		{pattern: `[[:/:]]`, want: [][]string{{`[[:/:]]`}}},
	}
	for _, test := range cases {
		got := splitTestingRegexp(test.pattern)
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("splitTestingRegexp(%q) = %#v, want %#v", test.pattern, got, test.want)
		}
		if _, originalError := regexp.Compile(test.pattern); originalError != nil &&
			allTestingPathComponentsCompile(got) {
			t.Errorf("malformed regexp %q became entirely valid after Go testing path splitting", test.pattern)
		}
	}
}

func allTestingPathComponentsCompile(paths [][]string) bool {
	for _, path := range paths {
		for _, component := range path {
			if _, err := regexp.Compile(rewriteTestingPattern(component)); err != nil {
				return false
			}
		}
	}
	return true
}

func TestExecutionSelectionArgumentBoundary(t *testing.T) {
	entries := testingSelectionCatalog()
	cases := []struct {
		name      string
		arguments []string
		want      []string
		wantError bool
	}{
		{
			name:      "double dash aliases",
			arguments: []string{"--test.run", `^Test(Pure|Network)$`, "--test.skip=^TestNetwork$"},
			want:      []string{"test/TestPure"},
		},
		{
			name:      "standard value consumes flag-shaped token",
			arguments: []string{"-test.outputdir", "-test.run=^TestNetwork$"},
			want: []string{
				"test/TestNetwork",
				"test/TestPure",
				"test/TestRegisterJoinManifestRoundtrip",
				"test/TestSharedForwardQueueChunkPolicy",
				"test/TestSubtests",
			},
		},
		{
			name:      "boolean does not consume next selection",
			arguments: []string{"-test.v", "-test.run=^TestNetwork$"},
			want:      []string{"test/TestNetwork"},
		},
		{
			name:      "positional boundary",
			arguments: []string{"positional", "-test.run=^TestNetwork$"},
			want: []string{
				"test/TestNetwork",
				"test/TestPure",
				"test/TestRegisterJoinManifestRoundtrip",
				"test/TestSharedForwardQueueChunkPolicy",
				"test/TestSubtests",
			},
		},
		{
			name:      "double dash boundary",
			arguments: []string{"--", "-test.run=^TestNetwork$"},
			want: []string{
				"test/TestNetwork",
				"test/TestPure",
				"test/TestRegisterJoinManifestRoundtrip",
				"test/TestSharedForwardQueueChunkPolicy",
				"test/TestSubtests",
			},
		},
		{
			name:      "unknown package flag",
			arguments: []string{"-custom", "-test.run=^TestPure$"},
			wantError: true,
		},
		{
			name:      "duplicate alias",
			arguments: []string{"-test.run=^TestPure$", "--test.run=^TestNetwork$"},
			wantError: true,
		},
		{
			name:      "duplicate skip",
			arguments: []string{"-test.skip=^TestPure$", "-test.skip=^TestNetwork$"},
			wantError: true,
		},
		{
			name:      "missing value",
			arguments: []string{"-test.run"},
			wantError: true,
		},
		{
			name:      "fuzz worker schema exclusion",
			arguments: []string{"-test.fuzzworker"},
			wantError: true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			selected, err := selectSemanticEntries(entries, test.arguments)
			if test.wantError {
				if err == nil {
					t.Fatalf("selectSemanticEntries(%q) succeeded, want error", test.arguments)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := plannedEntryKeys(selected); !reflect.DeepEqual(got, test.want) {
				t.Errorf("selected entries = %q, want %q", got, test.want)
			}
		})
	}
}

func (oracle *testingSelectionOracle) markTest(t *testing.T) {
	oracle.record("test", t.Name())
}

func (oracle *testingSelectionOracle) markSubtests(t *testing.T) {
	oracle.markTest(t)
	for _, name := range []string{"pure", "network", "space name", "literal|pipe", "group/leaf"} {
		t.Run(name, oracle.markTest)
	}
}

func (oracle *testingSelectionOracle) markBenchmark(b *testing.B) {
	oracle.record("benchmark", b.Name())
	b.SkipNow()
}

func (oracle *testingSelectionOracle) markSubbenchmarks(b *testing.B) {
	oracle.record("benchmark", b.Name())
	for _, name := range []string{"pure", "network", "space name", "literal|pipe", "group/leaf"} {
		b.Run(name, oracle.markBenchmark)
	}
}

func (oracle *testingSelectionOracle) record(kind, name string) {
	oracle.mu.Lock()
	defer oracle.mu.Unlock()
	oracle.selected[kind+"/"+name] = true
}

func assertTestingSelectionMatchesOracle(
	t *testing.T,
	entries []semanticEntry,
	scenario testingSelectionScenario,
) {
	t.Helper()
	arguments := testingSelectionArguments(scenario)
	selected, err := selectSemanticEntries(entries, arguments)
	if err != nil {
		t.Fatal(err)
	}
	want := runTestingSelectionOracle(t, scenario, entries)
	got := plannedEntryKeys(selected)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("planner selected %q, Go 1.26 testing selected %q", got, want)
	}
	if scenario.networkAccess == "" {
		return
	}
	access, class := classifySelection(selected, false)
	if access != scenario.networkAccess || class != scenario.selectionClass {
		t.Errorf(
			"selection class = %q, %q; want %q, %q",
			access,
			class,
			scenario.networkAccess,
			scenario.selectionClass,
		)
	}
}

func testingSelectionArguments(scenario testingSelectionScenario) []string {
	arguments := []string{"-test.run=" + scenario.run}
	if scenario.benchmark != "" {
		arguments = append(arguments, "-test.bench="+scenario.benchmark)
	}
	if scenario.skip != "" {
		arguments = append(arguments, "-test.skip="+scenario.skip)
	}
	return arguments
}

func runTestingSelectionOracle(
	t *testing.T,
	scenario testingSelectionScenario,
	entries []semanticEntry,
) []string {
	t.Helper()
	original := make(map[string]string)
	for name, value := range map[string]string{
		"test.run":   scenario.run,
		"test.bench": scenario.benchmark,
		"test.skip":  scenario.skip,
	} {
		registered := flag.Lookup(name)
		if registered == nil {
			t.Fatalf("Go testing oracle flag %s is not registered", name)
		}
		original[name] = registered.Value.String()
		if err := flag.Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		for name, value := range original {
			if err := flag.Set(name, value); err != nil {
				t.Errorf("restore %s: %v", name, err)
			}
		}
	})

	oracle := &testingSelectionOracle{selected: make(map[string]bool)}
	tests := []testing.InternalTest{
		// Benchmark-only plans use -test.run=^$. The empty sentinel prevents
		// testing.RunTests from emitting a package-level "no tests" warning while
		// remaining outside the compiler catalog compared by this oracle.
		{Name: "", F: func(*testing.T) {}},
		{Name: "TestPure", F: oracle.markTest},
		{Name: "TestNetwork", F: oracle.markTest},
		{Name: "TestSharedForwardQueueChunkPolicy", F: oracle.markTest},
		{Name: "TestRegisterJoinManifestRoundtrip", F: oracle.markTest},
		{Name: "TestSubtests", F: oracle.markSubtests},
	}
	benchmarks := []testing.InternalBenchmark{
		{Name: "BenchmarkPure", F: oracle.markBenchmark},
		{Name: "BenchmarkNetwork", F: oracle.markBenchmark},
		{Name: "BenchmarkSubtests", F: oracle.markSubbenchmarks},
	}
	if !testing.RunTests(regexp.MatchString, tests) {
		t.Fatal("testing.RunTests oracle failed")
	}
	testing.RunBenchmarks(regexp.MatchString, benchmarks)
	return oracle.topLevelEntryKeys(entries)
}

func testingSelectionCatalog() []semanticEntry {
	return []semanticEntry{
		{Kind: "test", Name: "TestPure"},
		{Kind: "test", Name: "TestNetwork", RequiresNetwork: true},
		{Kind: "test", Name: "TestSharedForwardQueueChunkPolicy"},
		{Kind: "test", Name: "TestRegisterJoinManifestRoundtrip", RequiresNetwork: true},
		{Kind: "test", Name: "TestSubtests", RequiresNetwork: true},
		{Kind: "benchmark", Name: "BenchmarkPure"},
		{Kind: "benchmark", Name: "BenchmarkNetwork", RequiresNetwork: true},
		{Kind: "benchmark", Name: "BenchmarkSubtests", RequiresNetwork: true},
	}
}

func (oracle *testingSelectionOracle) topLevelEntryKeys(entries []semanticEntry) []string {
	known := make(map[string]bool, len(entries))
	for _, entry := range entries {
		known[entry.Kind+"/"+entry.Name] = true
	}
	oracle.mu.Lock()
	defer oracle.mu.Unlock()
	keys := make([]string, 0, len(oracle.selected))
	for key := range oracle.selected {
		if known[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func plannedEntryKeys(entries []plannedSemanticEntry) []string {
	keys := make([]string, len(entries))
	for index, entry := range entries {
		keys[index] = entry.Kind + "/" + entry.Name
	}
	sort.Strings(keys)
	return keys
}
