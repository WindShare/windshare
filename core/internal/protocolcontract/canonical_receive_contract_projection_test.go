package protocolcontract

import (
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func artifactKindString(value receivecontract.ArtifactKind) string {
	switch value {
	case receivecontract.ArtifactOriginalFile:
		return "original-file"
	case receivecontract.ArtifactDirectoryTree:
		return "directory-tree"
	case receivecontract.ArtifactZipArchive:
		return "zip-archive"
	default:
		panic("unknown artifact kind")
	}
}

func directoryTreeLayoutKindString(value receivecontract.DirectoryTreeLayoutKind) string {
	switch value {
	case receivecontract.DirectoryTreeSingleFile:
		return "single-file"
	case receivecontract.DirectoryTreeResultRoot:
		return "result-root"
	case receivecontract.DirectoryTreeCatalogRoot:
		return "catalog-root"
	default:
		panic("unknown directory-tree layout")
	}
}

func resultRootClassString(value receivecontract.ResultRootClass) string {
	switch value {
	case receivecontract.ResultRootCompleteDirectory:
		return "complete-directory"
	case receivecontract.ResultRootDirectorySelection:
		return "directory-selection"
	case receivecontract.ResultRootSyntheticSelection:
		return "synthetic-selection"
	default:
		panic("unknown result-root class")
	}
}

func materializationPlanKindString(value receivecontract.MaterializationPlanKind) string {
	switch value {
	case receivecontract.PlanDirectTree:
		return "direct-tree"
	case receivecontract.PlanDirectAtomic:
		return "direct-atomic"
	case receivecontract.PlanWorkspaceThenPublish:
		return "workspace-then-publish"
	case receivecontract.PlanPortableHandoff:
		return "portable-handoff"
	case receivecontract.PlanDirectResumableZIP:
		return "direct-resumable-zip"
	default:
		panic("unknown materialization plan")
	}
}

func preparationPolicyString(value receivecontract.PreparationPolicy) string {
	switch value {
	case receivecontract.PreparationNone:
		return "none"
	case receivecontract.PreparationExactZip:
		return "exact-zip"
	case receivecontract.PreparationExactArtifact:
		return "exact-artifact"
	default:
		panic("unknown preparation policy")
	}
}

func destinationReservationKindString(value receivecontract.DestinationReservationKind) string {
	switch value {
	case receivecontract.ReservationContainerRoot:
		return "container-root"
	case receivecontract.ReservationNamedContainerEntry:
		return "named-container-entry"
	case receivecontract.ReservationAtomicTarget:
		return "atomic-target"
	default:
		panic("unknown destination reservation")
	}
}

func authorityKindString(value receivecontract.AuthorityKind) string {
	switch value {
	case receivecontract.AuthorityNativeContainer:
		return "native-container"
	case receivecontract.AuthorityFSAContainer:
		return "fsa-container"
	case receivecontract.AuthorityManagedAtomicTarget:
		return "managed-atomic-target"
	default:
		panic("unknown authority kind")
	}
}

func containerEntryKindString(value receivecontract.ContainerEntryKind) string {
	switch value {
	case receivecontract.ContainerEntrySingleFile:
		return "single-file"
	case receivecontract.ContainerEntryResultRoot:
		return "result-root"
	default:
		panic("unknown container entry kind")
	}
}

func guaranteeProfileString(value receivecontract.GuaranteeProfile) string {
	switch value {
	case receivecontract.GuaranteeNativeTree:
		return "native-tree"
	case receivecontract.GuaranteeFSATree:
		return "fsa-tree"
	case receivecontract.GuaranteeManagedAtomic:
		return "managed-atomic"
	case receivecontract.GuaranteeBrowserHandoff:
		return "browser-handoff"
	case receivecontract.GuaranteeFSAOwnedFile:
		return "fsa-owned-file"
	default:
		panic("unknown guarantee profile")
	}
}

func nameAuthorityString(value receivecontract.NameAuthority) string {
	switch value {
	case receivecontract.NameApplicationChosen:
		return "application-chosen"
	case receivecontract.NameUserChosen:
		return "user-chosen"
	case receivecontract.NameBrowserChosen:
		return "browser-chosen"
	default:
		panic("unknown name authority")
	}
}

func replacementGuaranteeString(value receivecontract.ReplacementGuarantee) string {
	switch value {
	case receivecontract.ReplacementAtomicNoReplace:
		return "atomic-no-replace"
	case receivecontract.ReplacementCoordinatedNoReplace:
		return "coordinated-no-replace"
	case receivecontract.ReplacementUserAuthorizedReplace:
		return "user-authorized-replace"
	case receivecontract.ReplacementUnknown:
		return "unknown"
	default:
		panic("unknown replacement guarantee")
	}
}

func deliveryModeString(value receivecontract.DeliveryMode) string {
	switch value {
	case receivecontract.DeliveryManagedTarget:
		return "managed-target"
	case receivecontract.DeliveryBrowserHandoff:
		return "browser-handoff"
	default:
		panic("unknown delivery mode")
	}
}

func targetVisibilityString(value receivecontract.TargetVisibility) string {
	switch value {
	case receivecontract.TargetHiddenUntilVerifiedPublication:
		return "hidden-until-verified-publication"
	case receivecontract.TargetCommittedObjectsVisible:
		return "committed-objects-visible"
	case receivecontract.TargetUnobservable:
		return "unobservable"
	case receivecontract.TargetOperationOwnedIncompleteFileVisible:
		return "operation-owned-incomplete-file-visible"
	default:
		panic("unknown target visibility")
	}
}

func artifactAvailabilityString(value receivecontract.ArtifactAvailability) string {
	switch value {
	case receivecontract.ArtifactVerifiedCompleteOnly:
		return "verified-complete-only"
	case receivecontract.ArtifactCommittedObjectsUsable:
		return "committed-objects-usable"
	case receivecontract.ArtifactHandoffOnly:
		return "handoff-only"
	default:
		panic("unknown artifact availability")
	}
}

func cleanupAuthorityString(value receivecontract.CleanupAuthority) string {
	switch value {
	case receivecontract.CleanupRollbackToAbsentBeforePublication:
		return "rollback-to-absent-before-publication"
	case receivecontract.CleanupNoWholeTargetRollback:
		return "no-whole-target-rollback"
	case receivecontract.CleanupOwnershipProofRequired:
		return "ownership-proof-required"
	case receivecontract.CleanupNoManagedCleanup:
		return "no-managed-cleanup"
	default:
		panic("unknown cleanup authority")
	}
}

func directoryAdmissionLayoutString(value transfer.DirectoryAdmissionLayout) string {
	switch value {
	case transfer.DirectoryAdmissionTreeSingleFile:
		return "directory-tree-single-file"
	case transfer.DirectoryAdmissionTreeResultRoot:
		return "directory-tree-result-root"
	case transfer.DirectoryAdmissionTreeCatalogRoot:
		return "directory-tree-catalog-root"
	case transfer.DirectoryAdmissionZipResultRoot:
		return "zip-result-root"
	default:
		panic("unknown directory admission layout")
	}
}
