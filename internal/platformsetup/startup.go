package platformsetup

import (
	"errors"
	"strings"
)

// StartupDecision describes installation history only. It never claims a current
// firewall verdict, so ordinary transfers need no system-policy query.
type StartupDecision uint8

const (
	SetupUnchosen StartupDecision = iota + 1
	SetupSkipped
	SetupRecorded
	SetupPathChanged
	SetupReadFailed
	SetupUnsupported
)

func (v StartupDecision) String() string {
	return enumName(int(v)-1, "unchosen", "skipped", "recorded", "executable-changed", "read-failed", "unsupported")
}
func (v StartupDecision) Valid() bool        { return v >= SetupUnchosen && v <= SetupUnsupported }
func (v StartupDecision) SuggestSetup() bool { return v == SetupUnchosen || v == SetupPathChanged }

type StartupReport struct {
	Decision             StartupDecision
	Executable           string
	ConfiguredExecutable string
	Error                string
}

func sameExecutable(first, second string) bool {
	// Both paths use the Windows adapter's long installation spelling, just like
	// rule ownership. Replacing a binary in place must not repeat setup.
	return strings.EqualFold(first, second)
}

func (service *Service) Startup() StartupReport {
	preference, err := service.Preferences.Load()
	report := StartupReport{Decision: SetupReadFailed, ConfiguredExecutable: preference.Executable}
	if err != nil {
		report.Error = err.Error()
		return report
	}
	if preference.Choice == Skip {
		report.Decision = SetupSkipped
		return report
	}
	report.Executable, err = service.Executable()
	if errors.Is(err, ErrUnsupported) {
		report.Decision = SetupUnsupported
		return report
	}
	if err != nil {
		report.Error = err.Error()
		return report
	}
	switch {
	case preference.Choice == Ask:
		report.Decision = SetupUnchosen
	case sameExecutable(preference.Executable, report.Executable):
		report.Decision = SetupRecorded
	default:
		report.Decision = SetupPathChanged
	}
	return report
}
