package protocolcontract

import (
	"bytes"
	"testing"

	p "github.com/windshare/windshare/core/session/protocolsession"
)

func turnControlCases(t *testing.T) []any {
	t.Helper()
	var cases []any
	add := func(name string, sender bool, body []byte, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		role, other := p.RoleReceiver, p.RoleSender
		if sender {
			role, other = other, role
		}
		if err := p.ValidateSessionControlBody(role, p.MessageTURNControl, body); err != nil {
			t.Fatal(name, err)
		}
		if p.ValidateSessionControlBody(other, p.MessageTURNControl, body) == nil {
			t.Fatal(name, "accepted in opposite direction")
		}
		cases = append(cases, map[string]any{"name": name, "sender": sender, "bodyB64": b64(body)})
	}
	for _, d := range []p.TURNDemand{
		{RequestID: 1, Mode: p.TURNParallel},
		{RequestID: 2, Mode: p.TURNAll, ServiceIDs: []string{"home"}},
		{RequestID: 3, Mode: p.TURNAll, ServiceIDs: []string{"\ufeffhome\ufeff", "home\u0085relay", "caf?"}},
	} {
		b, err := p.EncodeTURNDemand(d)
		add("demand", false, b, err)
	}
	home := p.TURNGrant{ServiceID: "home", GrantID: p.TURNGrantID{1}, URLs: []string{"turn:localhost:3478", "turns:example.org:443?transport=tcp"}, Username: "guest", Password: "fixture", ValidForMS: 1800000}
	for _, g := range []p.TURNGrants{
		{RequestID: 1, Status: p.TURNOK, HasFallback: true},
		{RequestID: 2, Status: p.TURNOK, Grants: []p.TURNGrant{home}},
		{RequestID: 3, Status: p.TURNUnconfigured}, {RequestID: 4, Status: p.TURNRetryLater, HasFallback: true},
		{RequestID: 5, Status: p.TURNPartialRetryable, Grants: []p.TURNGrant{home}},
	} {
		b, err := p.EncodeTURNGrants(g)
		add("grants", true, b, err)
	}
	reject := func(name string, sender bool, wire []any) {
		t.Helper()
		body := canonical(t, wire)
		if _, err := p.DecodeTURNDemand(body); err == nil {
			t.Fatal(name, "accepted invalid demand")
		}
		if _, err := p.DecodeTURNGrants(body); err == nil {
			t.Fatal(name, "accepted invalid grants")
		}
		cases = append(cases, map[string]any{"name": name, "sender": sender, "bodyB64": b64(body), "valid": false})
	}
	reject("unknown demand mode", false, []any{1, 1, 1, 3, []string{}})
	for _, id := range []string{"\u0085home", "home\u0085", "home\u00a0", "cafe\u0301"} {
		reject("invalid service ID", false, []any{1, 1, 1, 2, []string{id}})
	}
	reject("unknown grant status", true, []any{1, 2, 1, 5, false, []p.TURNGrant{}})
	reject("partial without grants", true, []any{1, 2, 1, 4, true, []p.TURNGrant{}})
	reject("retry with grants", true, []any{1, 2, 1, 3, true, []p.TURNGrant{home}})
	reject("unconfigured with grants", true, []any{1, 2, 1, 2, false, []p.TURNGrant{home}})
	reject("unconfigured with fallback", true, []any{1, 2, 1, 2, true, []p.TURNGrant{}})
	return cases
}

func TestTURNRejectsAmbiguousWireValues(t *testing.T) {
	id := p.TURNGrantID{1}
	row := func(grantID p.TURNGrantID, endpoint string) []any {
		return []any{"home", grantID, []string{endpoint}, "user", "password", 0}
	}
	grants := func(rows ...[]any) []any { return []any{1, 2, 1, 1, false, rows} }
	valid := row(id, "turn:localhost")
	for _, invalid := range []any{
		[]any{1, 1, 1, 1, []string{"home", "home"}}, []any{1, 3, 1, 1, []string{}},
		grants(valid, valid), grants(row(p.TURNGrantID{}, "turn:localhost")),
		grants(row(id, "turn:[::::]")), grants(row(id, "turns:bad_host:443")),
		grants(row(id, "turn:u@host")), grants(row(id, "turn:host:65536")),
	} {
		body, err := p.EncodeBody(invalid)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.DecodeTURNGrants(body); err == nil {
			t.Fatal("accepted invalid grants")
		}
		if _, err := p.DecodeTURNDemand(body); err == nil {
			t.Fatal("accepted invalid demand")
		}
	}
	if _, err := p.DecodeTURNGrants(bytes.Repeat([]byte{0}, p.MaxTURNBodyBytes+1)); err == nil {
		t.Fatal("accepted oversized grant message")
	}
}
