import type { ReceiverController } from '../controller'
import type { TaskViewActions } from '../tasks/TaskView'

export function taskActions(controller: ReceiverController): TaskViewActions {
  return {
    perform: action => {
      if (action.disabledReason !== null) return
      if (action.target.kind === 'active') controller.performLifecycleAction(action.target.action)
      else controller.performRetainedAction(action.target.operation, action.target.action)
    },
  }
}
