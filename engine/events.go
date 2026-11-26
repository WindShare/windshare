package engine

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

// RelayOrigin retains only scheme/host/port so logs and UI never see relay
// paths, query parameters, or embedded credentials.
type RelayOrigin struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   uint16 `json:"port"`
}

func (o RelayOrigin) Valid() bool {
	return (o.Scheme == "ws" || o.Scheme == "wss") && o.Host != "" && o.Port != 0
}

func (o RelayOrigin) HostPort() string {
	return net.JoinHostPort(o.Host, strconv.FormatUint(uint64(o.Port), 10))
}

func (o RelayOrigin) diagnosticValue() diagnostics.Value {
	return diagnostics.Object(diagnostics.Fields{
		"scheme": diagnostics.Text(o.Scheme),
		"host":   diagnostics.Text(o.Host),
		"port":   diagnostics.Number(float64(o.Port)),
	})
}

func NormalizeRelayOrigin(raw string) (RelayOrigin, error) {
	endpoint, err := v2.NormalizeRelayEndpoint(raw)
	if err != nil {
		return RelayOrigin{}, err
	}
	return relayOriginFromEndpoint(endpoint)
}

func relayOriginFromEndpoint(endpoint v2.RelayEndpoint) (RelayOrigin, error) {
	parsed, err := url.Parse(endpoint.DialURL)
	if err != nil {
		return RelayOrigin{}, err
	}
	base, ok := networkendpoint.BaseFromWebSocketURL(*parsed)
	if !ok {
		return RelayOrigin{}, errors.New("invalid relay websocket URL")
	}
	normalized, err := v2.NormalizeRelayEndpoint(base.String())
	if err != nil || normalized.DialURL != endpoint.DialURL {
		return RelayOrigin{}, errors.New("unnormalized relay endpoint")
	}
	portText := parsed.Port()
	if portText == "" {
		if parsed.Scheme == "ws" {
			portText = "80"
		} else {
			portText = "443"
		}
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return RelayOrigin{}, errors.New("invalid relay port")
	}
	origin := RelayOrigin{Scheme: parsed.Scheme, Host: parsed.Hostname(), Port: uint16(port)}
	if !origin.Valid() {
		return RelayOrigin{}, errors.New("invalid relay origin")
	}
	return origin, nil
}

type Transport string

const (
	TransportRelay  Transport = "relay"
	TransportWebRTC Transport = "webrtc"
)

type ContentPath string

const (
	ContentPathRelay          ContentPath = "relay"
	ContentPathDirect         ContentPath = "direct"
	ContentPathDirectAndRelay ContentPath = "direct_and_relay"
)

type RelayRecoveryState string

const (
	RelayRecoveryStarted   RelayRecoveryState = "started"
	RelayRecoverySucceeded RelayRecoveryState = "succeeded"
	RelayRecoveryFailed    RelayRecoveryState = "failed"
	RelayRecoveryWaiting   RelayRecoveryState = "waiting"
)

type RelayWaitReason string

const (
	RelayWaitConnection RelayWaitReason = ""
	RelayWaitBandwidth  RelayWaitReason = "bandwidth"
)

// RelayAvailability reports current relay admission counts for a live share.
type RelayAvailability struct {
	Available uint32 `json:"available"`
	Total     uint32 `json:"total"`
	EverReady bool   `json:"ever_ready"`
	Terminal  uint32 `json:"terminal"`
}

func (RelayAvailability) EngineEvent() {}

func (a RelayAvailability) DiagnosticRecord() diagnostics.Record {
	return diagnostics.New("relay_availability", diagnostics.Fields{
		"available":  diagnostics.Number(float64(a.Available)),
		"total":      diagnostics.Number(float64(a.Total)),
		"ever_ready": diagnostics.Bool(a.EverReady),
		"terminal":   diagnostics.Number(float64(a.Terminal)),
	})
}

type RelayConnected struct {
	Origin RelayOrigin
}

func (RelayConnected) EngineEvent() {}

func (e RelayConnected) DiagnosticRecord() diagnostics.Record {
	return diagnostics.New("relay_connected", diagnostics.Fields{
		"relay_origin": e.Origin.diagnosticValue(),
	})
}

type RelayRecovery struct {
	Origin            RelayOrigin
	Attempt           uint32
	State             RelayRecoveryState
	Failure           *Failure
	WaitReason        RelayWaitReason
	Authorization     serviceauthority.AdmissionState
	ShareInstance     [16]byte
	Generation        uint64
	Slow              bool
	Resume            bool
	Terminal          bool
	NextDelay         time.Duration
	ProtocolSessionID [16]byte
}

func (RelayRecovery) EngineEvent() {}

func (e RelayRecovery) DiagnosticRecord() diagnostics.Record {
	details := diagnostics.Fields{
		"connection_generation": diagnostics.Uint(e.Generation),
		"slow_wait":             diagnostics.Bool(e.Slow),
		"resume_registration":   diagnostics.Bool(e.Resume),
		"terminal":              diagnostics.Bool(e.Terminal),
		"next_delay_ms":         diagnostics.Millis(e.NextDelay),
	}
	if e.WaitReason != "" {
		details["wait_reason"] = diagnostics.Text(string(e.WaitReason))
	}
	if auth := e.Authorization.Name(); auth != "" {
		details["authorization"] = diagnostics.Text(auth)
	}
	if e.ShareInstance != ([16]byte{}) {
		details["share_instance"] = diagnostics.Identity(e.ShareInstance[:])
	}
	fields := diagnostics.Fields{
		"relay_origin": e.Origin.diagnosticValue(),
		"attempt":      diagnostics.Number(float64(e.Attempt)),
		"state":        diagnostics.Text(string(e.State)),
		"details":      diagnostics.Object(details),
	}
	if e.Failure != nil && e.Failure.Valid() {
		fields["failure"] = e.Failure.DiagnosticValue()
	}
	record := diagnostics.New("relay_recovering", fields)
	if e.ProtocolSessionID != ([16]byte{}) {
		record = record.Correlate(diagnostics.Fields{
			"protocol_session_id": diagnostics.Identity(e.ProtocolSessionID[:]),
		})
	}
	return record
}

type SourceOmissions struct {
	Summary catalog.SourceOmissions
}

func (SourceOmissions) EngineEvent() {}

func (e SourceOmissions) DiagnosticRecord() diagnostics.Record {
	// Default support logs retain omission counts and reason categories only.
	// Sample names and details can carry user filenames or colliding sibling spellings.
	samples := make([]diagnostics.Value, 0, len(e.Summary.Samples))
	for _, sample := range e.Summary.Samples {
		samples = append(samples, diagnostics.Object(diagnostics.Fields{
			"reason": diagnostics.Text(string(sample.Reason)),
		}))
	}
	return diagnostics.New("source_omissions", diagnostics.Fields{
		"count":   diagnostics.Uint(e.Summary.Count),
		"samples": diagnostics.Array(samples...),
	})
}

type ContentPathSelected struct {
	Path ContentPath
}

func (ContentPathSelected) EngineEvent() {}

func (e ContentPathSelected) DiagnosticRecord() diagnostics.Record {
	return diagnostics.New("content_path_selected", diagnostics.Fields{
		"content_path": diagnostics.Text(string(e.Path)),
	})
}

type Fallback struct {
	From    Transport
	To      Transport
	Failure Failure
}

func (Fallback) EngineEvent() {}

func (e Fallback) DiagnosticRecord() diagnostics.Record {
	return diagnostics.New("fallback", diagnostics.Fields{
		"from_transport": diagnostics.Text(string(e.From)),
		"to_transport":   diagnostics.Text(string(e.To)),
		"failure":        e.Failure.DiagnosticValue(),
	})
}

type Warning struct {
	Failure Failure
}

func (Warning) EngineEvent() {}

func (e Warning) DiagnosticRecord() diagnostics.Record {
	return diagnostics.New("warning", diagnostics.Fields{
		"failure": e.Failure.DiagnosticValue(),
	})
}

type TransferProgress struct {
	Operation           receivecontract.OperationID
	Job                 transfer.TransferJobID
	Snapshot            transfer.ReceiveProgressSnapshot
	CapacityWaitVisible bool
}

func (TransferProgress) EngineEvent() {}

func (e TransferProgress) DiagnosticRecord() diagnostics.Record {
	s := e.Snapshot
	f := s.FileOutcomes
	return diagnostics.New("transfer_progress", diagnostics.Fields{
		"receive_operation_id": diagnostics.Identity(e.Operation.Bytes()),
		"transfer_job_id":      diagnostics.Identity(e.Job.Bytes()),
		"progress": diagnostics.Object(diagnostics.Fields{
			"discovery":                  diagnostics.Text(discoveryStatusName(s.Discovery)),
			"counters_exact":             diagnostics.Bool(s.CountersExact),
			"source_omitted_entries":     diagnostics.Uint(s.SourceOmittedEntries),
			"discovered_files":           diagnostics.Uint(s.DiscoveredFiles),
			"discovered_bytes":           diagnostics.Uint(s.DiscoveredBytes),
			"published_files":            diagnostics.Uint(s.PublishedFiles),
			"published_bytes":            diagnostics.Uint(s.PublishedBytes),
			"previously_published_bytes": diagnostics.Uint(s.PreviouslyPublishedBytes),
			"verified_bytes":             diagnostics.Uint(s.VerifiedBytes),
			"newly_verified_bytes":       diagnostics.Uint(s.NewlyVerifiedBytes),
			"restart_reusable_bytes":     diagnostics.Uint(s.RestartReusableBytes),
			"file_outcomes": diagnostics.Object(diagnostics.Fields{
				"downloaded_files":           diagnostics.Uint(f.DownloadedFiles),
				"resumed_files":              diagnostics.Uint(f.ResumedFiles),
				"previously_published_files": diagnostics.Uint(f.PreviouslyPublishedFiles),
				"paused_files":               diagnostics.Uint(f.PausedFiles),
				"collision_files":            diagnostics.Uint(f.CollisionFiles),
				"item_blocked_files":         diagnostics.Uint(f.ItemBlockedFiles),
				"failed_files":               diagnostics.Uint(f.FailedFiles),
				"modified_time_warnings":     diagnostics.Uint(f.ModifiedTimeWarnings),
			}),
			"capacity_wait": diagnostics.Object(diagnostics.Fields{
				"active_waiters":      diagnostics.Uint(uint64(s.CapacityWait.ActiveWaiters)),
				"accumulated_wait_ms": diagnostics.Millis(s.CapacityWait.AccumulatedWait),
				"attempts":            diagnostics.Uint(s.CapacityWait.Attempts),
			}),
		}),
	})
}

type LaneAdopted struct {
	Session   protocolsession.ProtocolSessionID
	Lane      session.LaneIdentity
	Transport Transport
}

func (LaneAdopted) EngineEvent() {}

func (e LaneAdopted) DiagnosticRecord() diagnostics.Record {
	return diagnostics.New("lane_adopted", diagnostics.Fields{
		"transport": diagnostics.Text(string(e.Transport)),
	}).Correlate(diagnostics.Fields{
		"protocol_session_id": diagnostics.Identity(e.Session.Bytes()),
		"lane_id":             diagnostics.Number(float64(e.Lane.ID)),
		"lane_epoch":          diagnostics.Number(float64(e.Lane.Epoch)),
	})
}

type TaskLifecycle struct {
	TaskID         TaskID
	Workflow       string
	At             time.Time
	Stage          string
	StopReason     string
	Outcome        string
	FailureClass   string
	Failure        *Failure
	CleanupFailure *Failure
}

func (TaskLifecycle) EngineEvent() {}

func (e TaskLifecycle) DiagnosticRecord() diagnostics.Record {
	fields := diagnostics.Fields{
		"engine_task_id": diagnostics.Text(string(e.TaskID)),
		"stage":          diagnostics.Text(e.Stage),
		"stop_reason":    diagnostics.Text(e.StopReason),
		"observed_at":    diagnostics.Text(e.At.UTC().Format(time.RFC3339Nano)),
	}
	if e.Outcome != "" {
		fields["outcome"] = diagnostics.Text(e.Outcome)
	}
	if e.FailureClass != "" {
		fields["failure_class"] = diagnostics.Text(e.FailureClass)
	}
	if e.Failure != nil && e.Failure.Valid() {
		fields["failure"] = e.Failure.DiagnosticValue()
	}
	if e.CleanupFailure != nil && e.CleanupFailure.Valid() {
		fields["cleanup_failure"] = e.CleanupFailure.DiagnosticValue()
	}
	return diagnostics.New("engine_task_observed", fields).At(e.At)
}

func discoveryStatusName(status transfer.DiscoveryStatus) string {
	switch status {
	case transfer.DiscoveryComplete:
		return "complete"
	case transfer.DiscoveryFailed:
		return "failed"
	default:
		return "open"
	}
}

func translateWorkflowEvent(workflow string, envelope task.Observation, publish func(task.Observation) bool) bool {
	emit := func(event task.Event) bool {
		envelope.Event = event
		return publish(envelope)
	}
	switch value := envelope.Event.(type) {
	case task.Lifecycle:
		return emit(translateTaskLifecycle(workflow, envelope.TaskID, envelope.At, value))
	case share.Observation:
		switch {
		case value.RelayAvailability != nil:
			v := value.RelayAvailability
			return emit(RelayAvailability{
				Available: v.Available, Total: v.Total, EverReady: v.EverReady, Terminal: v.Terminal,
			})
		case value.RelayRecovery != nil:
			if translated, ok := translateSenderRecovery(*value.RelayRecovery); ok {
				return emit(translated)
			}
		}
		return false
	case share.SourceOmissionsObserved:
		return emit(SourceOmissions{Summary: value.Summary})
	case receive.Warning:
		return emit(Warning{Failure: translateReceiveWarning(value.Code, value.Cause)})
	case receive.RelayConnected:
		origin, err := relayOriginFromEndpoint(value.Endpoint)
		if err != nil {
			return false
		}
		return emit(RelayConnected{Origin: origin})
	case receive.RecoveryObserved:
		translated, relevant := translateReceiverRecovery(value.Value)
		if !relevant {
			return true
		}
		return emit(translated)
	case receive.PeerTerminated:
		if f, ok := translatePeerTerminationWarning(value.Value); ok {
			return emit(Warning{Failure: f})
		}
		return true
	case receive.ProgressObserved:
		return emit(TransferProgress{
			Operation:           value.Operation,
			Job:                 value.Job,
			Snapshot:            value.Value,
			CapacityWaitVisible: value.Value.CapacityWait.Visible(envelope.At),
		})
	case receive.ContentPathObserved:
		return emit(ContentPathSelected{Path: translateContentPath(value.Path)})
	case receive.FallbackObserved:
		return emit(Fallback{
			From:    TransportWebRTC,
			To:      TransportRelay,
			Failure: translateReceiveWarning(value.Code, nil),
		})
	case receive.LaneAdopted:
		return emit(LaneAdopted{Session: value.Session, Lane: value.Lane, Transport: TransportWebRTC})
	default:
		return false
	}
}

func translateTaskLifecycle(workflow string, id task.ID, at time.Time, value task.Lifecycle) TaskLifecycle {
	event := TaskLifecycle{
		TaskID:     id,
		Workflow:   workflow,
		At:         at,
		Stage:      taskStageName(value.State),
		StopReason: stopReasonName(value.StopReason),
	}
	if value.State == task.Finished {
		event.Outcome = outcomeName(value.Outcome)
		event.FailureClass = failureClassName(value.FailureClass)
		if value.Outcome != task.OutcomeSuccess && value.Outcome != task.OutcomeStopped {
			if f, ok := DescribeFailure(value.Err); ok {
				event.Failure = &f
			}
		}
		if f, ok := DescribeFailure(value.CleanupError); ok {
			event.CleanupFailure = &f
		}
	}
	return event
}

func taskStageName(state task.State) string {
	switch state {
	case task.Running:
		return "task_started"
	case task.Stopping:
		return "task_stopping"
	default:
		return "task_settled"
	}
}

func stopReasonName(reason task.StopReason) string {
	switch reason {
	case task.Cancelled:
		return "cancelled"
	case task.ShareStopped:
		return "share_stopped"
	case task.ApplicationClosed:
		return "application_closed"
	default:
		return "none"
	}
}

func outcomeName(outcome task.Outcome) string {
	switch outcome {
	case task.OutcomeSuccess:
		return "success"
	case task.OutcomePartial:
		return "partial"
	case task.OutcomePaused:
		return "paused"
	case task.OutcomeCancelled:
		return "cancelled"
	case task.OutcomeStopped:
		return "stopped"
	default:
		return "failed"
	}
}

func failureClassName(class task.FailureClass) string {
	switch class {
	case task.FailureNone:
		return "none"
	case task.FailureNetwork:
		return "network"
	case task.FailureUsage:
		return "usage"
	case task.FailureSourceDrift:
		return "source_drift"
	default:
		return "local"
	}
}

func translateContentPath(path receive.ContentPath) ContentPath {
	switch path {
	case receive.ContentPathDirect:
		return ContentPathDirect
	case receive.ContentPathDirectAndRelay:
		return ContentPathDirectAndRelay
	default:
		return ContentPathRelay
	}
}

func translateReceiveWarning(code receive.FailureCode, cause error) Failure {
	if mapped, ok := classifyReceiveCode(code); ok {
		return Failure{Code: mapped}
	}
	if f, ok := DescribeFailure(cause); ok {
		return f
	}
	return Failure{Code: FailureUnexpected}
}

func translatePeerTerminationWarning(value v2peer.ReceiverTerminationTrace) (Failure, bool) {
	for _, cause := range value.RetainedCauseClasses() {
		if f, ok := classifyReceiverCauseClass(cause); ok {
			return f, true
		}
	}
	if value.PeerShutdownFailed() || value.ChannelDrainFailed() {
		return Failure{Code: FailureDirectUnavailable}, true
	}
	return Failure{}, false
}

func classifyReceiverCauseClass(value v2peer.ReceiverCauseClass) (Failure, bool) {
	switch value {
	case v2peer.ReceiverCauseChannelAdmission:
		return Failure{Code: FailureDirectAdmission}, true
	case v2peer.ReceiverCauseDeadline:
		return Failure{Code: FailureDeadline}, true
	case v2peer.ReceiverCauseUnknown:
		return Failure{Code: FailureUnexpected}, true
	case v2peer.ReceiverCauseRuntimeClosed, v2peer.ReceiverCauseConfiguration,
		v2peer.ReceiverCauseOperationMissing, v2peer.ReceiverCauseNegotiationTimeout,
		v2peer.ReceiverCauseAdmissionTimeout, v2peer.ReceiverCauseCandidateLimit,
		v2peer.ReceiverCauseEventCapacity, v2peer.ReceiverCauseNegotiation,
		v2peer.ReceiverCauseProtocol, v2peer.ReceiverCausePeerShutdown,
		v2peer.ReceiverCauseChannelDrain:
		return Failure{Code: FailureDirectUnavailable}, true
	default:
		return Failure{}, false
	}
}

func translateSenderRecovery(value share.RelayRecovery) (RelayRecovery, bool) {
	origin, err := relayOriginFromEndpoint(value.Endpoint)
	if err != nil {
		return RelayRecovery{}, false
	}
	attempt := value.Attempt
	var state RelayRecoveryState
	switch attempt.State {
	case relayset.AttemptStarted:
		state = RelayRecoveryStarted
	case relayset.AttemptSucceeded:
		state = RelayRecoverySucceeded
	case relayset.AttemptFailed:
		state = RelayRecoveryFailed
	case relayset.AttemptAuthorization:
		state = RelayRecoveryWaiting
		if attempt.Terminal {
			state = RelayRecoveryFailed
		}
	case relayset.AttemptWaiting:
		state = RelayRecoveryWaiting
	default:
		return RelayRecovery{}, false
	}
	var failure *Failure
	if state == RelayRecoveryFailed {
		f, _ := DescribeFailure(attempt.Failure)
		// A previously usable route remains eligible for recovery; treat a
		// non-terminal 404 as transient unavailability rather than a route config error.
		if f.Code == FailureRelayRouteNotFound && !attempt.Terminal {
			f = Failure{Code: FailureRelayUnavailable}
		}
		if !f.Valid() {
			f = Failure{Code: FailureUnexpected}
		}
		failure = &f
	}
	return RelayRecovery{
		Origin:        origin,
		Attempt:       attempt.Number,
		State:         state,
		Failure:       failure,
		Authorization: attempt.Authorization,
		ShareInstance: attempt.ShareInstance,
		Generation:    attempt.Generation,
		Slow:          attempt.Slow,
		Resume:        attempt.Resume,
		Terminal:      attempt.Terminal,
		NextDelay:     attempt.NextDelay,
	}, true
}

func translateReceiverRecovery(value relayset.ReceiverRecoveryObservation) (RelayRecovery, bool) {
	origin, err := NormalizeRelayOrigin(value.Endpoint)
	if err != nil {
		return RelayRecovery{}, false
	}
	var state RelayRecoveryState
	var failure *Failure
	switch value.Phase {
	case relayset.ReceiverRecoveryConnecting:
		state = RelayRecoveryStarted
	case relayset.ReceiverRecoveryConnected:
		if value.Attempt == 1 {
			return RelayRecovery{}, false
		}
		state = RelayRecoverySucceeded
	case relayset.ReceiverRecoveryWaiting:
		state = RelayRecoveryWaiting
	case relayset.ReceiverRecoveryRetrying, relayset.ReceiverRecoveryTerminal:
		state = RelayRecoveryFailed
		f, _ := DescribeFailure(value.Err)
		if !f.Valid() {
			f = Failure{Code: FailureRelayUnavailable}
		}
		failure = &f
	default:
		return RelayRecovery{}, false
	}
	waitReason := RelayWaitConnection
	if _, waiting := errors.AsType[*framechannel.BandwidthWaitError](value.Err); waiting {
		waitReason = RelayWaitBandwidth
	}
	return RelayRecovery{
		Origin:            origin,
		Attempt:           value.Attempt,
		State:             state,
		Failure:           failure,
		WaitReason:        waitReason,
		Generation:        value.ConnectionGeneration,
		Slow:              value.Phase == relayset.ReceiverRecoveryWaiting,
		Terminal:          value.Phase == relayset.ReceiverRecoveryTerminal,
		NextDelay:         value.Delay,
		ProtocolSessionID: value.ProtocolSessionID,
	}, true
}
