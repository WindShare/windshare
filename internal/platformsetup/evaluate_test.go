package platformsetup

import "testing"

func defaultSnapshot() Snapshot {
	return Snapshot{Networks: []Network{{Interface: "Wi-Fi", Profile: Private}}, Profiles: []ProfilePolicy{{
		Profile: Private, Enabled: Yes, AllowInboundRules: Yes, AllowLocalRules: Yes, DefaultInbound: Blocked,
	}}}
}

func TestEvaluateEffectivePolicy(t *testing.T) {
	allow := Rule{Name: "administrator-allow", Profiles: []Profile{Private}, Protocol: AnyProtocol, Action: Allowed}
	block := Rule{Name: "administrator-block", Profiles: []Profile{Private}, Protocol: UDP, Action: Blocked}
	for _, test := range []struct {
		name       string
		change     func(*Snapshot)
		verdict    Verdict
		reason     Reason
		repairable bool
	}{
		{"default block", func(*Snapshot) {}, Blocked, DefaultBlock, true},
		{"another owner allows", func(s *Snapshot) { s.Rules = []Rule{allow} }, Allowed, ExplicitAllow, false},
		{"block beats allow", func(s *Snapshot) { s.Rules = []Rule{allow, block} }, Blocked, ExplicitBlock, false},
		{"firewall disabled", func(s *Snapshot) { s.Profiles[0].Enabled = No; s.Rules = []Rule{block} }, Allowed, FirewallDisabled, false},
		{"interface exempt", func(s *Snapshot) { s.Profiles[0].DisabledInterfaces = []string{"WI-FI"} }, Allowed, InterfaceExempt, false},
		{"block all inbound", func(s *Snapshot) { s.Profiles[0].AllowInboundRules = No; s.Rules = []Rule{allow} }, Blocked, InboundRulesDisabled, false},
		{"local merge denied", func(s *Snapshot) { s.Profiles[0].AllowLocalRules = No; r := allow; r.Local = true; s.Rules = []Rule{r} }, Blocked, LocalRulesDisabled, false},
		{"GPO allow with merge denied", func(s *Snapshot) { s.Profiles[0].AllowLocalRules = No; s.Rules = []Rule{allow} }, Allowed, ExplicitAllow, false},
		{"default allow", func(s *Snapshot) { s.Profiles[0].DefaultInbound = Allowed }, Allowed, DefaultAllow, false},
		{"scoped block", func(s *Snapshot) { r := block; r.Conditional = true; s.Rules = []Rule{allow, r} }, Unknown, ConditionalRule, false},
		{"scoped allow", func(s *Snapshot) { r := allow; r.Conditional = true; s.Rules = []Rule{r} }, Unknown, ConditionalRule, false},
		{"scoped allow cannot weaken broad allow", func(s *Snapshot) { r := allow; r.Conditional = true; s.Rules = []Rule{allow, r} }, Allowed, ExplicitAllow, false},
		{"another interface", func(s *Snapshot) { r := allow; r.Interfaces = []string{"Ethernet"}; s.Rules = []Rule{r} }, Blocked, DefaultBlock, true},
		{"unresolved interface block", func(s *Snapshot) { r := block; r.Interfaces = []string{"Wi*"}; s.Rules = []Rule{allow, r} }, Unknown, ConditionalRule, false},
		{"unresolved interface exemption", func(s *Snapshot) { s.Profiles[0].DisabledInterfaces = []string{"Wi*"} }, Unknown, IncompleteEvidence, false},
		{"another profile", func(s *Snapshot) { r := allow; r.Profiles = []Profile{Public}; s.Rules = []Rule{r} }, Blocked, DefaultBlock, true},
		{"unknown policy", func(s *Snapshot) { s.Profiles[0].Enabled = Unspecified }, Unknown, IncompleteEvidence, false},
		{"unknown merge", func(s *Snapshot) { s.Profiles[0].AllowLocalRules = Unspecified }, Unknown, IncompleteEvidence, false},
		{"missing profile", func(s *Snapshot) { s.Profiles = nil }, Unknown, IncompleteEvidence, false},
		{"duplicate profile", func(s *Snapshot) { s.Profiles = append(s.Profiles, s.Profiles[0]) }, Unknown, IncompleteEvidence, false},
		{"no active network", func(s *Snapshot) { s.Networks = nil }, Unknown, NoActiveNetwork, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := defaultSnapshot()
			test.change(&snapshot)
			findings := Evaluate(snapshot)
			got := findings[0]
			if len(findings) != 2 || got.Verdict != test.verdict || got.Reason != test.reason || got.Repairable() != test.repairable {
				t.Fatalf("unexpected policy: %+v", findings)
			}
		})
	}
}

func TestPolicyKeepsProtocolAndNetworkScopes(t *testing.T) {
	snapshot := defaultSnapshot()
	snapshot.Networks = append(snapshot.Networks, Network{Interface: "VPN", Profile: Public})
	snapshot.Profiles = append(snapshot.Profiles, ProfilePolicy{Profile: Public, Enabled: No})
	snapshot.Rules = []Rule{{Name: "udp-only", Profiles: []Profile{Private}, Protocol: UDP, Action: Allowed}}
	got := Evaluate(snapshot)
	if len(got) != 4 || got[0].Verdict != Allowed || got[1].Verdict != Blocked || got[2].Verdict != Allowed || got[3].Verdict != Allowed {
		t.Fatal(got)
	}
	if got[0].Rules[0].Name != "udp-only" || got[2].Network.Interface != "VPN" {
		t.Fatal("scope evidence lost", got)
	}
}
