package runtrace

import wire "github.com/windshare/windshare/engine/observation/wire"

type commandFailedPayload struct {
	ExitCode int          `json:"exit_code"`
	Failure  wire.Failure `json:"failure"`
}

func (commandFailedPayload) ObservationPayload() {}

type downloadConnectivity struct {
	DownloadID            string   `json:"download_id"`
	FirstDirectElapsedMS  *string  `json:"first_direct_elapsed_ms"`
	DirectBytes           string   `json:"direct_bytes"`
	TURNBytes             string   `json:"turn_bytes"`
	ApplicationRelayBytes string   `json:"application_relay_bytes"`
	UnknownBytes          string   `json:"unknown_bytes"`
	DirectFraction        *float64 `json:"direct_fraction"`
	FallbackStallMS       string   `json:"fallback_stall_ms"`
	Incomplete            bool     `json:"incomplete"`
	Final                 bool     `json:"final"`
}

type transferSettledPayload struct {
	DownloadConnectivity *downloadConnectivity `json:"download_connectivity,omitempty"`
	ResultStatus         string                `json:"result_status"`
	ExitCode             int                   `json:"exit_code"`
	Drift                string                `json:"drift"`
	ResultElapsedMS      string                `json:"result_elapsed_ms"`
	DestinationAdjusted  bool                  `json:"destination_adjusted"`
	FileOutcomes         wire.FileOutcomes     `json:"file_outcomes"`
	DirectoryFailures    string                `json:"directory_failures"`
	OmittedDiagnostics   string                `json:"omitted_diagnostics"`
	PublishedBytes       string                `json:"published_bytes"`
	CountersExact        bool                  `json:"counters_exact"`
	Failure              *wire.Failure         `json:"failure,omitempty"`
}

func (transferSettledPayload) ObservationPayload() {}

type sharingStoppedPayload struct {
	ExitCode        int           `json:"exit_code"`
	ResultElapsedMS string        `json:"result_elapsed_ms"`
	StoppedCleanly  bool          `json:"stopped_cleanly"`
	Failure         *wire.Failure `json:"failure,omitempty"`
}

func (sharingStoppedPayload) ObservationPayload() {}

type traceIncompletePayload struct {
	ContractRejected string `json:"contract_rejected"`
	Cause            string `json:"cause"`
	LifecycleDropped string `json:"lifecycle_dropped"`
	ProgressDropped  string `json:"progress_dropped"`
}

func (traceIncompletePayload) ObservationPayload() {}

type platformSetupPayload struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

func (platformSetupPayload) ObservationPayload() {}

type traceSummaryPayload struct {
	ContractRejected         string `json:"contract_rejected"`
	RejectionEvidenceDropped string `json:"rejection_evidence_dropped"`
	Incomplete               bool   `json:"incomplete"`
	LifecycleDropped         string `json:"lifecycle_dropped"`
	ProgressDropped          string `json:"progress_dropped"`
	EventsWritten            string `json:"events_written"`
	WriterFailed             bool   `json:"writer_failed"`
	FlushFailed              bool   `json:"flush_failed"`
	SchemaLimited            bool   `json:"schema_limited"`
}

func (traceSummaryPayload) ObservationPayload() {}
