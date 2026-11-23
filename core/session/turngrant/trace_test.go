package turngrant

import (
	"testing"

	d "github.com/windshare/windshare/core/diagnostics"
	p "github.com/windshare/windshare/core/session/protocolsession"
)

func TestCredentialEventsRetainTaskAndProtocolSessionOwnership(t *testing.T) {
	stream, err := d.NewStream(2, nil)
	if err != nil {
		t.Fatal(err)
	}
	sink, stop := stream.Capture()
	trace := Trace{Sink: sink.Bind(d.Scope{TaskID: "share-test"}), Session: p.ProtocolSessionID{1}}
	trace.Event("turn_grant_failed", "home", d.Fields{"reason": d.Text("issuer_unavailable")})
	stop()
	trace.Event("turn_grant_failed", "home", nil)
	if n := stream.Close().Enqueued; n != 1 {
		t.Fatal(n)
	}
	record := <-stream.Records()
	if record.Scope().TaskID != "share-test" || record.Correlation().Get("protocol_session_id").Text() != d.Identity(trace.Session[:]).Text() ||
		record.Payload().Get("service_id").Text() != "home" || record.Payload().Get("reason").Text() != "issuer_unavailable" {
		t.Fatal("missing diagnostic ownership")
	}
}
