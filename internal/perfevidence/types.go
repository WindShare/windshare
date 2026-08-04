package perfevidence

import "time"

const (
	ReportSchemaVersion = 1
	ReportKind          = "windshare-performance-diagnostic"
)

type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
)

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
	ID          string              `json:"id"`
	ModuleDir   string              `json:"moduleDir"`
	Package     string              `json:"package"`
	Benchmark   string              `json:"benchmark"`
	BenchTime   string              `json:"benchTime"`
	Contracts   []BenchmarkContract `json:"contracts"`
	HardOracles []MetricOracle      `json:"hardOracles,omitempty"`
}

type BenchmarkReading struct {
	Name       string             `json:"name"`
	Iterations uint64             `json:"iterations"`
	Metrics    map[string]float64 `json:"metrics"`
}

type CommandDiagnostic struct {
	OperationID string    `json:"operationId"`
	Executable  string    `json:"executable"`
	Arguments   []string  `json:"arguments"`
	Directory   string    `json:"directory"`
	ProcessID   int       `json:"processId,omitempty"`
	ExitCode    int       `json:"exitCode"`
	StartedAt   time.Time `json:"startedAt,omitzero"`
	FinishedAt  time.Time `json:"finishedAt,omitzero"`
	Outcome     Outcome   `json:"outcome"`
	Error       string    `json:"error,omitempty"`
	OutputTail  string    `json:"outputTail,omitempty"`
}

type BenchmarkSample struct {
	WorkloadID string             `json:"workloadId"`
	Index      int                `json:"index"`
	Status     Outcome            `json:"status"`
	Error      string             `json:"error,omitempty"`
	Command    CommandDiagnostic  `json:"command"`
	Rows       []BenchmarkReading `json:"rows,omitempty"`
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
	Limit      *float64   `json:"limit,omitempty"`
	Passed     bool       `json:"passed"`
	Error      string     `json:"error,omitempty"`
}

type WorkloadResult struct {
	Definition Workload             `json:"definition"`
	Status     Outcome              `json:"status"`
	Error      string               `json:"error,omitempty"`
	Samples    []BenchmarkSample    `json:"samples,omitempty"`
	Aggregates []BenchmarkAggregate `json:"aggregates,omitempty"`
	Oracles    []OracleResult       `json:"oracles,omitempty"`
}

type EnvironmentContext struct {
	OS                string `json:"os"`
	Architecture      string `json:"architecture"`
	LogicalProcessors int    `json:"logicalProcessors"`
	GoVersion         string `json:"goVersion"`
}

type Report struct {
	SchemaVersion      int                `json:"schemaVersion"`
	Kind               string             `json:"kind"`
	RunID              string             `json:"runId"`
	Status             Outcome            `json:"status"`
	Error              string             `json:"error,omitempty"`
	StartedAt          time.Time          `json:"startedAt"`
	FinishedAt         time.Time          `json:"finishedAt"`
	SamplesPerWorkload int                `json:"samplesPerWorkload"`
	Environment        EnvironmentContext `json:"environment"`
	Workloads          []WorkloadResult   `json:"workloads"`
}

type Event struct {
	Timestamp      time.Time `json:"timestamp"`
	RunID          string    `json:"run_id"`
	OperationID    string    `json:"operation_id"`
	Scenario       string    `json:"scenario"`
	Component      string    `json:"component"`
	Milestone      string    `json:"milestone"`
	Outcome        string    `json:"outcome"`
	WorkloadID     string    `json:"workload_id,omitempty"`
	SampleIndex    int       `json:"sample_index,omitempty"`
	ExitCode       *int      `json:"exit_code,omitempty"`
	DurationMillis int64     `json:"duration_ms,omitempty"`
	Detail         string    `json:"detail,omitempty"`
}

type EventLogger interface {
	Log(Event) error
}
