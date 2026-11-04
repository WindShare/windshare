package clievent

import (
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/internal/platformsetup"
)

// NetworkSetupObserved records why a transfer did or did not suggest setup.
// The run identity supplies correlation; no host-policy check occurs here.
type NetworkSetupObserved struct {
	command              Command
	decision             platformsetup.StartupDecision
	executable           appobs.DisplayName
	configuredExecutable appobs.DisplayName
	failure              appobs.DisplayName
}

func NewNetworkSetupObserved(command Command, report platformsetup.StartupReport) (NetworkSetupObserved, error) {
	value := NetworkSetupObserved{command, report.Decision, appobs.NewDisplayName(report.Executable),
		appobs.NewDisplayName(report.ConfiguredExecutable), appobs.NewDisplayName(report.Error)}
	if err := value.Validate(); err != nil {
		return NetworkSetupObserved{}, err
	}
	return value, nil
}

func (v NetworkSetupObserved) Command() Command { return v.command }
func (v NetworkSetupObserved) Level() appobs.Level {
	if v.SuggestSetup() {
		return appobs.LevelInfo
	}
	return appobs.LevelDebug
}
func (v NetworkSetupObserved) Decision() string             { return v.decision.String() }
func (v NetworkSetupObserved) SuggestSetup() bool           { return v.decision.SuggestSetup() }
func (v NetworkSetupObserved) Executable() string           { return v.executable.Text() }
func (v NetworkSetupObserved) ConfiguredExecutable() string { return v.configuredExecutable.Text() }
func (v NetworkSetupObserved) Failure() string              { return v.failure.Text() }
func (v NetworkSetupObserved) Validate() error {
	if !v.command.Valid() || !v.decision.Valid() {
		return ErrInvalidEvent
	}
	return nil
}
