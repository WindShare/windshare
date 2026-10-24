package runtrace

import wire "github.com/windshare/windshare/engine/observation/wire"

type commandFailedPayloadV4 struct {
	ExitCode int          `json:"exit_code"`
	Failure  wire.Failure `json:"failure"`
}

func (commandFailedPayloadV4) ObservationPayload() {}

type downloadConnectivityV4 struct {
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

type transferSettledPayloadV4 struct {
	DownloadConnectivity *downloadConnectivityV4 `json:"download_connectivity,omitempty"`
	ResultStatus         string                  `json:"result_status"`
	ExitCode             int                     `json:"exit_code"`
	Drift                string                  `json:"drift"`
	ResultElapsedMS      string                  `json:"result_elapsed_ms"`
	DestinationAdjusted  bool                    `json:"destination_adjusted"`
	FileOutcomes         wire.FileOutcomes       `json:"file_outcomes"`
	DirectoryFailures    string                  `json:"directory_failures"`
	OmittedDiagnostics   string                  `json:"omitted_diagnostics"`
	PublishedBytes       string                  `json:"published_bytes"`
	CountersExact        bool                    `json:"counters_exact"`
	Failure              *wire.Failure           `json:"failure,omitempty"`
}

func (transferSettledPayloadV4) ObservationPayload() {}

type sharingStoppedPayloadV4 struct {
	ExitCode        int           `json:"exit_code"`
	ResultElapsedMS string        `json:"result_elapsed_ms"`
	StoppedCleanly  bool          `json:"stopped_cleanly"`
	Failure         *wire.Failure `json:"failure,omitempty"`
}

func (sharingStoppedPayloadV4) ObservationPayload() {}

type traceIncompletePayloadV4 struct {
	ContractRejected string `json:"contract_rejected"`
	Cause            string `json:"cause"`
	LifecycleDropped string `json:"lifecycle_dropped"`
	ProgressDropped  string `json:"progress_dropped"`
}

func (traceIncompletePayloadV4) ObservationPayload() {}

type platformSetupPayloadV4 struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

func (platformSetupPayloadV4) ObservationPayload() {}

type traceSummaryPayloadV4 struct {
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

func (traceSummaryPayloadV4) ObservationPayload() {}
