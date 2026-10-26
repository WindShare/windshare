package receiversession

import (
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
)

type protocolTraceRecorder struct {
	producer observationstream.Producer[sessionobs.ProtocolObservation]
	consumer observationstream.Consumer[sessionobs.ProtocolObservation]
	events   []sessionobs.ProtocolObservation
}

func newProtocolTraceRecorder(runtime *sessioncore.Runtime) *protocolTraceRecorder {
	return testCore(runtime).recorder
}

func (recorder *protocolTraceRecorder) facts() []sessionobs.ProtocolObservation {
	for {
		select {
		case event := <-recorder.consumer:
			recorder.events = append(recorder.events, event)
		default:
			return append([]sessionobs.ProtocolObservation(nil), recorder.events...)
		}
	}
}

func (recorder *protocolTraceRecorder) snapshot() []sessionobs.ProtocolOperationObservation {
	var result []sessionobs.ProtocolOperationObservation
	for _, event := range recorder.facts() {
		if value, ok := event.(sessionobs.ProtocolOperationObservation); ok {
			result = append(result, value)
		}
	}
	return result
}
