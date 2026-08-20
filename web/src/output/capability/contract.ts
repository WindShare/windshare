import type { BrowserHandoffCapabilityRuntime } from '../portable/packaged-handoff'
import type {
  BrowserHandoffTargetOffer,
  EnvironmentOffers,
  FSADirectoryContainerOffer,
} from '../planning'

export const FSA_PARENT_DIRECTORY_ROUTE_ID = 'browser-fsa-parent-directory'
export const BROWSER_HANDOFF_TARGET_ROUTE_ID = 'browser-object-url-handoff'

export interface DirectoryPickerOptions {
  readonly mode: 'readwrite'
}

export interface BrowserCapabilityRuntime {
  /** The adapter calls this directly in the artifact-action stack. */
  readonly showDirectoryPicker?: (
    options: DirectoryPickerOptions,
  ) => Promise<FileSystemDirectoryHandle>
  readonly browserHandoff?: BrowserHandoffCapabilityRuntime
}

export interface AcquiredFSAParentAuthority {
  readonly kind: 'fsa-parent-directory-authority'
  readonly targetRouteId: string
  readonly offer: FSADirectoryContainerOffer
  readonly parent: FileSystemDirectoryHandle
}

export interface BrowserEnvironmentSnapshot {
  readonly offers: EnvironmentOffers
  readonly fsaParent: FSADirectoryContainerOffer | null
  readonly browserHandoff: BrowserHandoffTargetOffer | null
}

export interface AuthorityAcquiredDecision {
  readonly name: 'receive.authority.acquired'
  readonly operation_id_present: false
  readonly authority_kind: 'fsa-container'
  readonly name_authority: 'application-chosen'
  readonly replacement_guarantee: 'coordinated-no-replace'
  readonly delivery_mode: 'managed-target'
  readonly commit_visibility: 'prefix-visible'
  readonly rollback_guarantee: 'none'
}

export type CapabilityTrace = (decision: AuthorityAcquiredDecision) => void
