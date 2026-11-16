package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

const diagnosticProfilesPath = "scripts/ci/gopls/profiles.json"

type diagnosticProfile struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

type goPort struct {
	goos   string
	goarch string
}

type diagnosticBuild struct {
	GOOS       string   `json:"goos"`
	GOARCH     string   `json:"goarch"`
	CgoEnabled bool     `json:"cgo_enabled"`
	Tags       []string `json:"tags,omitempty"`
}

func readDiagnosticProfiles(root string) ([]diagnosticProfile, error) {
	raw, err := os.ReadFile(filepath.Join(root, diagnosticProfilesPath))
	if err != nil {
		return nil, err
	}
	var profiles []diagnosticProfile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profiles); err != nil {
		return nil, fmt.Errorf("read diagnostic profiles: %w", err)
	}
	names := map[string]bool{"default": true}
	for _, profile := range profiles {
		if profile.Name == "" || names[profile.Name] || len(profile.Tags) == 0 {
			return nil, fmt.Errorf("invalid diagnostic profile %q", profile.Name)
		}
		for _, tag := range profile.Tags {
			expr, err := constraint.Parse("//go:build " + tag)
			parsed, ok := expr.(*constraint.TagExpr)
			if err != nil || !ok || parsed.Tag != tag || tag == "ignore" {
				return nil, fmt.Errorf("invalid build tag %q in diagnostic profile %q", tag, profile.Name)
			}
		}
		names[profile.Name] = true
	}
	return profiles, nil
}

func goDistPorts() ([]goPort, error) {
	command := exec.Command("go", "tool", "dist", "list")
	// An empty value falls back to GOENV. Whitespace is a nonempty, zero-flag list.
	command.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS= ")
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list Go target ports: %w", err)
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return nil, errors.New("empty Go target port list")
	}
	var ports []goPort
	for _, entry := range strings.Fields(string(output)) {
		goos, goarch, ok := strings.Cut(entry, "/")
		if !ok || goos == "" || goarch == "" {
			return nil, fmt.Errorf("invalid Go target port %q", entry)
		}
		ports = append(ports, goPort{goos: goos, goarch: goarch})
	}
	return ports, nil
}

func diagnosticPorts(host build.Context, ports []goPort) []goPort {
	ordered := []goPort{{host.GOOS, host.GOARCH}}
	// Prefer product platforms before fallback implementations on secondary ports.
	for _, port := range slices.Concat([]goPort{
		{"linux", "amd64"}, {"windows", "amd64"}, {"darwin", "arm64"},
	}, ports) {
		if slices.Contains(ports, port) && !slices.Contains(ordered, port) {
			ordered = append(ordered, port)
		}
	}
	return ordered
}

func (config diagnosticBuild) context(host build.Context) build.Context {
	host.GOOS, host.GOARCH = config.GOOS, config.GOARCH
	host.CgoEnabled = config.CgoEnabled
	host.BuildTags = config.Tags
	return host
}

type diagnosticSource struct {
	path, module, directory string
	raw                     []byte
	commandFile, usesCgo    bool
}

func readDiagnosticSource(root, path, module string) (diagnosticSource, error) {
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	raw, err := os.ReadFile(fullPath)
	if err != nil {
		return diagnosticSource{}, err
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), fullPath, raw, parser.ImportsOnly|parser.ParseComments)
	if err != nil {
		return diagnosticSource{}, fmt.Errorf("read diagnostic source header %s: %w", path, err)
	}
	source := diagnosticSource{path: path, module: module, directory: filepath.Dir(fullPath), raw: raw}
	// Standalone generators are diagnosed individually, never with -tags=ignore:
	// enabling that tag would merge independent commands and sibling packages.
	source.commandFile = parsed.Name.Name == "main" && !strings.HasSuffix(path, "_test.go")
	for _, spec := range parsed.Imports {
		if spec.Path.Value == `"C"` {
			source.usesCgo = true
		}
	}
	return source, nil
}

func (source diagnosticSource) matches(context build.Context) (bool, error) {
	if source.usesCgo && !context.CgoEnabled {
		return false, nil
	}
	context.JoinPath = filepath.Join
	context.OpenFile = func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(source.raw)), nil
	}
	ok, err := context.MatchFile(source.directory, filepath.Base(source.path))
	if err != nil {
		return false, fmt.Errorf("read diagnostic source constraints %s: %w", source.path, err)
	}
	return ok, nil
}
