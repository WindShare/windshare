package humanoutput

import (
	"strconv"
	"strings"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/diagnostics"
)

func (visitor eventVisitor) VisitDiagnostic(event clievent.Diagnostic) error {
	if !visitor.verboseVisible() {
		return nil
	}
	switch event.Record.Name() {
	case "peer_attempt":
		visitor.renderer.insert(formatPeerAttempt(event.Record, visitor.symbols()))
	case "protocol_operation":
		if event.Record.Payload().Get("stage").Text() == "receiver_failed" {
			visitor.renderer.insert(formatProtocolOperationFailure(event.Record, visitor.symbols()))
		}
	}
	return nil
}
func formatPeerAttempt(record diagnostics.Record, symbols Symbols) terminalcanvas.Line {
	f := record.Payload()
	stage := f.Get("stage").Text()
	name := "Connection"
	switch f.Get("route").Text() {
	case "direct":
		name = "Direct connection"
	case "turn":
		name = "TURN relay connection"
	}
	status, warning := peerAttemptStatus(f)
	style, symbol := terminalcanvas.StyleMuted, symbols.Path
	if stage == "admitted" {
		style, symbol = terminalcanvas.StyleSuccess, symbols.Success
	}
	if warning {
		style, symbol = terminalcanvas.StyleWarning, symbols.Warning
	}
	message := name + " [" + record.Correlation().Get("peer_attempt_id").Text() + "]: " + status
	return statusLine(symbol, escapedDisplay(message), style)
}
func peerAttemptStatus(f diagnostics.Value) (string, bool) {
	if failure := f.Get("failure"); failure.Present() {
		return "failed. " + failure.Get("message").Text(), true
	}
	switch stage := f.Get("stage").Text(); stage {
	case "negotiation-deadline-armed":
		return "negotiating.", false
	case "negotiation-deadline-expired":
		return "negotiation timed out.", true
	case "datachannel-open":
		return "transport connected; awaiting admission.", false
	case "admission-deadline-armed":
		return "waiting for admission.", false
	case "admission-deadline-expired":
		return "admission timed out.", true
	case "admitted":
		return "connected.", false
	case "admission-response-settled":
		a := f.Get("admission")
		message, warning := "admission accepted.", false
		if a.Get("disposition").Text() == "rejected" {
			message, warning = "admission rejected ("+strings.ReplaceAll(f.Get("rejection").Get("code").Text(), "-", " ")+").", true
			if retry := f.Get("rejection").Get("retry_after_ms").Uint(); retry > 0 {
				message += " Retry after " + strconv.FormatUint(retry, 10) + " ms."
			}
		}
		if a.Get("response_delivery").Text() == "failed" {
			message += " Admission response delivery failed."
			warning = true
		}
		return message, warning
	default:
		return strings.ReplaceAll(stage, "-", " ") + ".", false
	}
}
func formatProtocolOperationFailure(record diagnostics.Record, symbols Symbols) terminalcanvas.Line {
	f := record.Payload()
	message := "Protocol operation " + strings.ReplaceAll(f.Get("request_kind").Text(), "_", " ") + " failed"
	if elapsed := f.Get("operation_elapsed_ms").Uint(); elapsed > 0 {
		message += " after " + FormatElapsed(time.Duration(elapsed)*time.Millisecond)
	}
	c := record.Correlation()
	if lane := c.Get("lane_id"); lane.Present() {
		message += " on lane " + lane.Text() + " epoch " + c.Get("lane_epoch").Text()
	}
	message += " (" + strings.ReplaceAll(f.Get("cause").Text(), "_", " ") + ")."
	return statusLine(symbols.Warning, escapedDisplay(message), terminalcanvas.StyleWarning)
}
