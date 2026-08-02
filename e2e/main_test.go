package e2e

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

type e2eBuildProfile string

const (
	goAuthorityExecutableEnvironment                 = "WINDSHARE_GO_EXECUTABLE"
	e2eBuildProfilePlain             e2eBuildProfile = "plain"
	e2eBuildProfileRace              e2eBuildProfile = "race"
)

type e2eBinaries struct {
	relay        string
	windshare    string
	processOwner string
}

type e2eBuildTarget struct {
	output    string
	packageID string
	arguments []string
}

type e2eBinaryFixture struct {
	profile         e2eBuildProfile
	createDirectory func() (string, error)
	removeDirectory func(string) error
	build           func(string, e2eBuildProfile) (e2eBinaries, error)

	initializeOnce sync.Once
	directory      string
	binaries       e2eBinaries
	initializeErr  error

	closeOnce sync.Once
	closeErr  error
}

var suiteBinaries = newE2EBinaryFixture(currentE2EBuildProfile)

// TestMain owns cleanup only. Deferring construction until a process scenario
// asks for binaries lets -short and -run filtering remain genuinely cheap.
func TestMain(m *testing.M) {
	code := m.Run()
	if err := suiteBinaries.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: clean build fixture:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func newE2EBinaryFixture(profile e2eBuildProfile) *e2eBinaryFixture {
	return &e2eBinaryFixture{
		profile: profile,
		createDirectory: func() (string, error) {
			return os.MkdirTemp("", "windshare-e2e-bin-")
		},
		removeDirectory: os.RemoveAll,
		build:           buildE2EBinaries,
	}
}

func (fixture *e2eBinaryFixture) Load() (e2eBinaries, error) {
	fixture.initializeOnce.Do(fixture.initialize)
	return fixture.binaries, fixture.initializeErr
}

func (fixture *e2eBinaryFixture) initialize() {
	directory, err := fixture.createDirectory()
	if err != nil {
		fixture.initializeErr = fmt.Errorf("create build directory: %w", err)
		return
	}
	fixture.directory = directory
	fixture.binaries, err = fixture.build(directory, fixture.profile)
	if err == nil {
		return
	}
	cleanupErr := fixture.removeDirectory(directory)
	fixture.directory = ""
	fixture.initializeErr = errors.Join(
		err,
		wrapError("remove incomplete build directory", cleanupErr),
	)
}

func (fixture *e2eBinaryFixture) Close() error {
	fixture.closeOnce.Do(func() {
		if fixture.directory == "" {
			return
		}
		fixture.closeErr = wrapError("remove build directory", fixture.removeDirectory(fixture.directory))
	})
	return fixture.closeErr
}

func wrapError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func loadE2EBinaries(t *testing.T) e2eBinaries {
	t.Helper()
	binaries, err := suiteBinaries.Load()
	if err != nil {
		t.Fatalf("build process E2E binaries: %v", err)
	}
	return binaries
}

// buildE2EBinaries uses the repository workspace so the independently released
// core module resolves to the source under test rather than a stale download.
func buildE2EBinaries(outDir string, profile e2eBuildProfile) (e2eBinaries, error) {
	plan, err := e2eBuildPlan(outDir, profile)
	if err != nil {
		return e2eBinaries{}, err
	}
	for _, target := range plan {
		command := exec.Command(e2eGoExecutable(), target.arguments...)
		command.Dir = repoRoot()
		if output, err := command.CombinedOutput(); err != nil {
			return e2eBinaries{}, fmt.Errorf("build %s with %s profile: %w\n%s", target.packageID, profile, err, output)
		}
	}
	return e2eBinaries{
		relay: plan[0].output, windshare: plan[1].output, processOwner: plan[2].output,
	}, nil
}

func e2eGoExecutable() string {
	// Hosted entrypoints retain their selected Go application across every
	// nested build; direct developer test runs keep conventional PATH behavior.
	if executable := os.Getenv(goAuthorityExecutableEnvironment); executable != "" {
		return executable
	}
	return "go"
}

func e2eBuildPlan(outDir string, profile e2eBuildProfile) ([]e2eBuildTarget, error) {
	if err := profile.validate(); err != nil {
		return nil, err
	}
	targets := []e2eBuildTarget{
		{output: filepath.Join(outDir, exeName("wsrelay")), packageID: "./relay/cmd/wsrelay"},
		{output: filepath.Join(outDir, exeName("windshare")), packageID: "./cmd/windshare"},
		{output: filepath.Join(outDir, exeName("testprocessowner")), packageID: "./cmd/testprocessowner"},
	}
	for index := range targets {
		targets[index].arguments = e2eBuildArguments(profile, targets[index].output, targets[index].packageID)
	}
	return targets, nil
}

func e2eBuildArguments(profile e2eBuildProfile, output, packageID string) []string {
	arguments := []string{"build"}
	if profile == e2eBuildProfileRace {
		arguments = append(arguments, "-race")
	}
	return append(arguments, "-o", output, packageID)
}

func (profile e2eBuildProfile) validate() error {
	switch profile {
	case e2eBuildProfilePlain, e2eBuildProfileRace:
		return nil
	default:
		return fmt.Errorf("unsupported E2E build profile %q", profile)
	}
}

func TestE2EBuildPlanAppliesProfileToEveryChild(t *testing.T) {
	t.Parallel()
	for _, profile := range []e2eBuildProfile{e2eBuildProfilePlain, e2eBuildProfileRace} {
		profile := profile
		t.Run(string(profile), func(t *testing.T) {
			t.Parallel()
			plan, err := e2eBuildPlan(t.TempDir(), profile)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan) != 3 ||
				plan[0].packageID != "./relay/cmd/wsrelay" ||
				plan[1].packageID != "./cmd/windshare" ||
				plan[2].packageID != "./cmd/testprocessowner" {
				t.Fatalf("build plan = %+v", plan)
			}
			for _, target := range plan {
				hasRace := len(target.arguments) > 1 && target.arguments[1] == "-race"
				if hasRace != (profile == e2eBuildProfileRace) {
					t.Fatalf("%s arguments = %v for %s profile", target.packageID, target.arguments, profile)
				}
			}
		})
	}
	if _, err := e2eBuildPlan(t.TempDir(), "coverage"); err == nil {
		t.Fatal("unsupported build profile was accepted")
	}
}

func TestE2EChildBuildProfileMatchesParentInstrumentation(t *testing.T) {
	if testing.Short() {
		t.Skip("building both provenance fixtures exceeds the short-test budget")
	}
	binaries := loadE2EBinaries(t)
	wantRace := currentE2EBuildProfile == e2eBuildProfileRace
	for component, filename := range map[string]string{
		"wsrelay":          binaries.relay,
		"windshare":        binaries.windshare,
		"testprocessowner": binaries.processOwner,
	} {
		component, filename := component, filename
		t.Run(component, func(t *testing.T) {
			hasRace, err := binaryUsesRaceInstrumentation(filename)
			if err != nil {
				t.Fatal(err)
			}
			if hasRace != wantRace {
				t.Fatalf(
					"%s race instrumentation = %t, want %t for parent profile %q",
					component, hasRace, wantRace, currentE2EBuildProfile,
				)
			}
		})
	}
}

func binaryUsesRaceInstrumentation(filename string) (bool, error) {
	information, err := buildinfo.ReadFile(filename)
	if err != nil {
		return false, fmt.Errorf("read build information from %s: %w", filename, err)
	}
	for _, setting := range information.Settings {
		if setting.Key != "-race" {
			continue
		}
		switch setting.Value {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return false, fmt.Errorf("%s has unsupported -race build setting %q", filename, setting.Value)
		}
	}
	// Go omits disabled boolean build settings, so absence is the canonical
	// plain-build representation rather than missing provenance.
	return false, nil
}

func TestE2EBinaryFixtureBuildsLazilyOnceAndCleansUp(t *testing.T) {
	createCalls, buildCalls, removeCalls := 0, 0, 0
	fixture := &e2eBinaryFixture{
		profile: e2eBuildProfileRace,
		createDirectory: func() (string, error) {
			createCalls++
			return "fixture-bin", nil
		},
		build: func(directory string, profile e2eBuildProfile) (e2eBinaries, error) {
			buildCalls++
			if directory != "fixture-bin" || profile != e2eBuildProfileRace {
				t.Fatalf("build input = %q, %q", directory, profile)
			}
			return e2eBinaries{relay: "relay", windshare: "windshare", processOwner: "owner"}, nil
		},
		removeDirectory: func(directory string) error {
			removeCalls++
			if directory != "fixture-bin" {
				t.Fatalf("cleanup directory = %q", directory)
			}
			return nil
		},
	}
	if createCalls != 0 || buildCalls != 0 {
		t.Fatal("fixture performed work before Load")
	}
	first, err := fixture.Load()
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.Load()
	if err != nil || second != first {
		t.Fatalf("second load = %+v, %v; first = %+v", second, err, first)
	}
	if createCalls != 1 || buildCalls != 1 {
		t.Fatalf("create calls = %d, build calls = %d", createCalls, buildCalls)
	}
	if err := fixture.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Close(); err != nil {
		t.Fatal(err)
	}
	if removeCalls != 1 {
		t.Fatalf("remove calls = %d", removeCalls)
	}
}

func TestE2EBinaryFixtureSurfacesBuildAndCleanupFailures(t *testing.T) {
	buildFailure := errors.New("compile failed")
	cleanupFailure := errors.New("directory locked")
	fixture := &e2eBinaryFixture{
		profile:         e2eBuildProfilePlain,
		createDirectory: func() (string, error) { return "fixture-bin", nil },
		build: func(string, e2eBuildProfile) (e2eBinaries, error) {
			return e2eBinaries{}, buildFailure
		},
		removeDirectory: func(string) error { return cleanupFailure },
	}
	if _, err := fixture.Load(); !errors.Is(err, buildFailure) || !errors.Is(err, cleanupFailure) {
		t.Fatalf("fixture load error = %v", err)
	}

	fixture = &e2eBinaryFixture{
		profile:         e2eBuildProfilePlain,
		createDirectory: func() (string, error) { return "fixture-bin", nil },
		build: func(string, e2eBuildProfile) (e2eBinaries, error) {
			return e2eBinaries{}, nil
		},
		removeDirectory: func(string) error { return cleanupFailure },
	}
	if _, err := fixture.Load(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Close(); !errors.Is(err, cleanupFailure) {
		t.Fatalf("fixture close error = %v", err)
	}
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}
