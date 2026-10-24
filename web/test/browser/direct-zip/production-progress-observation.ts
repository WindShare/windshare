import type { BoundReceiveOperation, DirectZipProgressSnapshot } from '../../../src/application/receive/contracts'
import { presentDirectZipProgress } from '../../../src/ui/progress-presentation'
import { presentReceiveLifecycle } from '../../../src/ui/lifecycle-presentation'
import type { ReceiveLifecycleState } from '../../../src/transfer/lifecycle/state'

export function observeProductionDirectZipProgress(selectedBytes: bigint) {
  const samples: Record<string, ReturnType<typeof capture>> = {}
  const notifications: ReturnType<typeof counters>[][] = []
  let unsubscribe: (() => void) | undefined

  function capture(operation: BoundReceiveOperation, lifecycle: ReceiveLifecycleState = operation.lifecycle) {
    const progress = operation.outputProgress?.getSnapshot()
    if (progress?.kind !== 'direct-zip') throw new Error('Production operation lost Direct ZIP progress')
    const presentation = presentDirectZipProgress({
      progress, selectedBytes: { kind: 'exact', bytes: selectedBytes }, lifecycle,
    })
    const lifecyclePresentation = presentReceiveLifecycle({
      state: lifecycle, artifact: operation.intent.artifact, plan: operation.intent.plan,
      activeControls: operation.activeControls,
    })
    return { ...counters(progress), primary: presentation.primary,
      percentage: presentation.percentage?.toString(), safeResume: presentation.safeResume,
      lifecycle: lifecycle.kind, category: lifecyclePresentation.category,
      actions: lifecyclePresentation.actions.map(action => action.kind) }
  }

  return {
    bind: (operation: BoundReceiveOperation) => {
      unsubscribe?.()
      const events: ReturnType<typeof counters>[] = []
      notifications.push(events)
      unsubscribe = operation.outputProgress?.subscribe(progress => {
        if (progress.kind !== 'direct-zip') throw new Error('Production progress changed its output kind')
        events.push(counters(progress))
      })
    },
    sample: (name: string, operation: BoundReceiveOperation, lifecycle?: ReceiveLifecycleState) => {
      samples[name] = capture(operation, lifecycle)
    },
    result: () => ({ samples, notifications }),
    close: () => unsubscribe?.(),
  }
}

function counters(progress: DirectZipProgressSnapshot) {
  return { operationId: progress.operationId, generation: progress.generation.toString(),
    received: progress.receivedSelectedBytes.toString(), written: progress.writtenSelectedBytes.toString(),
    safe: progress.safeResumeBytes.toString() }
}
