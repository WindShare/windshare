import type { V2OutputSettlementDeadline } from '../../src/transfer/settlement/v2-output'

export function deferred<T = void>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(accept => { resolve = accept })
  return { promise, resolve }
}

export function manualSettlementDeadline(): V2OutputSettlementDeadline & { expire(): void } {
  let expiration: (() => void) | undefined
  return {
    schedule: (_milliseconds, expire) => {
      if (expiration !== undefined) throw new Error('Concurrent settlement deadlines')
      expiration = expire
      return { cancel: () => { if (expiration === expire) expiration = undefined } }
    },
    expire: () => {
      if (expiration === undefined) throw new Error('No pending settlement')
      expiration()
    },
  }
}
