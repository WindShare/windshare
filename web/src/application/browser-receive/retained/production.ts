import type { ReceiveOperationMutationPort } from '../../../output/resume/authority'
import type { AuthorityOwnedReceiveOperationMutationResult } from '../../../output/resume/reopen-authority'
import type { BrowserReceiveWindow } from '../../../output/browser/environment'
import { listBrowserRetainedOperations, type BrowserRetainedCompositionOptions } from './inventory'
import { prepareRetainedActivation } from './activation'
import type { RetainedReceiveOperation } from '../../receive/contracts'

export function listRetainedOperations(windowPort: BrowserReceiveWindow, options: BrowserRetainedCompositionOptions, signal: AbortSignal) {
  let pending: Promise<import('../../../output/resume/reopen/cleanup').AuthorityOwnedReceiveOperationMutationPort> | undefined
  const mutations = () => pending ??= import('../../../output/resume/reopen/cleanup').then(module =>
    module.createBrowserReceiveOperationMutationPort({
      ...(options.outputTrace === undefined ? {} : { outputTrace: options.outputTrace }),
    }))
  const deferred: ReceiveOperationMutationPort<AuthorityOwnedReceiveOperationMutationResult> = {
    resume: (...args) => mutations().then(port => port.resume(...args)),
    cleanup: (...args) => mutations().then(port => port.cleanup(...args)),
    discard: (...args) => mutations().then(port => port.discard(...args)),
    catchUp: (...args) => mutations().then(port => port.catchUp(...args)),
    forget: (...args) => mutations().then(port => port.forget(...args)),
  }
  return listBrowserRetainedOperations(windowPort, {
    ...options,
    ...(options.openResumeSource === undefined && options.resumeMutations === undefined
      ? { resumeMutations: deferred } : {}),
    ...(options.openResumeSource === undefined && options.prepareActivation === undefined
      ? { prepareActivation: async (operation: RetainedReceiveOperation, preparationSignal: AbortSignal) => {
          // An enabled local action must remain usable if connectivity disappears
          // after discovery; preparation loads code without acquiring mutation authority.
          await prepareRetainedModules(operation, mutations)
          preparationSignal.throwIfAborted()
          return prepareRetainedActivation(operation, preparationSignal)
        } } : {}),
  }, signal)
}

async function prepareRetainedModules(operation: RetainedReceiveOperation, mutations: () => Promise<unknown>): Promise<void> {
  if (operation.actions.length === 0) return
  const work: Promise<unknown>[] = [mutations()]
  if (operation.actions.some(action => ['continue', 'save', 'redownload', 'catch-up', 'save-partial'].includes(action))) {
    work.push(import('./continuation'))
  }
  if (operation.browserDelivery !== undefined) work.push(import('../fsa/local-delivery'))
  if (operation.lifecycle.kind === 'resumable-receive' && operation.lifecycle.payloadKind === 'workspace-archive') {
    work.push(import('./progressive'))
  }
  if (operation.actions.includes('save-partial')) work.push(import('../../../output/portable/partial-export'))
  await Promise.all(work)
}
