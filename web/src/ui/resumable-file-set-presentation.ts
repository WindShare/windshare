import type { ReceiveLifecycleState } from '../output/workspace'
import type { MaterializationPlan } from '../transfer/intent'
import { formatBytes } from './v2-progress-presentation'

type ResumableFileSetState = Extract<ReceiveLifecycleState, {
  kind: 'resumable-receive'
  payloadKind: 'file-set'
}>

export function resumableFileSetDescription(
  state: ResumableFileSetState,
  planKind: MaterializationPlan['kind'],
): string {
  const completed = `${state.completedFileCount} file(s) and ${formatBytes(state.completedBytes)} are complete.`
  if (planKind !== 'direct-tree') return `${completed} Continuing still requires the sender and save permission.`
  return `${completed} Continue preserves verified partial files and may require temporary destination space; restarting incomplete files downloads only those files again.`
}
