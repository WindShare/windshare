package platformsetup

import "testing"

func TestLocalAddressScope(t *testing.T) {
	for _, test := range []struct {
		filters, addresses []string
		want               Setting
	}{
		{nil, nil, Yes},
		{[]string{"192.168.0.1"}, nil, Unspecified},
		{[]string{"192.168.0.1"}, []string{"192.168.0.1"}, Yes},
		{[]string{"192.168.0.1"}, []string{"100.64.0.1"}, No},
		{[]string{"192.168.0.0/24"}, []string{"192.168.0.2", "192.168.0.3"}, Yes},
		{[]string{"192.168.0.0/255.255.255.0"}, []string{"192.168.0.2"}, Yes},
		{[]string{"192.168.0.0/255.255.255.0"}, []string{"192.168.1.2"}, No},
		{[]string{"192.168.0.0/255.0.255.0"}, []string{"192.168.0.2"}, Unspecified},
		{[]string{"192.168.0.0/24"}, []string{"192.168.0.2", "fd00::1"}, Unspecified},
		{[]string{"192.168.0.0/24", "fd00::/8"}, []string{"192.168.0.2", "fd00::1"}, Yes},
		{[]string{"100.64.0.0-100.127.255.255"}, []string{"100.65.0.1"}, Yes},
		{[]string{"100.64.0.0-100.127.255.255"}, []string{"192.168.0.1"}, No},
		{[]string{"100.65.0.1-100.64.0.0"}, []string{"100.65.0.1"}, Unspecified},
		{[]string{"LocalSubnet"}, []string{"192.168.0.1"}, Unspecified},
		{[]string{"*"}, []string{"192.168.0.1"}, Yes},
		{[]string{"fd00::/8"}, []string{"192.168.0.1"}, No},
		{[]string{"fd00::/8"}, []string{"bad-address"}, Unspecified},
	} {
		if got := matchLocalAddresses(test.filters, test.addresses); got != test.want {
			t.Fatalf("%v %v: %v, want %v", test.filters, test.addresses, got, test.want)
		}
	}
}

func TestUnrelatedVPNRuleCannotHideMissingLANAllowance(t *testing.T) {
	snapshot := defaultSnapshot()
	snapshot.Networks[0].LocalAddresses = []string{"192.168.0.1"}
	snapshot.Rules = []Rule{{Name: "VPN", Profiles: []Profile{Private}, Protocol: AnyProtocol, Action: Allowed, LocalAddresses: []string{"100.64.0.1"}}}
	if got := Evaluate(snapshot)[0]; !got.Repairable() || len(got.Rules) != 0 {
		t.Fatal(got)
	}
	snapshot.Rules[0].LocalAddresses = []string{"192.168.0.1"}
	if got := Evaluate(snapshot)[0]; got.Verdict != Allowed {
		t.Fatal(got)
	}
	snapshot.Networks[0].LocalAddresses = append(snapshot.Networks[0].LocalAddresses, "fd00::1")
	if got := Evaluate(snapshot)[0]; got.Verdict != Unknown {
		t.Fatal(got)
	}
}

func TestAuthenticatedBypassMakesBlockOutcomeConditional(t *testing.T) {
	snapshot := defaultSnapshot()
	snapshot.Rules = []Rule{
		{Name: "block", Profiles: []Profile{Private}, Action: Blocked},
		{Name: "IPsec bypass", Profiles: []Profile{Private}, Action: Allowed, Conditional: true, OverridesBlocks: true},
	}
	if got := Evaluate(snapshot)[0]; got.Verdict != Unknown || got.Repairable() {
		t.Fatal(got)
	}
}
