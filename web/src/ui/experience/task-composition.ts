import type { V2ReceiverSnapshot } from '../v2-model'
import type { V2RetainedReceiveAction } from '../v2-receive-runtime'
import { activeTaskFacts, presentTask, retainedTaskFacts, type TaskPresentation } from '../tasks'
import { retainedContinuationReadiness } from '../controller/retained-readiness'
import { taskConnectionBlocking } from './share-presentation'

type RetainedAdmission = (
  operation: V2ReceiverSnapshot['retained']['operations'][number],
  action: V2RetainedReceiveAction,
) => Readonly<{ allowed: boolean; reason: string | null }>

export function composeTasks(snapshot: V2ReceiverSnapshot, admission?: RetainedAdmission,
  activeAdmission?: (action: import('../v2-lifecycle-presentation').LifecycleUserAction) => Readonly<{ allowed: boolean; reason: string | null }>): {
  readonly current: TaskPresentation | null
  readonly tasks: readonly TaskPresentation[]
} {
  const currentFacts = activeTaskFacts({
    output: snapshot.output, progress: snapshot.progress, display: snapshot.taskDisplay,
    blocking: taskConnectionBlocking(snapshot.connection),
    ...(activeAdmission === undefined ? {} : { actionAdmission: activeAdmission }),
  })
  let current = currentFacts === null ? null : presentTask(currentFacts)
  const retained = snapshot.retained.operations.map(operation => presentTask(retainedTaskFacts(
    operation, retainedContinuationReadiness(operation, snapshot.share?.shareInstance ?? null), {
      pending: snapshot.retained.pending,
      blocking: snapshot.share !== null && operation.shareInstance === snapshot.share.shareInstance
        ? taskConnectionBlocking(snapshot.connection) : null,
      ...(admission === undefined ? {} : { admission: action => admission(operation, action) }),
    },
  )))
  const pendingLocal = retained.find(task => task.operationId === snapshot.retained.pending?.operationId &&
    task.transition.reason === 'local-finalization-active')
  if (pendingLocal !== undefined && (current === null || current.operationId === pendingLocal.operationId)) {
    current = pendingLocal
  }
  if (current !== null && snapshot.activeReceiveOperationId !== current.operationId && pendingLocal === undefined) {
    const stored = retained.find(task => task.operationId === current?.operationId)
    // A displayed result has no live runtime. Local actions belong to the reloaded durable record.
    if (stored !== undefined && stored.generation > current.generation) current = stored
    else {
      const actions = stored?.generation === current.generation ? stored : null
      current = { ...current, primaryAction: actions?.primaryAction ?? null,
        secondaryActions: actions?.secondaryActions ?? [], destructiveActions: actions?.destructiveActions ?? [] }
    }
  }
  const tasks = retained.filter(task => task.operationId !== current?.operationId)
  if (current !== null) tasks.unshift(current)
  tasks.sort((left, right) => {
    if (left.operationId === current?.operationId) return -1
    if (right.operationId === current?.operationId) return 1
    return (right.createdAtMilliseconds ?? 0) - (left.createdAtMilliseconds ?? 0)
  })
  return { current, tasks }
}
