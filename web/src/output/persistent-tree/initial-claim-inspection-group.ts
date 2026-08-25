import type { PerformanceClaimPhaseObservation } from '../diagnostics/claim-batch-performance'
import type { PerformanceClaimInspectorState } from '../diagnostics/claim-inspector-performance'

interface InitialClaimInspectionGroupOptions<TInput, TOutput> {
  readonly work: readonly TInput[]
  readonly maximumConcurrent: number
  readonly inspect: (input: TInput) => Promise<TOutput>
  readonly performance?: PerformanceClaimPhaseObservation
  readonly observeState?: (state: PerformanceClaimInspectorState) => void
}

interface IndexedInspectionFailure {
  readonly workIndex: number
  readonly error: unknown
}

/** Runs one batch with bounded admission and drains accepted siblings before failing. */
export async function inspectInitialClaimGroup<TInput, TOutput>(
  options: InitialClaimInspectionGroupOptions<TInput, TOutput>,
): Promise<readonly TOutput[]> {
  if (!Number.isSafeInteger(options.maximumConcurrent) || options.maximumConcurrent < 1) {
    throw new TypeError('maximum concurrent initial claim inspections is invalid')
  }
  if (options.work.length === 0) return Object.freeze([])

  const results: (TOutput | undefined)[] = Array.from({ length: options.work.length })
  const failures: IndexedInspectionFailure[] = []
  let nextWorkIndex = 0
  let active = 0
  let admissionClosed = false

  const observeState = () => {
    try {
      options.observeState?.(Object.freeze({
        active,
        queuedMembers: admissionClosed ? 0 : options.work.length - nextWorkIndex,
      }))
    } catch {
      // Inspector measurements cannot acquire scheduling authority.
    }
  }
  const runWorker = async () => {
    while (!admissionClosed) {
      const workIndex = nextWorkIndex
      if (workIndex >= options.work.length) return
      nextWorkIndex += 1
      active += 1
      observeState()
      const performance = options.performance?.beginActive()
      try {
        results[workIndex] = await options.inspect(options.work[workIndex]!)
      } catch (error) {
        failures.push({ workIndex, error })
        admissionClosed = true
      } finally {
        performance?.finish()
        active -= 1
        observeState()
      }
    }
  }

  observeState()
  await Promise.all(Array.from(
    { length: Math.min(options.maximumConcurrent, options.work.length) },
    runWorker,
  ))
  const failure = failures.sort((left, right) => left.workIndex - right.workIndex)[0]
  if (failure !== undefined) throw failure.error
  if (results.some(result => result === undefined)) {
    throw new TypeError('initial claim inspection group stopped before draining its work')
  }
  return Object.freeze(results as TOutput[])
}
