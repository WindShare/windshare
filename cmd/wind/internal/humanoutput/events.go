package humanoutput

import (
	"strconv"
	"strings"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	appobs "github.com/windshare/windshare/engine/observation"
)

func formatSharingSubject(subject appobs.SharingSubject, symbols Symbols) terminalcanvas.Line {
	var description string
	switch subject.Kind() {
	case appobs.SharingFile:
		description = escapedDisplay(subject.Name().Text()) + " (file, " + FormatBytes(subject.FileBytes()) + ")"
	case appobs.SharingDirectory:
		name := escapedDisplay(subject.Name().Text())
		if !strings.HasSuffix(name, "/") && !strings.HasSuffix(name, `\`) {
			name += "/"
		}
		description = name + " (directory)"
	case appobs.SharingMultiple:
		description = formatCount(subject.SelectedItems()) + " selected items"
	}
	return statusLine(symbols.Arrow, "Sharing: "+description, terminalcanvas.StyleAccent)
}

func formatContentPath(path appobs.ContentPath, symbols Symbols) terminalcanvas.Line {
	name := titleName(path)
	if path == appobs.ContentPathDirectAndRelay {
		name = "Direct + Relay"
	}
	return statusLine(symbols.Path, "Content path: "+name, terminalcanvas.StyleAccent)
}

func formatFallback(event appobs.Fallback, symbols Symbols) terminalcanvas.Line {
	message := transportName(event.From()) + " path unavailable; using " + transportName(event.To()) + "."
	return statusLine(symbols.Warning, "Warning: "+message, terminalcanvas.StyleWarning)
}

func formatRelayRecovery(event appobs.RelayRecovering, symbols Symbols) terminalcanvas.Line {
	state := eventName(event.State())
	message := "Relay recovery attempt " + strconv.FormatUint(uint64(event.Attempt()), 10) + " " + state + "."
	if details, ok := event.Details(); ok {
		relay := event.Origin().Host() + ":" + strconv.FormatUint(uint64(event.Origin().Port()), 10)
		switch event.State() {
		case appobs.RelayRecoverySucceeded:
			message = "Relay connection restored: " + relay + "."
			if details.Generation == 1 && !details.Resume {
				message = "Relay connected: " + relay + "."
			}
		case appobs.RelayRecoveryWaiting:
			message = "Waiting for relay " + relay + "; retrying automatically."
		case appobs.RelayRecoveryFailed:
			if details.Terminal {
				message = "Relay recovery needs attention: " + relay + "."
			}
		}
	}
	style, symbol := terminalcanvas.StyleDefault, symbols.Relay
	if failure, ok := event.Failure(); ok {
		message += " " + failureMessage(failure)
		style, symbol = terminalcanvas.StyleWarning, symbols.Warning
	}
	return statusLine(symbol, message, style)
}

func formatRelayAvailability(event appobs.RelayAvailability, symbols Symbols) terminalcanvas.Line {
	if event.Available() > 0 {
		return statusLine(symbols.Relay, "New receivers can join through "+strconv.FormatUint(uint64(event.Available()), 10)+" of "+strconv.FormatUint(uint64(event.Total()), 10)+" relays.", terminalcanvas.StyleAccent)
	}
	message := "Waiting for a relay before publishing the link; press Ctrl-C to stop."
	if event.EverReady() {
		message = "New receivers are waiting for relay recovery. Healthy direct transfers can continue; the same link will recover automatically."
	}
	if event.Terminal() == event.Total() {
		message = "New receivers cannot join: all relays need attention. Healthy direct transfers can continue."
	}
	return statusLine(symbols.Warning, message, terminalcanvas.StyleWarning)
}

func formatLaneAdopted(event appobs.LaneAdopted, symbols Symbols) terminalcanvas.Line {
	lane := event.Lane()
	message := "Lane " + strconv.FormatUint(uint64(lane.ID()), 10) +
		" epoch " + strconv.FormatUint(uint64(lane.Epoch()), 10) +
		" adopted (" + transportName(event.Transport()) + ")."
	return statusLine(symbols.Path, message, terminalcanvas.StyleMuted)
}

func transportName(transport appobs.Transport) string {
	if transport == appobs.TransportWebRTC {
		return "WebRTC"
	}
	return "Relay"
}

func formatDiscoveryMilestone(snapshot appobs.ProgressSnapshot, symbols Symbols) terminalcanvas.Line {
	switch snapshot.Discovery() {
	case appobs.DiscoveryComplete:
		return statusLine(symbols.Discovery,
			"Discovery complete: "+formatFiles(snapshot.DiscoveredFiles())+symbols.Separator+FormatBytes(snapshot.DiscoveredBytes()),
			terminalcanvas.StyleDefault)
	case appobs.DiscoveryFailed:
		return statusLine(symbols.Failure, "Discovery failed.", terminalcanvas.StyleError)
	default:
		return statusLine(symbols.Discovery, "Discovering content"+symbols.Ellipsis, terminalcanvas.StyleDefault)
	}
}
