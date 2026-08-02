package perfevidence

import "time"

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
	StartedAt  time.Time       `json:"startedAt,omitempty"`
	FinishedAt time.Time       `json:"finishedAt,omitempty"`
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
