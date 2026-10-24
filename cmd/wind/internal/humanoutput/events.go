package humanoutput

import (
	"encoding/base64"
	"strconv"
	"strings"
	"time"

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
		relay := event.Authority().Host() + ":" + strconv.FormatUint(uint64(event.Authority().Port()), 10)
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

func formatPeerAttempt(event appobs.PeerAttemptObserved, symbols Symbols) terminalcanvas.Line {
	// The event sequence advances within an attempt. Its immutable identity keeps
	// interleaved attempts distinguishable and matches peer_attempt_id in the trace.
	identity := base64.RawURLEncoding.EncodeToString(event.PeerAttemptID().Bytes())
	status, warning := peerAttemptStatus(event)
	name := "Connection"
	if route, known := event.Route(); known {
		switch route {
		case appobs.LaneRouteDirect:
			name = "Direct connection"
		case appobs.LaneRouteTURN:
			name = "TURN relay connection"
		}
	}
	message := name + " [" + identity + "]: " + status
	style, symbol := terminalcanvas.StyleMuted, symbols.Path
	if event.Stage() == appobs.PeerAttemptAdmitted {
		style, symbol = terminalcanvas.StyleSuccess, symbols.Success
	}
	if warning {
		style, symbol = terminalcanvas.StyleWarning, symbols.Warning
	}
	return statusLine(symbol, message, style)
}

func peerAttemptStatus(event appobs.PeerAttemptObserved) (string, bool) {
	if _, failure, ok := event.Failure(); ok {
		return "failed. " + failureMessage(failure), true
	}
	switch event.Stage() {
	case appobs.PeerNegotiationDeadlineArmed:
		return "negotiating.", false
	case appobs.PeerNegotiationDeadlineExpired:
		return "negotiation timed out.", true
	case appobs.PeerDataChannelOpen:
		return "transport connected; awaiting admission.", false
	case appobs.PeerAdmissionDeadlineArmed:
		return "waiting for admission.", false
	case appobs.PeerAdmissionDeadlineExpired:
		return "admission timed out.", true
	case appobs.PeerAdmissionResponseSettled:
		return peerAdmissionStatus(event)
	case appobs.PeerAttemptAdmitted:
		return "connected.", false
	default:
		return strings.ReplaceAll(eventName(event.Stage()), "-", " ") + ".", false
	}
}

func peerAdmissionStatus(event appobs.PeerAttemptObserved) (string, bool) {
	disposition, delivery, _ := event.Admission()
	message := "admission accepted."
	warning := disposition == appobs.PeerAdmissionRejected
	if warning {
		code, retryMillis, _ := event.Rejection()
		message = "admission rejected (" + strings.ReplaceAll(eventName(code), "-", " ") + ")."
		if retryMillis != 0 {
			message += " Retry after " + strconv.FormatUint(retryMillis, 10) + " ms."
		}
	}
	if delivery == appobs.PeerResponseDeliveryFailed {
		message += " Admission response delivery failed."
		warning = true
	}
	return message, warning
}

func formatProtocolOperationFailure(
	event appobs.ProtocolObservationObserved,
	fact appobs.ProtocolOperationFact,
	symbols Symbols,
) terminalcanvas.Line {
	operation := strings.ReplaceAll(eventName(event.RequestKind()), "_", " ")
	message := "Protocol operation " + operation + " failed"
	if elapsed := fact.OperationElapsedMillis(); elapsed != 0 {
		message += " after " + FormatElapsed(time.Duration(elapsed)*time.Millisecond)
	}
	if lane, ok := fact.Lane(); ok {
		message += " on lane " + strconv.FormatUint(uint64(lane.ID()), 10) +
			" epoch " + strconv.FormatUint(uint64(lane.Epoch()), 10)
	}
	cause := strings.ReplaceAll(eventName(fact.Cause()), "_", " ")
	message += " (" + cause + ")."
	return statusLine(symbols.Warning, message, terminalcanvas.StyleWarning)
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
