import { encodeBase64Url } from '../../src/crypto/bytes'
import { offerArtifacts, type SelectionProjectionV1 } from '../../src/output/planning'
import { createSelectionSpec } from '../../src/transfer/intent'
import { nextProjectionEpoch } from '../../src/transfer/projection'
import {
  createBrowserReceiveComposition,
  type BrowserReceiveWindow,
} from '../../src/ui/v2-browser-receive-composition'
import { presentArtifactAction } from '../../src/ui/v2-artifact-presentation'

const ACTION_BUTTON_ID = 'windshare-artifact-action-activation'
const ACTION_LABEL = 'Save using original folder hierarchy'

export interface ArtifactActionActivationProof {
  readonly actionLabel: string
  readonly selectedArtifactKind: string
  readonly selectedPlanKind: string
  readonly pickerCallsBeforeClick: number
  readonly pickerCalls: number
  readonly pickerMode: string | null
  readonly clickWasTrusted: boolean
  readonly userActivationWasActive: boolean
  readonly pickerStartedBeforeActionReturned: boolean
  readonly authorityReleased: boolean
}

let proof: ArtifactActionActivationProof | undefined
let completion: Promise<void> | undefined

/**
 * The harness stops at acquired authority because namespace mutation belongs to
 * the FSA owner. This browser cut owns only the product click-to-picker boundary.
 */
export async function installArtifactActionActivationHarness(): Promise<ArtifactActionActivationProof> {
  document.getElementById(ACTION_BUTTON_ID)?.remove()
  const parent = await originPrivateRoot()
  let pickerCalls = 0
  let actionReturned = false
  let complete: (() => void) | undefined
  completion = new Promise<void>((resolve) => {
    complete = resolve
  })

  Object.defineProperty(window, 'showDirectoryPicker', {
    configurable: true,
    value: (options: { readonly mode?: string }) => {
      pickerCalls += 1
      proof = freezeProof({
        ...requiredProof(),
        pickerCalls,
        pickerMode: options.mode ?? null,
        userActivationWasActive: navigator.userActivation.isActive,
        pickerStartedBeforeActionReturned: !actionReturned,
      })
      return Promise.resolve(parent)
    },
  })

  const composition = createBrowserReceiveComposition(window as BrowserReceiveWindow)
  const selection = await createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const offered = await offerArtifacts(
    treeProjection(selection.digest),
    Object.freeze({ kind: 'complete' }),
    await composition.environment(new AbortController().signal),
  )
  if (offered.kind !== 'artifact-actions') {
    throw new TypeError('Browser composition did not offer an artifact action')
  }
  const action = [offered.primary, ...offered.alternatives]
    .find(candidate => candidate.plan.kind === 'direct-tree')
  if (action === undefined || action.artifact?.kind !== 'directory-tree') {
    throw new TypeError('Browser composition did not offer FSA DirectoryTree')
  }
  const presented = presentArtifactAction(action)
  if (presented.label !== ACTION_LABEL) {
    throw new TypeError('FSA DirectoryTree product label changed unexpectedly')
  }

  proof = freezeProof({
    actionLabel: presented.label,
    selectedArtifactKind: action.artifact.kind,
    selectedPlanKind: action.plan.kind,
    pickerCallsBeforeClick: pickerCalls,
    pickerCalls,
    pickerMode: null,
    clickWasTrusted: false,
    userActivationWasActive: false,
    pickerStartedBeforeActionReturned: false,
    authorityReleased: false,
  })

  const button = document.createElement('button')
  button.id = ACTION_BUTTON_ID
  button.type = 'button'
  button.textContent = presented.label
  button.addEventListener('click', (event) => {
    proof = freezeProof({ ...requiredProof(), clickWasTrusted: event.isTrusted })
    try {
      const authority = composition.startArtifactAuthority(action)
      actionReturned = true
      Promise.resolve(authority).then((started) => {
        started.release('activation boundary proven')
        proof = freezeProof({ ...requiredProof(), authorityReleased: true })
        complete?.()
      }, () => complete?.())
    } catch {
      actionReturned = true
      complete?.()
    }
  }, { once: true })
  document.body.append(button)
  return requiredProof()
}

export async function readArtifactActionActivationProof(): Promise<ArtifactActionActivationProof> {
  await completion
  return requiredProof()
}

function treeProjection(selectionDigest: string): SelectionProjectionV1 {
  const directoryId = identity(3)
  const selectedRoot = Object.freeze({
    kind: 'directory' as const,
    directoryId,
    sourcePath: 'photos',
    portableName: 'photos',
  })
  return Object.freeze({
    version: 1 as const,
    epoch: nextProjectionEpoch(0n),
    selectionDigest,
    selectedRoots: Object.freeze([selectedRoot]),
    selectedRootCountLowerBound: 1,
    selectedRootsTruncated: false,
    generations: Object.freeze([]),
    metrics: Object.freeze({
      fileCountLowerBound: 1,
      directoryCountLowerBound: 1,
      byteCountLowerBound: 1n,
    }),
    unsettledTargets: Object.freeze([]),
    proof: Object.freeze({
      kind: 'tree' as const,
      selectedRoots: Object.freeze([selectedRoot]),
      selectedRootCountLowerBound: 1,
      selectedRootsTruncated: false,
      layoutBasis: Object.freeze({
        kind: 'complete-directory' as const,
        anchor: Object.freeze({ directoryId, sourcePath: 'photos' }),
      }),
    }),
  })
}

async function originPrivateRoot(): Promise<FileSystemDirectoryHandle> {
  const storage = navigator.storage as StorageManager & {
    getDirectory(): Promise<FileSystemDirectoryHandle>
  }
  return storage.getDirectory()
}

function identity(seed: number): string {
  const bytes = new Uint8Array(16)
  bytes[0] = seed
  bytes[bytes.length - 1] = seed ^ 0xff
  return encodeBase64Url(bytes)
}

function requiredProof(): ArtifactActionActivationProof {
  if (proof === undefined) throw new DOMException('Activation proof is not installed', 'InvalidStateError')
  return proof
}

function freezeProof(value: ArtifactActionActivationProof): ArtifactActionActivationProof {
  return Object.freeze(value)
}
