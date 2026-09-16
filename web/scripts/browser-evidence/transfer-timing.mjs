// These functions are serialized by Playwright into the page. Keep their state
// local to that page so driver polling and metric capture cannot move the endpoint.
export function startTransferTiming({ mode }) {
  const clock = { mode, startedAt: performance.now(), completedAt: undefined, observer: undefined }
  if (globalThis.__windshareTransferTiming !== undefined) {
    throw new Error('A transfer clock is already active')
  }
  globalThis.__windshareTransferTiming = clock
  if (mode === 'folder') {
    const observe = () => {
      const task = document.querySelector('[data-task-stage="saved"]')
      if (task === null || task.getClientRects().length === 0) return
      clock.completedAt ??= performance.now()
      clock.observer.disconnect()
    }
    clock.observer = new MutationObserver(observe)
    clock.observer.observe(document.body, {
      subtree: true, childList: true, attributes: true,
      attributeFilter: ['data-task-stage', 'hidden', 'style'],
    })
    observe()
  }
}

export function completeZipTiming() {
  const clock = globalThis.__windshareTransferTiming
  if (clock?.mode !== 'zip') throw new Error('No ZIP transfer clock is active')
  clock.completedAt = performance.now()
}

export function finishTransferTiming() {
  const clock = globalThis.__windshareTransferTiming
  if (clock === undefined || !Number.isFinite(clock.completedAt)) {
    throw new Error('Transfer completion was not observed')
  }
  const detectedAt = performance.now()
  clock.observer?.disconnect()
  delete globalThis.__windshareTransferTiming
  return {
    elapsedMs: clock.completedAt - clock.startedAt,
    detectionLagMs: detectedAt - clock.completedAt,
    driverElapsedMs: detectedAt - clock.startedAt,
  }
}
