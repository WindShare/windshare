// Package connectionpath defines transport-neutral path lifecycle facts.
package connectionpath

// Milestone belongs to the TURN lifecycle. Consumers retain this vocabulary
// instead of maintaining a second list that can reject a producer's new event.
type Milestone uint8

const (
	MilestoneIssuanceStarted Milestone = iota + 1
	MilestoneIssuanceFailed
	MilestoneIssuanceDiscarded
	MilestoneCredentialsDelegated
	MilestoneReplacementAdmitted
	MilestonePathRetired
	MilestoneUsefulContent
	MilestoneConnecting
	MilestoneReady
	MilestoneRetired
	MilestoneUnavailable
	MilestoneRecoveryRequested
	MilestoneTransportSelection
	MilestoneEvaluationUnavailable
	milestoneLimit
)

var milestoneNames = [milestoneLimit]string{
	MilestoneIssuanceStarted:       "issuance_started",
	MilestoneIssuanceFailed:        "issuance_failed",
	MilestoneIssuanceDiscarded:     "issuance_discarded",
	MilestoneCredentialsDelegated:  "credentials_delegated",
	MilestoneReplacementAdmitted:   "replacement_admitted",
	MilestonePathRetired:           "path_retired",
	MilestoneUsefulContent:         "useful_content",
	MilestoneConnecting:            "connecting",
	MilestoneReady:                 "ready",
	MilestoneRetired:               "retired",
	MilestoneUnavailable:           "unavailable",
	MilestoneRecoveryRequested:     "recovery_requested",
	MilestoneTransportSelection:    "transport_selection",
	MilestoneEvaluationUnavailable: "evaluation_unavailable",
}

func (milestone Milestone) Name() (string, bool) {
	if milestone == 0 || milestone >= milestoneLimit {
		return "", false
	}
	name := milestoneNames[milestone]
	return name, name != ""
}

func (milestone Milestone) String() string {
	name, _ := milestone.Name()
	return name
}

// Milestones lets adapters exercise the complete source contract. The range is
// derived from the enum, so adding a value also adds it to consumer contract tests.
func Milestones() []Milestone {
	result := make([]Milestone, 0, milestoneLimit-1)
	for milestone := Milestone(1); milestone < milestoneLimit; milestone++ {
		result = append(result, milestone)
	}
	return result
}

// Unavailability can be reported before a service has been resolved.
func (milestone Milestone) RequiresServiceIdentity() bool {
	return milestone != MilestoneUnavailable
}
