import { openOriginPrivateRetainedArtifactBackend } from '../../origin-private/session'
import { nextReceiveLifecycleState } from '../../workspace/state'
import type { WorkspaceOperationStages } from '../../workspace/stages'
import type { ReopenLifecycleAuthority } from './model'
import type { WorkspaceContinuationInput } from './workspace-continuation-authority'

/** A browser handoff has no durable success acknowledgment; its sealed source remains reusable. */
export async function recoverLocalWorkspaceArtifact(
  input: WorkspaceContinuationInput,
  stages: WorkspaceOperationStages,
  checkpointDatabaseName?: string,
): Promise<ReopenLifecycleAuthority> {
  const state = input.snapshot.lifecycle
  if (state.kind !== 'artifact-sealed' && state.kind !== 'handing-off') {
    throw new TypeError('Artifact recovery requires an interrupted local publication')
  }
  const artifact = await stages.readRetainedPackage()
  const backend = await openOriginPrivateRetainedArtifactBackend({
    receiveIntent: input.snapshot.operation.receiveIntent,
    operationRepository: input.repository,
    namespace: input.target.namespace,
    ...(checkpointDatabaseName === undefined ? {} : { checkpointDatabaseName }),
    ...(input.diagnostics === undefined ? {} : { diagnostics: input.diagnostics }),
  })
  try {
    await backend.packagedArtifacts.readPackagedArtifact(artifact)
    const lifecycle = nextReceiveLifecycleState(state, {
      kind: 'waiting-to-save', packageDigest: artifact.digest,
    })
    await input.repository.commitTransition({
      operationId: state.operationId,
      expectedLifecycleGeneration: state.generation,
      expectedLeaseId: input.lease.leaseId,
      lifecycle,
    })
    return Object.freeze({ lifecycle, stages })
  } finally {
    await backend.close()
  }
}
