import type { V2ReceiverSnapshot } from '../v2-model'
import type { V2RetainedReceiveAction } from '../v2-receive-runtime'
import { activeTaskFacts, presentTask, retainedTaskFacts, type TaskFacts, type TaskPresentation } from '../tasks'
import { retainedContinuationReadiness } from '../controller/retained-readiness'
import { isReceiveOutputDelivered } from '../operation-ownership/completion'
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
  const retained = snapshot.retained.operations.map(operation => presentTask(withSettledTransferProgress(retainedTaskFacts(
    operation, retainedContinuationReadiness(operation, snapshot.share?.shareInstance ?? null), {
      pending: snapshot.retained.pending,
      blocking: snapshot.share !== null && operation.shareInstance === snapshot.share.shareInstance
        ? taskConnectionBlocking(snapshot.connection) : null,
      ...(admission === undefined ? {} : { admission: action => admission(operation, action) }),
    },
  ), currentFacts, snapshot.activeReceiveOperationId)))
  const pendingLocal = retained.find(task => task.operationId === snapshot.retained.pending?.operationId &&
    task.transition.reason === 'local-finalization-active')
  if (pendingLocal !== undefined && (current === null || current.operationId === pendingLocal.operationId)) {
    current = pendingLocal
  }
  if (current !== null && snapshot.activeReceiveOperationId !== current.operationId && pendingLocal === undefined) {
    const stored = retained.find(task => task.operationId === current?.operationId)
    // Child storage can change without advancing its terminal source lifecycle.
    // A detached result therefore takes both facts and actions from durable inventory.
    if (stored !== undefined && stored.generation >= current.generation) current = stored
    else {
      current = { ...current, primaryAction: null, secondaryActions: [], destructiveActions: [] }
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

function withSettledTransferProgress(retained: TaskFacts, current: TaskFacts | null, activeOperationId: string | null): TaskFacts {
  if (current === null || current.progress === null || current.progress.transferJobId.length === 0 ||
      activeOperationId === current.lifecycle.operationId || !isReceiveOutputDelivered(current.lifecycle) ||
      current.lifecycle.operationId !== retained.lifecycle.operationId ||
      current.lifecycle.receiveIntentDigest !== retained.lifecycle.receiveIntentDigest ||
      current.lifecycle.generation !== retained.lifecycle.generation) return retained
  // Inventory owns current child storage facts. The settled transfer still owns
  // the count and size observations that portable history does not persist.
  return Object.freeze({ ...retained, progress: current.progress })
}
