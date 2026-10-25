import type { ContentReplica } from '../../src/transfer/output/content-availability'

export function contentReplica(generation = 1n): ContentReplica {
  return { backend: 'test', operationId: 'test-operation', objectId: 'test-object',
    checkpointId: 'test-checkpoint', generation, payloadOffset: 0n }
}
