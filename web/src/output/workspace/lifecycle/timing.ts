import type { ReceiveLifecycleState } from '../state'

/** Wall-clock observations are display data, never evidence of publication or storage ownership. */
export interface ReceiveTiming {
  readonly startedAtMilliseconds: number
  readonly resultReadyAtMilliseconds?: number
}

export function snapshotReceiveTiming(input: unknown): ReceiveTiming | undefined {
  if (typeof input !== 'object' || input === null) return undefined
  const timing = input as Partial<ReceiveTiming>
  if (!validMilliseconds(timing.startedAtMilliseconds)) return undefined
  if (timing.resultReadyAtMilliseconds !== undefined &&
      (!validMilliseconds(timing.resultReadyAtMilliseconds) ||
        timing.resultReadyAtMilliseconds < timing.startedAtMilliseconds)) return undefined
  return Object.freeze({
    startedAtMilliseconds: timing.startedAtMilliseconds,
    ...(timing.resultReadyAtMilliseconds === undefined ? {} : {
      resultReadyAtMilliseconds: timing.resultReadyAtMilliseconds,
    }),
  })
}

export function receiveTimingFields(input: unknown): Readonly<{ timing?: ReceiveTiming }> {
  const timing = snapshotReceiveTiming(input)
  return timing === undefined ? {} : { timing }
}

export function advanceReceiveTiming(
  timing: ReceiveTiming | undefined,
  kind: ReceiveLifecycleState['kind'],
  clock: () => number,
): Readonly<{ timing?: ReceiveTiming }> {
  if (timing === undefined || timing.resultReadyAtMilliseconds !== undefined || !resultIsReady(kind)) {
    return receiveTimingFields(timing)
  }
  // Freeze at the first usable result. Later save dialogs, repeated handoffs and
  // cleanup must not inflate the time needed to receive and finalize that result.
  return receiveTimingFields({
    ...timing,
    resultReadyAtMilliseconds: Math.max(timing.startedAtMilliseconds, clock()),
  })
}

export function receiveElapsedMilliseconds(input: unknown): number | null {
  const timing = snapshotReceiveTiming(input)
  return timing?.resultReadyAtMilliseconds === undefined ? null
    : timing.resultReadyAtMilliseconds - timing.startedAtMilliseconds
}

function resultIsReady(kind: ReceiveLifecycleState['kind']): boolean {
  return kind === 'artifact-sealed' || kind === 'waiting-to-save' ||
    kind === 'published' || kind === 'download-started' || kind === 'partial-directory'
}

function validMilliseconds(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
}
