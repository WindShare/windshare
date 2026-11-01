import { expect, it, vi } from 'vitest'
import type { ReceiverConnectionSnapshot } from '../../src/receiver/connection-state'
import { core, deferred, FakeSession, FakeSessionFactory, supervisorFixture, TrackedRelay } from './v2-supervisor-fixture'

async function flush(): Promise<void> {
  for (let turn = 0; turn < 32; turn++) await Promise.resolve()
}

it('waits for another generation without dispatching or exhausting retries on a withdrawn last lane', async () => {
  const initial = new FakeSession([1])
  const replacement = new FakeSession([10])
  const factory = new FakeSessionFactory()
  factory.connectFreshImpl = async () => core(replacement, new TrackedRelay(10))
  const { supervisor } = supervisorFixture(initial, new TrackedRelay(1), factory)
  const states: ReceiverConnectionSnapshot[] = []
  supervisor.connection.subscribe(state => states.push(state))
  const activation = supervisor.connectivity.begin('download')
  try {
    initial.withdraw(1)
    expect(states.at(-1)?.kind).toBe('reconnecting')
    expect(supervisor.contentLaneCount(activation.routes)).toBe(0)
    const operation = vi.fn(async generation => generation.id)
    const pending = supervisor.execute(undefined, operation)
    await flush()
    expect(operation).not.toHaveBeenCalled()
    expect(initial.closeCalls).toBe(0)
    expect(factory.connectFreshCalls).toBe(0)

    initial.detach(1)
    await expect(pending).resolves.toMatchObject({ value: 2 })
    expect(operation).toHaveBeenCalledOnce()
    expect(factory.connectFreshCalls).toBe(1)
    expect(states.at(-1)?.kind).toBe('connected')
  } finally { activation.close(); await supervisor.close() }
})

it('continues using a healthy lane while another lane is draining', async () => {
  const session = new FakeSession([1, 2])
  const { supervisor, factory } = supervisorFixture(session, new TrackedRelay(1))
  try {
    session.withdraw(1)
    const result = await supervisor.execute(undefined, async generation => generation.session.availableLaneIds())
    expect(result.value).toEqual([2])
    expect(factory.connectFreshCalls).toBe(0)
    expect(session.closeCalls).toBe(0)
  } finally { await supervisor.close() }
})

it('owns buffered old generations without blocking subsequent recovery or explicit stop', async () => {
  const first = new FakeSession([1])
  const second = new FakeSession([10])
  const third = new FakeSession([20])
  const factory = new FakeSessionFactory()
  const replacements = [core(second, new TrackedRelay(10)), core(third, new TrackedRelay(20))]
  factory.connectFreshImpl = async () => replacements.shift()!
  const { supervisor } = supervisorFixture(first, new TrackedRelay(1), factory)
  const previous = (await supervisor.execute(undefined, async generation => generation)).value
  const idle = deferred<void>()
  vi.spyOn(previous.broker, 'waitForIdle').mockReturnValue(idle.promise)
  const close = previous.broker.close.bind(previous.broker)
  const closed = vi.spyOn(previous.broker, 'close').mockImplementation(() => { idle.resolve(); close() })
  try {
    first.withdraw(1)
    first.detach(1)
    await vi.waitFor(() => expect(supervisor.generationId).toBe(2))
    expect(closed).not.toHaveBeenCalled()
    second.withdraw(10)
    second.detach(10)
    await vi.waitFor(() => expect(supervisor.generationId).toBe(3))
    expect(closed).not.toHaveBeenCalled()
    await supervisor.close()
    expect(closed).toHaveBeenCalledOnce()
  } finally { idle.resolve(); await supervisor.close() }
})
