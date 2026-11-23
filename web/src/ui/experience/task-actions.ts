import type { ReceiverController, TaskActionCallbacks } from '../controller'
import type { TaskViewActions } from '../tasks/TaskView'

export function taskActions(
  controller: ReceiverController,
  callbacks?: TaskActionCallbacks,
): TaskViewActions {
  return {
    perform: action => {
      if (action.disabledReason !== null) return
      if (action.target.kind === 'active') controller.performLifecycleAction(action.target.action, callbacks)
      else controller.performRetainedAction(action.target.task, action.target.action, callbacks)
    },
  }
}
