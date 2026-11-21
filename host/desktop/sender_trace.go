package desktop

import (
	"errors"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine"
)

// Application.mu serializes capture changes with share admission. Tasks get
// one sink for their admission so routine shares avoid installing observers,
// while engine-scoped infrastructure follows the active trace toggle.
type senderTrace struct {
	stream     *diagnostics.Stream
	sink       diagnostics.Sink
	stop       func()
	engineSink diagnostics.Sink
	setEngine  func(bool)
}

func newSenderTrace(stream *diagnostics.Stream, enabled bool) *senderTrace {
	t := &senderTrace{stream: stream}
	if stream != nil {
		t.engineSink, t.setEngine = stream.Controlled(enabled)
	}
	if enabled && stream != nil {
		t.sink, t.stop = stream.Capture()
	}
	return t
}

func (t *senderTrace) state() SenderTraceState {
	switch {
	case t == nil:
		return SenderTraceUnavailable
	case t.stop == nil:
		return SenderTraceOff
	default:
		return SenderTraceOn
	}
}

func (t *senderTrace) captureSink() diagnostics.Sink {
	if t == nil {
		return diagnostics.Sink{}
	}
	return t.sink
}

func (t *senderTrace) engineDiagnostics() diagnostics.Sink {
	if t == nil {
		return diagnostics.Sink{}
	}
	return t.engineSink
}

func (t *senderTrace) setEnabled(enabled bool) bool {
	if enabled == (t.stop != nil) {
		return false
	}
	if enabled {
		t.sink, t.stop = t.stream.Capture()
	} else {
		t.stop()
		t.sink, t.stop = diagnostics.Sink{}, nil
	}
	if t.setEngine != nil {
		t.setEngine(enabled)
	}
	return true
}

func (a *Application) setSenderTrace(request Request) error {
	if a.closing {
		return engine.ErrClosed
	}
	if request.SenderTrace == nil {
		return errors.New("sender_trace is required")
	}
	trace := a.deps.senderTrace
	if trace == nil {
		return errors.New("sender trace storage is unavailable")
	}
	if enabled := *request.SenderTrace; trace.setEnabled(enabled) {
		a.deps.log("host.sender_trace_changed", map[string]any{
			"request_id": request.ID, "enabled": enabled,
		})
	}
	return nil
}
