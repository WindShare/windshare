import { createRoot, type Root } from 'react-dom/client'
import { flushSync } from 'react-dom'
import type { ReceiveLifecycleState } from '../../src/output/workspace'
import { CompatibleNameRepairPanel } from '../../src/ui/compatible-name/CompatibleNameRepairPanel'
import { presentCompatibleNameRepair } from '../../src/ui/compatible-name-repair-presentation'

let root: Root | undefined
let clipboardResolve: (() => void) | undefined
let clipboardReject: (() => void) | undefined
let copiedText: string | undefined

export function mountRepair(mode: 'receiving' | 'stopped' | 'pending' | 'completed'): void {
  root?.unmount()
  const container = document.createElement('div')
  document.body.replaceChildren(container)
  root = createRoot(container)
  const state: ReceiveLifecycleState = mode === 'completed'
    ? { kind: 'published', operationId: 'operation', receiveIntentDigest: 'intent',
        generation: 1n, receiptDigest: 'receipt', cleanupState: 'clean' }
    : { kind: 'receiving', operationId: 'operation', receiveIntentDigest: 'intent',
        generation: 1n, activeLeaseId: 'lease' }
  const presentation = presentCompatibleNameRepair({
    state,
    context: mode === 'receiving' ? 'receive-lifecycle' : 'retained-operation',
    summary: {
      committedCount: 1,
      logicalPathSample: [['a.cfg']],
      pairDisplayNames: { script: 'restore.windshare-abc234.ps1', sidecar: 'restore.windshare-abc234.data' },
      placement: 'inside-logical-root',
      sidecarSync: mode === 'pending' ? 'pending' : 'current',
      terminalSettlement: mode === 'completed' ? 'complete' : 'none',
      latestObservedFooter: { committedCount: 1, state: mode === 'completed' ? 'completed' : 'active' },
    },
  })
  flushSync(() => root!.render(
    <CompatibleNameRepairPanel repair={presentation} writeClipboard={captureClipboard} catchUp={() => undefined} />,
  ))
}

export function finishCopy(success: boolean): string | undefined {
  if (success) clipboardResolve?.()
  else clipboardReject?.()
  return copiedText
}

function captureClipboard(text: string): Promise<void> {
  copiedText = text
  return new Promise<void>((resolve, reject) => {
    clipboardResolve = resolve
    clipboardReject = () => reject(new Error('clipboard unavailable'))
  })
}
