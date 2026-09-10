import type { V2LifecycleMutation } from '../../../src/ui/v2-receive-runtime'
import { createMemberRollbackFixture, readMemberRollbackState, MEMBER_ROLLBACK_SIGNAL } from './member-rollback-fixture'
import { finishMemberRollbackFixture } from './member-rollback-probe'
import { observeProductionDirectZipFileSystem } from './production-fsa-observation'

export type AuthorizationResponse = PermissionState | 'cancelled' | 'pending'
interface PermissionHandle extends FileSystemHandle {
  queryPermission(options?: { mode?: string }): Promise<PermissionState>
  requestPermission(options?: { mode?: string }): Promise<PermissionState>
}

export async function prepareAuthorizationProbe(databaseName: string) {
  const fixture = await createMemberRollbackFixture(databaseName, 'unchanged-revision')
  const active = await fixture.resume()
  const prototype = FileSystemHandle.prototype as PermissionHandle
  const originalQuery = prototype.queryPermission
  const originalRequest = prototype.requestPermission
  const directoryPrototype = FileSystemDirectoryHandle.prototype
  const originalLookup = directoryPrototype.getFileHandle
  const fileSystem = observeProductionDirectZipFileSystem()
  let permission: PermissionState = 'prompt'
  let response: AuthorizationResponse = 'granted'
  let resolvePermission: ((state: PermissionState) => void) | undefined
  let insideClick = false
  let lookups = 0
  const requests: { mode: string | undefined; directory: string; insideClick: boolean; userActivation: boolean }[] = []
  const isParent = (handle: FileSystemHandle) => handle.kind === 'directory' && handle.name === databaseName
  prototype.queryPermission = function (options) {
    return isParent(this) ? Promise.resolve(permission) : originalQuery.call(this, options)
  }
  prototype.requestPermission = function (options) {
    if (!isParent(this)) return originalRequest.call(this, options)
    requests.push({ mode: options?.mode, directory: this.name, insideClick,
      userActivation: navigator.userActivation.isActive })
    if (response === 'cancelled') return Promise.reject(new DOMException('Authorization cancelled', 'AbortError'))
    if (response === 'pending') return new Promise(resolve => { resolvePermission = resolve })
    permission = response
    return Promise.resolve(permission)
  }
  directoryPrototype.getFileHandle = function (name, options) {
    if (isParent(this)) lookups += 1
    return originalLookup.call(this, name, options)
  }
  const button = document.createElement('button')
  button.textContent = 'Authorize and continue'
  document.body.append(button)
  const summarize = (mutation: V2LifecycleMutation) => ({
    lifecycle: mutation.lifecycle.kind, resumeTransfer: mutation.resumeTransfer, error: undefined as string | undefined,
  })
  let attempt: Promise<ReturnType<typeof summarize>> | undefined
  button.onclick = () => {
    insideClick = true
    try {
      attempt = Promise.resolve(active.startLifecycleAction('continue', active.lifecycle)).then(summarize,
        (error: unknown) => ({ lifecycle: active.lifecycle.kind, resumeTransfer: undefined,
          error: error instanceof Error ? error.name : String(error) }))
    } finally { insideClick = false }
  }
  const close = async () => {
    button.remove()
    prototype.queryPermission = originalQuery
    prototype.requestPermission = originalRequest
    directoryPrototype.getFileHandle = originalLookup
    fileSystem.restore()
    await fixture.close()
  }
  try {
    // Revoke a live operation's permission at the browser boundary. Automatic
    // verification must surface the gate without trying to show a permission prompt.
    try { await active.plans.openDirectResumableZip(fixture.intent, MEMBER_ROLLBACK_SIGNAL) }
    catch (error) { await active.settleTransferAdmissionFailure(error) }
    const snapshot = async () => {
      const state = await readMemberRollbackState(databaseName, fixture.intent.operationId)
      return {
        lifecycle: active.lifecycle.kind, generation: active.lifecycle.generation.toString(),
        checkpoint: state.checkpoint.digest, candidate: state.candidate?.digest,
        bytes: Array.from(new Uint8Array(await (await fixture.file.getFile()).arrayBuffer())),
        fileSystem: fileSystem.snapshot(), lookups,
      }
    }
    return {
      initial: await snapshot(),
      requests,
      configure: (next: AuthorizationResponse) => { response = next },
      grantPending: () => { permission = 'granted'; resolvePermission?.(permission) },
      result: () => attempt,
      snapshot,
      replaceTargetContents: async () => {
        const writer = await fixture.file.createWritable()
        await writer.write(Uint8Array.of(90, 91, 92))
        await writer.close()
      },
      finish: () => finishMemberRollbackFixture(fixture, active),
      close,
    }
  } catch (error) { await close(); throw error }
}
