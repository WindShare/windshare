package cli

import (
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/humanoutput"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/internal/diaglog"
	"github.com/windshare/windshare/internal/platformsetup"
)

const (
	commandShare              = "share"
	commandGet                = "get"
	defaultDiagnosticCapacity = 256
)

var (
	errCommandRuntimeConfig = errors.New("command observation runtime configuration is invalid")
	errUserTraceOpen        = errors.New("user trace could not be opened")
)

type runTraceRecorder interface {
	RecordRun(diaglog.RunRecord) bool
	AddDropped(uint64)
	CloseRun() diaglog.RunStatus
	Path() string
}

type userTraceOpener func(
	target diaglog.Target,
	command string,
	opts diaglog.RunOptions,
) (runTraceRecorder, error)

type terminalOutcome struct {
	event       string
	level       string
	payload     any
	renderHuman func(*humanoutput.Renderer)
}

type commandRuntime struct {
	command             string
	clock               commandClock
	human               *humanoutput.Renderer
	trace               runTraceRecorder
	canvas              canvasHealth
	detailedDiagnostics bool

	diagnosticStream *diagnostics.Stream
	diagDone         chan struct{}

	mu                  sync.Mutex
	stagedFinalProgress *engine.TransferProgress
	stagedTerminal      *terminalOutcome
	closeOnce           sync.Once
}

// canvasHealth is deliberately narrower than TerminalCanvas. Runtime users can
// observe presentation health but cannot turn a writer failure into authority.
type canvasHealth interface {
	Err() error
	FinishProgress()
}

func (a *App) newCommandRuntime(
	command string,
	options observationOptions,
) (*commandRuntime, error) {
	if command != commandShare && command != commandGet {
		return nil, errCommandRuntimeConfig
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	terminal := a.terminalOutput()
	renderer, err := humanoutput.New(humanoutput.Config{
		Canvas:       terminal.canvas,
		Capabilities: terminal.caps,
		Clock:        terminal.clock,
		CellWidth:    terminal.width,
		Verbose:      options.verbose,
	})
	if err != nil {
		return nil, errCommandRuntimeConfig
	}

	recorder, err := a.openCommandTrace(command, options, terminal, renderer)
	if err != nil {
		return nil, err
	}

	runtime := &commandRuntime{
		command:             command,
		clock:               terminal.clock,
		human:               renderer,
		trace:               recorder,
		canvas:              terminal.canvas,
		detailedDiagnostics: options.verbose || options.traceEnabled(),
	}
	if runtime.detailedDiagnostics {
		runtime.diagnosticStream, err = diagnostics.NewStream(defaultDiagnosticCapacity, terminal.clock.Now)
		if err != nil {
			if recorder != nil {
				recorder.CloseRun()
			}
			return nil, err
		}
		runtime.diagDone = make(chan struct{})
		go runtime.drainDiagnostics()
	}
	return runtime, nil
}

func (a *App) openCommandTrace(
	command string,
	options observationOptions,
	terminal *terminalOutputState,
	renderer *humanoutput.Renderer,
) (runTraceRecorder, error) {
	if !options.traceEnabled() {
		return nil, nil
	}
	opener := a.openUserTrace
	if opener == nil {
		opener = openNativeUserTrace
	}
	target, targetErr := options.traceTarget()
	if targetErr != nil {
		renderer.RenderTraceOpenFailure(false)
		return nil, errUserTraceOpen
	}
	recorder, err := opener(target, command, diaglog.RunOptions{Now: terminal.clock.Now})
	if err == nil && recorder != nil {
		if options.traceDirectoryMode() {
			renderer.RenderTracePath(recorder.Path())
		}
		return recorder, nil
	}
	if recorder != nil {
		recorder.CloseRun()
	}
	exists := errors.Is(err, diaglog.ErrTraceExists)
	renderer.RenderTraceOpenFailure(exists)
	if exists {
		return nil, errors.Join(errUserTraceOpen, diaglog.ErrTraceExists)
	}
	return nil, errUserTraceOpen
}

func openNativeUserTrace(
	target diaglog.Target,
	command string,
	opts diaglog.RunOptions,
) (runTraceRecorder, error) {
	recorder, err := diaglog.OpenRun(target, command, opts)
	if err != nil {
		// Returning a typed-nil *diaglog.Recorder would present as non-nil interface.
		return nil, err
	}
	return recorder, nil
}

func (runtime *commandRuntime) drainDiagnostics() {
	defer close(runtime.diagDone)
	for rec := range runtime.diagnosticStream.Records() {
		runtime.observeDiagnostic(rec)
	}
}

func (runtime *commandRuntime) observeDiagnostic(rec diagnostics.Record) {
	runtime.human.RenderDiagnostic(rec)
	if runtime.trace == nil || !rec.Valid() {
		return
	}
	scope := rec.Scope()
	entry := diaglog.RunRecord{
		Level:      "debug",
		Event:      rec.Name(),
		Scope:      &scope,
		SourceTime: rec.ObservedAt(),
		Payload:    rec.Payload(),
	}
	if corr := rec.Correlation(); corr.Present() {
		entry.Correlation = corr
	}
	runtime.trace.RecordRun(entry)
}

func (runtime *commandRuntime) ObserveEngine(obs engine.Observation) {
	if runtime == nil || obs.Event == nil {
		return
	}
	isShare := runtime.command == commandShare
	switch value := obs.Event.(type) {
	case engine.RelayAvailability:
		runtime.human.RenderRelayAvailability(value)
	case engine.RelayConnected:
		runtime.human.RenderRelayConnected(isShare)
	case engine.RelayRecovery:
		runtime.human.RenderRelayRecovery(value)
	case engine.SourceOmissions:
		runtime.human.RenderSourceOmissions(value.Summary)
	case engine.ContentPathSelected:
		runtime.human.RenderContentPath(value.Path)
	case engine.Fallback:
		runtime.human.RenderFallback(value)
	case engine.Warning:
		runtime.human.RenderWarning(value.Failure)
	case engine.TransferProgress:
		runtime.human.RenderProgress(value)
	case engine.LaneAdopted:
		runtime.human.RenderLaneAdopted(value)
	case engine.TaskLifecycle:
		// Task lifecycle milestones belong to diagnostic traces, not human stderr.
	}
	if source, ok := obs.Event.(diagnostics.Source); ok {
		runtime.recordSource(source)
	}
}

func (runtime *commandRuntime) PublishShareReady(summary liveshare.SelectedRootSummary, origin engine.RelayOrigin) {
	if runtime == nil {
		return
	}
	subject := humanoutput.SharingSubjectFromSummary(summary)
	runtime.human.RenderReady()
	runtime.human.RenderSharingSubject(subject)
	runtime.human.RenderRelayConnected(true)
	if runtime.trace == nil {
		return
	}
	runtime.trace.RecordRun(diaglog.RunRecord{Level: "info", Event: "ready"})
	runtime.trace.RecordRun(diaglog.RunRecord{
		Level:   "info",
		Event:   "sharing_subject_selected",
		Payload: sharingSubjectTracePayload(subject),
	})
	runtime.recordSource(engine.RelayConnected{Origin: origin})
}

func (runtime *commandRuntime) ObserveNetworkSetup(report platformsetup.StartupReport) {
	if runtime == nil {
		return
	}
	runtime.human.RenderNetworkSetup(report)
	if runtime.trace == nil {
		return
	}
	level := "debug"
	if report.Decision.SuggestSetup() {
		level = "info"
	}
	runtime.trace.RecordRun(diaglog.RunRecord{
		Level: level,
		Event: "network_setup",
		Payload: networkSetupTracePayload{
			Decision:             report.Decision.String(),
			Executable:           report.Executable,
			ConfiguredExecutable: report.ConfiguredExecutable,
			Error:                report.Error,
			SuggestSetup:         report.Decision.SuggestSetup(),
		},
	})
}

func (runtime *commandRuntime) AddDropped(count uint64) {
	if runtime == nil || runtime.trace == nil || count == 0 {
		return
	}
	runtime.trace.AddDropped(count)
}

// failToStart handles failures before a task exists. Derive both exit code and
// terminal presentation from one classification so share and get stay aligned.
func (runtime *commandRuntime) failToStart(cause error) int {
	failure := classifyFailure(cause)
	exit := ExitFailure
	switch failure.Code {
	case engine.FailureNetworkUnavailable:
		exit = ExitNetwork
	case engine.FailureInvalidInput, engine.FailureCapabilityInvalid:
		exit = ExitUsage
	}
	runtime.StageCommandFailure(exit, failure)
	return exit
}

func (runtime *commandRuntime) StageCommandFailure(exit int, failure engine.Failure) {
	if runtime == nil {
		return
	}
	if !failure.Valid() {
		failure = engine.Failure{Code: engine.FailureUnexpected}
	}
	isShare := runtime.command == commandShare
	runtime.stageTerminal(nil, terminalOutcome{
		event: "command_failed",
		level: "error",
		payload: commandFailedTracePayload{
			ExitCode: exit,
			Failure:  failure.DiagnosticValue(),
		},
		renderHuman: func(h *humanoutput.Renderer) {
			h.RenderCommandFailed(isShare, failure)
		},
	})
}

func (runtime *commandRuntime) StageShareResult(exit int, result humanoutput.ShareResult) {
	if runtime == nil {
		return
	}
	level := "info"
	if !result.StoppedCleanly {
		level = "error"
	}
	publication := "unpublished"
	if result.Published {
		publication = "published"
	}
	payload := sharingStoppedTracePayload{
		Publication:     publication,
		ExitCode:        exit,
		ResultElapsedMS: strconv.FormatInt(max(result.Elapsed.Milliseconds(), 0), 10),
		StoppedCleanly:  result.StoppedCleanly,
	}
	if result.Failure != nil && result.Failure.Valid() {
		val := result.Failure.DiagnosticValue()
		payload.Failure = &val
	}
	runtime.stageTerminal(nil, terminalOutcome{
		event:   "sharing_stopped",
		level:   level,
		payload: payload,
		renderHuman: func(h *humanoutput.Renderer) {
			h.RenderShareResult(result)
		},
	})
}

func (runtime *commandRuntime) StageTransferResult(
	finalProgress engine.TransferProgress,
	exit int,
	result humanoutput.TransferResult,
	connectivity downloadmetrics.Snapshot,
) {
	if runtime == nil {
		return
	}
	level := "info"
	switch result.Status {
	case humanoutput.ResultPartial, humanoutput.ResultPaused, humanoutput.ResultStopped:
		level = "warn"
	case humanoutput.ResultFailed:
		level = "error"
	}
	drift := "none"
	if result.SourceDrift {
		drift = "source"
	}
	payload := transferSettledTracePayload{
		SourceOmittedEntries: strconv.FormatUint(result.SourceOmittedEntries, 10),
		ResultStatus:         string(result.Status),
		ExitCode:             exit,
		Drift:                drift,
		ResultElapsedMS:      strconv.FormatInt(max(result.Elapsed.Milliseconds(), 0), 10),
		DestinationAdjusted:  result.DestinationAdjusted,
		FileOutcomes:         fileOutcomesTracePayload(result.Files),
		DirectoryFailures:    strconv.FormatUint(result.DirectoryFailures, 10),
		OmittedDiagnostics:   strconv.FormatUint(result.OmittedDiagnostics, 10),
		PublishedBytes:       strconv.FormatUint(result.PublishedBytes, 10),
		CountersExact:        result.CountersExact,
	}
	if connectivity.DownloadID != "" {
		payload.DownloadConnectivity = buildDownloadConnectivityPayload(connectivity)
	}
	if result.Failure != nil && result.Failure.Valid() {
		val := result.Failure.DiagnosticValue()
		payload.Failure = &val
	}
	progressCopy := finalProgress
	runtime.stageTerminal(&progressCopy, terminalOutcome{
		event:   "transfer_settled",
		level:   level,
		payload: payload,
		renderHuman: func(h *humanoutput.Renderer) {
			h.RenderTransferResult(result)
		},
	})
}

func (runtime *commandRuntime) stageTerminal(finalProgress *engine.TransferProgress, outcome terminalOutcome) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.stagedTerminal != nil {
		return
	}
	runtime.stagedFinalProgress = finalProgress
	runtime.stagedTerminal = &outcome
}

// Close flushes producer diagnostics before recording the staged terminal
// event, seals the trace, reports incomplete tracing at most once, and renders
// the terminal human summary last so it is always the semantic suffix.
func (runtime *commandRuntime) Close() {
	if runtime == nil {
		return
	}
	runtime.closeOnce.Do(func() {
		if runtime.diagnosticStream != nil {
			completion := runtime.diagnosticStream.Close()
			runtime.AddDropped(completion.CapacityDropped)
			<-runtime.diagDone
		}
		runtime.mu.Lock()
		finalProgress := runtime.stagedFinalProgress
		terminal := runtime.stagedTerminal
		runtime.mu.Unlock()

		if finalProgress != nil {
			runtime.human.RenderProgress(*finalProgress)
			runtime.recordSource(*finalProgress)
		}
		if terminal != nil && runtime.trace != nil {
			runtime.trace.RecordRun(diaglog.RunRecord{
				Level:   terminal.level,
				Event:   terminal.event,
				Payload: terminal.payload,
			})
		}
		if runtime.trace != nil {
			status := runtime.trace.CloseRun()
			if !status.Complete() {
				runtime.human.RenderTraceIncomplete(status.WriterFailed)
			}
		}
		if terminal != nil {
			terminal.renderHuman(runtime.human)
			return
		}
		runtime.human.FinishProgress()
	})
}

func (runtime *commandRuntime) recordSource(source diagnostics.Source) {
	if runtime == nil || runtime.trace == nil || source == nil {
		return
	}
	rec := source.DiagnosticRecord()
	if !rec.Valid() {
		return
	}
	entry := diaglog.RunRecord{
		Level:   sourceRecordLevel(rec),
		Event:   rec.Name(),
		Payload: rec.Payload(),
	}
	if corr := rec.Correlation(); corr.Present() {
		entry.Correlation = corr
	}
	runtime.trace.RecordRun(entry)
}

func sourceRecordLevel(rec diagnostics.Record) string {
	switch rec.Name() {
	case "warning", "fallback", "source_omissions":
		return "warn"
	case "relay_recovering":
		if rec.Payload().Get("failure").Present() {
			return "warn"
		}
		return "info"
	case "transfer_progress", "lane_adopted", "engine_task_observed":
		return "debug"
	default:
		return "info"
	}
}

func (runtime *commandRuntime) Clock() commandClock {
	if runtime == nil {
		return nil
	}
	return runtime.clock
}

func (runtime *commandRuntime) HumanOutputError() error {
	if runtime == nil || runtime.canvas == nil {
		return nil
	}
	return runtime.canvas.Err()
}

func (runtime *commandRuntime) detailedDiagnosticsEnabled() bool {
	return runtime != nil && runtime.detailedDiagnostics
}

func (runtime *commandRuntime) traceRecordingEnabled() bool {
	return runtime != nil && runtime.trace != nil
}

func (runtime *commandRuntime) diagnosticSink() diagnostics.Sink {
	if runtime == nil || runtime.diagnosticStream == nil {
		return diagnostics.Sink{}
	}
	return runtime.diagnosticStream.Sink()
}

func classifyFailure(cause error) engine.Failure {
	if f, ok := engine.DescribeFailure(cause); ok {
		return f
	}
	return engine.Failure{Code: engine.FailureUnexpected}
}

func exitCodeForClass(class engine.FailureClass) int {
	switch class {
	case engine.FailureNone:
		return ExitOK
	case engine.FailureUsage:
		return ExitUsage
	case engine.FailureNetwork:
		return ExitNetwork
	case engine.FailureSourceDrift:
		return ExitDrift
	default:
		return ExitFailure
	}
}

type sharingSubjectPayload struct {
	SubjectKind   string `json:"subject_kind"`
	SelectedItems string `json:"selected_items"`
	FileBytes     string `json:"file_bytes,omitempty"`
}

func sharingSubjectTracePayload(subject humanoutput.SharingSubject) sharingSubjectPayload {
	switch subject.Kind {
	case humanoutput.SharingFile:
		return sharingSubjectPayload{
			SubjectKind:   "file",
			SelectedItems: "1",
			FileBytes:     strconv.FormatUint(subject.FileBytes, 10),
		}
	case humanoutput.SharingDirectory:
		return sharingSubjectPayload{
			SubjectKind:   "directory",
			SelectedItems: "1",
		}
	default:
		return sharingSubjectPayload{
			SubjectKind:   "multiple",
			SelectedItems: strconv.FormatUint(subject.SelectedItems, 10),
		}
	}
}

type networkSetupTracePayload struct {
	Decision             string `json:"decision"`
	Executable           string `json:"executable,omitempty"`
	ConfiguredExecutable string `json:"configured_executable,omitempty"`
	Error                string `json:"error,omitempty"`
	SuggestSetup         bool   `json:"suggest_setup"`
}

type commandFailedTracePayload struct {
	ExitCode int               `json:"exit_code"`
	Failure  diagnostics.Value `json:"failure"`
}

type sharingStoppedTracePayload struct {
	Publication     string             `json:"publication"`
	ExitCode        int                `json:"exit_code"`
	ResultElapsedMS string             `json:"result_elapsed_ms"`
	StoppedCleanly  bool               `json:"stopped_cleanly"`
	Failure         *diagnostics.Value `json:"failure,omitempty"`
}

type downloadConnectivityTracePayload struct {
	DownloadID            string   `json:"download_id"`
	FirstDirectElapsedMS  *string  `json:"first_direct_elapsed_ms"`
	DirectBytes           string   `json:"direct_bytes"`
	ApplicationRelayBytes string   `json:"application_relay_bytes"`
	TURNBytes             string   `json:"turn_bytes"`
	UnknownBytes          string   `json:"unknown_bytes"`
	DirectFraction        *float64 `json:"direct_fraction"`
	FallbackStallMS       string   `json:"fallback_stall_ms"`
	Incomplete            bool     `json:"incomplete"`
	Final                 bool     `json:"final"`
}

func buildDownloadConnectivityPayload(m downloadmetrics.Snapshot) *downloadConnectivityTracePayload {
	summary := &downloadConnectivityTracePayload{
		DownloadID:            m.DownloadID,
		DirectBytes:           strconv.FormatUint(m.DirectBytes, 10),
		ApplicationRelayBytes: strconv.FormatUint(m.ApplicationRelayBytes, 10), TURNBytes: strconv.FormatUint(m.TURNBytes, 10),
		UnknownBytes:    strconv.FormatUint(m.UnknownBytes, 10),
		DirectFraction:  m.DirectFraction,
		FallbackStallMS: strconv.FormatInt(max(m.FallbackStall.Milliseconds(), 0), 10),
		Incomplete:      m.Incomplete,
		Final:           m.Final,
	}
	if m.FirstDirectElapsed != nil {
		value := strconv.FormatInt(max(m.FirstDirectElapsed.Milliseconds(), 0), 10)
		summary.FirstDirectElapsedMS = &value
	}
	return summary
}

type transferSettledTracePayload struct {
	SourceOmittedEntries string                            `json:"source_omitted_entries"`
	DownloadConnectivity *downloadConnectivityTracePayload `json:"download_connectivity,omitempty"`
	ResultStatus         string                            `json:"result_status"`
	ExitCode             int                               `json:"exit_code"`
	Drift                string                            `json:"drift"`
	ResultElapsedMS      string                            `json:"result_elapsed_ms"`
	DestinationAdjusted  bool                              `json:"destination_adjusted"`
	FileOutcomes         map[string]string                 `json:"file_outcomes"`
	DirectoryFailures    string                            `json:"directory_failures"`
	OmittedDiagnostics   string                            `json:"omitted_diagnostics"`
	PublishedBytes       string                            `json:"published_bytes"`
	CountersExact        bool                              `json:"counters_exact"`
	Failure              *diagnostics.Value                `json:"failure,omitempty"`
}

func fileOutcomesTracePayload(f transfer.FileOutcomeSummary) map[string]string {
	return map[string]string{
		"downloaded_files":           strconv.FormatUint(f.DownloadedFiles, 10),
		"resumed_files":              strconv.FormatUint(f.ResumedFiles, 10),
		"previously_published_files": strconv.FormatUint(f.PreviouslyPublishedFiles, 10),
		"paused_files":               strconv.FormatUint(f.PausedFiles, 10),
		"collision_files":            strconv.FormatUint(f.CollisionFiles, 10),
		"item_blocked_files":         strconv.FormatUint(f.ItemBlockedFiles, 10),
		"failed_files":               strconv.FormatUint(f.FailedFiles, 10),
		"modified_time_warnings":     strconv.FormatUint(f.ModifiedTimeWarnings, 10),
	}
}

// Presentation and engine adapters draw their timers and timestamps from the
// same command clock so an injected clock can drive a whole command.
type commandClock interface {
	Now() time.Time
	NewTimer(time.Duration) commandTimer
	NewTicker(time.Duration) commandTicker
}

type commandTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type commandTicker interface {
	C() <-chan time.Time
	Stop()
}

type systemCommandClock struct {
	now func() time.Time
}

func newSystemCommandClock(now func() time.Time) systemCommandClock {
	if now == nil {
		now = time.Now
	}
	return systemCommandClock{now: now}
}

func (clock systemCommandClock) Now() time.Time {
	return clock.now()
}

func (systemCommandClock) NewTimer(delay time.Duration) commandTimer {
	return systemCommandTimer{timer: time.NewTimer(delay)}
}

func (systemCommandClock) NewTicker(interval time.Duration) commandTicker {
	return systemCommandTicker{ticker: time.NewTicker(interval)}
}

type systemCommandTimer struct{ timer *time.Timer }

func (timer systemCommandTimer) C() <-chan time.Time { return timer.timer.C }
func (timer systemCommandTimer) Stop() bool          { return timer.timer.Stop() }

type systemCommandTicker struct{ ticker *time.Ticker }

func (ticker systemCommandTicker) C() <-chan time.Time { return ticker.ticker.C }
func (ticker systemCommandTicker) Stop()               { ticker.ticker.Stop() }
