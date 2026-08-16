package humanoutput

import (
	"strconv"
	"strings"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/cmd/windshare/internal/terminalcanvas"
)

func formatSharingSubject(subject clievent.SharingSubject, symbols Symbols) terminalcanvas.Line {
	var description string
	switch subject.Kind() {
	case clievent.SharingFile:
		description = escapedDisplay(subject.Name().Text()) + " (file, " + FormatBytes(subject.FileBytes()) + ")"
	case clievent.SharingDirectory:
		name := escapedDisplay(subject.Name().Text())
		if !strings.HasSuffix(name, "/") && !strings.HasSuffix(name, `\`) {
			name += "/"
		}
		description = name + " (directory)"
	case clievent.SharingMultiple:
		description = formatCount(subject.SelectedItems()) + " selected items"
	}
	return statusLine(symbols.Arrow, "Sharing: "+description, terminalcanvas.StyleAccent)
}

func formatContentPath(path clievent.ContentPath, symbols Symbols) terminalcanvas.Line {
	name := titleName(path)
	if path == clievent.ContentPathDirectAndRelay {
		name = "Direct + Relay"
	}
	return statusLine(symbols.Path, "Content path: "+name, terminalcanvas.StyleAccent)
}

func formatFallback(event clievent.Fallback, symbols Symbols) terminalcanvas.Line {
	message := transportPathName(event.From()) + " path unavailable; using " + transportPathName(event.To()) + "."
	return statusLine(symbols.Warning, "Warning: "+message, terminalcanvas.StyleWarning)
}

func formatRelayRecovery(event clievent.RelayRecovering, symbols Symbols) terminalcanvas.Line {
	state := eventName(event.State())
	message := "Relay recovery attempt " + strconv.FormatUint(uint64(event.Attempt()), 10) + " " + state + "."
	style, symbol := terminalcanvas.StyleDefault, symbols.Relay
	if failure, ok := event.Failure(); ok {
		message += " " + failureMessage(failure)
		style, symbol = terminalcanvas.StyleWarning, symbols.Warning
	}
	return statusLine(symbol, message, style)
}

func formatLaneAdopted(event clievent.LaneAdopted, symbols Symbols) terminalcanvas.Line {
	lane := event.Lane()
	message := "Lane " + strconv.FormatUint(uint64(lane.ID()), 10) +
		" epoch " + strconv.FormatUint(uint64(lane.Epoch()), 10) +
		" adopted (" + transportName(event.Transport()) + ")."
	return statusLine(symbols.Path, message, terminalcanvas.StyleMuted)
}

func transportPathName(transport clievent.Transport) string {
	if transport == clievent.TransportWebRTC {
		return "Direct"
	}
	return "Relay"
}

func transportName(transport clievent.Transport) string {
	if transport == clievent.TransportWebRTC {
		return "WebRTC"
	}
	return "Relay"
}

func formatPeerAttempt(event clievent.PeerAttemptObserved, symbols Symbols) terminalcanvas.Line {
	message := "Direct connection attempt " + strconv.FormatUint(event.Sequence(), 10) +
		": " + eventName(event.Stage()) + "."
	style, symbol := terminalcanvas.StyleMuted, symbols.Path
	if _, failure, ok := event.Failure(); ok {
		message += " " + failureMessage(failure)
		style, symbol = terminalcanvas.StyleWarning, symbols.Warning
	}
	return statusLine(symbol, message, style)
}

func formatProtocolOperationFailure(
	event clievent.ProtocolOperationObserved,
	symbols Symbols,
) terminalcanvas.Line {
	operation := strings.ReplaceAll(eventName(event.RequestKind()), "_", " ")
	message := "Protocol operation " + operation + " failed"
	if event.Stage() == clievent.ProtocolOperationSenderResponseSettled {
		message = "Protocol response for " + operation + " failed"
	}
	if elapsed := event.OperationElapsedMillis(); elapsed != 0 {
		message += " after " + FormatElapsed(time.Duration(elapsed)*time.Millisecond)
	}
	if lane, ok := event.Lane(); ok {
		message += " on lane " + strconv.FormatUint(uint64(lane.ID()), 10) +
			" epoch " + strconv.FormatUint(uint64(lane.Epoch()), 10)
	}
	cause := strings.ReplaceAll(eventName(event.Cause()), "_", " ")
	message += " (" + cause + ")."
	return statusLine(symbols.Warning, message, terminalcanvas.StyleWarning)
}

func formatDiscoveryMilestone(snapshot clievent.ProgressSnapshot, symbols Symbols) terminalcanvas.Line {
	switch snapshot.Discovery() {
	case clievent.DiscoveryComplete:
		return statusLine(symbols.Discovery,
			"Discovery complete: "+formatFiles(snapshot.DiscoveredFiles())+symbols.Separator+FormatBytes(snapshot.DiscoveredBytes()),
			terminalcanvas.StyleDefault)
	case clievent.DiscoveryFailed:
		return statusLine(symbols.Failure, "Discovery failed.", terminalcanvas.StyleError)
	default:
		return statusLine(symbols.Discovery, "Discovering content"+symbols.Ellipsis, terminalcanvas.StyleDefault)
	}
}
