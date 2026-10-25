package receivecontract

type NameAuthority uint8

const (
	NameApplicationChosen NameAuthority = iota + 1
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

type CleanupAuthority uint8

const (
	CleanupRollbackToAbsentBeforePublication CleanupAuthority = iota + 1
	CleanupNoWholeTargetRollback
)

type GuaranteeProfile uint8

const (
	GuaranteeNativeTree GuaranteeProfile = iota + 1
	GuaranteeManagedAtomic
)

type GuaranteeSet struct {
	profile      GuaranteeProfile
	name         NameAuthority
	replacement  ReplacementGuarantee
	delivery     DeliveryMode
	visibility   TargetVisibility
	availability ArtifactAvailability
	cleanup      CleanupAuthority
}

func NativeTreeGuarantees() GuaranteeSet {
	return GuaranteeSet{
		profile: GuaranteeNativeTree, name: NameApplicationChosen,
		replacement: ReplacementAtomicNoReplace, delivery: DeliveryManagedTarget,
		visibility: TargetCommittedObjectsVisible, availability: ArtifactCommittedObjectsUsable,
		cleanup: CleanupNoWholeTargetRollback,
	}
}

func ManagedAtomicGuarantees(name NameAuthority) (GuaranteeSet, error) {
	if name != NameApplicationChosen && name != NameUserChosen {
		return GuaranteeSet{}, ErrInvalidReceiveContract
	}
	return GuaranteeSet{
		profile: GuaranteeManagedAtomic, name: name,
		replacement: ReplacementAtomicNoReplace, delivery: DeliveryManagedTarget,
		visibility: TargetHiddenUntilVerifiedPublication, availability: ArtifactVerifiedCompleteOnly,
		cleanup: CleanupRollbackToAbsentBeforePublication,
	}, nil
}

func (guarantees GuaranteeSet) valid() bool {
	switch guarantees.profile {
	case GuaranteeNativeTree:
		return guarantees == NativeTreeGuarantees()
	case GuaranteeManagedAtomic:
		expected, err := ManagedAtomicGuarantees(guarantees.name)
		return err == nil && guarantees == expected
	default:
		return false
	}
}

func (guarantees GuaranteeSet) canonicalBytes() []byte {
	return append(append(append(append(append(append(
		frame([]byte{byte(guarantees.profile)}),
		frame([]byte{byte(guarantees.name)})...),
		frame([]byte{byte(guarantees.replacement)})...),
		frame([]byte{byte(guarantees.delivery)})...),
		frame([]byte{byte(guarantees.visibility)})...),
		frame([]byte{byte(guarantees.availability)})...),
		frame([]byte{byte(guarantees.cleanup)})...)
}

func (guarantees GuaranteeSet) Profile() GuaranteeProfile          { return guarantees.profile }
func (guarantees GuaranteeSet) NameAuthority() NameAuthority       { return guarantees.name }
func (guarantees GuaranteeSet) Replacement() ReplacementGuarantee  { return guarantees.replacement }
func (guarantees GuaranteeSet) Delivery() DeliveryMode             { return guarantees.delivery }
func (guarantees GuaranteeSet) TargetVisibility() TargetVisibility { return guarantees.visibility }
func (guarantees GuaranteeSet) ArtifactAvailability() ArtifactAvailability {
	return guarantees.availability
}
func (guarantees GuaranteeSet) CleanupAuthority() CleanupAuthority { return guarantees.cleanup }
