package humanoutput

import (
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
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
	// The event sequence advances within an attempt. Its immutable identity keeps
	// interleaved attempts distinguishable and matches peer_attempt_id in the trace.
	identity := base64.RawURLEncoding.EncodeToString(event.PeerAttemptID().Bytes())
	status, warning := peerAttemptStatus(event)
	message := "Direct connection [" + identity + "]: " + status
	style, symbol := terminalcanvas.StyleMuted, symbols.Path
	if event.Stage() == clievent.PeerAttemptAdmitted {
		style, symbol = terminalcanvas.StyleSuccess, symbols.Success
	}
	if warning {
		style, symbol = terminalcanvas.StyleWarning, symbols.Warning
	}
	return statusLine(symbol, message, style)
}

func peerAttemptStatus(event clievent.PeerAttemptObserved) (string, bool) {
	if _, failure, ok := event.Failure(); ok {
		return "failed. " + failureMessage(failure), true
	}
	switch event.Stage() {
	case clievent.PeerNegotiationDeadlineArmed:
		return "negotiating.", false
	case clievent.PeerNegotiationDeadlineExpired:
		return "negotiation timed out.", true
	case clievent.PeerDataChannelOpen:
		return "transport connected; awaiting admission.", false
	case clievent.PeerAdmissionDeadlineArmed:
		return "waiting for admission.", false
	case clievent.PeerAdmissionDeadlineExpired:
		return "admission timed out.", true
	case clievent.PeerAdmissionResponseSettled:
		return peerAdmissionStatus(event)
	case clievent.PeerAttemptAdmitted:
		return "connected.", false
	default:
		return strings.ReplaceAll(eventName(event.Stage()), "-", " ") + ".", false
	}
}

func peerAdmissionStatus(event clievent.PeerAttemptObserved) (string, bool) {
	disposition, delivery, _ := event.Admission()
	message := "admission accepted."
	warning := disposition == clievent.PeerAdmissionRejected
	if warning {
		code, retryMillis, _ := event.Rejection()
		message = "admission rejected (" + strings.ReplaceAll(eventName(code), "-", " ") + ")."
		if retryMillis != 0 {
			message += " Retry after " + strconv.FormatUint(retryMillis, 10) + " ms."
		}
	}
	if delivery == clievent.PeerResponseDeliveryFailed {
		message += " Admission response delivery failed."
		warning = true
	}
	return message, warning
}

func formatProtocolOperationFailure(
	event clievent.ProtocolObservationObserved,
	fact clievent.ProtocolOperationFact,
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
