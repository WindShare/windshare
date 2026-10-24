package humanoutput

import (
	"errors"
	"sync"

	fsobs "github.com/windshare/windshare/adapters/filesystem/diagnostic"
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"

	appobs "github.com/windshare/windshare/engine/observation"
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
// failures through Render.
type Renderer struct {
	mu           sync.Mutex
	canvas       *terminalcanvas.Canvas
	capabilities terminalcanvas.CapabilityProvider
	clock        terminalcanvas.Clock
	cellWidth    terminalcanvas.CellWidthFunc
	verbose      bool

	rate                   rateWindow
	receiveOperation       appobs.ReceiveOperationID
	transferJob            appobs.TransferJobID
	hasProgressIdentity    bool
	redirectedDiscovery    appobs.DiscoveryStatus
	hasRedirectedDiscovery bool
	lastProgress           appobs.ProgressSnapshot
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

// Render consumes immutable application facts and command presentation results.
// Calls are serialized because rate samples and redirected phase transitions are
// command-scoped state even when producers happen to be concurrent.
func (renderer *Renderer) Render(event clievent.Event) error {
	if renderer == nil || event == nil {
		return clievent.ErrInvalidEvent
	}
	if _, terminal := event.(clievent.TerminalEvent); terminal {
		return clievent.ErrInvalidEvent
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()

	capabilities := renderer.capabilities.Snapshot()
	return renderEvent(event, eventVisitor{renderer: renderer, capabilities: capabilities})
}

// RenderTerminal is intentionally separate from Render: a running command can
// drain ordinary work without accidentally presenting authority before trace
// sealing has made diagnostic completeness final.
func (renderer *Renderer) RenderTerminal(event clievent.TerminalEvent) error {
	if renderer == nil || event == nil {
		return clievent.ErrInvalidEvent
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	return renderEvent(event, eventVisitor{
		renderer:     renderer,
		capabilities: renderer.capabilities.Snapshot(),
	})
}

// RenderTracePath reports only local human state. The generated path is escaped
// before layout and is never modeled as a trace event, avoiding self-disclosure
// in the NDJSON artifact.
func (renderer *Renderer) RenderTracePath(path string) error {
	if renderer == nil || path == "" {
		return clievent.ErrInvalidEvent
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	renderer.insert(terminalcanvas.Plain("Trace: " + escapedDisplay(path)))
	return nil
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

type eventVisitor struct {
	renderer     *Renderer
	capabilities terminalcanvas.Capabilities
}

func (visitor eventVisitor) mainVisible() bool {
	return visitor.capabilities.Interactive || visitor.renderer.verbose
}

func (visitor eventVisitor) verboseVisible() bool {
	return visitor.renderer.verbose
}

func (visitor eventVisitor) symbols() Symbols {
	return SelectSymbols(visitor.capabilities.Unicode)
}

func (visitor eventVisitor) VisitReady(appobs.Ready) error {
	if visitor.mainVisible() {
		visitor.renderer.insert(
			terminalcanvas.Plain(""),
			statusLine(visitor.symbols().Ready, "WindShare is ready", terminalcanvas.StyleSuccess),
			terminalcanvas.Plain(""),
		)
	}
	return nil
}

func (visitor eventVisitor) VisitSharingSubjectSelected(event appobs.SharingSubjectSelected) error {
	if visitor.mainVisible() {
		visitor.renderer.insert(formatSharingSubject(event.Subject(), visitor.symbols()))
	}
	return nil
}

func (visitor eventVisitor) VisitRelayConnected(event appobs.RelayConnected) error {
	if clievent.CommandOf(event) == clievent.CommandShare && visitor.mainVisible() {
		visitor.renderer.insert(
			statusLine(visitor.symbols().Arrow, "Relay: Connected", terminalcanvas.StyleAccent),
			terminalcanvas.Plain(""),
			statusLine("", "Waiting for receivers"+visitor.symbols().Separator+"Press Ctrl+C to stop", terminalcanvas.StyleMuted),
		)
	} else if clievent.CommandOf(event) == clievent.CommandGet && visitor.verboseVisible() {
		visitor.renderer.insert(statusLine(visitor.symbols().Relay, "Relay connected", terminalcanvas.StyleAccent))
	}
	return nil
}

func (visitor eventVisitor) VisitRelayRecovering(event appobs.RelayRecovering) error {
	if !visitor.verboseVisible() {
		details, detailed := event.Details()
		if event.State() == appobs.RelayRecoveryStarted || (event.State() == appobs.RelayRecoveryFailed && detailed && !details.Terminal) {
			return nil
		}
	}
	visitor.renderer.insert(formatRelayRecovery(event, visitor.symbols()))
	return nil
}

func (visitor eventVisitor) VisitRelayAvailability(event appobs.RelayAvailability) error {
	visitor.renderer.insert(formatRelayAvailability(event, visitor.symbols()))
	return nil
}

func (visitor eventVisitor) VisitContentPathSelected(event appobs.ContentPathSelected) error {
	if visitor.mainVisible() {
		visitor.renderer.insert(formatContentPath(event.Path(), visitor.symbols()))
	}
	return nil
}

func (visitor eventVisitor) VisitFallback(event appobs.Fallback) error {
	visitor.renderer.insert(formatFallback(event, visitor.symbols()))
	return nil
}

func (visitor eventVisitor) VisitTransferProgress(event appobs.TransferProgress) error {
	renderer := visitor.renderer
	snapshot := event.Snapshot()
	renderer.lastProgress = snapshot
	renderer.hasLastProgress = true
	if !renderer.hasProgressIdentity || renderer.receiveOperation != event.ReceiveOperationID() ||
		renderer.transferJob != event.TransferJobID() {
		renderer.rate.reset()
		renderer.receiveOperation = event.ReceiveOperationID()
		renderer.transferJob = event.TransferJobID()
		renderer.hasProgressIdentity = true
		renderer.hasRedirectedDiscovery = false
	}

	if !visitor.capabilities.Interactive {
		if !renderer.verbose || renderer.hasRedirectedDiscovery && renderer.redirectedDiscovery == snapshot.Discovery() {
			return nil
		}
		renderer.redirectedDiscovery = snapshot.Discovery()
		renderer.hasRedirectedDiscovery = true
		renderer.insert(formatDiscoveryMilestone(snapshot, visitor.symbols()))
		return nil
	}

	estimate := renderer.rate.observe(renderer.clock.Now(), snapshot)
	renderer.canvas.ReplaceProgress(FormatProgress(snapshot, ProgressMetrics{
		RateBytesPerSecond: estimate.bytesPerSecond,
		RateValid:          estimate.valid,
		RateStable:         estimate.stable,
	}, ProgressLayout{
		Columns:   visitor.capabilities.Columns,
		Unicode:   visitor.capabilities.Unicode,
		CellWidth: renderer.cellWidth,
	}))
	return nil
}

func (visitor eventVisitor) VisitWarning(event appobs.Warning) error {
	visitor.renderer.insert(failureLine("Warning", event.Failure(), visitor.symbols().Warning, terminalcanvas.StyleWarning))
	return nil
}

func (visitor eventVisitor) VisitCommandFailed(event clievent.CommandFailed) error {
	visitor.renderer.finish(failureLine("Error", event.Failure(), visitor.symbols().Failure, terminalcanvas.StyleError))
	return nil
}

func (visitor eventVisitor) VisitTransferSettled(event clievent.TransferSettled) error {
	last := visitor.renderer.lastProgress
	if visitor.capabilities.Interactive && event.Result().Status() == clievent.ResultSuccess &&
		visitor.renderer.hasLastProgress && last.Discovery() == appobs.DiscoveryComplete &&
		last.CountersExact() && last.DiscoveredBytes() == 0 &&
		terminalFileOutcomes(last.FileOutcomes()) >= last.DiscoveredFiles() {
		visitor.renderer.canvas.ReplaceProgress(FormatProgress(
			last,
			ProgressMetrics{SuccessfulSettlement: true},
			ProgressLayout{
				Columns:   visitor.capabilities.Columns,
				Unicode:   visitor.capabilities.Unicode,
				CellWidth: visitor.renderer.cellWidth,
			},
		))
	}
	visitor.renderer.finish(formatTransferResult(event.Result(), visitor.symbols())...)
	return nil
}

func (visitor eventVisitor) VisitSharingStopped(event clievent.SharingStopped) error {
	visitor.renderer.finish(formatShareResult(event.Result(), visitor.symbols()))
	return nil
}

func (visitor eventVisitor) VisitTraceIncomplete(event clievent.TraceIncomplete) error {
	var reason string
	switch event.Cause() {
	case clievent.TraceIncompleteContractRejection:
		reason = " Diagnostic events were rejected."
	case clievent.TraceIncompleteLifecycleDrop:
		reason = " Diagnostic records were dropped."
	case clievent.TraceIncompleteWriter:
		reason = " Writing the trace failed."
	case clievent.TraceIncompleteFlush:
		reason = " Flushing the trace failed."
	case clievent.TraceIncompleteSchemaLimit:
		reason = " Trace recording encountered an internal error."
	case clievent.TraceIncompleteRejectionEvidence:
		reason = " Event rejection details were lost."
	}
	visitor.renderer.insert(statusLine(visitor.symbols().Warning, "Warning: Trace is incomplete."+reason, terminalcanvas.StyleWarning))
	return nil
}

func (visitor eventVisitor) VisitLaneAdopted(event appobs.LaneAdopted) error {
	if visitor.verboseVisible() {
		visitor.renderer.insert(formatLaneAdopted(event, visitor.symbols()))
	}
	return nil
}

// The relay/WebRTC lifecycle, transfer internals, filesystem authority,
// terminal root/send settlement, catalog storage, and root-prefetch records are stable trace decisions.
// Rendering them in verbose mode would recreate the internal state-machine
// ledger that this boundary is designed to remove.
func (eventVisitor) VisitRelayLifecycleObserved(appobs.RelayLifecycleObserved) error   { return nil }
func (eventVisitor) VisitWebRTCLifecycleObserved(appobs.WebRTCLifecycleObserved) error { return nil }

func (visitor eventVisitor) VisitPeerAttemptObserved(event appobs.PeerAttemptObserved) error {
	if visitor.verboseVisible() {
		visitor.renderer.insert(formatPeerAttempt(event, visitor.symbols()))
	}
	return nil
}

func (eventVisitor) VisitTransferLifecycleObserved(appobs.TransferLifecycleObserved) error {
	return nil
}
func (eventVisitor) VisitFilesystemOutputObserved(fsobs.FilesystemOutputObserved) error {
	return nil
}
func (eventVisitor) VisitSenderTerminalSendObserved(appobs.SenderTerminalSendObserved) error {
	return nil
}
func (eventVisitor) VisitSenderSessionTerminated(appobs.SenderSessionTerminated) error {
	return nil
}
func (eventVisitor) VisitCatalogStorageObserved(appobs.CatalogStorageObserved) error { return nil }
func (eventVisitor) VisitRootPrefetchObserved(appobs.RootPrefetchObserved) error     { return nil }
func (eventVisitor) VisitSenderCapacityObserved(appobs.SenderCapacityObserved) error { return nil }
func (eventVisitor) VisitSenderRevisionObserved(appobs.SenderRevisionObserved) error { return nil }
func (visitor eventVisitor) VisitProtocolObservationObserved(event appobs.ProtocolObservationObserved) error {
	if fact, ok := event.Fact().(appobs.ProtocolOperationFact); ok && visitor.verboseVisible() && fact.Stage() == appobs.ProtocolOperationReceiverFailed {
		visitor.renderer.insert(formatProtocolOperationFailure(event, fact, visitor.symbols()))
	}
	return nil
}

func (eventVisitor) VisitEngineTaskObserved(appobs.EngineTaskObserved) error { return nil }

func (eventVisitor) VisitNativeConnectivityObserved(appobs.NativeConnectivityObserved) error {
	return nil
}

func (eventVisitor) VisitLaneSettlementObserved(appobs.LaneSettlementObserved) error { return nil }
func (eventVisitor) VisitObserverLossObserved(appobs.ObserverLossObserved) error     { return nil }

func (visitor eventVisitor) VisitPlatformSetupObserved(event clievent.PlatformSetupObserved) error {
	if visitor.verboseVisible() {
		visitor.renderer.insert(terminalcanvas.Plain("Firewall setup: " + event.State() + " (" + event.Reason() + ")"))
	}
	return nil
}
func (eventVisitor) VisitReceiverTerminationObserved(appobs.ReceiverTerminationObserved) error {
	return nil
}

func renderEvent(event clievent.Event, visitor eventVisitor) error {
	if err := event.Validate(); err != nil {
		return err
	}
	switch value := event.(type) {
	case appobs.Ready:
		return visitor.VisitReady(value)
	case appobs.SharingSubjectSelected:
		return visitor.VisitSharingSubjectSelected(value)
	case appobs.RelayConnected:
		return visitor.VisitRelayConnected(value)
	case appobs.RelayRecovering:
		return visitor.VisitRelayRecovering(value)
	case appobs.RelayAvailability:
		return visitor.VisitRelayAvailability(value)
	case appobs.ContentPathSelected:
		return visitor.VisitContentPathSelected(value)
	case appobs.Fallback:
		return visitor.VisitFallback(value)
	case appobs.TransferProgress:
		return visitor.VisitTransferProgress(value)
	case appobs.Warning:
		return visitor.VisitWarning(value)
	case clievent.CommandFailed:
		return visitor.VisitCommandFailed(value)
	case clievent.TransferSettled:
		return visitor.VisitTransferSettled(value)
	case clievent.SharingStopped:
		return visitor.VisitSharingStopped(value)
	case clievent.TraceIncomplete:
		return visitor.VisitTraceIncomplete(value)
	case appobs.LaneAdopted:
		return visitor.VisitLaneAdopted(value)
	case appobs.RelayLifecycleObserved:
		return visitor.VisitRelayLifecycleObserved(value)
	case appobs.WebRTCLifecycleObserved:
		return visitor.VisitWebRTCLifecycleObserved(value)
	case appobs.PeerAttemptObserved:
		return visitor.VisitPeerAttemptObserved(value)
	case appobs.TransferLifecycleObserved:
		return visitor.VisitTransferLifecycleObserved(value)
	case fsobs.FilesystemOutputObserved:
		return visitor.VisitFilesystemOutputObserved(value)
	case appobs.SenderTerminalSendObserved:
		return visitor.VisitSenderTerminalSendObserved(value)
	case appobs.SenderSessionTerminated:
		return visitor.VisitSenderSessionTerminated(value)
	case appobs.CatalogStorageObserved:
		return visitor.VisitCatalogStorageObserved(value)
	case appobs.RootPrefetchObserved:
		return visitor.VisitRootPrefetchObserved(value)
	case appobs.SenderCapacityObserved:
		return visitor.VisitSenderCapacityObserved(value)
	case appobs.SenderRevisionObserved:
		return visitor.VisitSenderRevisionObserved(value)
	case appobs.ProtocolObservationObserved:
		return visitor.VisitProtocolObservationObserved(value)
	case appobs.EngineTaskObserved:
		return visitor.VisitEngineTaskObserved(value)
	case appobs.NativeConnectivityObserved:
		return visitor.VisitNativeConnectivityObserved(value)
	case appobs.LaneSettlementObserved:
		return visitor.VisitLaneSettlementObserved(value)
	case appobs.ObserverLossObserved:
		return visitor.VisitObserverLossObserved(value)
	case clievent.PlatformSetupObserved:
		return visitor.VisitPlatformSetupObserved(value)
	case appobs.ReceiverTerminationObserved:
		return visitor.VisitReceiverTerminationObserved(value)
	default:
		return nil
	}
}
