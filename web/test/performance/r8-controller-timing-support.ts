import type { V2ReceiverController } from '../../src/ui/v2-controller'

export function pendingUntilAbort(signal: AbortSignal, message: string): Promise<never> {
  return new Promise<never>((_resolve, reject) => {
    const abort = () => reject(signal.reason ?? new DOMException(message, 'AbortError'))
    signal.addEventListener('abort', abort, { once: true })
    if (signal.aborted) abort()
  })
}

export function waitForBrowsing(receiver: V2ReceiverController): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const observe = () => {
      const snapshot = receiver.getSnapshot()
      if (snapshot.phase === 'browsing') {
        unsubscribe()
        resolve()
      } else if (snapshot.phase === 'failed') {
        unsubscribe()
        reject(new Error(snapshot.error ?? 'Controller timing fixture failed'))
      }
    }
    const unsubscribe = receiver.subscribe(observe)
    observe()
  })
}
