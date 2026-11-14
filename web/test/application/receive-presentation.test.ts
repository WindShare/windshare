import { describe, expect, it, vi } from 'vitest'
import { ReceivePresentation, type PresentationClock } from '../../src/application/receive/observation/presentation'
import type { TransferProgressSource } from '../../src/transfer/progress/observation'
import { EMPTY_PROGRESS } from '../../src/ui/model'

function clock() {
  let now = 0
  const timers: Array<{ due: number; cancelled: boolean; callback: () => void }> = []
  const source: PresentationClock = {
    now: () => now,
    schedule: (delay, callback) => {
      const timer = { due: now + delay, cancelled: false, callback }
      timers.push(timer)
      return { cancel: () => { timer.cancelled = true } }
    },
  }
  return { source, timers, advance: (milliseconds: number) => {
    now += milliseconds
    for (const timer of timers) if (!timer.cancelled && timer.due <= now) {
      timer.cancelled = true; timer.callback()
    }
  } }
}
function progress(id: string) {
  let bytes = 0n
  const read = vi.fn(() => ({ ...EMPTY_PROGRESS, transferJobId: id, receivedObjectBytes: bytes, recoverableBytes: 0n, partial: false }))
  const source: TransferProgressSource = { transferJobId: id, read }
  return { source, read, receive: () => { bytes++ } }
}

describe('receive presentation cadence', () => {
  it('coalesces receipt, write and runtime counters and commits transitions with the latest facts', () => {
    const time = clock()
    const task = progress('first')
    const revisions: unknown[] = []
    const presentation = new ReceivePresentation((source, state) => revisions.push({ progress: source?.read(), state }), time.source)
    presentation.observe(task.source, 'transition')
    for (let fragment = 0; fragment < 1000; fragment++) {
      task.receive()
      presentation.observe(task.source, 'counters')
      presentation.changed('counters')
    }
    expect(task.read).toHaveBeenCalledTimes(1)
    expect(time.timers).toHaveLength(1)
    time.advance(249)
    expect(revisions).toHaveLength(1)
    time.advance(1)
    expect(revisions.at(-1)).toMatchObject({ progress: { receivedObjectBytes: 1000n }, state: true })
    task.receive()
    presentation.observe(task.source, 'counters')
    presentation.changed('transition')
    expect(revisions.at(-1)).toMatchObject({ progress: { receivedObjectBytes: 1001n }, state: true })
    time.advance(250)
    expect(revisions).toHaveLength(3)
  })

  it('fences cancelled timer callbacks across replacement and disposal, independent of rendering', () => {
    const time = clock()
    const revisions: string[] = []
    const presentation = new ReceivePresentation(source => { if (source) revisions.push(source.transferJobId) }, time.source)
    const first = progress('first').source
    const second = progress('second').source
    presentation.observe(first, 'transition')
    presentation.observe(first, 'counters')
    const stale = time.timers[0]!.callback
    presentation.reset()
    presentation.observe(second, 'transition')
    stale()
    expect(revisions).toEqual(['first', 'second'])
    presentation.observe(second, 'counters')
    // Background timers can run much later; publication reads the current source.
    time.advance(30_000)
    expect(revisions).toEqual(['first', 'second', 'second'])
    presentation.observe(second, 'counters')
    presentation.close()
    for (const timer of time.timers) timer.callback()
    expect(revisions).toHaveLength(3)
  })
})
