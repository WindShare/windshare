package clievent

var platformSetupStates = [...]string{"", "configured", "declined", "unavailable"}
var platformSetupReasons = [...]string{
	"", "application-udp-tcp-rules-created", "user-skipped", "rule-removed",
	"firewall-command-unavailable-or-denied", "first-setup-not-run", "status-unreadable",
	"status-invalid", "install-path-changed", "config-directory-unavailable",
	"executable-unavailable", "platform-firewall-setup-unsupported",
}

// PlatformSetupObserved describes installation policy, never ICE reachability.
// Closed codes prevent a mutable status file from injecting trace text or paths.
type PlatformSetupObserved struct {
	command Command
	state   uint8
	reason  uint8
}

func NewPlatformSetupObserved(command Command, state, reason string) (PlatformSetupObserved, error) {
	value := PlatformSetupObserved{command: command}
	for index, name := range platformSetupStates {
		if index != 0 && name == state {
			value.state = uint8(index)
		}
	}
	for index, name := range platformSetupReasons {
		if index != 0 && name == reason {
			value.reason = uint8(index)
		}
	}
	if !command.Valid() || value.state == 0 || value.reason == 0 {
		return PlatformSetupObserved{}, ErrInvalidEvent
	}
	return value, nil
}

func (PlatformSetupObserved) event()                 {}
func (value PlatformSetupObserved) Command() Command { return value.command }
func (PlatformSetupObserved) Level() Level           { return LevelDebug }
func (value PlatformSetupObserved) State() string    { return platformSetupStates[value.state] }
func (value PlatformSetupObserved) Reason() string   { return platformSetupReasons[value.reason] }
func (value PlatformSetupObserved) Accept(visitor Visitor) error {
	if visitor == nil || !value.command.Valid() || value.state == 0 || int(value.state) >= len(platformSetupStates) || value.reason == 0 || int(value.reason) >= len(platformSetupReasons) {
		return ErrInvalidEvent
	}
	return visitor.VisitPlatformSetupObserved(value)
}
