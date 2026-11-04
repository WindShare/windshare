package networkcommand

import "github.com/windshare/windshare/internal/platformsetup"

type jsonFinding struct {
	Interface      string     `json:"interface"`
	LocalAddresses []string   `json:"local_addresses"`
	Profile        string     `json:"profile"`
	Protocol       string     `json:"protocol"`
	Verdict        string     `json:"verdict"`
	Reason         string     `json:"reason"`
	Repairable     bool       `json:"repairable"`
	Rules          []jsonRule `json:"rules"`
}

type jsonRule struct {
	Name         string `json:"name"`
	Action       string `json:"action"`
	Local        bool   `json:"local"`
	Restrictions string `json:"restrictions,omitempty"`
}

type jsonReport struct {
	Executable      string                   `json:"executable"`
	Choice          string                   `json:"choice"`
	PreferenceError string                   `json:"preference_error,omitempty"`
	PolicyError     string                   `json:"policy_error,omitempty"`
	Operation       *platformsetup.Operation `json:"operation,omitempty"`
	Findings        []jsonFinding            `json:"findings"`
}

func newJSONReport(report platformsetup.Report, operation *platformsetup.Operation) jsonReport {
	result := jsonReport{Executable: report.Executable, Choice: report.Choice.String(), PreferenceError: report.PreferenceError,
		PolicyError: report.PolicyError, Operation: operation, Findings: []jsonFinding{}}
	for _, finding := range report.Findings {
		item := jsonFinding{Interface: finding.Network.Interface, LocalAddresses: finding.Network.LocalAddresses, Profile: finding.Network.Profile.String(), Protocol: finding.Protocol.String(),
			Verdict: finding.Verdict.String(), Reason: finding.Reason.String(), Repairable: finding.Repairable(), Rules: []jsonRule{}}
		for _, rule := range finding.Rules {
			item.Rules = append(item.Rules, jsonRule{rule.Name, rule.Action.String(), rule.Local, rule.Restrictions})
		}
		result.Findings = append(result.Findings, item)
	}
	return result
}
