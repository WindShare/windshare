import { describe, expect, it } from 'vitest'
import {
  acquireArtifactReader,
  acquireArtifactWriter,
  withArtifactCleanup,
} from '../../src/output/origin-private/export-readers'

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

  it('rejects writer acquisition while a reader holds the object and defers cleanup while a writer is open', async () => {
    const reader = await acquireArtifactReader('operation-rw')
    await expect(acquireArtifactWriter('operation-rw')).rejects.toMatchObject({ name: 'InvalidStateError' })
    reader.release()

    const writer = await acquireArtifactWriter('operation-rw')
    await expect(acquireArtifactWriter('operation-rw')).rejects.toMatchObject({ name: 'InvalidStateError' })
    let cleaned = false
    const cleanup = withArtifactCleanup('operation-rw', async () => { cleaned = true })
    await Promise.resolve()
    expect(cleaned).toBe(false)
    writer.release()
    await cleanup
    expect(cleaned).toBe(true)
  })
})
