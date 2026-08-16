package humanoutput

import (
	"errors"
	"sync"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/cmd/windshare/internal/terminalcanvas"
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
	receiveOperation       clievent.ReceiveOperationID
	transferJob            clievent.TransferJobID
	hasProgressIdentity    bool
	redirectedDiscovery    clievent.DiscoveryStatus
	hasRedirectedDiscovery bool
	lastProgress           clievent.ProgressSnapshot
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

// Render validates through clievent's sealed visitor boundary before emitting.
// Calls are serialized because rate samples and redirected phase transitions are
// command-scoped state even when producers happen to be concurrent.
func (renderer *Renderer) Render(event clievent.Event) error {
	if renderer == nil || event == nil {
		return clievent.ErrInvalidEvent
	}
	renderer.mu.Lock()
	defer renderer.mu.Unlock()

	capabilities := renderer.capabilities.Snapshot()
	return event.Accept(eventVisitor{renderer: renderer, capabilities: capabilities})
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

func (visitor eventVisitor) VisitReady(clievent.Ready) error {
	if visitor.mainVisible() {
		visitor.renderer.insert(
			terminalcanvas.Plain(""),
			statusLine(visitor.symbols().Ready, "WindShare is ready", terminalcanvas.StyleSuccess),
			terminalcanvas.Plain(""),
		)
	}
	return nil
}

func (visitor eventVisitor) VisitSharingSubjectSelected(event clievent.SharingSubjectSelected) error {
	if visitor.mainVisible() {
		visitor.renderer.insert(formatSharingSubject(event.Subject(), visitor.symbols()))
	}
	return nil
}

func (visitor eventVisitor) VisitRelayConnected(event clievent.RelayConnected) error {
	if event.Command() == clievent.CommandShare && visitor.mainVisible() {
		visitor.renderer.insert(
			statusLine(visitor.symbols().Arrow, "Relay: Connected", terminalcanvas.StyleAccent),
			terminalcanvas.Plain(""),
			statusLine("", "Waiting for receivers"+visitor.symbols().Separator+"Press Ctrl+C to stop", terminalcanvas.StyleMuted),
		)
	} else if event.Command() == clievent.CommandGet && visitor.verboseVisible() {
		visitor.renderer.insert(statusLine(visitor.symbols().Relay, "Relay connected", terminalcanvas.StyleAccent))
	}
	return nil
}

func (visitor eventVisitor) VisitRelayRecovering(event clievent.RelayRecovering) error {
	if event.State() != clievent.RelayRecoveryFailed && !visitor.verboseVisible() {
		return nil
	}
	visitor.renderer.insert(formatRelayRecovery(event, visitor.symbols()))
	return nil
}

func (visitor eventVisitor) VisitContentPathSelected(event clievent.ContentPathSelected) error {
	if visitor.mainVisible() {
		visitor.renderer.insert(formatContentPath(event.Path(), visitor.symbols()))
	}
	return nil
}

func (visitor eventVisitor) VisitFallback(event clievent.Fallback) error {
	visitor.renderer.insert(formatFallback(event, visitor.symbols()))
	return nil
}

func (visitor eventVisitor) VisitTransferProgress(event clievent.TransferProgress) error {
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

func (visitor eventVisitor) VisitWarning(event clievent.Warning) error {
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
		visitor.renderer.hasLastProgress && last.Discovery() == clievent.DiscoveryComplete &&
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

func (visitor eventVisitor) VisitTraceIncomplete(clievent.TraceIncomplete) error {
	visitor.renderer.insert(statusLine(visitor.symbols().Warning, "Warning: Trace is incomplete.", terminalcanvas.StyleWarning))
	return nil
}

func (visitor eventVisitor) VisitLaneAdopted(event clievent.LaneAdopted) error {
	if visitor.verboseVisible() {
		visitor.renderer.insert(formatLaneAdopted(event, visitor.symbols()))
	}
	return nil
}

// The relay/WebRTC lifecycle, transfer internals, filesystem authority,
// terminal settlement, catalog storage, and root-prefetch records are stable trace decisions.
// Rendering them in verbose mode would recreate the internal state-machine
// ledger that this boundary is designed to remove.
func (eventVisitor) VisitRelayLifecycleObserved(clievent.RelayLifecycleObserved) error   { return nil }
func (eventVisitor) VisitWebRTCLifecycleObserved(clievent.WebRTCLifecycleObserved) error { return nil }

func (visitor eventVisitor) VisitPeerAttemptObserved(event clievent.PeerAttemptObserved) error {
	if visitor.verboseVisible() {
		visitor.renderer.insert(formatPeerAttempt(event, visitor.symbols()))
	}
	return nil
}

func (eventVisitor) VisitTransferLifecycleObserved(clievent.TransferLifecycleObserved) error {
	return nil
}
func (eventVisitor) VisitFilesystemOutputObserved(clievent.FilesystemOutputObserved) error {
	return nil
}
func (eventVisitor) VisitSenderTerminalObserved(clievent.SenderTerminalObserved) error { return nil }
func (eventVisitor) VisitCatalogStorageObserved(clievent.CatalogStorageObserved) error { return nil }
func (eventVisitor) VisitRootPrefetchObserved(clievent.RootPrefetchObserved) error     { return nil }
func (visitor eventVisitor) VisitProtocolOperationObserved(event clievent.ProtocolOperationObserved) error {
	if visitor.verboseVisible() && event.Cause() != clievent.ProtocolOperationCauseNone {
		visitor.renderer.insert(formatProtocolOperationFailure(event, visitor.symbols()))
	}
	return nil
}
