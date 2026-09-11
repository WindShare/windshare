import { openBrowserFolderDelivery } from '../../../src/output/browser-delivery/assembly.ts'
import { RecoveryCostObserver } from '../../../src/output/planning/recovery-cost.ts'
import { browserStagingQuota } from '../../../src/output/planning/staging-storage.ts'
import { DEFAULT_STAGING_BUDGET_POLICY } from '../../../src/output/staging-budget/contracts.ts'
import { contentBytes } from '../fsa-small-file/content.mjs'
import { CHUNK_BYTES, emit, inventory, openCase, verifyFile } from './browser-storage.mjs'
import { deleteFixtureDatabase, identity, loadFixture, openProductTarget } from './product-fixture.mjs'
import { observeStorageTransactions } from './transaction-observation.mjs'

const IMPLEMENTATION = 'WindShare openBrowserFolderDelivery production assembly with real FSA, OPFS Worker, IndexedDB delivery and staging budget'
const SIMULATED_RATE_BYTES_PER_SECOND = 1024n
const held = new Map()

class EvidenceRecoveryCosts extends RecoveryCostObserver {
  observeReceipt() {
    // Locally generated fixture bytes are not a network speed measurement.
  }
  snapshot(atMilliseconds) {
    return { ...super.snapshot(atMilliseconds), receivedBytesPerSecond: SIMULATED_RATE_BYTES_PER_SECOND }
  }
}

function observeTarget(session, fixture, tree, input) {
  const entries = new Map(fixture.input.files.map(file => [file.path, file]))
  return new Proxy({}, {
    get(_unused, key) {
      const target = session
      if (key !== 'beginFile' && key !== 'beginDirectFile') {
        const value = Reflect.get(target, key, target)
        return typeof value === 'function' ? value.bind(target) : value
      }
      return async (request, ...args) => {
        const transaction = await target[key](request, ...args)
        const path = request.materializationRelativePath.join('/')
        const entry = entries.get(path)
        const staged = entry?.placement === 'staged' && fixture.input.mode !== 'direct'
        let written = 0
        return new Proxy({}, {
          get(_unused, property) {
            const file = transaction
            if (property === 'writeRange') return async (offset, bytes, signal) => {
              await file.writeRange(offset, bytes, signal)
              written += bytes.length
              await emit('target-write', { caseId: input.caseId, path, origin: staged ? 'staging' : 'source', bytes: bytes.length })
              if (input.sample !== false) await inventory(tree, input.caseId, 'product-target-write')
              if (staged && input.failAfterBytes !== undefined && input.failAfterBytes !== null && written >= input.failAfterBytes) {
                throw new Error('Injected destination write failure')
              }
            }
            if (property === 'commit') return async signal => {
              const result = await file.commit(signal)
              if (input.sample !== false) await inventory(tree, input.caseId, 'product-target-committed-before-cleanup')
              return result
            }
            const value = Reflect.get(file, property, file)
            return typeof value === 'function' ? value.bind(file) : value
          },
        })
      }
    },
  })
}

async function open(input, reopen = false) {
  const context = await openProductTarget(input, reopen)
  if (input.bypassDelivery) {
    const value = { ...context, materialization: observeTarget(context.target, context.fixture, context.tree, input), drain: async () => undefined }
    held.set(input.caseId, value)
    return value
  }
  const costs = new EvidenceRecoveryCosts()
  let observations = Promise.resolve()
  const materialization = await openBrowserFolderDelivery({
    target: observeTarget(context.target, context.fixture, context.tree, input),
    intent: context.target.intent, storage: { getDirectory: async () => context.tree.stages },
    storageFacts: async () => ({ opfs: 'usable', pressure: 'normal',
      persistence: await navigator.storage.persisted() ? 'persisted' : 'not-persisted',
      quota: browserStagingQuota(await navigator.storage.estimate()) }),
    preference: context.fixture.input.mode === 'direct' ? 'direct' : 'automatic',
    operationLease: { operationId: context.target.intent.operationId, leaseId: identity(80) },
    databaseName: context.fixture.databaseName,
    capacityDatabaseName: `${input.caseId}-capacity`,
    budgetPolicy: { ...DEFAULT_STAGING_BUDGET_POLICY, maximumTaskFiles: 1, maximumSiteFiles: 1 },
    costs,
    trace: trace => {
      if (input.sample === false) return
      observations = observations.then(async () => {
        await emit('production-trace', { caseId: input.caseId, trace: JSON.parse(JSON.stringify(trace, (_, value) => typeof value === 'bigint' ? value.toString() : value)) })
        if (trace.transition === 'copy-failed') await emit('copy-failed', { caseId: input.caseId, path: trace.file_id })
        if (trace.transition === 'cleaned' && trace.placement === 'staged') await emit('stage-removed', { caseId: input.caseId, path: trace.file_id })
      })
    },
  })
  const value = { ...context, materialization, drain: () => observations }
  held.set(input.caseId, value)
  return value
}

export async function runCase(input) {
  const context = await open(input)
  const { materialization, tree, payloadTarget } = context
  await inventory(tree, input.caseId, 'product-ready')
  const stopStorageObservation = observeStorageTransactions()
  const start = performance.now()
  let status = 'saved'
  let storageTransactions
  let milliseconds
  try {
    for (const entry of input.files) {
      await materialization.ensureDirectory(entry.path.split('/').slice(0, -1))
      const transaction = await materialization.beginFile({
        sourceAuthenticationPath: ['photos', ...entry.path.split('/')],
        materializationRelativePath: entry.path.split('/'),
        openRevision: async () => {
          await emit('revision-opened', { caseId: input.caseId, path: entry.path })
          return { fileId: identity(entry.ordinal + 1), fileRevision: identity(entry.ordinal + 100), exactSize: BigInt(entry.sizeBytes) }
        },
      })
      const bytes = contentBytes(entry.ordinal, entry.sizeBytes)
      await emit('source-read', { caseId: input.caseId, path: entry.path, bytes: bytes.length })
      for (let offset = 0; offset < bytes.length; offset += CHUNK_BYTES) {
        await transaction.writeRange(BigInt(offset), bytes.subarray(offset, offset + CHUNK_BYTES))
        if (input.sample !== false) await inventory(tree, input.caseId, 'product-receiving')
      }
      await transaction.commit()
      await context.drain()
      if (input.sample !== false) await inventory(tree, input.caseId, 'product-file-complete')
    }
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)
    if (!message.includes('Injected destination write failure')) throw new Error(`Production evidence receive failed: ${message}`, { cause: error })
    status = 'copy-failed'
  } finally {
    storageTransactions = stopStorageObservation()
    milliseconds = performance.now() - start
    await emit('storage-transactions', { caseId: input.caseId, observation: storageTransactions })
  }
  await context.drain()
  const summary = JSON.parse(JSON.stringify(materialization.summary ?? null, (_, value) => typeof value === 'bigint' ? value.toString() : value))
  await inventory(tree, input.caseId, status)
  const verified = []
  if (status === 'saved') for (const entry of input.files) verified.push(await verifyFile(payloadTarget, entry))
  await materialization.close()
  context.repository.close()
  held.delete(input.caseId)
  return { status, milliseconds, storageTransactions, implementation: input.bypassDelivery ? 'WindShare existing FSA session/checkpoint/ledger without staged-delivery wrapper' : IMPLEMENTATION, simulatedReceivingRateBytesPerSecond: SIMULATED_RATE_BYTES_PER_SECOND.toString(), summary, verified }
}

export async function resumeCase({ caseId }) {
  const context = await open({ caseId, sample: true }, true)
  const start = performance.now()
  await inventory(context.tree, caseId, 'product-reopened-retained')
  await context.materialization.saveStagedFiles()
  await context.drain()
  const verified = []
  for (const entry of context.fixture.input.files) verified.push(await verifyFile(context.payloadTarget, entry))
  await inventory(context.tree, caseId, 'product-offline-saved')
  await context.materialization.close()
  context.repository.close()
  held.delete(caseId)
  return { status: 'saved', milliseconds: performance.now() - start, implementation: IMPLEMENTATION, verified }
}

export async function cleanupCase({ caseId }) {
  const active = held.get(caseId)
  if (active) { await active.materialization.close(); active.repository.close(); held.delete(caseId) }
  const fixture = await loadFixture(caseId)
  await deleteFixtureDatabase(fixture.databaseName)
  await deleteFixtureDatabase(`${caseId}-capacity`)
  await deleteFixtureDatabase(`${caseId}-fixture`)
  const tree = await openCase(caseId, false)
  await tree.root.removeEntry(caseId, { recursive: true })
  await tree.nativeParent?.removeEntry(caseId, { recursive: true })
  await emit('case-cleaned', { caseId })
}
