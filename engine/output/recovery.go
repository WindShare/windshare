package output

type AttentionReason string

const (
	AttentionNone        AttentionReason = ""
	AttentionDestination AttentionReason = "destination-ownership-unknown"
	AttentionRegistry    AttentionReason = "registry-ownership-unknown"
	AttentionLease       AttentionReason = "lease-ownership-unknown"
	AttentionOperation   AttentionReason = "operation-ownership-unknown"
	AttentionCleanup     AttentionReason = "cleanup-uncertain"
)

func (reason AttentionReason) Valid() bool {
	switch reason {
	case AttentionDestination, AttentionRegistry, AttentionLease, AttentionOperation, AttentionCleanup:
		return true
	default:
		return false
	}
}
