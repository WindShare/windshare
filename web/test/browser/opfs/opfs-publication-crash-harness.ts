import { IndexedDbReceiveOperationRepository } from '../../../src/output/browser/indexeddb-repository'
import { IndexedDbReceiveResumeSource } from '../../../src/output/browser/indexeddb-resume-state'
import { acquireBrowserReceiveOperationLease } from '../../../src/output/browser/session-lease'
import { openOriginPrivateRetainedArtifactBackend } from '../../../src/output/origin-private/session'
import { ReceiveOperationResumeAuthority } from '../../../src/output/resume/authority'
import { createBrowserReceiveOperationMutationPort } from '../../../src/output/resume/reopen-authority'
import { WorkspaceOperationStages } from '../../../src/output/workspace/stages'
import { createOperationID } from '../../../src/transfer/intent'
import type { BrowserReceiveWindow } from '../../../src/ui/browser-receive/contracts'
import { handoffRetainedWorkspacePackage } from '../../../src/ui/browser-receive/workspace-publication'
import type { DurablePackageFixture, DurableReceiveFixture } from '../durable-recovery-harness'
import { durableIdentities, durableIntent } from '../durable-output-fixture'

export async function interruptRetainedHandoff(fixture: DurablePackageFixture) {
  const intent = await durableIntent(await durableIdentities(fixture.key))
  const repository = await IndexedDbReceiveOperationRepository.open(fixture.checkpointDatabaseName)
  const lease = await acquireBrowserReceiveOperationLease(repository, intent.operationId)
  try {
    const stages = await WorkspaceOperationStages.open({
      repository, receiveIntent: intent, leaseId: lease.leaseId, clock: () => Date.now(), contentRequests: { count: () => 0n },
    })
    const artifact = await stages.readRetainedPackage()
    await stages.startHandoff({
      package: artifact, publicationAttemptId: createOperationID(),
      suggestedName: intent.artifact.suggestedName, packagedFileSupported: true,
    })
    // Stop at the real persisted cut before a browser result can be recorded.
    return { operationId: intent.operationId, packageDigest: artifact.digest }
  } finally {
    await lease.release()
    repository.close()
  }
}

export async function recoverAndSaveInterruptedHandoff(fixture: DurablePackageFixture) {
  const source = await IndexedDbReceiveResumeSource.open(fixture.checkpointDatabaseName)
  const authority = new ReceiveOperationResumeAuthority({
    source,
    mutations: createBrowserReceiveOperationMutationPort({
      checkpointDatabaseName: fixture.checkpointDatabaseName,
      workspaceBudgetDatabaseName: fixture.admissionDatabaseName,
    }),
  })
  const inventory = await authority.listResumeState()
  try {
    const reference = inventory.operations[0]
    if (reference === undefined) throw new Error('Interrupted handoff disappeared from fresh inventory')
    const priorState = reference.descriptor.lifecycle.kind
    const result = await authority.resume(reference)
    if (result.kind !== 'continuation' || result.continuation.kind !== 'workspace-retained') {
      throw new Error('Interrupted handoff did not reopen local artifact authority')
    }
    const operation = result.continuation.operation
    try {
      const backend = await openOriginPrivateRetainedArtifactBackend({
        receiveIntent: operation.intent, operationRepository: operation.repository,
        namespace: operation.namespace, checkpointDatabaseName: fixture.checkpointDatabaseName,
      })
      try {
        const artifact = await operation.stages.readRetainedPackage()
        const state = await handoffRetainedWorkspacePackage(window as BrowserReceiveWindow, operation, backend)
        return { priorState, normalizedState: operation.lifecycle.kind, state: state.kind,
          packageDigest: artifact.digest, objectId: artifact.packageOwnedObjectId }
      } finally { await backend.close() }
    } finally { await operation.close() }
  } finally {
    inventory.close()
    source.close()
  }
}

export async function recoverCompletedOriginal(fixture: DurableReceiveFixture, stopAfterSeal = false) {
  const source = await IndexedDbReceiveResumeSource.open(fixture.checkpointDatabaseName)
  const authority = new ReceiveOperationResumeAuthority({
    source,
    mutations: createBrowserReceiveOperationMutationPort({
      checkpointDatabaseName: fixture.checkpointDatabaseName,
      workspaceBudgetDatabaseName: fixture.admissionDatabaseName,
    }),
  })
  const inventory = await authority.listResumeState()
  try {
    const reference = inventory.operations[0]
    if (reference === undefined) throw new Error('Completed original disappeared from fresh inventory')
    const continuation = reference.descriptor.continuation
    const priorState = reference.descriptor.lifecycle.kind
    const result = await authority.resume(reference)
    if (result.kind !== 'continuation' || result.continuation.kind !== 'workspace-package') {
      throw new Error('Completed original reopened a sender-content route')
    }
    const operation = result.continuation.operation
    try {
      if (stopAfterSeal) return { continuation, priorState, state: operation.lifecycle.kind }
      const packaged = await operation.packageContinuation.execute(new AbortController().signal)
      if (packaged.kind !== 'sealed') throw new Error('Completed original did not package locally')
      const backend = await openOriginPrivateRetainedArtifactBackend({
        receiveIntent: operation.intent, operationRepository: operation.repository,
        namespace: operation.namespace, checkpointDatabaseName: fixture.checkpointDatabaseName,
      })
      try {
        const file = await backend.packagedArtifacts.readPackagedArtifact(packaged.package)
        const state = await handoffRetainedWorkspacePackage(window as BrowserReceiveWindow,
          { ...operation, lifecycle: packaged.state }, backend)
        return { continuation, priorState, state: state.kind,
          objectId: packaged.package.packageOwnedObjectId, bytes: [...new Uint8Array(await file.arrayBuffer())] }
      } finally { await backend.close() }
    } finally { await operation.close() }
  } finally {
    inventory.close()
    source.close()
  }
}
