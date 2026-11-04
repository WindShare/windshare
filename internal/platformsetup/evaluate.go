package platformsetup

import (
	"slices"
	"strings"
)

// Evaluate reports the configuration observed within InspectionScope. It does
// not attempt to resolve arbitrary address ranges, ports, IPsec, or global rules.
func Evaluate(snapshot Snapshot) []Finding {
	if len(snapshot.Networks) == 0 {
		return unknownFindings(NoActiveNetwork)
	}
	var findings []Finding
	for _, network := range snapshot.Networks {
		for _, protocol := range []Protocol{UDP, TCP} {
			findings = append(findings, evaluateScope(snapshot, network, protocol))
		}
	}
	return findings
}

func evaluateScope(snapshot Snapshot, network Network, protocol Protocol) Finding {
	finding := Finding{Network: network, Protocol: protocol, Reason: IncompleteEvidence}
	profile := networkProfile(snapshot.Profiles, network.Profile)
	if profile == nil {
		return finding
	}
	if profile.Enabled == No {
		finding.Status, finding.Reason = RuleNotRequired, FirewallDisabled
		return finding
	}
	uncertainExemption := false
	for _, alias := range profile.DisabledInterfaces {
		if alias == "*" || strings.EqualFold(alias, network.Interface) {
			finding.Status, finding.Reason = RuleNotRequired, InterfaceExempt
			return finding
		}
		if strings.ContainsAny(alias, "*?[") {
			uncertainExemption = true
		}
	}
	if uncertainExemption {
		return finding
	}
	if profile.Enabled != Yes || profile.AllowInboundRules == Unspecified || profile.AllowLocalRules == Unspecified {
		return finding
	}
	if profile.AllowInboundRules == No {
		switch profile.DefaultInbound {
		case Block:
			finding.Status, finding.Reason = PolicyRestricted, InboundRulesDisabled
		case Allow:
			finding.Status, finding.Reason = RuleNotRequired, DefaultAllow
		}
		return finding
	}
	allow, block, conditional, conditionalBlock := false, false, false, false
	for _, rule := range snapshot.Rules {
		if rule.Protocol != AnyProtocol && rule.Protocol != protocol || !slices.Contains(rule.Profiles, network.Profile) {
			continue
		}
		finding.Rules = append(finding.Rules, rule)
		if rule.Local && profile.AllowLocalRules == No {
			continue
		}
		if rule.Conditional || rule.Action == UnknownAction {
			conditional = true
			conditionalBlock = conditionalBlock || rule.Action != Allow
			continue
		}
		allow = allow || rule.Action == Allow
		block = block || rule.Action == Block
	}
	switch {
	case block:
		finding.Status, finding.Reason = PolicyRestricted, ExplicitBlock
	case conditionalBlock:
		finding.Status, finding.Reason = RuleRestricted, ConditionalRule
	case allow:
		finding.Status, finding.Reason = RulePresent, ExplicitAllow
	case profile.DefaultInbound == Allow:
		finding.Status, finding.Reason = RuleNotRequired, DefaultAllow
	case conditional:
		finding.Status, finding.Reason = RuleRestricted, ConditionalRule
	case profile.DefaultInbound == Block && profile.AllowLocalRules == No:
		finding.Status, finding.Reason = PolicyRestricted, LocalRulesDisabled
	case profile.DefaultInbound == Block:
		finding.Status, finding.Reason = RuleMissing, NoMatchingRule
	}
	return finding
}

func networkProfile(profiles []ProfilePolicy, profile Profile) *ProfilePolicy {
	if profile == UnknownProfile {
		return nil
	}
	var result *ProfilePolicy
	for index := range profiles {
		if profiles[index].Profile != profile {
			continue
		}
		// Conflicting observations cannot establish a profile's configuration.
		if result != nil {
			return nil
		}
		result = &profiles[index]
	}
	return result
}

func unknownFindings(reason Reason) []Finding {
	return []Finding{{Protocol: UDP, Reason: reason}, {Protocol: TCP, Reason: reason}}
}
