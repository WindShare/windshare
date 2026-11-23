import { afterEach, describe, expect, it, vi } from 'vitest'
import { DisconnectCheckpointGuard, DISCONNECT_CHECKPOINT_DELAY_MS as delay } from '../../src/application/receive/disconnect-checkpoint-guard'
import { ReceiverConnectionState } from '../../src/receiver/connection-state'
import type { ActiveCheckpointSummary } from '../../src/transfer/job'

const saved: ActiveCheckpointSummary = { allReceivedSaved: true, savedFiles: 1, skippedFiles: 0, failedFiles: 0, savedBytes: 12n }

function fixture(checkpointActive = vi.fn(async (signal: AbortSignal) => { signal.throwIfAborted(); return saved })) {
  const state = new ReceiverConnectionState()
  state.reconnecting({ kind: 'connecting' })
  const job = { checkpointActive, allReceivedSaved: true }
  const changed = vi.fn(), trace = vi.fn()
  const guard = new DisconnectCheckpointGuard({ subscribeConnection: listener => state.subscribe(listener) },
    { job, taskId: 'task-a', changed, trace: { current: trace } })
  return { state, guard, job, changed, trace }
}

describe('disconnect checkpoint rounds', () => {
  afterEach(() => { vi.useRealTimers() })

  it('starts on the first snapshot, ignores short outages and keeps one deadline through backoff changes', async () => {
    vi.useFakeTimers()
    const { state, guard, job, trace } = fixture()
    await vi.advanceTimersByTimeAsync(delay - 1)
    state.connected()
    await vi.advanceTimersByTimeAsync(1)
    expect(job.checkpointActive).not.toHaveBeenCalled()
    state.reconnecting({ kind: 'connecting' })
    await vi.advanceTimersByTimeAsync(delay / 2)
    state.reconnecting({ kind: 'waiting', reason: 'server', retryAt: 40_000 })
    state.reconnecting({ kind: 'connecting' })
    await vi.advanceTimersByTimeAsync(delay / 2)
    expect(guard.checkpointed).toBe(true)
    expect(trace).toHaveBeenCalledWith({ eventName: 'checkpoint', payload: expect.objectContaining({
      task_id: 'task-a', round: 2, saved_files: 1, skipped_files: 0, failed_files: 0, saved_bytes: '12',
    }) })
    guard.invalidate()
    await vi.advanceTimersByTimeAsync(delay * 2)
    expect(guard.checkpointed).toBe(false)
    expect(job.checkpointActive).toHaveBeenCalledOnce()
    guard.close()
  })

  it('drops late results after reconnect, end, or close, including an older round during a new outage', async () => {
    vi.useFakeTimers()
    const finish: Array<(value: ActiveCheckpointSummary) => void> = []
    const checkpoint = vi.fn((signal: AbortSignal) => {
      signal.throwIfAborted()
      return new Promise<ActiveCheckpointSummary>(resolve => finish.push(resolve))
    })
    const { state, guard } = fixture(checkpoint)
    await vi.advanceTimersByTimeAsync(delay)
    state.connected()
    expect(checkpoint.mock.calls[0]?.[0].aborted).toBe(true)
    state.reconnecting({ kind: 'connecting' })
    finish[0]!(saved)
    await vi.advanceTimersByTimeAsync(delay)
    expect(guard.checkpointed).toBe(false)
    state.failed(new Error('unavailable'))
    finish[1]!(saved)
    await vi.advanceTimersByTimeAsync(0)
    expect(guard.checkpointed).toBe(false)
    state.reconnecting({ kind: 'connecting' })
    await vi.advanceTimersByTimeAsync(delay)
    guard.close()
    finish[2]!(saved)
    await vi.advanceTimersByTimeAsync(0)
    expect(guard.checkpointed).toBe(false)
  })

  it('contains a rejected round, allows the next outage, and closes without publishing an intermediate state', async () => {
    vi.useFakeTimers()
    const checkpoint = vi.fn(async (signal: AbortSignal) => { signal.throwIfAborted(); return saved })
      .mockRejectedValueOnce(new DOMException('disk', 'InvalidStateError'))
    const { state, guard, changed, trace } = fixture(checkpoint)
    await vi.advanceTimersByTimeAsync(delay)
    expect(guard.checkpointed).toBe(false)
    expect(trace).toHaveBeenCalledWith({ eventName: 'checkpoint', payload: expect.objectContaining({
      task_id: 'task-a', round: 1, outcome: 'failed', error_class: 'invalid_state',
    }) })
    state.connected()
    state.reconnecting({ kind: 'connecting' })
    await vi.advanceTimersByTimeAsync(delay)
    expect(guard.checkpointed).toBe(true)
    guard.close()
    expect(guard.checkpointed).toBe(false)
    expect(changed).toHaveBeenCalledOnce()
  })

  it('requires a fully durable cut at publication and clears it when the share ends', async () => {
    vi.useFakeTimers()
    let notify!: (snapshot: { kind: 'connected' | 'reconnecting' | 'ended' }) => void
    const unsubscribe = vi.fn()
    const job = { checkpointActive: vi.fn(async () => saved), allReceivedSaved: false }
    const guard = new DisconnectCheckpointGuard({ subscribeConnection: listener => {
      notify = snapshot => {
        if (snapshot.kind === 'ended') listener({ kind: 'ended', reason: 'share-stopped' })
        else if (snapshot.kind === 'reconnecting') listener({ kind: 'reconnecting', activity: { kind: 'connecting' } })
        else listener({ kind: 'connected' })
      }
      return unsubscribe
    } }, { taskId: 'task-b', job, changed: vi.fn() })
    notify({ kind: 'reconnecting' })
    await vi.advanceTimersByTimeAsync(delay)
    expect(guard.checkpointed).toBe(false)
    notify({ kind: 'connected' })
    job.allReceivedSaved = true
    notify({ kind: 'reconnecting' })
    await vi.advanceTimersByTimeAsync(delay)
    expect(guard.checkpointed).toBe(true)
    notify({ kind: 'ended' })
    expect(guard.checkpointed).toBe(false)
    guard.close()
    expect(unsubscribe).toHaveBeenCalledOnce()
  })
})
