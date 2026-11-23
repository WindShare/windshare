package turnsession

import (
	d "github.com/windshare/windshare/core/diagnostics"
	p "github.com/windshare/windshare/core/session/protocolsession"
)

// Trace carries the task recorder across issuing and negotiation. Credentials,
// ICE URLs and bearer tokens never enter diagnostic records.
type Trace struct {
	Sink    d.Sink
	Session p.ProtocolSessionID
}

func (t Trace) Event(name, service string, fields d.Fields) {
	if !t.Sink.Enabled() {
		return
	}
	if fields == nil {
		fields = d.Fields{}
	}
	if service != "" {
		fields["service_id"] = d.Text(service)
	}
	record := d.New(name, fields)
	if t.Session != (p.ProtocolSessionID{}) {
		record = record.Correlate(d.Fields{"protocol_session_id": d.Identity(t.Session[:])})
	}
	t.Sink.Emit(record)
}
