package networkcommand

import "github.com/windshare/windshare/internal/platformsetup"

type jsonFinding struct {
	Interface    string     `json:"interface"`
	Profile      string     `json:"profile"`
	Protocol     string     `json:"protocol"`
	Status       string     `json:"status"`
	Reason       string     `json:"reason"`
	SuggestSetup bool       `json:"suggest_setup"`
	Rules        []jsonRule `json:"rules"`
}

type jsonRule struct {
	Name         string `json:"name"`
	Action       string `json:"action"`
	Local        bool   `json:"local"`
	Restrictions string `json:"restrictions,omitempty"`
}

type jsonReport struct {
	Scope                string                   `json:"scope"`
	Executable           string                   `json:"executable"`
	Choice               string                   `json:"choice"`
	ConfiguredExecutable string                   `json:"configured_executable,omitempty"`
	PreferenceError      string                   `json:"preference_error,omitempty"`
	InspectionError      string                   `json:"inspection_error,omitempty"`
	Operation            *platformsetup.Operation `json:"operation,omitempty"`
	Findings             []jsonFinding            `json:"findings"`
}

func newJSONReport(report platformsetup.Report, operation *platformsetup.Operation) jsonReport {
	result := jsonReport{Scope: platformsetup.InspectionScope, Executable: report.Executable, Choice: report.Preference.Choice.String(),
		ConfiguredExecutable: report.Preference.Executable, PreferenceError: report.PreferenceError,
		InspectionError: report.InspectionError, Operation: operation, Findings: []jsonFinding{}}
	for _, finding := range report.Findings {
		item := jsonFinding{Interface: finding.Network.Interface, Profile: finding.Network.Profile.String(), Protocol: finding.Protocol.String(),
			Status: finding.Status.String(), Reason: finding.Reason.String(), SuggestSetup: finding.SuggestSetup(), Rules: []jsonRule{}}
		for _, rule := range finding.Rules {
			item.Rules = append(item.Rules, jsonRule{rule.Name, rule.Action.String(), rule.Local, rule.Restrictions})
		}
		result.Findings = append(result.Findings, item)
	}
	return result
}
