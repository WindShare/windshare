import { describe, expect, it, vi } from 'vitest'
import { continueProgressiveZip } from '../../src/ui/browser-receive/retained-progressive'
import type { BrowserReceiveWindow } from '../../src/ui/browser-receive/contracts'
import type { AuthorityOwnedReceiveOperationContinuation } from '../../src/output/resume/reopen-authority'

const ports = vi.hoisted(() => ({ reopen: vi.fn(), handoff: vi.fn() }))
vi.mock('../../src/ui/browser-receive/workspace-operation', () => ({
  WorkspaceReceiveOperation: { reopenProgressive: ports.reopen },
}))
vi.mock('../../src/ui/browser-receive/workspace-publication', () => ({
  handoffRetainedWorkspacePackage: ports.handoff,
}))

type Operation = Extract<AuthorityOwnedReceiveOperationContinuation, { kind: 'workspace-progressive-zip' }>['operation']
function fixture(requirement: 'remote-content-needed' | 'local-finalization') {
  const checkpoint = { generation: 2n, sealedLength: 90n }
  const lifecycle = { kind: 'waiting-to-save' }
  const close = vi.fn(async () => undefined)
  const finalize = vi.fn(async () => checkpoint)
  const seal = vi.fn(async () => lifecycle)
  const operation = {
    progressiveContinuation: { requirement, backend: { archive: { finalize }, store: {} } },
    stages: { progressive: { seal } }, close,
  } as unknown as Operation
  return { operation, close, finalize, seal, checkpoint, lifecycle }
}

describe('native ZIP retained continuation routing', () => {
  it('finalizes and hands off locally without constructing a network receive runtime', async () => {
    const f = fixture('local-finalization')
    ports.handoff.mockResolvedValueOnce(undefined)
    const previousReopens = ports.reopen.mock.calls.length
    const result = await continueProgressiveZip({} as BrowserReceiveWindow, f.operation, new AbortController().signal)
    expect(result).toEqual({ kind: 'completed' })
    expect(f.finalize).toHaveBeenCalledOnce()
    expect(f.seal).toHaveBeenCalledWith(f.operation.progressiveContinuation.backend.store, f.checkpoint)
    expect(ports.handoff).toHaveBeenLastCalledWith({}, { ...f.operation, lifecycle: f.lifecycle },
      f.operation.progressiveContinuation.backend, undefined)
    expect(ports.reopen.mock.calls).toHaveLength(previousReopens)
    expect(f.close).toHaveBeenCalledOnce()
  })

  it('hands remote continuation ownership to the receive runtime without local finalization', async () => {
    const f = fixture('remote-content-needed')
    const runtime = { detach: vi.fn() }
    ports.reopen.mockResolvedValueOnce(runtime)
    expect(await continueProgressiveZip({} as BrowserReceiveWindow, f.operation, new AbortController().signal))
      .toEqual({ kind: 'receive-continuation', runtime })
    expect(f.finalize).not.toHaveBeenCalled()
    expect(f.close).not.toHaveBeenCalled()
  })

  it('releases the reopened authority when local finalization fails', async () => {
    const f = fixture('local-finalization')
    f.finalize.mockRejectedValueOnce(new Error('quota'))
    await expect(continueProgressiveZip({} as BrowserReceiveWindow, f.operation, new AbortController().signal))
      .rejects.toThrow('quota')
    expect(f.seal).not.toHaveBeenCalled()
    expect(f.close).toHaveBeenCalledOnce()
  })
})
