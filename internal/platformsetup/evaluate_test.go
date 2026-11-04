package platformsetup

import "testing"

func defaultSnapshot() Snapshot {
	return Snapshot{Networks: []Network{{Interface: "Wi-Fi", Profile: Private}}, Profiles: []ProfilePolicy{{
		Profile: Private, Enabled: Yes, AllowInboundRules: Yes, AllowLocalRules: Yes, DefaultInbound: Block,
	}}}
}

func TestEvaluateExecutableConfiguration(t *testing.T) {
	allow := Rule{Name: "administrator-allow", Profiles: []Profile{Private}, Protocol: AnyProtocol, Action: Allow}
	block := Rule{Name: "administrator-block", Profiles: []Profile{Private}, Protocol: UDP, Action: Block}
	for _, test := range []struct {
		name    string
		change  func(*Snapshot)
		status  RuleStatus
		reason  Reason
		suggest bool
	}{
		{"missing rule", func(*Snapshot) {}, RuleMissing, NoMatchingRule, true},
		{"another owner allows", func(s *Snapshot) { s.Rules = []Rule{allow} }, RulePresent, ExplicitAllow, false},
		{"explicit block", func(s *Snapshot) { s.Rules = []Rule{allow, block} }, PolicyRestricted, ExplicitBlock, false},
		{"firewall disabled", func(s *Snapshot) { s.Profiles[0].Enabled = No; s.Rules = []Rule{block} }, RuleNotRequired, FirewallDisabled, false},
		{"interface exempt", func(s *Snapshot) { s.Profiles[0].DisabledInterfaces = []string{"WI-FI"} }, RuleNotRequired, InterfaceExempt, false},
		{"inbound rules ignored", func(s *Snapshot) { s.Profiles[0].AllowInboundRules = No; s.Rules = []Rule{allow} }, PolicyRestricted, InboundRulesDisabled, false},
		{"ignored rules with default allow", func(s *Snapshot) { s.Profiles[0].AllowInboundRules = No; s.Profiles[0].DefaultInbound = Allow }, RuleNotRequired, DefaultAllow, false},
		{"local merge denied", func(s *Snapshot) { s.Profiles[0].AllowLocalRules = No; r := allow; r.Local = true; s.Rules = []Rule{r} }, PolicyRestricted, LocalRulesDisabled, false},
		{"GPO rule with merge denied", func(s *Snapshot) { s.Profiles[0].AllowLocalRules = No; s.Rules = []Rule{allow} }, RulePresent, ExplicitAllow, false},
		{"default allow", func(s *Snapshot) { s.Profiles[0].DefaultInbound = Allow }, RuleNotRequired, DefaultAllow, false},
		{"restricted block", func(s *Snapshot) { r := block; r.Conditional = true; s.Rules = []Rule{allow, r} }, RuleRestricted, ConditionalRule, false},
		{"restricted allow", func(s *Snapshot) { r := allow; r.Conditional = true; s.Rules = []Rule{r} }, RuleRestricted, ConditionalRule, false},
		{"broad allow with restricted allow", func(s *Snapshot) { r := allow; r.Conditional = true; s.Rules = []Rule{allow, r} }, RulePresent, ExplicitAllow, false},
		{"unknown action", func(s *Snapshot) { r := allow; r.Action = UnknownAction; s.Rules = []Rule{r} }, RuleRestricted, ConditionalRule, false},
		{"unresolved exemption", func(s *Snapshot) { s.Profiles[0].DisabledInterfaces = []string{"Wi*"} }, UnknownStatus, IncompleteEvidence, false},
		{"another profile", func(s *Snapshot) { r := allow; r.Profiles = []Profile{Public}; s.Rules = []Rule{r} }, RuleMissing, NoMatchingRule, true},
		{"unknown policy", func(s *Snapshot) { s.Profiles[0].Enabled = Unspecified }, UnknownStatus, IncompleteEvidence, false},
		{"unknown merge", func(s *Snapshot) { s.Profiles[0].AllowLocalRules = Unspecified }, UnknownStatus, IncompleteEvidence, false},
		{"missing profile", func(s *Snapshot) { s.Profiles = nil }, UnknownStatus, IncompleteEvidence, false},
		{"duplicate profile", func(s *Snapshot) { s.Profiles = append(s.Profiles, s.Profiles[0]) }, UnknownStatus, IncompleteEvidence, false},
		{"no active network", func(s *Snapshot) { s.Networks = nil }, UnknownStatus, NoActiveNetwork, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := defaultSnapshot()
			test.change(&snapshot)
			findings := Evaluate(snapshot)
			got := findings[0]
			if len(findings) != 2 || got.Status != test.status || got.Reason != test.reason || got.SuggestSetup() != test.suggest {
				t.Fatalf("unexpected configuration: %+v", findings)
			}
		})
	}
}

func TestInspectionKeepsProtocolAndNetworkScopes(t *testing.T) {
	snapshot := defaultSnapshot()
	snapshot.Networks = append(snapshot.Networks, Network{Interface: "VPN", Profile: Public})
	snapshot.Profiles = append(snapshot.Profiles, ProfilePolicy{Profile: Public, Enabled: No})
	snapshot.Rules = []Rule{{Name: "udp-only", Profiles: []Profile{Private}, Protocol: UDP, Action: Allow}}
	got := Evaluate(snapshot)
	if len(got) != 4 || got[0].Status != RulePresent || got[1].Status != RuleMissing || got[2].Status != RuleNotRequired || got[3].Status != RuleNotRequired {
		t.Fatal(got)
	}
	if got[0].Rules[0].Name != "udp-only" || got[2].Network.Interface != "VPN" {
		t.Fatal("scope evidence lost", got)
	}
}
