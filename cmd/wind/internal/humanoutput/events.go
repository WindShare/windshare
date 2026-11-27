package humanoutput

import (
	"strconv"
	"strings"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/engine"
)

type SharingSubjectKind uint8

const (
	SharingFile SharingSubjectKind = iota + 1
	SharingDirectory
	SharingMultiple
)

type SharingSubject struct {
	Kind          SharingSubjectKind
	Name          string
	FileBytes     uint64
	SelectedItems uint64
}

func SharingSubjectFromSummary(summary liveshare.SelectedRootSummary) SharingSubject {
	if single, ok := summary.SingleRoot(); ok {
		if single.Kind() == liveshare.SelectedRootKindFile {
			size, _ := single.FileSize()
			return SharingSubject{Kind: SharingFile, Name: single.Name(), FileBytes: size, SelectedItems: 1}
		}
		return SharingSubject{Kind: SharingDirectory, Name: single.Name(), SelectedItems: 1}
	}
	return SharingSubject{Kind: SharingMultiple, SelectedItems: summary.SelectedCount()}
}

func formatSharingSubject(subject SharingSubject, symbols Symbols) terminalcanvas.Line {
	var description string
	switch subject.Kind {
	case SharingFile:
		description = escapedDisplay(subject.Name) + " (file, " + FormatBytes(subject.FileBytes) + ")"
	case SharingDirectory:
		name := escapedDisplay(subject.Name)
		if !strings.HasSuffix(name, "/") && !strings.HasSuffix(name, `\`) {
			name += "/"
		}
		description = name + " (directory)"
	case SharingMultiple:
		description = formatCount(subject.SelectedItems) + " selected items"
	}
	return statusLine(symbols.Arrow, "Sharing: "+description, terminalcanvas.StyleAccent)
}

func formatContentPath(path engine.ContentPath, symbols Symbols) terminalcanvas.Line {
	name := "Relay"
	switch path {
	case engine.ContentPathDirect:
		name = "Direct"
	case engine.ContentPathDirectAndRelay:
		name = "Direct + Relay"
	}
	return statusLine(symbols.Path, "Content path: "+name, terminalcanvas.StyleAccent)
}

func formatFallback(event engine.Fallback, symbols Symbols) terminalcanvas.Line {
	message := transportName(event.From) + " path unavailable; using " + transportName(event.To) + "."
	return statusLine(symbols.Warning, "Warning: "+message, terminalcanvas.StyleWarning)
}

func formatRelayRecovery(event engine.RelayRecovery, symbols Symbols) terminalcanvas.Line {
	if event.Failure != nil && event.Failure.Code == engine.FailureRelayStopped {
		return statusLine(symbols.Warning, failureReason(*event.Failure), terminalcanvas.StyleWarning)
	}
	if event.WaitReason == engine.RelayWaitBandwidth {
		return statusLine(symbols.Relay, "Relay bandwidth is busy; your download will resume automatically.", terminalcanvas.StyleDefault)
	}
	switch event.Authorization {
	case serviceauthority.AdmissionPendingSetup:
		return statusLine(symbols.Relay, "Setting up official-service access; your selection is retained.", terminalcanvas.StyleDefault)
	case serviceauthority.AdmissionUserActionRequired:
		return statusLine(symbols.Warning, "Official-service access requires attention before continuing.", terminalcanvas.StyleWarning)
	case serviceauthority.AdmissionExpired:
		return statusLine(symbols.Relay, "Refreshing account access; this share will continue automatically.", terminalcanvas.StyleDefault)
	case serviceauthority.AdmissionTemporaryFailure:
		return statusLine(symbols.Warning, "Account service unavailable; retrying automatically.", terminalcanvas.StyleWarning)
	case serviceauthority.AdmissionDenied:
		return statusLine(symbols.Warning, "Official-service access denied. Check your account.", terminalcanvas.StyleWarning)
	}
	relay := event.Origin.HostPort()
	message := "Relay recovery attempt " + strconv.FormatUint(uint64(event.Attempt), 10) + " " + string(event.State) + "."
	hasDetails := event.Generation > 0 || event.Slow || event.Resume || event.Terminal || event.NextDelay > 0
	if hasDetails {
		switch event.State {
		case engine.RelayRecoverySucceeded:
			message = "Relay connection restored: " + relay + "."
			if event.Generation == 1 && !event.Resume {
				message = "Relay connected: " + relay + "."
			}
		case engine.RelayRecoveryWaiting:
			message = "Waiting for relay " + relay + "; retrying automatically."
		case engine.RelayRecoveryFailed:
			if event.Terminal {
				message = "Relay recovery needs attention: " + relay + "."
			}
		}
	}
	style, symbol := terminalcanvas.StyleDefault, symbols.Relay
	if event.Failure != nil && event.Failure.Valid() {
		detail := failureReason(*event.Failure)
		if event.Terminal {
			detail = failureMessage(*event.Failure)
		}
		message += " " + detail
		style, symbol = terminalcanvas.StyleWarning, symbols.Warning
	}
	return statusLine(symbol, message, style)
}

func formatRelayAvailability(event engine.RelayAvailability, symbols Symbols) terminalcanvas.Line {
	if event.Available > 0 {
		return statusLine(symbols.Relay, "New receivers can join through "+strconv.FormatUint(uint64(event.Available), 10)+" of "+strconv.FormatUint(uint64(event.Total), 10)+" relays.", terminalcanvas.StyleAccent)
	}
	message := "Waiting for a relay before publishing the link; press Ctrl-C to stop."
	if event.EverReady {
		message = "New receivers are waiting for relay recovery. Healthy direct transfers can continue; the same link will recover automatically."
	}
	if event.Terminal == event.Total {
		message = "New receivers cannot join: all relays need attention. Healthy direct transfers can continue."
	}
	return statusLine(symbols.Warning, message, terminalcanvas.StyleWarning)
}

func formatLaneAdopted(event engine.LaneAdopted, symbols Symbols) terminalcanvas.Line {
	message := "Lane " + strconv.FormatUint(uint64(event.Lane.ID), 10) +
		" epoch " + strconv.FormatUint(uint64(event.Lane.Epoch), 10) +
		" adopted (" + transportName(event.Transport) + ")."
	return statusLine(symbols.Path, message, terminalcanvas.StyleMuted)
}

func transportName(transport engine.Transport) string {
	if transport == engine.TransportWebRTC {
		return "WebRTC"
	}
	return "Relay"
}

func formatDiscoveryMilestone(snapshot transfer.ReceiveProgressSnapshot, symbols Symbols) terminalcanvas.Line {
	switch snapshot.Discovery {
	case transfer.DiscoveryComplete:
		return statusLine(symbols.Discovery,
			"Discovery complete: "+formatFiles(snapshot.DiscoveredFiles)+symbols.Separator+FormatBytes(snapshot.DiscoveredBytes),
			terminalcanvas.StyleDefault)
	case transfer.DiscoveryFailed:
		return statusLine(symbols.Failure, "Discovery failed.", terminalcanvas.StyleError)
	default:
		return statusLine(symbols.Discovery, "Discovering content"+symbols.Ellipsis, terminalcanvas.StyleDefault)
	}
}
