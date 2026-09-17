import { describe, expect, it } from 'vitest'

import { collisionName, MAX_RESULT_COMPONENT_BYTES } from '../../src/transfer/intent'
import { loadVectorFile } from '../vectors'

function requiredRecord(value: unknown): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new TypeError('invalid collision-name vector record')
  }
  return value as Record<string, unknown>
}

const vectors = loadVectorFile(new URL('../../../core/testvectors/name-collision-v1.json', import.meta.url))

describe('Go↔TypeScript collision names', () => {
  for (const vector of vectors.cases) {
    it(`preserves a usable deterministic name: ${vector.name}`, async () => {
      const { operationId, requestedName, collisionIndex, fileLike } = requiredRecord(vector.input)
      const { reservedName } = requiredRecord(vector.expected)
      if (typeof operationId !== 'string' || typeof requestedName !== 'string' ||
          typeof collisionIndex !== 'number' || typeof fileLike !== 'boolean' ||
          typeof reservedName !== 'string') {
        throw new TypeError('invalid collision-name vector')
      }

      await expect(collisionName(operationId, requestedName, 0, fileLike)).resolves.toBe(requestedName)
      await expect(collisionName(operationId, requestedName, collisionIndex, fileLike)).resolves.toBe(reservedName)
      await expect(collisionName(operationId, requestedName, collisionIndex, fileLike)).resolves.toBe(reservedName)
      const next = await collisionName(operationId, requestedName, collisionIndex + 1, fileLike)
      expect(next).not.toBe(reservedName)
      expect(new TextEncoder().encode(reservedName).byteLength).toBeLessThanOrEqual(MAX_RESULT_COMPONENT_BYTES)
    })
  }
})
