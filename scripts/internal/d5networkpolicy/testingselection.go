package main

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type executionTestFlagKind uint8

const (
	executionValueFlag executionTestFlagKind = iota
	executionBooleanFlag
	executionRunFlag
	executionBenchmarkFlag
	executionSkipFlag
	executionUnsupportedSelectionFlag
)

// The plan compiler accepts only flags owned by Go 1.26's testing package.
// Otherwise a package-defined value flag could consume a following -test.run or
// -test.bench token that the planner mistook for a real framework selection.
var executionTestFlags = map[string]executionTestFlagKind{
	"test.artifacts":            executionBooleanFlag,
	"test.bench":                executionBenchmarkFlag,
	"test.benchmem":             executionBooleanFlag,
	"test.benchtime":            executionValueFlag,
	"test.blockprofile":         executionValueFlag,
	"test.blockprofilerate":     executionValueFlag,
	"test.count":                executionValueFlag,
	"test.coverprofile":         executionValueFlag,
	"test.cpu":                  executionValueFlag,
	"test.cpuprofile":           executionValueFlag,
	"test.failfast":             executionBooleanFlag,
	"test.fullpath":             executionBooleanFlag,
	"test.fuzz":                 executionUnsupportedSelectionFlag,
	"test.fuzzcachedir":         executionUnsupportedSelectionFlag,
	"test.fuzzminimizetime":     executionUnsupportedSelectionFlag,
	"test.fuzztime":             executionUnsupportedSelectionFlag,
	"test.fuzzworker":           executionUnsupportedSelectionFlag,
	"test.gocoverdir":           executionValueFlag,
	"test.list":                 executionUnsupportedSelectionFlag,
	"test.memprofile":           executionValueFlag,
	"test.memprofilerate":       executionValueFlag,
	"test.mutexprofile":         executionValueFlag,
	"test.mutexprofilefraction": executionValueFlag,
	"test.outputdir":            executionValueFlag,
	"test.paniconexit0":         executionBooleanFlag,
	"test.parallel":             executionValueFlag,
	"test.run":                  executionRunFlag,
	"test.short":                executionBooleanFlag,
	"test.shuffle":              executionValueFlag,
	"test.skip":                 executionSkipFlag,
	"test.testlogfile":          executionValueFlag,
	"test.timeout":              executionValueFlag,
	"test.trace":                executionValueFlag,
	"test.v":                    executionBooleanFlag,
}

type executionSelection struct {
	run       string
	benchmark string
	skip      string
}

type testingPathPattern []*regexp.Regexp

type testingPatternAlternatives []testingPathPattern

type testingNameMatcher struct {
	filter testingPatternAlternatives
	skip   testingPatternAlternatives
}

func selectSemanticEntries(entries []semanticEntry, arguments []string) ([]plannedSemanticEntry, error) {
	selection, err := parseExecutionSelection(arguments)
	if err != nil {
		return nil, err
	}
	testMatch, err := compileTestingNameMatcher(selection.run, "-test.run", selection.skip)
	if err != nil {
		return nil, err
	}
	var benchmarkMatch testingNameMatcher
	if selection.benchmark != "" {
		benchmarkMatch, err = compileTestingNameMatcher(selection.benchmark, "-test.bench", selection.skip)
		if err != nil {
			return nil, err
		}
	}
	var selected []plannedSemanticEntry
	for _, entry := range entries {
		var matches bool
		switch entry.Kind {
		case "test":
			matches, _ = testMatch.match(entry.Name)
		case "benchmark":
			if selection.benchmark != "" {
				matches, _ = benchmarkMatch.match(entry.Name)
			}
		default:
			return nil, fmt.Errorf("unsupported compiler-derived testing entry kind %q", entry.Kind)
		}
		if matches {
			selected = append(selected, plannedSemanticEntry{
				Kind:            entry.Kind,
				Name:            entry.Name,
				RequiresNetwork: entry.RequiresNetwork,
			})
		}
	}
	sort.Slice(selected, func(left, right int) bool {
		if selected[left].Kind != selected[right].Kind {
			return selected[left].Kind < selected[right].Kind
		}
		return selected[left].Name < selected[right].Name
	})
	return selected, nil
}

func parseExecutionSelection(arguments []string) (executionSelection, error) {
	var selection executionSelection
	seenSelections := make(map[executionTestFlagKind]bool)
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" || argument == "-" || !strings.HasPrefix(argument, "-") {
			// Go's flag parser stops at the first positional argument. Tokens after
			// that boundary cannot alter testing's selection state.
			break
		}
		name, value, hasValue := splitExecutionFlag(argument)
		kind, known := executionTestFlags[name]
		if !known {
			return executionSelection{}, fmt.Errorf("unsupported test executable flag %q", argument)
		}
		switch kind {
		case executionUnsupportedSelectionFlag:
			if name == "test.list" {
				return executionSelection{}, errors.New("-test.list belongs to the non-network enumeration lifecycle")
			}
			return executionSelection{}, errors.New("fuzz execution is absent from the test/benchmark execution-plan schema")
		case executionBooleanFlag:
			continue
		case executionValueFlag, executionRunFlag, executionBenchmarkFlag, executionSkipFlag:
			if !hasValue {
				index++
				if index >= len(arguments) {
					return executionSelection{}, fmt.Errorf("-%s requires a value", name)
				}
				value = arguments[index]
			}
		}
		if kind == executionValueFlag {
			continue
		}
		if seenSelections[kind] {
			return executionSelection{}, fmt.Errorf("duplicate -%s selection", name)
		}
		seenSelections[kind] = true
		switch kind {
		case executionRunFlag:
			selection.run = value
		case executionBenchmarkFlag:
			selection.benchmark = value
		case executionSkipFlag:
			selection.skip = value
		}
	}
	return selection, nil
}

func splitExecutionFlag(argument string) (name, value string, hasValue bool) {
	name = strings.TrimPrefix(argument, "-")
	name = strings.TrimPrefix(name, "-")
	name, value, hasValue = strings.Cut(name, "=")
	return name, value, hasValue
}

func compileTestingNameMatcher(pattern, patternName, skip string) (testingNameMatcher, error) {
	matcher := testingNameMatcher{
		// testing.newMatcher gives an absent/empty run pattern a zero-component
		// simple match, which accepts every name without treating "" as a regexp.
		filter: testingPatternAlternatives{testingPathPattern{}},
	}
	var err error
	if pattern != "" {
		matcher.filter, err = compileTestingPattern(pattern, patternName)
		if err != nil {
			return testingNameMatcher{}, err
		}
	}
	if skip != "" {
		matcher.skip, err = compileTestingPattern(skip, "-test.skip")
		if err != nil {
			return testingNameMatcher{}, err
		}
	}
	return matcher, nil
}

func compileTestingPattern(pattern, name string) (testingPatternAlternatives, error) {
	paths := splitTestingRegexp(pattern)
	compiled := make(testingPatternAlternatives, len(paths))
	for pathIndex, path := range paths {
		compiled[pathIndex] = make(testingPathPattern, len(path))
		for componentIndex, component := range path {
			component = rewriteTestingPattern(component)
			match, err := regexp.Compile(component)
			if err != nil {
				return nil, fmt.Errorf(
					"compile component %d of alternative %d of %s (%q): %w",
					componentIndex,
					pathIndex,
					name,
					component,
					err,
				)
			}
			compiled[pathIndex][componentIndex] = match
		}
	}
	return compiled, nil
}

// splitTestingRegexp models testing.splitRegexp. Slash and top-level alternation
// are a selection language around each regexp component, so parsing the input as
// one regexp cannot recover Go's complete-path alternatives after the fact.
func splitTestingRegexp(pattern string) [][]string {
	path := make([]string, 0, strings.Count(pattern, "/"))
	alternatives := make([][]string, 0, strings.Count(pattern, "|"))
	characterClassDepth := 0
	parenthesisDepth := 0
	for index := 0; index < len(pattern); {
		switch pattern[index] {
		case '[':
			characterClassDepth++
		case ']':
			characterClassDepth--
			if characterClassDepth < 0 {
				// An unmatched closing bracket is legal regexp text.
				characterClassDepth = 0
			}
		case '(':
			if characterClassDepth == 0 {
				parenthesisDepth++
			}
		case ')':
			if characterClassDepth == 0 {
				parenthesisDepth--
			}
		case '\\':
			index++
		case '/':
			if characterClassDepth == 0 && parenthesisDepth == 0 {
				path = append(path, pattern[:index])
				pattern = pattern[index+1:]
				index = 0
				continue
			}
		case '|':
			if characterClassDepth == 0 && parenthesisDepth == 0 {
				path = append(path, pattern[:index])
				pattern = pattern[index+1:]
				index = 0
				alternatives = append(alternatives, path)
				path = make([]string, 0, len(path))
				continue
			}
		}
		index++
	}
	path = append(path, pattern)
	if len(alternatives) == 0 {
		return [][]string{path}
	}
	return append(alternatives, path)
}

func rewriteTestingPattern(pattern string) string {
	var rewritten strings.Builder
	for _, character := range pattern {
		switch {
		case unicode.IsSpace(character):
			rewritten.WriteByte('_')
		case !strconv.IsPrint(character):
			quoted := strconv.QuoteRune(character)
			rewritten.WriteString(quoted[1 : len(quoted)-1])
		default:
			rewritten.WriteRune(character)
		}
	}
	return rewritten.String()
}

func (matcher testingNameMatcher) match(name string) (ok, partial bool) {
	components := strings.Split(name, "/")
	ok, partial = matcher.filter.match(components)
	if !ok {
		return false, false
	}
	skipped, partialSkip := matcher.skip.match(components)
	if skipped && !partialSkip {
		return false, false
	}
	return true, partial
}

func (alternatives testingPatternAlternatives) match(name []string) (ok, partial bool) {
	for _, path := range alternatives {
		if ok, partial = path.match(name); ok {
			return ok, partial
		}
	}
	return false, false
}

func (pattern testingPathPattern) match(name []string) (ok, partial bool) {
	for index, component := range name {
		if index >= len(pattern) {
			break
		}
		if !pattern[index].MatchString(component) {
			return false, false
		}
	}
	return true, len(name) < len(pattern)
}
