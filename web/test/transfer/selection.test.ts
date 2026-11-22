import { describe, expect, it } from 'vitest'
import { SelectionPolicy } from '../../src/catalog/selection'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createTaskId,
  policyFromSelectionSpec,
  selectionKey,
  selectionSpecFromPolicy,
} from '../../src/transfer/selection'

describe('selection specification', () => {
  it('roundtrips selection policies with explicit overrides and deterministic keys', () => {
    const dirId = new Uint8Array(16).fill(1)
    const dirIdText = encodeBase64Url(dirId)
    const fileSkipId = new Uint8Array(16).fill(2)
    const fileSkipText = encodeBase64Url(fileSkipId)
    const fileKeepId = new Uint8Array(16).fill(3)
    const fileKeepText = encodeBase64Url(fileKeepId)

    const policy = new SelectionPolicy(false)
    policy.set(
      { kind: 'directory', id: dirId, idText: dirIdText, name: 'Photos' },
      ['root'],
      true,
    )
    policy.set(
      { kind: 'file', id: fileSkipId, idText: fileSkipText, name: 'skip.tmp', expectedSize: 10n },
      ['root', dirIdText],
      false,
    )

    const spec = selectionSpecFromPolicy('share-1', 'root', policy.snapshot())
    const restored = policyFromSelectionSpec(spec)
    expect(restored.defaultSelected).toBe(false)
    expect(restored.selected(
      { kind: 'file', id: fileKeepId, idText: fileKeepText, name: 'keep.jpg', expectedSize: 10n },
      ['root', dirIdText],
    )).toBe(true)
    expect(restored.selected(
      { kind: 'file', id: fileSkipId, idText: fileSkipText, name: 'skip.tmp', expectedSize: 10n },
      ['root', dirIdText],
    )).toBe(false)

    const specAgain = selectionSpecFromPolicy('share-1', 'root', restored)
    expect(selectionKey(specAgain)).toBe(selectionKey(spec))
  })

  it('generates distinct URL-safe task identifiers', () => {
    const first = createTaskId()
    const second = createTaskId()
    expect(first).not.toBe(second)
    expect(first).toMatch(/^[A-Za-z0-9_-]{22}$/u)
  })
})
