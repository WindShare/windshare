package perfevidence

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

func InspectHost(ctx context.Context, runner CommandRunner, repositoryRoot string) HostMetadata {
	memoryBytes, memoryProbe, memoryErr := physicalMemory()
	if memoryErr != nil {
		memoryProbe = memoryProbe + ": " + memoryErr.Error()
	}
	cpu, cpuErr := cpuModel()
	metadata := HostMetadata{
		OS: runtime.GOOS, OSVersion: osDescription(), Architecture: runtime.GOARCH, LogicalProcessors: runtime.NumCPU(),
		CPUModel: cpu, PhysicalMemoryBytes: memoryBytes, MemoryProbe: memoryProbe,
		Tools: make(map[string]ToolVersion, 4),
	}
	if memoryErr != nil {
		metadata.RequiredErrors = append(metadata.RequiredErrors, "physical-memory-probe-failed")
	}
	if cpuErr != nil {
		metadata.RequiredErrors = append(metadata.RequiredErrors, "cpu-model-probe-failed")
	} else if strings.TrimSpace(metadata.CPUModel) == "" {
		metadata.RequiredErrors = append(metadata.RequiredErrors, "cpu-model-missing")
	}
	if metadata.OS == "" || metadata.OSVersion == "" || metadata.Architecture == "" || metadata.LogicalProcessors < 1 {
		metadata.RequiredErrors = append(metadata.RequiredErrors, "operating-system-identity-incomplete")
	}
	probes := []struct {
		name       string
		executable string
		arguments  []string
		directory  string
	}{
		{name: "go", executable: hostGoExecutable(), arguments: []string{"version"}},
		{name: "node", executable: "node", arguments: []string{"--version"}},
		{name: "pnpm", executable: "pnpm", arguments: []string{"--version"}},
		{name: "playwright", executable: "pnpm", arguments: []string{"exec", "playwright", "--version"}, directory: filepath.Join(repositoryRoot, "web")},
	}
	for _, probe := range probes {
		result, err := runner.Run(ctx, Command{
			Executable: probe.executable, Arguments: probe.arguments, Directory: probe.directory,
		})
		version := ToolVersion{Value: strings.TrimSpace(string(result.Output))}
		if err != nil || result.ExitCode != 0 {
			version.Value = ""
			version.Error = fmt.Sprintf("exit %d: %v: %s", result.ExitCode, err, strings.TrimSpace(string(result.Output)))
		}
		metadata.Tools[probe.name] = version
	}
	return metadata
}

func hostGoExecutable() string {
	if executable := os.Getenv(goAuthorityExecutableEnvironment); executable != "" {
		return executable
	}
	return "go"
}

func requiredIdentityErrors(host HostMetadata, source SnapshotIdentity) []string {
	errorsByID := make(map[string]struct{}, len(host.RequiredErrors)+8)
	for _, issue := range host.RequiredErrors {
		errorsByID[issue] = struct{}{}
	}
	if source.SHA256 == "" {
		errorsByID["source-snapshot-identity-missing"] = struct{}{}
	}
	toolchain := source.Toolchain
	toolchainLocations := source.Diagnostics.Toolchain
	if len(toolchain.ExecutableSHA256) != 64 || toolchain.Version == "" || toolchain.GoVersion == "" ||
		len(toolchain.Tools) == 0 || len(toolchain.BuildInputs) == 0 || toolchainLocations.ExecutablePath == "" ||
		toolchainLocations.GoRoot == "" || toolchainLocations.GoToolDir == "" {
		errorsByID["go-toolchain-identity-incomplete"] = struct{}{}
	}
	requiredEnvironment := map[string]string{
		"CGO_ENABLED":  "0",
		"GOENV":        "off",
		"GOEXPERIMENT": "",
		"GOFLAGS":      "",
		"GOOS":         runtime.GOOS,
		"GOARCH":       runtime.GOARCH,
		"GOPROXY":      "off",
		"GOSUMDB":      "off",
		"GOTOOLCHAIN":  "local",
	}
	observed := make(map[string]string, len(source.Diagnostics.ProcessEnvironment))
	for _, variable := range source.Diagnostics.ProcessEnvironment {
		observed[variable.Name] = variable.Value
	}
	for name, expected := range requiredEnvironment {
		if value, found := observed[name]; !found || value != expected {
			errorsByID["controlled-go-environment-incomplete"] = struct{}{}
		}
	}
	for _, required := range []string{"GOCACHE", "GOMODCACHE", "GOWORK", "TEMP"} {
		if observed[required] == "" {
			errorsByID["controlled-go-environment-incomplete"] = struct{}{}
		}
	}
	if len(source.Diagnostics.EffectiveGoEnv) == 0 {
		errorsByID["effective-go-environment-missing"] = struct{}{}
	}
	result := make([]string, 0, len(errorsByID))
	for issue := range errorsByID {
		result = append(result, issue)
	}
	sort.Strings(result)
	return result
}
