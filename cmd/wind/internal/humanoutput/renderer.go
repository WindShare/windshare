package humanoutput

import (
	"errors"
	"sync"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/internal/platformsetup"
)

var ErrInvalidConfig = errors.New("human output configuration is invalid")

// Config keeps presentation policy independent from the Canvas that owns stderr.
// Capabilities are shared with Canvas so visibility, symbols, and width decisions
// describe the same terminal snapshot used for the eventual write.
type Config struct {
	Canvas       *terminalcanvas.Canvas
	Capabilities terminalcanvas.CapabilityProvider
	Clock        terminalcanvas.Clock
	CellWidth    terminalcanvas.CellWidthFunc
	Verbose      bool
}

// Renderer serializes command events into semantic terminal lines. Writer
// failures remain observable only through Canvas.Err and never become command
// failures.
type Renderer struct {
	mu           sync.Mutex
	canvas       *terminalcanvas.Canvas
	capabilities terminalcanvas.CapabilityProvider
	clock        terminalcanvas.Clock
	cellWidth    terminalcanvas.CellWidthFunc
	verbose      bool

	rate                   rateWindow
	receiveOperation       receivecontract.OperationID
	transferJob            transfer.TransferJobID
	hasProgressIdentity    bool
	redirectedDiscovery    transfer.DiscoveryStatus
	hasRedirectedDiscovery bool
	lastProgress           transfer.ReceiveProgressSnapshot
	hasLastProgress        bool
}

func New(config Config) (*Renderer, error) {
	if config.Canvas == nil || config.Capabilities == nil || config.Clock == nil {
		return nil, ErrInvalidConfig
	}
	return &Renderer{
		canvas: config.Canvas, capabilities: config.Capabilities,
		clock: config.Clock, cellWidth: config.CellWidth, verbose: config.Verbose,
	}, nil
}

func (renderer *Renderer) insert(lines ...terminalcanvas.Line) {
	renderer.canvas.Insert(lines)
}

func (renderer *Renderer) finish(lines ...terminalcanvas.Line) {
	renderer.canvas.FinishProgress()
	renderer.insert(lines...)
	renderer.rate.reset()
	renderer.hasProgressIdentity = false
	renderer.hasRedirectedDiscovery = false
	renderer.hasLastProgress = false
}

func (renderer *Renderer) mainVisible(caps terminalcanvas.Capabilities) bool {
	return caps.Interactive || renderer.verbose
}

func (renderer *Renderer) RenderReady() {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	caps := renderer.capabilities.Snapshot()
	if !renderer.mainVisible(caps) {
		return
	}
	symbols := SelectSymbols(caps.Unicode)
	renderer.insert(
		terminalcanvas.Plain(""),
		statusLine(symbols.Ready, "WindShare is ready", terminalcanvas.StyleSuccess),
		terminalcanvas.Plain(""),
	)
}

func (renderer *Renderer) RenderSharingSubject(subject SharingSubject) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	caps := renderer.capabilities.Snapshot()
	if !renderer.mainVisible(caps) {
		return
	}
	renderer.insert(formatSharingSubject(subject, SelectSymbols(caps.Unicode)))
}

func (renderer *Renderer) RenderRelayConnected(isShare bool) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	caps := renderer.capabilities.Snapshot()
	symbols := SelectSymbols(caps.Unicode)
	if isShare && renderer.mainVisible(caps) {
		renderer.insert(
			statusLine(symbols.Arrow, "Relay: Connected", terminalcanvas.StyleAccent),
			terminalcanvas.Plain(""),
			statusLine("", "Waiting for receivers"+symbols.Separator+"Press Ctrl+C to stop", terminalcanvas.StyleMuted),
		)
	} else if !isShare && renderer.verbose {
		renderer.insert(statusLine(symbols.Relay, "Relay connected", terminalcanvas.StyleAccent))
	}
}

func (renderer *Renderer) RenderRelayRecovery(event engine.RelayRecovery) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	if !renderer.verbose {
		if event.State == engine.RelayRecoveryStarted {
			return
		}
		hasDetails := event.Generation > 0 || event.Slow || event.Resume || event.NextDelay > 0
		if event.State == engine.RelayRecoveryFailed && hasDetails && !event.Terminal &&
			event.WaitReason == engine.RelayWaitConnection && event.Authorization == 0 &&
			(event.Failure == nil || event.Failure.Code != engine.FailureRelayStopped) {
			return
		}
	}
	caps := renderer.capabilities.Snapshot()
	renderer.insert(formatRelayRecovery(event, SelectSymbols(caps.Unicode)))
}

func (renderer *Renderer) RenderRelayAvailability(event engine.RelayAvailability) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	caps := renderer.capabilities.Snapshot()
	renderer.insert(formatRelayAvailability(event, SelectSymbols(caps.Unicode)))
}

func (renderer *Renderer) RenderContentPath(path engine.ContentPath) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	caps := renderer.capabilities.Snapshot()
	if !renderer.mainVisible(caps) {
		return
	}
	renderer.insert(formatContentPath(path, SelectSymbols(caps.Unicode)))
}

func (renderer *Renderer) RenderFallback(event engine.Fallback) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	caps := renderer.capabilities.Snapshot()
	renderer.insert(formatFallback(event, SelectSymbols(caps.Unicode)))
}

func (renderer *Renderer) RenderProgress(event engine.TransferProgress) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	caps := renderer.capabilities.Snapshot()
	snapshot := event.Snapshot
	renderer.lastProgress = snapshot
	renderer.hasLastProgress = true
	if !renderer.hasProgressIdentity || renderer.receiveOperation != event.Operation ||
		renderer.transferJob != event.Job {
		renderer.rate.reset()
		renderer.receiveOperation = event.Operation
		renderer.transferJob = event.Job
		renderer.hasProgressIdentity = true
		renderer.hasRedirectedDiscovery = false
	}

	if !caps.Interactive {
		if !renderer.verbose || renderer.hasRedirectedDiscovery && renderer.redirectedDiscovery == snapshot.Discovery {
			return
		}
		renderer.redirectedDiscovery = snapshot.Discovery
		renderer.hasRedirectedDiscovery = true
		renderer.insert(formatDiscoveryMilestone(snapshot, SelectSymbols(caps.Unicode)))
		return
	}

	estimate := renderer.rate.observe(renderer.clock.Now(), snapshot)
	renderer.canvas.ReplaceProgress(FormatProgress(snapshot, event.CapacityWaitVisible, ProgressMetrics{
		RateBytesPerSecond: estimate.bytesPerSecond,
		RateValid:          estimate.valid,
		RateStable:         estimate.stable,
	}, ProgressLayout{
		Columns:   caps.Columns,
		Unicode:   caps.Unicode,
		CellWidth: renderer.cellWidth,
	}))
}

func (renderer *Renderer) RenderWarning(failure engine.Failure) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	caps := renderer.capabilities.Snapshot()
	renderer.insert(statusLine(SelectSymbols(caps.Unicode).Warning, "Warning: "+failureReason(failure), terminalcanvas.StyleWarning))
}

func (renderer *Renderer) RenderSourceOmissions(summary catalog.SourceOmissions) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	caps := renderer.capabilities.Snapshot()
	renderer.insert(formatSourceOmissions(summary, SelectSymbols(caps.Unicode))...)
}

func (renderer *Renderer) RenderLaneAdopted(event engine.LaneAdopted) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	if !renderer.verbose {
		return
	}
	caps := renderer.capabilities.Snapshot()
	renderer.insert(formatLaneAdopted(event, SelectSymbols(caps.Unicode)))
}

func (renderer *Renderer) RenderDiagnostic(record diagnostics.Record) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	if !renderer.verbose {
		return
	}
	symbols := SelectSymbols(renderer.capabilities.Snapshot().Unicode)
	switch record.Name() {
	case "peer_attempt":
		renderer.insert(formatPeerAttempt(record, symbols))
	case "protocol_operation":
		if record.Payload().Get("stage").Text() == "receiver_failed" {
			renderer.insert(formatProtocolOperationFailure(record, symbols))
		}
	}
}

func (renderer *Renderer) RenderNetworkSetup(report platformsetup.StartupReport) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	if report.Decision.SuggestSetup() {
		renderer.insert(terminalcanvas.Plain("Network setup: wind setup network; dismiss: wind setup network --skip"))
	} else if renderer.verbose {
		renderer.insert(terminalcanvas.Plain("Network setup: " + report.Decision.String()))
	}
}

// RenderTracePath reports only local human state. The generated path is escaped
// before layout and is never modeled as a trace event, avoiding self-disclosure
// in the NDJSON artifact.
func (renderer *Renderer) RenderTracePath(path string) {
	if renderer == nil || path == "" {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	renderer.insert(terminalcanvas.Plain("Trace: " + escapedDisplay(path)))
}

func (renderer *Renderer) RenderTraceIncomplete(writerFailed bool) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	reason := " Diagnostic records were dropped."
	if writerFailed {
		reason = " Writing the trace failed."
	}
	symbols := SelectSymbols(renderer.capabilities.Snapshot().Unicode)
	renderer.insert(statusLine(symbols.Warning, "Warning: Trace is incomplete."+reason, terminalcanvas.StyleWarning))
}

func (renderer *Renderer) RenderTraceOpenFailure(exists bool) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	message := "The trace is incomplete."
	if exists {
		message = "The trace path already exists; prior evidence was preserved and command/output state was untouched. Choose a new --trace path, use --trace-dir, or omit tracing."
	}
	symbols := SelectSymbols(renderer.capabilities.Snapshot().Unicode)
	renderer.finish(statusLine(symbols.Failure, "Error: "+message, terminalcanvas.StyleError))
}

func (renderer *Renderer) RenderCommandFailed(isShare bool, failure engine.Failure) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	message := failureMessage(failure)
	if isShare && (failure.Code == engine.FailureNetworkUnavailable || failure.Code == engine.FailureAccountStorageError) {
		message += " Alternatively, " + ConfigureRelaysGuidance + "."
	}
	symbols := SelectSymbols(renderer.capabilities.Snapshot().Unicode)
	renderer.finish(statusLine(symbols.Failure, "Error: "+message, terminalcanvas.StyleError))
}

func (renderer *Renderer) RenderTransferResult(result TransferResult) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	caps := renderer.capabilities.Snapshot()
	last := renderer.lastProgress
	if caps.Interactive && result.Status == ResultSuccess &&
		renderer.hasLastProgress && last.Discovery == transfer.DiscoveryComplete &&
		last.CountersExact && last.DiscoveredBytes == 0 &&
		terminalFileOutcomes(last.FileOutcomes) >= last.DiscoveredFiles {
		renderer.canvas.ReplaceProgress(FormatProgress(
			last,
			false,
			ProgressMetrics{SuccessfulSettlement: true},
			ProgressLayout{
				Columns:   caps.Columns,
				Unicode:   caps.Unicode,
				CellWidth: renderer.cellWidth,
			},
		))
	}
	renderer.finish(formatTransferResult(result, SelectSymbols(caps.Unicode))...)
}

func (renderer *Renderer) RenderShareResult(result ShareResult) {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	caps := renderer.capabilities.Snapshot()
	renderer.finish(formatShareResult(result, SelectSymbols(caps.Unicode)))
}

func (renderer *Renderer) FinishProgress() {
	if renderer == nil {
		return
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	renderer.finish()
}
