package platformsetup

import (
	"slices"
	"strings"
)

// Evaluate needs no privileges or I/O. Explicit blocks dominate allows; uncertainty
// about a potentially matching block must never become an optimistic allow verdict.
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
	var profile ProfilePolicy
	found := false
	for _, candidate := range snapshot.Profiles {
		if candidate.Profile == network.Profile && candidate.Profile != UnknownProfile {
			if found { // Conflicting snapshots are not evidence of an effective policy.
				return finding
			}
			profile, found = candidate, true
		}
	}
	if !found {
		return finding
	}
	if profile.Enabled == No {
		finding.Verdict, finding.Reason = Allowed, FirewallDisabled
		return finding
	}
	exempt := matchInterface(profile.DisabledInterfaces, network.Interface)
	if exempt == Yes {
		finding.Verdict, finding.Reason = Allowed, InterfaceExempt
		return finding
	}
	if profile.Enabled != Yes || exempt == Unspecified {
		return finding
	}
	if profile.AllowInboundRules == No {
		finding.Verdict, finding.Reason = Blocked, InboundRulesDisabled
		return finding
	}
	if profile.AllowInboundRules != Yes || profile.AllowLocalRules == Unspecified {
		return finding
	}
	evidence := collectRules(snapshot.Rules, network, protocol, profile.AllowLocalRules)
	finding.Rules = evidence.rules
	switch {
	case evidence.block && evidence.mayBypassBlocks:
		finding.Reason = ConditionalRule
	case evidence.block:
		finding.Verdict, finding.Reason = Blocked, ExplicitBlock
	case evidence.conditionalBlock:
		finding.Reason = ConditionalRule
	case evidence.allow:
		finding.Verdict, finding.Reason = Allowed, ExplicitAllow
	case profile.DefaultInbound == Allowed:
		finding.Verdict, finding.Reason = Allowed, DefaultAllow
	case evidence.conditionalAllow:
		finding.Reason = ConditionalRule
	case profile.DefaultInbound == Blocked && profile.AllowLocalRules == No:
		finding.Verdict, finding.Reason = Blocked, LocalRulesDisabled
	case profile.DefaultInbound == Blocked:
		finding.Verdict, finding.Reason = Blocked, DefaultBlock
	}
	return finding
}

type ruleEvidence struct {
	rules                                            []Rule
	allow, block, conditionalAllow, conditionalBlock bool
	mayBypassBlocks                                  bool
}

func collectRules(rules []Rule, network Network, protocol Protocol, allowLocal Setting) ruleEvidence {
	var evidence ruleEvidence
	for _, rule := range rules {
		if rule.Protocol != AnyProtocol && rule.Protocol != protocol ||
			!slices.Contains(rule.Profiles, network.Profile) ||
			rule.Local && allowLocal == No {
			continue
		}
		interfaceMatch := Yes
		if len(rule.Interfaces) > 0 {
			interfaceMatch = matchInterface(rule.Interfaces, network.Interface)
		}
		if interfaceMatch == No {
			continue
		}
		addressMatch := matchLocalAddresses(rule.LocalAddresses, network.LocalAddresses)
		if addressMatch == No {
			continue
		}
		evidence.rules = append(evidence.rules, rule)
		evidence.mayBypassBlocks = evidence.mayBypassBlocks || rule.OverridesBlocks && rule.Action == Allowed
		if rule.Conditional || rule.Action == Unknown || interfaceMatch == Unspecified || addressMatch == Unspecified {
			evidence.conditionalAllow = evidence.conditionalAllow || rule.Action == Allowed
			evidence.conditionalBlock = evidence.conditionalBlock || rule.Action != Allowed
			continue
		}
		evidence.allow = evidence.allow || rule.Action == Allowed
		evidence.block = evidence.block || rule.Action == Blocked
	}
	return evidence
}

func matchInterface(aliases []string, name string) Setting {
	result := No
	for _, alias := range aliases {
		if alias == "*" || strings.EqualFold(alias, name) {
			return Yes
		}
		// Windows wildcard syntax is richer than a filesystem glob. An unresolved
		// selector must not hide a block or manufacture an interface exemption.
		if strings.ContainsAny(alias, "*?[") {
			result = Unspecified
		}
	}
	return result
}

func unknownFindings(reason Reason) []Finding {
	return []Finding{{Protocol: UDP, Reason: reason}, {Protocol: TCP, Reason: reason}}
}
