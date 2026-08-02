import { types as nodeTypes } from 'node:util'

const MAXIMUM_SAFE_COUNT = Number.MAX_SAFE_INTEGER
const MAXIMUM_JOURNAL_EVENTS = 65_536
const MAXIMUM_JOURNAL_BYTES = 16 * 1024 * 1024
const MAXIMUM_LABEL_CHARACTERS = 128
const MAXIMUM_LABEL_BYTES = 512
const MAXIMUM_FAILURE_MESSAGE_BYTES = 1_024
const MAXIMUM_PORTABLE_DEPTH = 32
const MAXIMUM_PORTABLE_ENTRIES = 65_536
const MAXIMUM_PORTABLE_STRING_CHARACTERS = MAXIMUM_JOURNAL_BYTES
const MAXIMUM_PORTABLE_STRING_BYTES = MAXIMUM_JOURNAL_BYTES
const MAXIMUM_PORTABLE_KEY_CHARACTERS = 128
const MAXIMUM_PORTABLE_KEY_BYTES = 512
const SNAPSHOT_FIELDS = Object.freeze([
  'capturedBytes',
  'capturedEvents',
  'completed',
  'events',
  'failure',
  'observedBytes',
  'observedEvents',
  'truncated',
])

/**
 * Producers own this bounded journal so diagnostic consumers cannot delay or
 * mutate lifecycle settlement. Only an immutable pull snapshot crosses the
 * ownership boundary after the producer has finished.
 */
export function createOwnedTraceJournal(options) {
  const { label, maximumEvents, maximumBytes } = requireJournalOptions(options)
  const events = []
  let observedEvents = 0
  let capturedEvents = 0
  let observedBytes = 0
  let capturedBytes = 0
  let completed = false
  let truncated = false
  let failure = null

  const recordFailure = (code, suffix) => {
    failure ??= frozenDataRecord([
      ['code', code],
      ['message', boundedFailureMessage(label, suffix)],
    ])
  }
  const append = (event) => {
    if (completed) {
      recordFailure('event-after-completion', 'received an event after completion')
      return false
    }
    let canonical
    let encoded
    try {
      canonical = canonicalPortableValue(event)
      encoded = Buffer.from(JSON.stringify(canonical), 'utf8')
    } catch {
      recordFailure('invalid-event', 'rejected an invalid portable event')
      return false
    }
    observedEvents = boundedAdd(observedEvents, 1, recordFailure)
    observedBytes = boundedAdd(observedBytes, encoded.byteLength, recordFailure)
    if (observedEvents > maximumEvents || observedBytes > maximumBytes) {
      truncated = true
      recordFailure('capacity-exceeded', 'exceeded its bounded evidence capacity')
      return false
    }
    events.push(canonical)
    capturedEvents += 1
    capturedBytes += encoded.byteLength
    return true
  }
  const finish = () => {
    if (completed) {
      recordFailure('duplicate-completion', 'completed more than once')
      return
    }
    completed = true
  }
  const snapshot = () => frozenDataRecord([
    ['events', Object.freeze([...events])],
    ['observedEvents', observedEvents],
    ['capturedEvents', capturedEvents],
    ['observedBytes', observedBytes],
    ['capturedBytes', capturedBytes],
    ['truncated', truncated],
    ['completed', completed],
    ['failure', failure],
  ])
  return Object.freeze({ append, finish, view: Object.freeze({ snapshot }) })
}

/**
 * Canonicalizing again closes a time-of-check/time-of-use gap when a snapshot
 * originated outside this module. Callers should consume the returned value.
 */
export function requireCompleteOwnedTraceSnapshot(snapshot, label = 'owned trace journal') {
  const canonicalLabel = requireBoundedLabel(label, 'trace snapshot label')
  let canonical
  try {
    canonical = canonicalPortableValue(snapshot)
  } catch {
    throw new Error(`${canonicalLabel} is not a canonical portable snapshot`)
  }
  if (!sameStrings(Object.keys(canonical).sort(), SNAPSHOT_FIELDS)) {
    throw new Error(`${canonicalLabel} has an invalid evidence shape`)
  }
  if (
    canonical.failure !== null || canonical.completed !== true || canonical.truncated !== false ||
    !Array.isArray(canonical.events) ||
    !nonnegativeSafeInteger(canonical.observedEvents) ||
    !nonnegativeSafeInteger(canonical.capturedEvents) ||
    !nonnegativeSafeInteger(canonical.observedBytes) ||
    !nonnegativeSafeInteger(canonical.capturedBytes) ||
    canonical.observedEvents !== canonical.capturedEvents ||
    canonical.observedBytes !== canonical.capturedBytes ||
    canonical.capturedEvents !== canonical.events.length ||
    canonical.capturedEvents > MAXIMUM_JOURNAL_EVENTS ||
    canonical.capturedBytes > MAXIMUM_JOURNAL_BYTES ||
    encodedEventBytes(canonical.events) !== canonical.capturedBytes
  ) {
    throw new Error(`${canonicalLabel} did not settle with complete bounded evidence`)
  }
  return canonical
}

function requireJournalOptions(options) {
  const descriptors = requireDataDescriptors(options, 'trace journal options')
  const names = Reflect.ownKeys(descriptors)
  if (
    names.some((name) => typeof name !== 'string') ||
    !sameStrings(names.sort(), ['label', 'maximumBytes', 'maximumEvents'])
  ) throw new Error('trace journal options are invalid')
  for (const name of names) requireEnumerableDataDescriptor(descriptors[name], 'trace journal option')
  const label = requireBoundedLabel(descriptors.label.value, 'trace journal label')
  const maximumEvents = descriptors.maximumEvents.value
  const maximumBytes = descriptors.maximumBytes.value
  requirePositiveInteger(maximumEvents, 'trace journal maximum events', MAXIMUM_JOURNAL_EVENTS)
  requirePositiveInteger(maximumBytes, 'trace journal maximum bytes', MAXIMUM_JOURNAL_BYTES)
  return { label, maximumEvents, maximumBytes }
}

function canonicalPortableValue(value) {
  const budget = {
    entries: MAXIMUM_PORTABLE_ENTRIES,
    // Per-journal capacity determines whether a valid event is truncated; this
    // global ceiling only prevents a single value from becoming unbounded.
    stringBytes: MAXIMUM_PORTABLE_STRING_BYTES,
  }
  return canonicalizePortableValue(value, budget, new WeakSet(), 0)
}

function canonicalizePortableValue(value, budget, visiting, depth) {
  if (value === null || typeof value === 'boolean') return value
  if (typeof value === 'string') {
    requireBoundedString(
      value,
      MAXIMUM_PORTABLE_STRING_CHARACTERS,
      budget.stringBytes,
      'portable trace string',
    )
    return value
  }
  if (typeof value === 'number') {
    if (!Number.isSafeInteger(value)) throw new Error('portable trace number is not a safe integer')
    return value
  }
  if (typeof value !== 'object' || nodeTypes.isProxy(value)) {
    throw new Error('portable trace value is not inert data')
  }
  if (depth >= MAXIMUM_PORTABLE_DEPTH) throw new Error('portable trace depth exceeded')
  if (visiting.has(value)) throw new Error('portable trace data contains a cycle')
  visiting.add(value)
  try {
    const prototype = Object.getPrototypeOf(value)
    if (Array.isArray(value)) {
      if (prototype !== Array.prototype) throw new Error('portable trace array has a non-data prototype')
      return canonicalizeArray(value, budget, visiting, depth)
    }
    if (prototype !== Object.prototype && prototype !== null) {
      throw new Error('portable trace record has a non-data prototype')
    }
    return canonicalizeRecord(value, budget, visiting, depth)
  } finally {
    visiting.delete(value)
  }
}

function canonicalizeArray(value, budget, visiting, depth) {
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const names = Reflect.ownKeys(descriptors)
  if (names.some((name) => typeof name !== 'string')) {
    throw new Error('portable trace array contains symbol fields')
  }
  const lengthDescriptor = descriptors.length
  if (
    lengthDescriptor === undefined || !Object.hasOwn(lengthDescriptor, 'value') ||
    !nonnegativeSafeInteger(lengthDescriptor.value) ||
    lengthDescriptor.value > MAXIMUM_PORTABLE_ENTRIES
  ) throw new Error('portable trace array length is invalid')
  const length = lengthDescriptor.value
  if (names.length !== length + 1) throw new Error('portable trace array is sparse or has extra fields')
  consumeEntries(budget, length)
  const result = new Array(length)
  for (let index = 0; index < length; index += 1) {
    const descriptor = descriptors[String(index)]
    requireEnumerableDataDescriptor(descriptor, 'portable trace array entry')
    result[index] = canonicalizePortableValue(descriptor.value, budget, visiting, depth + 1)
  }
  return Object.freeze(result)
}

function canonicalizeRecord(value, budget, visiting, depth) {
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const names = Reflect.ownKeys(descriptors)
  if (names.some((name) => typeof name !== 'string')) {
    throw new Error('portable trace record contains symbol fields')
  }
  consumeEntries(budget, names.length)
  const result = Object.create(null)
  for (const name of names.sort()) {
    requireBoundedString(
      name,
      MAXIMUM_PORTABLE_KEY_CHARACTERS,
      MAXIMUM_PORTABLE_KEY_BYTES,
      'portable trace key',
    )
    const descriptor = descriptors[name]
    requireEnumerableDataDescriptor(descriptor, 'portable trace record field')
    Object.defineProperty(result, name, {
      value: canonicalizePortableValue(descriptor.value, budget, visiting, depth + 1),
      enumerable: true,
      writable: false,
      configurable: false,
    })
  }
  return Object.freeze(result)
}

function requireDataDescriptors(value, label) {
  if (
    value === null || typeof value !== 'object' || Array.isArray(value) || nodeTypes.isProxy(value)
  ) throw new Error(`${label} must be an inert data record`)
  const prototype = Object.getPrototypeOf(value)
  if (prototype !== Object.prototype && prototype !== null) {
    throw new Error(`${label} must be an inert data record`)
  }
  return Object.getOwnPropertyDescriptors(value)
}

function requireEnumerableDataDescriptor(descriptor, label) {
  if (
    descriptor === undefined || !Object.hasOwn(descriptor, 'value') ||
    descriptor.enumerable !== true
  ) throw new Error(`${label} must be enumerable inert data`)
}

function consumeEntries(budget, count) {
  if (count > budget.entries) throw new Error('portable trace entry budget exceeded')
  budget.entries -= count
}

function encodedEventBytes(events) {
  let total = 0
  for (const event of events) {
    const byteLength = Buffer.byteLength(JSON.stringify(event), 'utf8')
    if (total > MAXIMUM_JOURNAL_BYTES - byteLength) return MAXIMUM_SAFE_COUNT
    total += byteLength
  }
  return total
}

function frozenDataRecord(entries) {
  const result = Object.create(null)
  for (const [name, value] of entries) {
    Object.defineProperty(result, name, {
      value,
      enumerable: true,
      writable: false,
      configurable: false,
    })
  }
  return Object.freeze(result)
}

function boundedFailureMessage(label, suffix) {
  const message = `${label} ${suffix}`
  if (Buffer.byteLength(message, 'utf8') > MAXIMUM_FAILURE_MESSAGE_BYTES) {
    return 'owned trace journal rejected evidence with an oversized diagnostic'
  }
  return message
}

function requireBoundedLabel(value, label) {
  if (typeof value !== 'string' || value.length === 0) throw new Error(`${label} is required`)
  requireBoundedString(value, MAXIMUM_LABEL_CHARACTERS, MAXIMUM_LABEL_BYTES, label)
  return value
}

function requireBoundedString(value, maximumCharacters, maximumBytes, label) {
  if (
    typeof value !== 'string' || value.length > maximumCharacters ||
    Buffer.byteLength(value, 'utf8') > maximumBytes
  ) throw new Error(`${label} exceeds its bounded portable representation`)
}

function boundedAdd(value, increment, recordFailure) {
  if (value > MAXIMUM_SAFE_COUNT - increment) {
    recordFailure('counter-overflow', 'counter overflowed')
    return MAXIMUM_SAFE_COUNT
  }
  return value + increment
}

function requirePositiveInteger(value, label, maximum) {
  if (!Number.isSafeInteger(value) || value < 1 || value > maximum) {
    throw new Error(`${label} must be a bounded positive integer`)
  }
}

function nonnegativeSafeInteger(value) {
  return Number.isSafeInteger(value) && value >= 0
}

function sameStrings(actual, expected) {
  return actual.length === expected.length && actual.every((value, index) => value === expected[index])
}
