package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestCoreImportPathUsesExactPathFamily(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		importPath string
		want       bool
	}{
		{name: "core root", importPath: coreImportPath, want: true},
		{name: "core child", importPath: coreImportPath + "/content", want: true},
		{name: "similar sibling", importPath: coreImportPath + "ish/content", want: false},
		{name: "repository root", importPath: repositoryModulePath, want: false},
		{name: "outside core", importPath: repositoryModulePath + "/transport/webrtc", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isCoreImportPath(test.importPath); got != test.want {
				t.Fatalf("isCoreImportPath(%q) = %t, want %t", test.importPath, got, test.want)
			}
		})
	}
}

func TestPackagePolicyEnforcesCoreAndModuleBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		scope       graphScope
		pkg         listedPackage
		wantProblem string
	}{
		{
			name: "core package remains internal despite root module ownership",
			pkg: listedPackage{
				ImportPath: coreImportPath + "/content",
				Module:     &listedModule{Path: repositoryModulePath},
			},
		},
		{
			name:  "synthetic test package uses its tested core package identity",
			scope: testDeltaScope,
			pkg: listedPackage{
				ImportPath: coreImportPath + ".test",
				ForTest:    coreImportPath,
				Module:     &listedModule{Path: repositoryModulePath},
			},
		},
		{
			name: "foreign nested module cannot claim a core import path",
			pkg: listedPackage{
				ImportPath: coreImportPath + "/plugin/codec",
				Module:     &listedModule{Path: coreImportPath + "/plugin"},
			},
			wantProblem: "owned by module " + coreImportPath + "/plugin",
		},
		{
			name:  "synthetic core test requires repository module ownership",
			scope: testDeltaScope,
			pkg: listedPackage{
				ImportPath: coreImportPath + "/plugin/codec.test",
				ForTest:    coreImportPath + "/plugin/codec",
			},
			wantProblem: "no auditable repository module identity",
		},
		{
			name: "WindShare package outside core",
			pkg: listedPackage{
				ImportPath: repositoryModulePath + "/transport/webrtc",
				Module:     &listedModule{Path: repositoryModulePath},
			},
			wantProblem: "outside the core boundary",
		},
		{
			name: "similar core prefix is not core",
			pkg: listedPackage{
				ImportPath: coreImportPath + "ish/content",
				Module:     &listedModule{Path: repositoryModulePath},
			},
			wantProblem: "outside the core boundary",
		},
		{
			name: "allowlisted CBOR module",
			pkg: listedPackage{
				ImportPath: "github.com/fxamacker/cbor/v2",
				Module:     &listedModule{Path: "github.com/fxamacker/cbor/v2"},
			},
		},
		{
			name: "unreviewed third-party module",
			pkg: listedPackage{
				ImportPath: "example.com/codec",
				Module:     &listedModule{Path: "example.com/codec"},
			},
			wantProblem: "not allowlisted",
		},
		{
			name: "allowlisted module with local replacement",
			pkg: listedPackage{
				ImportPath: "github.com/fxamacker/cbor/v2",
				Module: &listedModule{
					Path:    "github.com/fxamacker/cbor/v2",
					Replace: &listedModule{Path: "../fork"},
				},
			},
			wantProblem: "unreviewed source",
		},
		{
			name: "WebSocket module has capability diagnostic",
			pkg: listedPackage{
				ImportPath: "github.com/coder/websocket",
				Module:     &listedModule{Path: "github.com/coder/websocket"},
			},
			wantProblem: "prohibited WebSocket transport module",
		},
		{
			name: "Pion family has capability diagnostic",
			pkg: listedPackage{
				ImportPath: "github.com/pion/webrtc/v4",
				Module:     &listedModule{Path: "github.com/pion/webrtc/v4"},
			},
			wantProblem: "prohibited Pion concrete transport module",
		},
		{
			name: "HTTP is prohibited in production",
			pkg: listedPackage{
				ImportPath: "net/http",
				Standard:   true,
			},
			wantProblem: "prohibited HTTP transport capability",
		},
		{
			name:  "HTTP is not a test exception",
			scope: testDeltaScope,
			pkg: listedPackage{
				ImportPath: "net/http/internal",
				Standard:   true,
			},
			wantProblem: "prohibited HTTP transport capability",
		},
		{
			name: "os exec is prohibited in production",
			pkg: listedPackage{
				ImportPath: "os/exec",
				Standard:   true,
			},
			wantProblem: "native test-only capability",
		},
		{
			name:  "os exec is allowed only in test delta",
			scope: testDeltaScope,
			pkg: listedPackage{
				ImportPath: "os/exec",
				Standard:   true,
			},
		},
		{
			name: "transitive net remains valid for Windows production graph",
			pkg: listedPackage{
				ImportPath: "net",
				Standard:   true,
			},
		},
		{
			name: "core cannot directly import net in production",
			pkg: listedPackage{
				ImportPath: coreImportPath + "/content",
				Imports:    []string{"net"},
				Module:     &listedModule{Path: repositoryModulePath},
			},
			wantProblem: "directly imports native test-only capability net",
		},
		{
			name:  "core test delta may directly import net",
			scope: testDeltaScope,
			pkg: listedPackage{
				ImportPath: coreImportPath + "/content [" + coreImportPath + "/content.test]",
				ForTest:    coreImportPath + "/content",
				Imports:    []string{"net"},
				Module:     &listedModule{Path: repositoryModulePath},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			scope := test.scope
			if scope == "" {
				scope = productionScope
			}
			problems := packagePolicyViolations(scope, test.pkg)
			if test.wantProblem == "" {
				if len(problems) != 0 {
					t.Fatalf("packagePolicyViolations() = %v, want no problems", problems)
				}
				return
			}
			if len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), test.wantProblem) {
				t.Fatalf("packagePolicyViolations() = %v, want a problem containing %q", problems, test.wantProblem)
			}
		})
	}
}

func TestRunBoundaryListsProductionAndTestGraphsForEveryTarget(t *testing.T) {
	t.Parallel()

	corePackage := listedPackage{
		ImportPath: coreImportPath + "/content",
		Module:     &listedModule{Path: repositoryModulePath},
	}
	lister := &recordingGraphLister{
		production: []listedPackage{corePackage},
		tests: []listedPackage{
			corePackage,
			{
				ImportPath: coreImportPath + "/content [" + coreImportPath + "/content.test]",
				ForTest:    coreImportPath + "/content",
				Imports:    []string{"os/exec"},
				Module:     &listedModule{Path: repositoryModulePath},
			},
			{ImportPath: "os/exec", Standard: true},
		},
	}
	var progress bytes.Buffer
	findings := runBoundary(context.Background(), lister, &progress)
	if len(findings) != 0 {
		t.Fatalf("runBoundary() findings = %v, want none", findings)
	}

	wantCalls := []listCall{
		{Target: goTarget{GOOS: "linux", GOARCH: boundaryGOARCH}},
		{Target: goTarget{GOOS: "linux", GOARCH: boundaryGOARCH}, IncludeTests: true},
		{Target: goTarget{GOOS: "windows", GOARCH: boundaryGOARCH}},
		{Target: goTarget{GOOS: "windows", GOARCH: boundaryGOARCH}, IncludeTests: true},
		{Target: goTarget{GOOS: "darwin", GOARCH: boundaryGOARCH}},
		{Target: goTarget{GOOS: "darwin", GOARCH: boundaryGOARCH}, IncludeTests: true},
	}
	if len(lister.calls) != len(wantCalls) {
		t.Fatalf("List() calls = %v, want %v", lister.calls, wantCalls)
	}
	for i, want := range wantCalls {
		if got := lister.calls[i]; got != want {
			t.Errorf("List() call %d = %+v, want %+v", i, got, want)
		}
	}
	for _, target := range supportedTargets {
		if !strings.Contains(progress.String(), "target="+target.String()) {
			t.Errorf("progress output does not include %s: %s", target, progress.String())
		}
	}
	if !strings.Contains(progress.String(), "native_test_capabilities=os/exec") {
		t.Errorf("progress output does not name the test-only capability: %s", progress.String())
	}
}

func TestDecodePackageStreamPreservesModuleAndTestIdentity(t *testing.T) {
	t.Parallel()

	input := strings.NewReader(`
{"ImportPath":"github.com/windshare/windshare/core/link","Standard":false,"Module":{"Path":"github.com/windshare/windshare"}}
{"ImportPath":"github.com/windshare/windshare/core/link.test","ForTest":"github.com/windshare/windshare/core/link","Imports":["os/exec"],"Module":{"Path":"github.com/windshare/windshare"}}
`)
	packages, err := decodePackageStream(input)
	if err != nil {
		t.Fatalf("decodePackageStream() error = %v", err)
	}
	if len(packages) != 2 {
		t.Fatalf("decodePackageStream() returned %d packages, want 2", len(packages))
	}
	if packages[0].Module == nil || packages[0].Module.Path != repositoryModulePath {
		t.Fatalf("first package module = %#v, want %q", packages[0].Module, repositoryModulePath)
	}
	if packages[1].ForTest != coreImportPath+"/link" {
		t.Fatalf("second package ForTest = %q, want core link", packages[1].ForTest)
	}
}

func TestTargetEnvironmentOwnsCrossCompilationAndWorkspaceSelection(t *testing.T) {
	t.Parallel()

	environment := targetEnvironment(
		[]string{
			"PATH=example",
			"GOOS=plan9",
			"GoArCh=arm64",
			"CGO_ENABLED=1",
			"GOWORK=parent.work",
		},
		goTarget{GOOS: "darwin", GOARCH: boundaryGOARCH},
	)
	joined := strings.Join(environment, "\n")
	for _, expected := range []string{
		"PATH=example",
		"CGO_ENABLED=0",
		"GOARCH=" + boundaryGOARCH,
		"GOOS=darwin",
		"GOWORK=off",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("targetEnvironment() = %v, want %q", environment, expected)
		}
	}
	for _, excluded := range []string{"GOOS=plan9", "GoArCh=arm64", "CGO_ENABLED=1", "GOWORK=parent.work"} {
		if strings.Contains(joined, excluded) {
			t.Errorf("targetEnvironment() retained caller override %q: %v", excluded, environment)
		}
	}
}

func TestCommandGraphListerExecutesHermeticGoList(t *testing.T) {
	t.Parallel()

	lister := commandGraphLister{
		GoExecutable:     os.Args[0],
		GoArgumentPrefix: []string{"-test.run=TestCommandGraphListerHelper", "--"},
		RepositoryRoot:   t.TempDir(),
		BaseEnv: append(
			os.Environ(),
			"WINDSHARE_CORE_BOUNDARY_HELPER=success",
		),
	}
	packages, err := lister.List(
		context.Background(),
		goTarget{GOOS: "darwin", GOARCH: boundaryGOARCH},
		true,
	)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(packages) != 1 || packages[0].ImportPath != coreImportPath+"/fixture" {
		t.Fatalf("List() packages = %#v, want core fixture", packages)
	}
	if packages[0].ForTest != coreImportPath+"/fixture" {
		t.Fatalf("List() ForTest = %q, want core fixture", packages[0].ForTest)
	}
}

func TestCommandGraphListerReportsCommandAndDecodeFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		helperMode  string
		wantProblem string
	}{
		{name: "command", helperMode: "failure", wantProblem: "fixture list failure"},
		{name: "decode", helperMode: "malformed", wantProblem: "decode go list output"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lister := commandGraphLister{
				GoExecutable:     os.Args[0],
				GoArgumentPrefix: []string{"-test.run=TestCommandGraphListerHelper", "--"},
				RepositoryRoot:   t.TempDir(),
				BaseEnv: append(
					os.Environ(),
					"WINDSHARE_CORE_BOUNDARY_HELPER="+test.helperMode,
				),
			}
			_, err := lister.List(
				context.Background(),
				goTarget{GOOS: "linux", GOARCH: boundaryGOARCH},
				false,
			)
			if err == nil || !strings.Contains(err.Error(), test.wantProblem) {
				t.Fatalf("List() error = %v, want problem containing %q", err, test.wantProblem)
			}
		})
	}
}

func TestCommandGraphListerHelper(t *testing.T) {
	mode := os.Getenv("WINDSHARE_CORE_BOUNDARY_HELPER")
	if mode == "" {
		return
	}
	if os.Getenv("GOARCH") != boundaryGOARCH || os.Getenv("CGO_ENABLED") != "0" || os.Getenv("GOWORK") != "off" {
		os.Stderr.WriteString("fixture did not receive the owned target environment")
		os.Exit(2)
	}
	if mode == "failure" {
		os.Stderr.WriteString("fixture list failure")
		os.Exit(2)
	}
	if mode == "malformed" {
		os.Stdout.WriteString("{")
		os.Exit(0)
	}

	includeTests := false
	for _, argument := range os.Args {
		if argument == "-test" {
			includeTests = true
			break
		}
	}
	if !includeTests || os.Getenv("GOOS") != "darwin" {
		os.Stderr.WriteString("fixture did not receive the requested test graph and GOOS")
		os.Exit(2)
	}
	if err := json.NewEncoder(os.Stdout).Encode(listedPackage{
		ImportPath: coreImportPath + "/fixture",
		ForTest:    coreImportPath + "/fixture",
		Module:     &listedModule{Path: repositoryModulePath},
	}); err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(2)
	}
	os.Exit(0)
}

type listCall struct {
	Target       goTarget
	IncludeTests bool
}

type recordingGraphLister struct {
	production []listedPackage
	tests      []listedPackage
	calls      []listCall
}

func (l *recordingGraphLister) List(
	_ context.Context,
	target goTarget,
	includeTests bool,
) ([]listedPackage, error) {
	l.calls = append(l.calls, listCall{Target: target, IncludeTests: includeTests})
	if includeTests {
		return l.tests, nil
	}
	return l.production, nil
}
