import { describe, expect, it } from 'vitest'
import { acquireArtifactReader, withArtifactCleanup } from '../../src/output/origin-private/export-readers'

describe('artifact export readers', () => {
  it('keeps repeated readers alive independently and defers only their operation cleanup', async () => {
    const first = await acquireArtifactReader('operation-a')
    const second = await acquireArtifactReader('operation-a')
    let removed = false
    const cleanup = withArtifactCleanup('operation-a', async () => { removed = true })
    await withArtifactCleanup('operation-b', async () => undefined)
    expect(removed).toBe(false)
    first.release()
    first.release()
    await Promise.resolve()
    expect(removed).toBe(false)
    second.release()
    await cleanup
    expect(removed).toBe(true)
  })
})
