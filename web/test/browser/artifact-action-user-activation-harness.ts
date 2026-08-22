import { encodeBase64Url } from '../../src/crypto/bytes'
import { offerArtifacts, type SelectionProjectionV1 } from '../../src/output/planning'
import { createSelectionSpec, type ArtifactChoiceID } from '../../src/transfer/intent'
import { nextProjectionEpoch } from '../../src/transfer/projection'
import {
  createBrowserReceiveComposition,
  type BrowserReceiveWindow,
} from '../../src/ui/v2-browser-receive-composition'
import { presentArtifactChoice } from '../../src/ui/v2-artifact-presentation'
import {
  V2AuthorityActivationCoordinator,
} from '../../src/ui/controller/authority-activation'
import type { V2ActiveProjection } from '../../src/ui/controller/projection-observation'
import { V2ControllerObservability } from '../../src/ui/controller/controller-observability'

const ACTION_BUTTON_ID = 'windshare-artifact-action-activation'
const ACTION_LABEL = 'Save using original folder hierarchy'

export interface ArtifactChoiceActivationProof {
  readonly actionLabel: string
  readonly selectedArtifactKind: string
  readonly selectedPlanKind: string
  readonly pickerCallsBeforeClick: number
  readonly pickerCalls: number
  readonly pickerMode: string | null
  readonly clickWasTrusted: boolean
  readonly userActivationWasActive: boolean
  readonly pickerStartedBeforeActionReturned: boolean
  readonly firstChoiceAccepted: boolean
  readonly reentrantChoiceAccepted: boolean
  readonly repeatedChoiceAccepted: boolean
  readonly adoptionPreparationCalls: number
  readonly authorityReleased: boolean
}

let proof: ArtifactChoiceActivationProof | undefined
let completion: Promise<void> | undefined

/**
 * The harness stops at acquired authority because namespace mutation belongs to
 * the FSA owner. This browser cut owns only the product click-to-picker boundary.
 */
export async function installArtifactActionActivationHarness(): Promise<ArtifactChoiceActivationProof> {
  document.getElementById(ACTION_BUTTON_ID)?.remove()
  const parent = await originPrivateRoot()
  let pickerCalls = 0
  let adoptionPreparationCalls = 0
  let actionReturned = false
  const activeChoice: { choiceId?: ArtifactChoiceID } = {}
  const coordinatorOwner: { current?: V2AuthorityActivationCoordinator } = {}
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
        reentrantChoiceAccepted: activeChoice.choiceId === undefined
          ? false
          : coordinatorOwner.current?.choose(activeChoice.choiceId) ?? false,
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
  const projection = treeProjection(selection.digest)
  const discovery = Object.freeze({ kind: 'discovering' as const })
  const environment = await composition.environment(new AbortController().signal)
  const offered = await offerArtifacts(projection, discovery, environment)
  if (offered.kind !== 'artifact-actions') {
    throw new TypeError('Browser composition did not offer an artifact action')
  }
  const choice = [offered.primary, ...offered.alternatives]
    .find(candidate => candidate.route.kind === 'direct-tree')
  if (choice === undefined || choice.choice.artifactKind !== 'directory-tree') {
    throw new TypeError('Browser composition did not offer FSA DirectoryTree')
  }
  activeChoice.choiceId = choice.choice.choiceId
  const presented = presentArtifactChoice(choice)
  if (presented.label !== ACTION_LABEL) {
    throw new TypeError('FSA DirectoryTree product label changed unexpectedly')
  }

  proof = freezeProof({
    actionLabel: presented.label,
    selectedArtifactKind: choice.choice.artifactKind,
    selectedPlanKind: choice.choice.plan.kind,
    pickerCallsBeforeClick: pickerCalls,
    pickerCalls,
    pickerMode: null,
    clickWasTrusted: false,
    userActivationWasActive: false,
    pickerStartedBeforeActionReturned: false,
    firstChoiceAccepted: false,
    reentrantChoiceAccepted: false,
    repeatedChoiceAccepted: false,
    adoptionPreparationCalls,
    authorityReleased: false,
  })

  const joined = Object.freeze({}) as V2ActiveProjection['joined']
  const state = Object.freeze({ projection, discovery })
  const active: V2ActiveProjection = {
    revision: 1,
    joined,
    selection,
    frozenSelection: Object.freeze({}) as V2ActiveProjection['frozenSelection'],
    epoch: projection.epoch,
    controller: new AbortController(),
    protocolSessionId: identity(4),
    state,
    environment,
  }
  let publishPlanning: (() => void) | undefined
  const planningPublished = new Promise<void>((resolve) => { publishPlanning = resolve })
  const coordinator = new V2AuthorityActivationCoordinator({
    receive: composition,
    activeReceive: {
      prepareAdoption: () => {
        adoptionPreparationCalls += 1
        proof = freezeProof({ ...requiredProof(), adoptionPreparationCalls })
        throw new Error('unresolved choice must not prepare adoption')
      },
    },
    observability: new V2ControllerObservability({}),
    currentProjection: () => active,
    currentJoinedShare: () => joined,
    choiceBlocked: () => false,
    retryProjection: () => undefined,
    publishProjection: () => publishPlanning?.(),
    adoptReceiveIntent: () => false,
    refreshRetainedInventory: () => undefined,
    publishActionError: () => undefined,
    planner: async () => offered,
    createActivationId: () => identity(5),
  })
  coordinatorOwner.current = coordinator
  coordinator.observeProjection(active)
  await planningPublished

  const button = document.createElement('button')
  button.id = ACTION_BUTTON_ID
  button.type = 'button'
  button.textContent = presented.label
  button.addEventListener('click', (event) => {
    proof = freezeProof({ ...requiredProof(), clickWasTrusted: event.isTrusted })
    try {
      const firstChoiceAccepted = coordinator?.choose(choice.choice.choiceId) ?? false
      const repeatedChoiceAccepted = coordinator?.choose(choice.choice.choiceId) ?? false
      actionReturned = true
      proof = freezeProof({
        ...requiredProof(),
        firstChoiceAccepted,
        repeatedChoiceAccepted,
      })
      Promise.resolve().then(() => {
        coordinator?.close('activation boundary proven')
        proof = freezeProof({ ...requiredProof(), authorityReleased: true })
        complete?.()
      })
    } catch {
      actionReturned = true
      complete?.()
    }
  }, { once: true })
  document.body.append(button)
  return requiredProof()
}

export async function readArtifactActionActivationProof(): Promise<ArtifactChoiceActivationProof> {
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
      layoutBasis: Object.freeze({ kind: 'unsettled' as const }),
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

function requiredProof(): ArtifactChoiceActivationProof {
  if (proof === undefined) throw new DOMException('Activation proof is not installed', 'InvalidStateError')
  return proof
}

function freezeProof(value: ArtifactChoiceActivationProof): ArtifactChoiceActivationProof {
  return Object.freeze(value)
}
