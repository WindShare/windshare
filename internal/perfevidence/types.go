package perfevidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	SchemaVersion          = 4
	EvidenceKind           = "windshare-performance-evidence"
	MinimumBaselineSamples = 20
	MaximumSampleCount     = 100
)

type SourceFile struct {
	Path    string `json:"path"`
	Origin  string `json:"origin"`
	Kind    string `json:"kind"`
	Mode    uint32 `json:"mode"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256,omitempty"`
	Missing bool   `json:"missing,omitempty"`
	// Committed is meaningful only for workspace inputs. Baseline eligibility
	// requires every compiled workspace byte to match the recorded Git commit.
	Committed bool `json:"committed,omitempty"`
}
type SourceIdentity struct {
	Commit        string       `json:"commit"`
	WorktreeDirty bool         `json:"worktreeDirty"`
	StatusSHA256  string       `json:"statusSha256"`
	SourceSHA256  string       `json:"sourceSha256"`
	Files         []SourceFile `json:"files"`
}
type ToolVersion struct {
	Value string `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}
type HostMetadata struct {
	OS                  string                 `json:"os"`
	OSVersion           string                 `json:"osVersion"`
	Architecture        string                 `json:"architecture"`
	LogicalProcessors   int                    `json:"logicalProcessors"`
	CPUModel            string                 `json:"cpuModel,omitempty"`
	PhysicalMemoryBytes uint64                 `json:"physicalMemoryBytes"`
	MemoryProbe         string                 `json:"memoryProbe"`
	Tools               map[string]ToolVersion `json:"tools"`
	RequiredErrors      []string               `json:"requiredErrors,omitempty"`
}
type EnvironmentVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type ToolBinaryIdentity struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type ToolchainInputRoot string

const (
	ToolchainInputGoRoot    ToolchainInputRoot = "goroot"
	ToolchainInputGoToolDir ToolchainInputRoot = "gotooldir"
)

type ToolchainInputIdentity struct {
	Root   ToolchainInputRoot `json:"root"`
	Path   string             `json:"path"`
	Bytes  int64              `json:"bytes"`
	SHA256 string             `json:"sha256"`
}
type ToolchainIdentity struct {
	ExecutableSHA256 string                   `json:"executableSha256"`
	Version          string                   `json:"version"`
	GoVersion        string                   `json:"goVersion"`
	Tools            []ToolBinaryIdentity     `json:"tools"`
	BuildInputs      []ToolchainInputIdentity `json:"buildInputs"`
}
type ToolchainDiagnostics struct {
	ExecutablePath string `json:"executablePath"`
	GoRoot         string `json:"goRoot"`
	GoToolDir      string `json:"goToolDir"`
}
type ModuleIdentity struct {
	Path            string `json:"path"`
	Version         string `json:"version,omitempty"`
	Sum             string `json:"sum,omitempty"`
	GoModSum        string `json:"goModSum,omitempty"`
	ReplacementPath string `json:"replacementPath,omitempty"`
	Replacement     string `json:"replacement,omitempty"`
	Local           bool   `json:"local"`
}
type OverlayMapping struct {
	LogicalPath string `json:"logicalPath"`
	StubPath    string `json:"stubPath"`
	Package     string `json:"package"`
	StubSHA256  string `json:"stubSha256"`
}
type BuildGraphIdentity struct {
	WorkloadID               string           `json:"workloadId"`
	PackageImportPath        string           `json:"packageImportPath"`
	ClosureSHA256            string           `json:"closureSha256"`
	OverlaySHA256            string           `json:"overlaySha256"`
	OverlayPath              string           `json:"overlayPath"`
	PerformanceTests         []string         `json:"performanceTests"`
	SuppressedTests          []string         `json:"suppressedTests"`
	BenchmarkHarnessPackages []string         `json:"benchmarkHarnessPackages,omitempty"`
	OverlayMappings          []OverlayMapping `json:"overlayMappings"`
	DependencyPackages       []string         `json:"dependencyPackages"`
}
type OverlayFileDiagnostics struct {
	WorkloadID string `json:"workloadId"`
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
}
type SnapshotDiagnostics struct {
	ProcessEnvironment []EnvironmentVariable    `json:"processEnvironment"`
	EffectiveGoEnv     []EnvironmentVariable    `json:"effectiveGoEnv"`
	Toolchain          ToolchainDiagnostics     `json:"toolchain"`
	OverlayFiles       []OverlayFileDiagnostics `json:"overlayFiles"`
}
type SnapshotIdentity struct {
	SHA256                    string                `json:"sha256"`
	Git                       SourceIdentity        `json:"git"`
	Files                     []SourceFile          `json:"files"`
	ConsumptionInputs         []SourceFile          `json:"consumptionInputs"`
	CompiledInputsMatchCommit bool                  `json:"compiledInputsMatchCommit"`
	UncommittedInputs         []string              `json:"uncommittedInputs,omitempty"`
	BuildEnvironment          []EnvironmentVariable `json:"buildEnvironment"`
	Toolchain                 ToolchainIdentity     `json:"toolchain"`
	Modules                   []ModuleIdentity      `json:"modules"`
	BuildGraphs               []BuildGraphIdentity  `json:"buildGraphs"`
	Diagnostics               SnapshotDiagnostics   `json:"diagnostics"`
}
type BenchmarkContract struct {
	Name            string   `json:"name"`
	RequiredMetrics []string `json:"requiredMetrics"`
}
type Comparison string

const (
	Equal              Comparison = "equal"
	LessThan           Comparison = "less-than"
	LessThanOrEqual    Comparison = "less-than-or-equal"
	GreaterThan        Comparison = "greater-than"
	GreaterThanOrEqual Comparison = "greater-than-or-equal"
)

type MetricOracle struct {
	ID         string     `json:"id"`
	Benchmark  string     `json:"benchmark,omitempty"`
	Metric     string     `json:"metric"`
	Comparison Comparison `json:"comparison"`
	Limit      float64    `json:"limit"`
}
type Workload struct {
	ID                       string              `json:"id"`
	ModuleDir                string              `json:"moduleDir"`
	Package                  string              `json:"package"`
	Benchmark                string              `json:"benchmark"`
	BenchTime                string              `json:"benchTime"`
	BenchmarkHarnessPackages []string            `json:"benchmarkHarnessPackages,omitempty"`
	Contracts                []BenchmarkContract `json:"contracts"`
	HardOracles              []MetricOracle      `json:"hardOracles,omitempty"`
}
type BinaryEvidence struct {
	Path              string `json:"path"`
	Bytes             int64  `json:"bytes"`
	SHA256            string `json:"sha256"`
	GoBuildID         string `json:"goBuildId"`
	GoVersionMetadata string `json:"goVersionMetadata"`
	BuildGraphSHA256  string `json:"buildGraphSha256"`
}
type EvidencePhase string

const (
	EvidencePhaseBuild               EvidencePhase = "build"
	EvidencePhaseSample              EvidencePhase = "sample"
	EvidencePhaseProfile             EvidencePhase = "profile"
	EvidencePhaseProfileVerification EvidencePhase = "profile-verification"
)

type EvidenceOutcome string

const (
	EvidenceOutcomeSucceeded EvidenceOutcome = "succeeded"
	EvidenceOutcomeFailed    EvidenceOutcome = "failed"
)

type CommandEvidence struct {
	Phase      EvidencePhase   `json:"phase"`
	Outcome    EvidenceOutcome `json:"outcome"`
	Error      string          `json:"error,omitempty"`
	Executable string          `json:"executable,omitempty"`
	Arguments  []string        `json:"arguments,omitempty"`
	Directory  string          `json:"directory,omitempty"`
	ProcessID  int             `json:"processId,omitempty"`
	ExitCode   int             `json:"exitCode"`
	StartedAt  time.Time       `json:"startedAt,omitzero"`
	FinishedAt time.Time       `json:"finishedAt,omitzero"`
	Artifacts  []ArtifactFile  `json:"artifacts,omitempty"`
}
type BenchmarkSample struct {
	WorkloadID string             `json:"workloadId"`
	Index      int                `json:"index"`
	Command    CommandEvidence    `json:"command"`
	Rows       []BenchmarkReading `json:"rows"`
}
type BenchmarkReading struct {
	Name       string             `json:"name"`
	Iterations uint64             `json:"iterations"`
	Metrics    map[string]float64 `json:"metrics"`
}
type BenchmarkAggregate struct {
	Benchmark string  `json:"benchmark"`
	Metric    string  `json:"metric"`
	Samples   int     `json:"samples"`
	Minimum   float64 `json:"minimum"`
	P50       float64 `json:"p50"`
	P95       float64 `json:"p95"`
	Maximum   float64 `json:"maximum"`
}
type OracleResult struct {
	ID         string     `json:"id"`
	Benchmark  string     `json:"benchmark,omitempty"`
	Metric     string     `json:"metric,omitempty"`
	Comparison Comparison `json:"comparison,omitempty"`
	Limit      float64    `json:"limit,omitempty"`
	Passed     bool       `json:"passed"`
	Error      string     `json:"error,omitempty"`
}
type ProfileEvidence struct {
	Command      CommandEvidence   `json:"command"`
	Verification []CommandEvidence `json:"verification"`
	Binary       ArtifactFile      `json:"binary"`
	CPU          ArtifactFile      `json:"cpu"`
	Memory       ArtifactFile      `json:"memory"`
}
type WorkloadEvidence struct {
	Definition Workload             `json:"definition"`
	Build      CommandEvidence      `json:"build"`
	Binary     BinaryEvidence       `json:"binary"`
	Samples    []BenchmarkSample    `json:"samples"`
	Aggregates []BenchmarkAggregate `json:"aggregates"`
	Oracles    []OracleResult       `json:"oracles"`
	Profile    *ProfileEvidence     `json:"profile,omitempty"`
}
type ArtifactFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type SamplingPolicy struct {
	ProcessesPerWorkload   int    `json:"processesPerWorkload"`
	MinimumBaselineSamples int    `json:"minimumBaselineSamples"`
	Classification         string `json:"classification"`
}
type BaselineAssessment struct {
	Eligible bool     `json:"eligible"`
	Reasons  []string `json:"reasons,omitempty"`
}
type Evidence struct {
	SchemaVersion int                `json:"schemaVersion"`
	Kind          string             `json:"kind"`
	RunID         string             `json:"runId"`
	Status        string             `json:"status"`
	Error         string             `json:"error,omitempty"`
	StartedAt     time.Time          `json:"startedAt"`
	FinishedAt    time.Time          `json:"finishedAt"`
	Source        SnapshotIdentity   `json:"source"`
	Host          HostMetadata       `json:"host"`
	Sampling      SamplingPolicy     `json:"sampling"`
	Baseline      BaselineAssessment `json:"baseline"`
	Workloads     []WorkloadEvidence `json:"workloads"`
	Artifacts     []ArtifactFile     `json:"artifacts"`
}
type Publication struct {
	EvidenceID string
	Path       string
}
type semanticPath struct {
	Physical string
	Logical  string
}

func comparableBuildEnvironment(
	values []EnvironmentVariable,
	paths map[string]semanticPath,
) ([]EnvironmentVariable, error) {
	result := append([]EnvironmentVariable(nil), values...)
	seen := make(map[string]struct{}, len(result))
	for index := range result {
		if _, duplicate := seen[result[index].Name]; duplicate {
			return nil, fmt.Errorf("build environment repeats %s", result[index].Name)
		}
		seen[result[index].Name] = struct{}{}
		semantic, pathValue := paths[result[index].Name]
		if !pathValue {
			continue
		}
		if !samePath(result[index].Value, semantic.Physical) {
			return nil, fmt.Errorf("controlled %s path escaped its semantic location", result[index].Name)
		}
		result[index].Value = semantic.Logical
	}
	for name := range paths {
		if _, found := seen[name]; !found {
			return nil, fmt.Errorf("controlled build environment omitted %s", name)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result, nil
}

func overlayFileDiagnostics(
	artifactRoot string,
	overlays map[string]workloadOverlay,
) ([]OverlayFileDiagnostics, error) {
	result := make([]OverlayFileDiagnostics, 0, len(overlays))
	for workloadID, overlay := range overlays {
		relative, inside := relativeWithin(artifactRoot, overlay.OverlayPath)
		if !inside {
			return nil, fmt.Errorf("workload %s overlay is outside the evidence artifact root", workloadID)
		}
		if overlay.OverlayFileSHA256 == "" {
			return nil, fmt.Errorf("workload %s overlay file identity is missing", workloadID)
		}
		result = append(result, OverlayFileDiagnostics{
			WorkloadID: workloadID,
			Path:       filepath.ToSlash(relative),
			SHA256:     overlay.OverlayFileSHA256,
		})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].WorkloadID < result[right].WorkloadID })
	return result, nil
}

func snapshotIdentitySHA(identity SnapshotIdentity) (string, error) {
	// Runtime locations remain observable, but they cannot redefine the byte
	// identity used to compare two equivalent sealed builds.
	comparable := struct {
		Git                       SourceIdentity        `json:"git"`
		Files                     []SourceFile          `json:"files"`
		ConsumptionInputs         []SourceFile          `json:"consumptionInputs"`
		CompiledInputsMatchCommit bool                  `json:"compiledInputsMatchCommit"`
		UncommittedInputs         []string              `json:"uncommittedInputs,omitempty"`
		BuildEnvironment          []EnvironmentVariable `json:"buildEnvironment"`
		Toolchain                 ToolchainIdentity     `json:"toolchain"`
		Modules                   []ModuleIdentity      `json:"modules"`
		BuildGraphs               []BuildGraphIdentity  `json:"buildGraphs"`
	}{
		Git: identity.Git, Files: identity.Files, ConsumptionInputs: identity.ConsumptionInputs,
		CompiledInputsMatchCommit: identity.CompiledInputsMatchCommit,
		UncommittedInputs:         identity.UncommittedInputs,
		BuildEnvironment:          identity.BuildEnvironment,
		Toolchain:                 identity.Toolchain,
		Modules:                   identity.Modules,
		BuildGraphs:               identity.BuildGraphs,
	}
	return hashJSON(comparable)
}

func hashJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return hashBytes(encoded), nil
}

func processEnvironmentEvidence(environment []string) ([]EnvironmentVariable, error) {
	result := make([]EnvironmentVariable, 0, len(environment))
	seen := make(map[string]struct{}, len(environment))
	for _, assignment := range environment {
		name, value, found := strings.Cut(assignment, "=")
		if !found || name == "" {
			return nil, fmt.Errorf("controlled process environment contains malformed assignment %q", assignment)
		}
		name = strings.ToUpper(name)
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("controlled process environment repeats %s", name)
		}
		seen[name] = struct{}{}
		result = append(result, EnvironmentVariable{Name: name, Value: value})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result, nil
}

func replaceEvidenceEnvironment(values []EnvironmentVariable, name, value string) []EnvironmentVariable {
	result := append([]EnvironmentVariable(nil), values...)
	for index := range result {
		if result[index].Name == name {
			result[index].Value = value
			return result
		}
	}
	return append(result, EnvironmentVariable{Name: name, Value: value})
}

func VerifyPublication(path, expectedID string) error {
	return VerifyPublicationWithBudget(path, expectedID, DefaultEvidenceStoreBudget())
}

func VerifyPublicationWithBudget(path, expectedID string, budget EvidenceStoreBudget) error {
	authority, err := openTreeAuthority(path)
	if err != nil {
		return err
	}
	return errors.Join(verifyPublicationAuthorityWithBudget(authority, expectedID, budget), authority.close())
}

func verifyPublicationAuthorityWithBudget(
	authority *stageDirectoryAuthority,
	expectedID string,
	budget EvidenceStoreBudget,
) error {
	meter, err := newEvidenceStoreMeter(budget)
	if err != nil {
		return err
	}
	manifestBytes, err := readAuthorityFileWithMeter(authority, manifestName, evidenceMetadataFile, meter)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var manifest publicationManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	payload, err := readAuthorityFileWithMeter(authority, payloadName, evidencePayloadFile, meter)
	if err != nil {
		return fmt.Errorf("read payload: %w", err)
	}
	computedID := hashBytes(payload)
	if manifest.SchemaVersion != SchemaVersion || manifest.PayloadFile != payloadName ||
		manifest.EvidenceID != computedID || manifest.PayloadSHA256 != computedID ||
		(expectedID != "" && expectedID != computedID) {
		return errors.New("evidence manifest does not match its canonical payload")
	}
	var evidence Evidence
	if err := json.Unmarshal(payload, &evidence); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	if evidence.SchemaVersion != SchemaVersion || evidence.Kind != EvidenceKind {
		return errors.New("evidence payload has an unsupported contract")
	}
	expected := make(map[string]ArtifactFile, len(evidence.Artifacts))
	for _, artifact := range evidence.Artifacts {
		if _, duplicate := expected[artifact.Path]; duplicate {
			return fmt.Errorf("evidence repeats artifact %s", artifact.Path)
		}
		expected[artifact.Path] = artifact
	}
	actual, err := inspectArtifactsAuthorityWithMeter(
		authority, meter, map[string]struct{}{manifestName: {}, payloadName: {}},
	)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("evidence has %d artifact files; manifest records %d", len(actual), len(expected))
	}
	for _, artifact := range actual {
		if expected[artifact.Path] != artifact {
			return fmt.Errorf("artifact %s differs from its manifest", artifact.Path)
		}
	}
	if err := verifyEvidenceArtifactBindings(evidence, expected); err != nil {
		return err
	}
	return nil
}

func verifyEvidenceArtifactBindings(evidence Evidence, artifacts map[string]ArtifactFile) error {
	produced := make(map[string]string)
	for _, workload := range evidence.Workloads {
		workloadID := workload.Definition.ID
		if err := verifyCommandArtifactBindings(
			workload.Build, EvidencePhaseBuild, evidence.Status,
			"workload "+workloadID+" build", artifacts, produced,
		); err != nil {
			return err
		}
		binary := artifactFromBinary(workload.Binary)
		binaryPresent := workload.Binary != (BinaryEvidence{})
		if !binaryPresent {
			if workload.Build.Outcome != EvidenceOutcomeFailed || evidence.Status != string(EvidenceOutcomeFailed) {
				return fmt.Errorf("workload %s omits its binary outside a failed build", workloadID)
			}
			if len(workload.Samples) != 0 || workload.Profile != nil {
				return fmt.Errorf("workload %s continued after a failed build without a binary", workloadID)
			}
		} else {
			if err := verifyBoundArtifact(binary, artifacts, "workload "+workloadID+" binary", true); err != nil {
				return err
			}
			if workload.Binary.GoBuildID == "" || workload.Binary.GoVersionMetadata == "" ||
				!validSHA256(workload.Binary.BuildGraphSHA256) {
				return fmt.Errorf("workload %s binary omits its reproducible build identity", workloadID)
			}
			if !commandClaimsArtifact(workload.Build, binary) {
				return fmt.Errorf("workload %s build does not claim its binary artifact", workloadID)
			}
		}
		if workload.Build.Outcome == EvidenceOutcomeSucceeded && !binaryPresent {
			return fmt.Errorf("workload %s successful build has no binary identity", workloadID)
		}
		for index, sample := range workload.Samples {
			if err := verifyCommandArtifactBindings(
				sample.Command, EvidencePhaseSample, evidence.Status,
				fmt.Sprintf("workload %s sample %d", workloadID, index+1), artifacts, produced,
			); err != nil {
				return err
			}
		}
		if workload.Profile == nil {
			continue
		}
		profile := workload.Profile
		if err := verifyCommandArtifactBindings(
			profile.Command, EvidencePhaseProfile, evidence.Status,
			"workload "+workloadID+" profile", artifacts, produced,
		); err != nil {
			return err
		}
		for index, verification := range profile.Verification {
			if err := verifyCommandArtifactBindings(
				verification, EvidencePhaseProfileVerification, evidence.Status,
				fmt.Sprintf("workload %s profile verification %d", workloadID, index+1), artifacts, produced,
			); err != nil {
				return err
			}
		}
		profileBinaryPresent := profile.Binary != (ArtifactFile{})
		cpuPresent := profile.CPU != (ArtifactFile{})
		memoryPresent := profile.Memory != (ArtifactFile{})
		if profileBinaryPresent {
			if !binaryPresent || profile.Binary != binary {
				return fmt.Errorf("workload %s profile names a different binary identity", workloadID)
			}
			if err := verifyBoundArtifact(profile.Binary, artifacts, "workload "+workloadID+" profile binary", true); err != nil {
				return err
			}
		}
		for name, identity := range map[string]ArtifactFile{"CPU": profile.CPU, "memory": profile.Memory} {
			if identity == (ArtifactFile{}) {
				continue
			}
			if err := verifyBoundArtifact(
				identity, artifacts, fmt.Sprintf("workload %s %s profile", workloadID, name), true,
			); err != nil {
				return err
			}
		}
		if profile.Command.Outcome == EvidenceOutcomeSucceeded {
			if !profileBinaryPresent || !cpuPresent || !memoryPresent {
				return fmt.Errorf("workload %s successful profile omits a required identity", workloadID)
			}
			if profile.CPU.Path == profile.Memory.Path || profile.CPU.Path == binary.Path || profile.Memory.Path == binary.Path {
				return fmt.Errorf("workload %s profile identities are not distinct", workloadID)
			}
			if !commandClaimsArtifact(profile.Command, profile.CPU) ||
				!commandClaimsArtifact(profile.Command, profile.Memory) {
				return fmt.Errorf("workload %s profile command does not claim both profile artifacts", workloadID)
			}
		}
	}
	return nil
}

func verifyCommandArtifactBindings(
	command CommandEvidence,
	expectedPhase EvidencePhase,
	evidenceStatus string,
	label string,
	artifacts map[string]ArtifactFile,
	produced map[string]string,
) error {
	if command.Phase != expectedPhase {
		return fmt.Errorf("%s has phase %q, want %q", label, command.Phase, expectedPhase)
	}
	switch command.Outcome {
	case EvidenceOutcomeSucceeded:
		if command.Error != "" {
			return fmt.Errorf("%s succeeded with an error diagnostic", label)
		}
	case EvidenceOutcomeFailed:
		if strings.TrimSpace(command.Error) == "" {
			return fmt.Errorf("%s failed without an error diagnostic", label)
		}
		if evidenceStatus != string(EvidenceOutcomeFailed) {
			return fmt.Errorf("%s failed inside non-failed evidence", label)
		}
	default:
		return fmt.Errorf("%s has unsupported outcome %q", label, command.Outcome)
	}
	for _, identity := range command.Artifacts {
		if err := verifyBoundArtifact(identity, artifacts, label+" artifact", false); err != nil {
			return err
		}
		if previous, duplicate := produced[identity.Path]; duplicate {
			return fmt.Errorf("artifact %s is claimed by both %s and %s", identity.Path, previous, label)
		}
		produced[identity.Path] = label
	}
	return nil
}
