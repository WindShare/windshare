import type { V2ReceiverController } from '../v2-controller'
import type { V2ReceiverSnapshot } from '../v2-model'
import type { TaskViewActions } from '../tasks/TaskView'

export function taskActions(controller: V2ReceiverController, snapshot: V2ReceiverSnapshot): TaskViewActions {
  return {
    perform: action => {
      if (action.disabledReason !== null) return
      if (action.target.kind === 'active') controller.performLifecycleAction(action.target.action)
      else controller.performRetainedAction(action.target.operation, action.target.action)
    },
    catchUp: operationId => {
      const retained = snapshot.retained.operations.find(operation => operation.operationId === operationId)
      if (retained?.actions.includes('catch-up')) controller.performRetainedAction(retained, 'catch-up')
      else if (snapshot.output.lifecycle?.operationId === operationId) controller.catchUpStoppedCompatibleNames()
    },
  }
}
