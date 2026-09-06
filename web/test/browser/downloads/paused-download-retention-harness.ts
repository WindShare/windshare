import { createBrowserReceiveOperationMutationPort } from '../../../src/output/resume/reopen-authority'
import { offerArtifacts } from '../../../src/output/planning'
import { createSelectionSpec } from '../../../src/transfer/intent'
import { V2SelectionPolicy } from '../../../src/catalog/v2-selection'
import { TransferJob } from '../../../src/transfer/v2-job'
import { EMPTY_TRANSFER_FAILURE_SUMMARY, transferWorkerSettlement } from '../../../src/transfer/outcome'
import type { V2PlanExecutionAuthority } from '../../../src/transfer/output-session'
import { createBrowserReceiveComposition, type BrowserReceiveWindow } from '../../../src/ui/v2-browser-receive-composition'
import { catalogFixture, directoryEntry, fileEntry, identity, identityText, readerFixture } from '../../transfer/v2-job-fixture'
import { commitProductionChoice, productZipProjection } from '../durable-preparation-harness'

export async function provePausedDownloadRetention() {
  const selection = await createSelectionSpec({
    shareInstance: identityText(1), syntheticRoot: identityText(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const directory = directoryEntry(identity(30), 'micro-share')
  const file = fileEntry(identity(20), 'retained.bin', 4n)
  const projection = productZipProjection(selection.digest, directory.idText)
  const exportHandle = await (await navigator.storage.getDirectory())
    .getFileHandle(`partial-export-${crypto.randomUUID()}.zip`, { create: true })
  const windowPort = new Proxy(window, { get(target, property): unknown {
    if (property === 'showSaveFilePicker') return async () => exportHandle
    return Reflect.get(target, property, target)
  } }) as BrowserReceiveWindow
  const composition = createBrowserReceiveComposition(windowPort, {
    resumeMutations: createBrowserReceiveOperationMutationPort(),
  })
  const signal = new AbortController().signal
  const environment = await composition.environment(signal)
  const offered = await offerArtifacts(projection, { kind: 'complete' }, environment)
  if (offered.kind !== 'artifact-actions') throw new Error('Missing artifact offers')
  const choice = [offered.primary, ...offered.alternatives].find(candidate =>
    candidate.route.kind === 'workspace-then-publish' && candidate.choice.artifactKind === 'zip-archive')
  if (choice === undefined) throw new Error('Missing durable ZIP route')
  const display = { objectLabel: 'Retained folder', createdAtMilliseconds: 1234 }
  const runtime = await commitProductionChoice(composition, selection, projection, environment, choice, signal, display)
  try {
    const catalog = catalogFixture([
      { id: identity(2), entries: [directory], generation: identity(90) },
      { id: directory.id, entries: [file], generation: identity(91) },
    ])
    const readers = readerFixture([file])
    const openWorkspaceZip: V2PlanExecutionAuthority['openWorkspaceZip'] = async (intent, openSignal) => {
      const admission = await runtime.plans.openWorkspaceZip(intent, openSignal)
      if (admission.kind !== 'accepted') return admission
      const execution = admission.execution
      return { kind: 'accepted', execution: {
        ...execution,
        // Stop at the same durable cut as a user pause after the first complete file.
        // Keeping discovery open ensures inventory cannot infer local finalization.
        discoveryComplete: async () => {},
        settle: (request, settleSignal) => execution.pause({
          ...request, worker: transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY),
          reason: 'user pause with completed local file',
          selectionFacts: { discoveredFileCount: 1n, discoveredBytes: 4n, discovery: 'complete' },
        }, settleSignal),
      } }
    }
    const result = await new TransferJob({
      descriptor: { shareInstance: identity(1), syntheticRoot: identity(2),
        syntheticRootId: identityText(2), chunkSize: 2 } as never,
      catalog: catalog.catalog, selection: new V2SelectionPolicy(true), intent: runtime.intent,
      plans: { ...runtime.plans, openWorkspaceZip }, revisions: readers.revisions, broker: readers.broker,
      lanes: { size: 1 }, transferJobId: runtime.transferJobId,
      revisionCapacity: { generation: { waitForProtocolSessionReplacement: async () => {
        throw new Error('Fixture does not replace its protocol session')
      } } },
    }).run(signal)
    const before = await composition.retained.list(signal)
    const hiddenWhileOwned = !before.operations.some(row => row.operationId === runtime.intent.operationId)
    before.close()
    await runtime.detach()
    const inventory = await composition.retained.list(signal)
    try {
      const row = inventory.operations.find(candidate => candidate.operationId === runtime.intent.operationId)
      if (row === undefined) throw new Error('Retained operation disappeared after detach')
      const copiedRejected = await Promise.resolve(inventory.act({ ...row }, 'save-partial', signal))
        .then(() => false, () => true)
      await inventory.act(row, 'save-partial', signal)
      const exportedBytes = [...new Uint8Array(await (await exportHandle.getFile()).arrayBuffer())]
      return {
        hiddenWhileOwned, copiedRejected, exportedBytes, operationId: row.operationId,
        sameOperation: row.operationId === runtime.intent.operationId,
        sameIntent: row.receiveIntentDigest === runtime.intent.digest,
        sameGeneration: row.lifecycleGeneration === result.lifecycle.generation,
        lifecycle: row.lifecycle.kind, continuation: row.continuation,
        display: row.display, actions: row.actions,
        completeFileCount: row.lifecycle.kind === 'resumable-receive' &&
          row.lifecycle.payloadKind === 'opfs-zip' ? row.lifecycle.completedFileCount.toString() : null,
      }
    } finally { inventory.close() }
  } finally { await runtime.detach() }
}
