package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const maxCustomConstraintTags = 16

type goPort struct {
	goos   string
	goarch string
}

// Known Go OS/arch identifiers are platform targets, never `-tags` build profiles,
// even when a caller supplies a reduced port set in tests.
var reservedBuildTags = map[string]bool{
	"ignore": true, "unix": true, "cgo": true, "gc": true, "gccgo": true,
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true,
	"hurd": true, "illumos": true, "ios": true, "js": true, "linux": true,
	"nacl": true, "netbsd": true, "openbsd": true, "plan9": true, "solaris": true,
	"wasip1": true, "windows": true, "zos": true,
	"386": true, "amd64": true, "amd64p32": true, "arm": true, "armbe": true,
	"arm64": true, "arm64be": true, "loong64": true, "mips": true, "mipsle": true,
	"mips64": true, "mips64le": true, "mips64p32": true, "mips64p32le": true,
	"ppc": true, "ppc64": true, "ppc64le": true, "riscv": true, "riscv64": true,
	"s390": true, "s390x": true, "sparc": true, "sparc64": true, "wasm": true,
}

func goDistPorts() ([]goPort, error) {
	command := exec.Command("go", "tool", "dist", "list")
	command.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=")
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list Go target ports: %w", err)
	}
	entries := strings.Fields(string(output))
	if len(entries) == 0 {
		return nil, errors.New("empty Go target port list")
	}
	ports := make([]goPort, 0, len(entries))
	for _, entry := range entries {
		goos, goarch, ok := strings.Cut(entry, "/")
		if !ok || goos == "" || goarch == "" {
			return nil, fmt.Errorf("invalid Go target port %q", entry)
		}
		ports = append(ports, goPort{goos: goos, goarch: goarch})
	}
	return ports, nil
}

// gopls synthesizes cross-platform views across GOOS/GOARCH ports automatically,
// but files gated on custom build tags remain orphaned unless checked in a session
// whose `-tags` configuration includes them.
func resolveDiagnosticSource(
	dir, name, relPath string,
	context build.Context,
	ports []goPort,
) ([]string, bool, error) {
	base := context
	base.BuildTags = nil
	matched, err := base.MatchFile(dir, name)
	if err != nil {
		return nil, false, fmt.Errorf("read diagnostic source constraints %s: %w", relPath, err)
	}
	if matched {
		return nil, true, nil
	}

	fullPath := filepath.Join(dir, name)
	raw, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, false, fmt.Errorf("read diagnostic source %s: %w", relPath, err)
	}
	buffered := base
	buffered.JoinPath = filepath.Join
	buffered.OpenFile = func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	}

	matchedPort, err := matchesAnyPort(buffered, dir, name, ports, base, nil)
	if err != nil {
		return nil, false, fmt.Errorf("read diagnostic source constraints %s: %w", relPath, err)
	}
	if matchedPort {
		return nil, false, nil
	}

	return resolveTaggedOrIgnoredSource(buffered, dir, name, fullPath, relPath, raw, base, ports)
}

func resolveTaggedOrIgnoredSource(
	buffered build.Context,
	dir, name, fullPath, relPath string,
	raw []byte,
	host build.Context,
	ports []goPort,
) ([]string, bool, error) {
	packageName, candidates, err := parseHeaderCustomTags(fullPath, raw, ports)
	if err != nil {
		return nil, false, fmt.Errorf("parse package header %s: %w", relPath, err)
	}
	if len(candidates) > maxCustomConstraintTags {
		return nil, false, fmt.Errorf("source %s exceeds %d custom build tags", relPath, maxCustomConstraintTags)
	}

	hostPorts := []goPort{{goos: host.GOOS, goarch: host.GOARCH}}
	if tags, ok := findMatchingTagSubset(buffered, dir, name, hostPorts, host, candidates); ok {
		return tags, true, nil
	}
	if tags, ok := findMatchingTagSubset(buffered, dir, name, ports, host, candidates); ok {
		return tags, false, nil
	}

	// gopls loads standalone `//go:build ignore` commands as command-line-arguments
	// packages without `-tags=ignore`, which would collide with sibling package files.
	if packageName == "main" && !strings.HasSuffix(name, "_test.go") {
		if ok, matchErr := matchesAnyPort(buffered, dir, name, slices.Concat(hostPorts, ports), host, []string{"ignore"}); matchErr == nil && ok {
			return nil, false, nil
		}
	}

	return nil, false, fmt.Errorf("source %s has no analyzable build configuration", relPath)
}

func matchesAnyPort(
	buffered build.Context,
	dir, name string,
	ports []goPort,
	host build.Context,
	tags []string,
) (bool, error) {
	for _, port := range ports {
		buffered.GOOS = port.goos
		buffered.GOARCH = port.goarch
		buffered.CgoEnabled = port.goos == host.GOOS && port.goarch == host.GOARCH && host.CgoEnabled
		buffered.BuildTags = tags
		ok, err := buffered.MatchFile(dir, name)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func findMatchingTagSubset(
	buffered build.Context,
	dir, name string,
	ports []goPort,
	host build.Context,
	candidates []string,
) ([]string, bool) {
	for size := 1; size <= len(candidates); size++ {
		subset := make([]string, size)
		if searchTagCombinations(buffered, dir, name, ports, host, candidates, subset, 0, 0) {
			return subset, true
		}
	}
	return nil, false
}

func searchTagCombinations(
	buffered build.Context,
	dir, name string,
	ports []goPort,
	host build.Context,
	candidates, subset []string,
	start, depth int,
) bool {
	if depth == len(subset) {
		ok, err := matchesAnyPort(buffered, dir, name, ports, host, subset)
		return err == nil && ok
	}
	remaining := len(subset) - depth
	for index := start; index <= len(candidates)-remaining; index++ {
		subset[depth] = candidates[index]
		if searchTagCombinations(buffered, dir, name, ports, host, candidates, subset, index+1, depth+1) {
			return true
		}
	}
	return false
}

func parseHeaderCustomTags(path string, raw []byte, ports []goPort) (string, []string, error) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, raw, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		return "", nil, err
	}
	exprs, err := headerConstraintExprs(parsed)
	if err != nil {
		return "", nil, err
	}
	portNames := make(map[string]bool, len(ports)*2)
	for _, port := range ports {
		portNames[port.goos] = true
		portNames[port.goarch] = true
	}
	var tags []string
	for _, expr := range exprs {
		collectCustomExprTags(expr, portNames, &tags)
	}
	slices.Sort(tags)
	return parsed.Name.Name, slices.Compact(tags), nil
}

func headerConstraintExprs(parsed *ast.File) ([]constraint.Expr, error) {
	var goBuildExprs, plusBuildExprs []constraint.Expr
	for _, group := range parsed.Comments {
		for _, comment := range group.List {
			if comment.Slash >= parsed.Package {
				break
			}
			expr, isGoBuild, ok, err := parseConstraintComment(comment.Text)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if isGoBuild {
				goBuildExprs = append(goBuildExprs, expr)
			} else {
				plusBuildExprs = append(plusBuildExprs, expr)
			}
		}
	}
	if len(goBuildExprs) > 0 {
		return goBuildExprs, nil
	}
	return plusBuildExprs, nil
}

func parseConstraintComment(text string) (constraint.Expr, bool, bool, error) {
	if constraint.IsGoBuild(text) {
		expr, err := constraint.Parse(text)
		return expr, true, true, err
	}
	if constraint.IsPlusBuild(text) {
		expr, err := constraint.Parse(text)
		return expr, false, true, err
	}
	return nil, false, false, nil
}

func collectCustomExprTags(expr constraint.Expr, portNames map[string]bool, out *[]string) {
	switch node := expr.(type) {
	case *constraint.TagExpr:
		if !isReservedBuildTag(node.Tag, portNames) {
			*out = append(*out, node.Tag)
		}
	case *constraint.NotExpr:
		collectCustomExprTags(node.X, portNames, out)
	case *constraint.AndExpr:
		collectCustomExprTags(node.X, portNames, out)
		collectCustomExprTags(node.Y, portNames, out)
	case *constraint.OrExpr:
		collectCustomExprTags(node.X, portNames, out)
		collectCustomExprTags(node.Y, portNames, out)
	}
}

func isReservedBuildTag(tag string, portNames map[string]bool) bool {
	return reservedBuildTags[tag] ||
		portNames[tag] ||
		strings.HasPrefix(tag, "go1.") ||
		strings.HasPrefix(tag, "goexperiment.")
}
