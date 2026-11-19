package desktop

import (
	"errors"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine"
)

// Application.mu serializes capture changes with share admission. Tasks get
// one sink for their admission so routine shares avoid installing observers.
type senderTrace struct {
	stream *diagnostics.Stream
	sink   diagnostics.Sink
	stop   func()
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
