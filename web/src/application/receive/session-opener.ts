import type { SaveBackendKind } from '../../output/model'
import type { BrowserOutputWindow, OutputSessionHandle, StartBackendTaskInput } from '../../output/backends/contract'
import { openNewFSASession } from '../../output/backends/fsa'
import { browserOPFSEnvironment, openNewOPFSSession, type OPFSEnvironment } from '../../output/backends/opfs'
import { openPortableSession } from '../../output/backends/portable'
import { diagnosticBackendName, type OutputTraceSource } from '../../output/diagnostics'
import type { ReceiveStore } from '../../output/store/database'

export interface OpenNewOutputSessionInput {
  readonly backend: SaveBackendKind
  readonly task: StartBackendTaskInput
  readonly parentHandle: FileSystemDirectoryHandle | undefined
  readonly windowPort: BrowserOutputWindow
  readonly opfsEnv?: OPFSEnvironment
  readonly outputTrace?: OutputTraceSource
  readonly openStoreOptional: () => Promise<ReceiveStore | undefined>
  readonly requireStore: () => Promise<ReceiveStore>
}

export async function openNewOutputSession(input: OpenNewOutputSessionInput): Promise<OutputSessionHandle> {
  const diagnostics = input.outputTrace === undefined
    ? undefined
    : Object.freeze({ backend: diagnosticBackendName(input.backend), trace: input.outputTrace })
  const locks = input.windowPort.navigator?.locks

  if (input.backend === 'portable') {
    const store = await input.openStoreOptional()
    return openPortableSession({
      task: input.task,
      windowPort: input.windowPort,
      ...(store === undefined ? {} : { store }),
      ...(diagnostics === undefined ? {} : { diagnostics }),
    })
  }

  const store = await input.requireStore()
  if (input.backend === 'fsa') {
    if (input.parentHandle === undefined) {
      throw new DOMException('No folder was selected', 'AbortError')
    }
    return openNewFSASession({
      task: input.task,
      parentHandle: input.parentHandle,
      store,
      ...(locks === undefined ? {} : { locks }),
      ...(diagnostics === undefined ? {} : { diagnostics }),
    })
  }

  const opfsEnv = input.opfsEnv ?? browserOPFSEnvironment(input.windowPort.navigator?.storage, locks)
  return openNewOPFSSession({
    task: input.task,
    store,
    env: opfsEnv,
    windowPort: input.windowPort,
    ...(diagnostics === undefined ? {} : { diagnostics }),
  })
}
