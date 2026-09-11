const MAXIMUM_STORE_GROUPS = 32
const MAXIMUM_ERROR_OBSERVATIONS = 8
const OVERFLOW_GROUP = 'additional-store-groups'

/** Counts native storage transactions inside the measured file loop without retaining request values. */
export function observeStorageTransactions() {
  const original = IDBDatabase.prototype.transaction
  const originalAbort = IDBTransaction.prototype.abort
  const abortCalls = []
  const errors = []
  const groups = new Map()
  IDBTransaction.prototype.abort = function (...args) {
    if (abortCalls.length < MAXIMUM_ERROR_OBSERVATIONS) abortCalls.push({ stores: Array.from(this.objectStoreNames), stack: new Error('Explicit IndexedDB abort').stack })
    return Reflect.apply(originalAbort, this, args)
  }
  IDBDatabase.prototype.transaction = function (...args) {
    const started = performance.now()
    const transaction = Reflect.apply(original, this, args)
    const names = typeof args[0] === 'string' ? [args[0]] : Array.from(args[0])
    const proposedKey = `${transaction.mode}:${names.sort().join(',')}`
    const key = groups.has(proposedKey) || groups.size < MAXIMUM_STORE_GROUPS ? proposedKey : OVERFLOW_GROUP
    let group = groups.get(key)
    if (!group) {
      group = { started: 0, completed: 0, aborted: 0, summedLifetimeMilliseconds: 0 }
      groups.set(key, group)
    }
    group.started += 1
    transaction.addEventListener('error', event => {
      const error = event.target?.error ?? transaction.error
      if (errors.length < MAXIMUM_ERROR_OBSERVATIONS) errors.push({ group: key, name: error?.name ?? null, message: error?.message ?? null })
    })
    transaction.addEventListener('complete', () => {
      group.completed += 1
      group.summedLifetimeMilliseconds += performance.now() - started
    }, { once: true })
    transaction.addEventListener('abort', () => { group.aborted += 1 }, { once: true })
    return transaction
  }
  return () => {
    IDBDatabase.prototype.transaction = original
    IDBTransaction.prototype.abort = originalAbort
    return {
      abortCalls, errors,
      basis: 'native IndexedDB transactions opened during the measured file loop; lifetimes may overlap and are not an exclusive wall-time attribution',
      groups: Object.fromEntries(groups),
      totalStarted: [...groups.values()].reduce((sum, group) => sum + group.started, 0),
    }
  }
}
