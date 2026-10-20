package turnpath

// CleanupOutcome describes local TURN teardown. Protocol cleanup does not
// assert that the remote service acknowledged allocation deletion.
type CleanupOutcome uint8

const (
	CleanupProtocol CleanupOutcome = iota + 1
	CleanupDeadline
)

func (outcome CleanupOutcome) Valid() bool {
	return outcome == CleanupProtocol || outcome == CleanupDeadline
}
func (outcome CleanupOutcome) EventKind() string {
	switch outcome {
	case CleanupProtocol:
		return "turn_close_protocol"
	case CleanupDeadline:
		return "turn_close_forced"
	default:
		return ""
	}
}
func (outcome CleanupOutcome) Reason() string {
	if outcome == CleanupDeadline {
		return "protocol_cleanup_deadline"
	}
	return ""
}
