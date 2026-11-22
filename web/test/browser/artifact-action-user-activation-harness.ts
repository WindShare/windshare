import { encodeBase64Url } from '../../src/crypto/bytes'
import { SelectionPolicy } from '../../src/catalog/selection'
import type { JoinedBrowserShare } from '../../src/application/browser-share/gateway'
import { ReceiveCoordinator } from '../../src/application/receive/coordinator'
import type { BrowserOutputWindow } from '../../src/output/backends/contract'
import { presentSavingActions } from '../../src/ui/saving'

const ACTION_BUTTON_ID = 'windshare-artifact-action-activation'
const ACTION_LABEL = 'Save to folder'

export interface ArtifactChoiceActivationProof {
  readonly actionLabel: string
  readonly selectedArtifactKind: string
  readonly selectedBackend: string
  readonly pickerCallsBeforeClick: number
  readonly pickerCalls: number
  readonly pickerMode: string | null
  readonly clickWasTrusted: boolean
  readonly userActivationWasActive: boolean
  readonly pickerStartedBeforeActionReturned: boolean
  readonly firstChoiceAccepted: boolean
  readonly reentrantChoiceAccepted: boolean
  readonly repeatedChoiceAccepted: boolean
  readonly authorityReleased: boolean
}

let proof: ArtifactChoiceActivationProof | undefined
let completion: Promise<void> | undefined

/**
 * Verifies the real Chromium user-activation boundary: clicking the folder download action
 * synchronously calls `showDirectoryPicker({ mode: 'readwrite' })` before the handler returns.
 */
export async function installArtifactActionActivationHarness(): Promise<ArtifactChoiceActivationProof> {
  document.getElementById(ACTION_BUTTON_ID)?.remove()
  const parent = await navigator.storage.getDirectory()
  let pickerCalls = 0
  let actionReturned = false
  let choiceId = ''
  const coordinatorHolder: { current?: ReceiveCoordinator } = {}
  let complete: (() => void) | undefined
  completion = new Promise<void>(resolve => { complete = resolve })

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
        reentrantChoiceAccepted: coordinatorHolder.current?.startDownload(choiceId) ?? false,
      })
      return Promise.resolve(parent)
    },
  })

  const joined = {
    descriptor: {
      shareInstanceId: identity(1),
      syntheticRootId: identity(2),
    },
    selection: new SelectionPolicy(true),
    inspectSelection: async () => ({
      kind: 'tree' as const,
      suggestedName: 'photos',
      knownFileCount: 2,
      knownBytes: 128n,
      complete: true,
    }),
  } as unknown as JoinedBrowserShare

  const coordinator = new ReceiveCoordinator({
    windowPort: window as unknown as BrowserOutputWindow,
    databaseName: `windshare-activation-${crypto.randomUUID()}`,
    currentJoinedShare: () => joined,
  })
  coordinatorHolder.current = coordinator
  coordinator.refreshOffers(joined)
  await waitUntil(() => coordinator!.getSnapshot().offers?.kind === 'ready')

  const offers = coordinator.getSnapshot().offers
  if (offers?.kind !== 'ready') throw new TypeError('Offers did not become ready')
  const fsaChoice = [offers.primary, ...offers.alternatives].find(candidate => candidate.backend === 'fsa')
  if (fsaChoice === undefined) throw new TypeError('Browser environment did not offer FSA folder route')
  choiceId = fsaChoice.choiceId
  const saving = presentSavingActions({ offers })
  const presented = saving.primary
  if (presented === null || presented.choice.choiceId !== fsaChoice.choiceId) {
    throw new TypeError('FSA folder choice is not the primary saving action')
  }
  if (presented.label !== ACTION_LABEL) throw new TypeError('FSA folder label changed unexpectedly')

  proof = freezeProof({
    actionLabel: presented.label,
    selectedArtifactKind: fsaChoice.artifactKind,
    selectedBackend: fsaChoice.backend,
    pickerCallsBeforeClick: pickerCalls,
    pickerCalls,
    pickerMode: null,
    clickWasTrusted: false,
    userActivationWasActive: false,
    pickerStartedBeforeActionReturned: false,
    firstChoiceAccepted: false,
    reentrantChoiceAccepted: false,
    repeatedChoiceAccepted: false,
    authorityReleased: false,
  })

  const button = document.createElement('button')
  button.id = ACTION_BUTTON_ID
  button.type = 'button'
  button.textContent = presented.label
  button.addEventListener('click', event => {
    proof = freezeProof({ ...requiredProof(), clickWasTrusted: event.isTrusted })
    try {
      const firstChoiceAccepted = coordinator?.startDownload(choiceId) ?? false
      const repeatedChoiceAccepted = coordinator?.startDownload(choiceId) ?? false
      actionReturned = true
      proof = freezeProof({ ...requiredProof(), firstChoiceAccepted, repeatedChoiceAccepted })
      Promise.resolve().then(async () => {
        coordinator?.cancelActivation()
        await coordinator?.dispose()
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

async function waitUntil(predicate: () => boolean): Promise<void> {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    if (predicate()) return
    await new Promise(resolve => setTimeout(resolve, 10))
  }
  throw new Error('Timed out waiting for artifact offers')
}

function identity(seed: number): string {
  const bytes = new Uint8Array(16).fill(seed)
  return encodeBase64Url(bytes)
}

function requiredProof(): ArtifactChoiceActivationProof {
  if (proof === undefined) throw new DOMException('Activation proof is not installed', 'InvalidStateError')
  return proof
}

function freezeProof(value: ArtifactChoiceActivationProof): ArtifactChoiceActivationProof {
  return Object.freeze(value)
}
