package clievent

import (
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/internal/platformsetup"
)

// NetworkPolicyObserved carries current policy classifications in a run's trace.
// User choice and policy are independent; neither asserts actual reachability.
type NetworkPolicyObserved struct {
	command       Command
	choice        platformsetup.Choice
	profile       platformsetup.Profile
	protocol      platformsetup.Protocol
	verdict       platformsetup.Verdict
	reason        platformsetup.Reason
	suggest       bool
	interfaceName appobs.DisplayName
}

func NewNetworkPolicyObserved(command Command, choice platformsetup.Choice, finding platformsetup.Finding, suggest bool) (NetworkPolicyObserved, error) {
	value := NetworkPolicyObserved{command, choice, finding.Network.Profile, finding.Protocol, finding.Verdict, finding.Reason, suggest, appobs.NewDisplayName(finding.Network.Interface)}
	if err := value.Validate(); err != nil {
		return NetworkPolicyObserved{}, err
	}
	return value, nil
}

func (v NetworkPolicyObserved) Command() Command { return v.command }
func (v NetworkPolicyObserved) Level() appobs.Level {
	if v.suggest {
		return appobs.LevelInfo
	}
	return appobs.LevelDebug
}
func (v NetworkPolicyObserved) Choice() string     { return v.choice.String() }
func (v NetworkPolicyObserved) Profile() string    { return v.profile.String() }
func (v NetworkPolicyObserved) Protocol() string   { return v.protocol.String() }
func (v NetworkPolicyObserved) Verdict() string    { return v.verdict.String() }
func (v NetworkPolicyObserved) Reason() string     { return v.reason.String() }
func (v NetworkPolicyObserved) SuggestSetup() bool { return v.suggest }
func (v NetworkPolicyObserved) Interface() string  { return v.interfaceName.Text() }
func (v NetworkPolicyObserved) Validate() error {
	if !v.command.Valid() || !v.choice.Valid() || !v.profile.Valid() || !v.protocol.Valid() || !v.verdict.Valid() || !v.reason.Valid() ||
		v.suggest && (v.choice == platformsetup.Skip || v.verdict != platformsetup.Blocked || v.reason != platformsetup.DefaultBlock) {
		return ErrInvalidEvent
	}
	return nil
}
