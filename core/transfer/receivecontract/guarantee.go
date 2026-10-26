package receivecontract

// Choosing a name does not grant permission to replace an existing object.
type NameChooser uint8

const (
	NameApplicationChosen NameChooser = iota + 1
	NameUserChosen
)

type ReplacementGuarantee uint8

const (
	ReplacementAtomicNoReplace ReplacementGuarantee = iota + 1
)

type DeliveryMode uint8

const (
	DeliveryManagedTarget DeliveryMode = iota + 1
)

type TargetVisibility uint8

const (
	TargetHiddenUntilVerifiedPublication TargetVisibility = iota + 1
	TargetCommittedObjectsVisible
)

type ArtifactAvailability uint8

const (
	ArtifactVerifiedCompleteOnly ArtifactAvailability = iota + 1
	ArtifactCommittedObjectsUsable
)

// Cleanup limits describe the rollback boundary; deleting objects still requires
// the output provider to verify its live control over them.
type CleanupContract uint8

const (
	CleanupRollbackToAbsentBeforePublication CleanupContract = iota + 1
	CleanupNoWholeTargetRollback
)

type GuaranteeProfile uint8

const (
	GuaranteeNativeTree GuaranteeProfile = iota + 1
	GuaranteeManagedAtomic
)

type GuaranteeSet struct {
	profile         GuaranteeProfile
	nameChooser     NameChooser
	replacement     ReplacementGuarantee
	delivery        DeliveryMode
	visibility      TargetVisibility
	availability    ArtifactAvailability
	cleanupContract CleanupContract
}

func NativeTreeGuarantees() GuaranteeSet {
	return GuaranteeSet{
		profile: GuaranteeNativeTree, nameChooser: NameApplicationChosen,
		replacement: ReplacementAtomicNoReplace, delivery: DeliveryManagedTarget,
		visibility: TargetCommittedObjectsVisible, availability: ArtifactCommittedObjectsUsable,
		cleanupContract: CleanupNoWholeTargetRollback,
	}
}

func ManagedAtomicGuarantees(nameChooser NameChooser) (GuaranteeSet, error) {
	if nameChooser != NameApplicationChosen && nameChooser != NameUserChosen {
		return GuaranteeSet{}, ErrInvalidReceiveContract
	}
	return GuaranteeSet{
		profile: GuaranteeManagedAtomic, nameChooser: nameChooser,
		replacement: ReplacementAtomicNoReplace, delivery: DeliveryManagedTarget,
		visibility: TargetHiddenUntilVerifiedPublication, availability: ArtifactVerifiedCompleteOnly,
		cleanupContract: CleanupRollbackToAbsentBeforePublication,
	}, nil
}

func (guarantees GuaranteeSet) valid() bool {
	switch guarantees.profile {
	case GuaranteeNativeTree:
		return guarantees == NativeTreeGuarantees()
	case GuaranteeManagedAtomic:
		expected, err := ManagedAtomicGuarantees(guarantees.nameChooser)
		return err == nil && guarantees == expected
	default:
		return false
	}
}

func (guarantees GuaranteeSet) canonicalBytes() []byte {
	return append(append(append(append(append(append(
		frame([]byte{byte(guarantees.profile)}),
		frame([]byte{byte(guarantees.nameChooser)})...),
		frame([]byte{byte(guarantees.replacement)})...),
		frame([]byte{byte(guarantees.delivery)})...),
		frame([]byte{byte(guarantees.visibility)})...),
		frame([]byte{byte(guarantees.availability)})...),
		frame([]byte{byte(guarantees.cleanupContract)})...)
}

func (guarantees GuaranteeSet) Profile() GuaranteeProfile          { return guarantees.profile }
func (guarantees GuaranteeSet) NameChooser() NameChooser           { return guarantees.nameChooser }
func (guarantees GuaranteeSet) Replacement() ReplacementGuarantee  { return guarantees.replacement }
func (guarantees GuaranteeSet) Delivery() DeliveryMode             { return guarantees.delivery }
func (guarantees GuaranteeSet) TargetVisibility() TargetVisibility { return guarantees.visibility }
func (guarantees GuaranteeSet) ArtifactAvailability() ArtifactAvailability {
	return guarantees.availability
}
func (guarantees GuaranteeSet) CleanupContract() CleanupContract { return guarantees.cleanupContract }
