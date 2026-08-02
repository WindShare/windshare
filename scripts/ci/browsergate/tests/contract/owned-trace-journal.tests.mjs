import assert from 'node:assert/strict'

import {
  createOwnedTraceJournal,
  requireCompleteOwnedTraceSnapshot,
} from '../../owned-trace-journal.mjs'

recordsImmutablePortableEvidence()
rejectsActiveDataWithoutInvokingIt()
rejectsProxyEvidenceWithoutEnteringItsTraps()
rejectsSparseAndDecoratedArrays()
preservesPrototypeNamedFieldsWithoutPrototypeMutation()
enforcesStructuralBudgets()
reportsCapacityAndSettlementViolations()
authenticatesPulledSnapshotsBeforeConsumption()

function recordsImmutablePortableEvidence() {
  const journal = newJournal()
  const source = { operationId: 'journal-contract', context: { states: ['started'] } }
  assert.equal(journal.append(source), true)
  source.context.states[0] = 'mutated-after-append'
  journal.finish()

  const snapshot = requireCompleteOwnedTraceSnapshot(
    journal.view.snapshot(),
    'owned trace journal contract',
  )
  assert.equal(snapshot.events[0].context.states[0], 'started')
  assert.equal(Object.getPrototypeOf(snapshot), null)
  assert.equal(Object.getPrototypeOf(snapshot.events[0]), null)
  assert.equal(Object.isFrozen(snapshot.events[0].context.states), true)
  assert.equal(snapshot.observedEvents, 1)
  assert.equal(snapshot.capturedEvents, 1)
  assert.equal(
    snapshot.capturedBytes,
    Buffer.byteLength(JSON.stringify(snapshot.events[0]), 'utf8'),
  )
}

function rejectsActiveDataWithoutInvokingIt() {
  let getterCalls = 0
  const event = {}
  Object.defineProperty(event, 'active', {
    enumerable: true,
    get() {
      getterCalls += 1
      throw new Error('the journal must never invoke evidence accessors')
    },
  })
  assertInvalidEvent(event)
  assert.equal(getterCalls, 0)

  const hidden = { visible: true }
  Object.defineProperty(hidden, 'hidden', { enumerable: false, value: 'authority' })
  assertInvalidEvent(hidden)
  assertInvalidEvent({ visible: true, [Symbol('hidden')]: 'authority' })
}

function rejectsProxyEvidenceWithoutEnteringItsTraps() {
  let trapCalls = 0
  const proxy = new Proxy({}, {
    getPrototypeOf() {
      trapCalls += 1
      throw new Error('proxy trap entered')
    },
    ownKeys() {
      trapCalls += 1
      throw new Error('proxy trap entered')
    },
  })
  assertInvalidEvent(proxy)
  assert.equal(trapCalls, 0)
}

function rejectsSparseAndDecoratedArrays() {
  assertInvalidEvent(new Array(1))

  let getterCalls = 0
  const active = []
  Object.defineProperty(active, '0', {
    enumerable: true,
    configurable: true,
    get() {
      getterCalls += 1
      return 'active'
    },
  })
  active.length = 1
  assertInvalidEvent(active)
  assert.equal(getterCalls, 0)

  const hidden = ['visible']
  Object.defineProperty(hidden, 'hidden', { enumerable: false, value: true })
  assertInvalidEvent(hidden)

  const symbol = ['visible']
  symbol[Symbol('hidden')] = true
  assertInvalidEvent(symbol)
}

function preservesPrototypeNamedFieldsWithoutPrototypeMutation() {
  const event = Object.create(null)
  Object.defineProperty(event, '__proto__', {
    enumerable: true,
    value: { polluted: false },
  })
  const journal = newJournal()
  assert.equal(journal.append(event), true)
  journal.finish()
  const snapshot = requireCompleteOwnedTraceSnapshot(journal.view.snapshot())
  const captured = snapshot.events[0]
  assert.equal(Object.getPrototypeOf(captured), null)
  assert.equal(Object.hasOwn(captured, '__proto__'), true)
  assert.equal(captured.__proto__.polluted, false)
  assert.equal({}.polluted, undefined)
}

function enforcesStructuralBudgets() {
  assert.throws(
    () => createOwnedTraceJournal({
      label: 'x'.repeat(129),
      maximumEvents: 1,
      maximumBytes: 1,
    }),
    /bounded portable representation/u,
  )
  assertInvalidEvent({ ['k'.repeat(129)]: true })

  let nested = { terminal: true }
  for (let depth = 0; depth < 33; depth += 1) nested = { nested }
  assertInvalidEvent(nested)
  const cyclic = {}
  cyclic.self = cyclic
  assertInvalidEvent(cyclic)
}

function reportsCapacityAndSettlementViolations() {
  const overflow = createOwnedTraceJournal({
    label: 'overflow contract',
    maximumEvents: 1,
    maximumBytes: 1_024,
  })
  assert.equal(overflow.append({ sequence: 1 }), true)
  assert.equal(overflow.append({ sequence: 2 }), false)
  overflow.finish()
  const overflowSnapshot = overflow.view.snapshot()
  assert.equal(overflowSnapshot.truncated, true)
  assert.equal(overflowSnapshot.failure.code, 'capacity-exceeded')
  assert.equal(overflowSnapshot.observedEvents, 2)
  assert.equal(overflowSnapshot.capturedEvents, 1)
  assert.throws(() => requireCompleteOwnedTraceSnapshot(overflowSnapshot), /did not settle/u)

  const late = newJournal()
  late.finish()
  assert.equal(late.append({ sequence: 1 }), false)
  assert.equal(late.view.snapshot().failure.code, 'event-after-completion')

  const duplicate = newJournal()
  duplicate.finish()
  duplicate.finish()
  assert.equal(duplicate.view.snapshot().failure.code, 'duplicate-completion')
}

function authenticatesPulledSnapshotsBeforeConsumption() {
  let trapCalls = 0
  const proxy = new Proxy({}, {
    getPrototypeOf() {
      trapCalls += 1
      return Object.prototype
    },
  })
  assert.throws(
    () => requireCompleteOwnedTraceSnapshot(proxy, 'hostile snapshot'),
    /not a canonical portable snapshot/u,
  )
  assert.equal(trapCalls, 0)

  const journal = newJournal()
  journal.finish()
  const valid = journal.view.snapshot()
  const decorated = { ...valid }
  Object.defineProperty(decorated, 'hidden', { enumerable: false, value: true })
  assert.throws(
    () => requireCompleteOwnedTraceSnapshot(decorated, 'decorated snapshot'),
    /not a canonical portable snapshot/u,
  )
}

function assertInvalidEvent(event) {
  const journal = newJournal()
  assert.equal(journal.append(event), false)
  journal.finish()
  const snapshot = journal.view.snapshot()
  assert.equal(snapshot.failure.code, 'invalid-event')
  assert.equal(snapshot.completed, true)
  assert.equal(snapshot.observedEvents, 0)
  assert.equal(snapshot.capturedEvents, 0)
}

function newJournal() {
  return createOwnedTraceJournal({
    label: 'owned trace journal contract',
    maximumEvents: 16,
    maximumBytes: 64 * 1024,
  })
}

process.stdout.write('owned trace journal hostile-data contracts: PASS\n')
