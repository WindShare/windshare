package platformsetup

import (
	"context"
	"errors"
	"os"
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
	Load() (Choice, error)
	Save(Choice) error
}

type Service struct {
	Platform    Platform
	Preferences Preferences
	Executable  func() (string, error)
}

type Report struct {
	Executable      string
	Choice          Choice
	PreferenceError string
	PolicyError     string
	Findings        []Finding
}

func (report Report) Repairable() bool {
	for _, finding := range report.Findings {
		if finding.Repairable() {
			return true
		}
	}
	return false
}

func New() *Service {
	return &Service{Platform: nativePlatform(), Preferences: FilePreferences{}, Executable: os.Executable}
}

func (service *Service) Check(ctx context.Context) Report {
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	report := Report{}
	choice, err := service.Preferences.Load()
	report.Choice = choice
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
	report.PolicyError, report.Findings = err.Error(), unknownFindings(reason)
	return report
}

func (service *Service) Skip() error { return service.Preferences.Save(Skip) }

// Setup always rechecks policy, including after a failed or declined operation.
// A successful rule write is deliberately not a successful-connectivity verdict.
func (service *Service) Setup(ctx context.Context, action RuleAction, allowElevation bool) (Operation, Report) {
	executable, err := service.Executable()
	if err != nil {
		return Operation{State: OperationFailed, Detail: err.Error()}, service.Check(ctx)
	}
	operation := service.Platform.Apply(ctx, executable, action, allowElevation)
	var preferenceErr error
	if operation.State == RulesApplied {
		preferenceErr = service.Preferences.Save(Configure)
	}
	report := service.Check(ctx)
	if preferenceErr != nil {
		report.PreferenceError = preferenceErr.Error()
	}
	return operation, report
}
