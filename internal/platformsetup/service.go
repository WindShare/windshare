package platformsetup

import (
	"context"
	"errors"
	"time"
)

const CheckTimeout = 8 * time.Second

var ErrUnsupported = errors.New("network setup is unsupported on this platform")

type RuleAction uint8

const (
	EnsureRules RuleAction = iota
	RemoveRules
)

type OperationState string

const (
	RulesApplied          OperationState = "applied"
	RulesRemoved          OperationState = "removed"
	AuthorizationDeclined OperationState = "authorization-declined"
	PermissionRequired    OperationState = "permission-required"
	OperationFailed       OperationState = "failed"
	OperationUnsupported  OperationState = "unsupported"
)

type Operation struct {
	State  OperationState `json:"state"`
	Detail string         `json:"detail,omitempty"`
}

func (operation Operation) Succeeded() bool {
	return operation.State == RulesApplied || operation.State == RulesRemoved
}

type Platform interface {
	Inspect(context.Context, string) (Snapshot, error)
	Apply(context.Context, string, RuleAction, bool) Operation
}

type Preferences interface {
	Load() (Preference, error)
	Save(Preference) error
}

type Service struct {
	Platform    Platform
	Preferences Preferences
	Executable  func() (string, error) // Canonical installation path used by rule ownership.
}

type Report struct {
	Executable      string
	Preference      Preference
	PreferenceError string
	InspectionError string
	Findings        []Finding
}

func (report Report) SuggestSetup() bool {
	for _, finding := range report.Findings {
		if finding.SuggestSetup() {
			return true
		}
	}
	return false
}

func New() *Service {
	return &Service{Platform: nativePlatform(), Preferences: FilePreferences{}, Executable: installationExecutable}
}

func (service *Service) Check(ctx context.Context) Report {
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	report := Report{}
	preference, err := service.Preferences.Load()
	report.Preference = preference
	if err != nil {
		report.PreferenceError = err.Error()
	}
	executable, err := service.Executable()
	report.Executable = executable
	if err == nil {
		var snapshot Snapshot
		snapshot, err = service.Platform.Inspect(ctx, executable)
		if err == nil {
			report.Findings = Evaluate(snapshot)
			return report
		}
	}
	reason := ReadFailed
	if errors.Is(err, ErrUnsupported) {
		reason = UnsupportedPlatform
	}
	report.InspectionError, report.Findings = err.Error(), unknownFindings(reason)
	return report
}

func (service *Service) Skip() error { return service.Preferences.Save(Preference{Choice: Skip}) }

// SupportsSetup reports whether Setup can apply host firewall rules for this
// installation. Non-Windows hosts return false through ErrUnsupported.
func (service *Service) SupportsSetup() bool {
	executable := service.Executable
	if executable == nil {
		executable = installationExecutable
	}
	_, err := executable()
	return !errors.Is(err, ErrUnsupported)
}

// Explicit setup always inspects current executable rules, even after a failed
// operation. A successful write records installation history, not reachability.
func (service *Service) Setup(ctx context.Context, action RuleAction, allowElevation bool) (Operation, Report) {
	executable, err := service.Executable()
	if err != nil {
		// Executable() is the first platform gate. Keep its unsupported signal so
		// JSON and human setup output match Check/Startup instead of looking like
		// a failed rule write.
		state := OperationFailed
		if errors.Is(err, ErrUnsupported) {
			state = OperationUnsupported
		}
		return Operation{State: state, Detail: err.Error()}, service.Check(ctx)
	}
	operation := service.Platform.Apply(ctx, executable, action, allowElevation)
	var preferenceErr error
	switch operation.State {
	case RulesApplied:
		preferenceErr = service.Preferences.Save(Preference{Choice: Configure, Executable: executable})
	case RulesRemoved:
		preferenceErr = service.forgetInstallation(executable)
	}
	report := service.Check(ctx)
	if preferenceErr != nil {
		report.PreferenceError = preferenceErr.Error()
	}
	return operation, report
}

func (service *Service) forgetInstallation(executable string) error {
	preference, err := service.Preferences.Load()
	if err != nil {
		return err
	}
	// Removing one installation must preserve a user's skip choice and a receipt
	// belonging to another installation.
	if preference.Choice == Configure && sameExecutable(preference.Executable, executable) {
		return service.Preferences.Save(Preference{})
	}
	return nil
}
