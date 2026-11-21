export function operationDeadlineSignal(
  parent: AbortSignal,
  remainingMilliseconds: number,
  timeoutReason: unknown,
): {
  readonly signal: AbortSignal
  readonly close: () => void
} {
  const controller = new AbortController()
  const abort = () => controller.abort(
    parent.reason ?? new DOMException('Operation aborted', 'AbortError'),
  )
  parent.addEventListener('abort', abort, { once: true })
  if (parent.aborted) abort()
  const timer = globalThis.setTimeout(() => {
    controller.abort(timeoutReason)
  }, Math.max(0, Math.ceil(remainingMilliseconds)))
  return {
    signal: controller.signal,
    close: () => {
      globalThis.clearTimeout(timer)
      parent.removeEventListener('abort', abort)
    },
  }
}
