import type { FileManifestEntry, TransferPlan } from '../contracts'
import type { DownloadTarget } from '../download'
import type { OutputChoice, OutputChoiceId } from './model'
import { ReceiverPublicError } from './model'

const ARCHIVE_NAME = 'windshare.zip'
const TEMPORARY_PREFIX = '.windshare-download-'
const MAX_TEMPORARY_NAME_ATTEMPTS = 8
const BACKING_FILE_RELEASE_ATTEMPTS = 3
const TEMPORARY_ID_PATTERN = /^[a-zA-Z0-9-]{1,128}$/u

interface SaveFilePickerOptions {
  readonly suggestedName?: string
}

interface PickerWindow extends Window {
  showDirectoryPicker?: () => Promise<FileSystemDirectoryHandle>
  showSaveFilePicker?: (
    options?: SaveFilePickerOptions,
  ) => Promise<FileSystemFileHandle>
}

interface OriginStorage {
  getDirectory?: () => Promise<FileSystemDirectoryHandle>
}

export interface PreparedBrowserOutput {
  transferTarget<T>(receiver: (target: DownloadTarget) => T): T
  commit(): Promise<void>
  abort(reason: unknown): Promise<void>
}

export interface BrowserOutputRuntime {
  readonly browserWindow: PickerWindow
  readonly storage: OriginStorage
  readonly randomId: () => string
  present(file: File, suggestedName: string, release: () => Promise<void>): void
}

export type BrowserOutputPreparer = (
  plan: TransferPlan,
  choice: OutputChoiceId,
) => Promise<PreparedBrowserOutput>

type TargetOwnershipState = 'owned' | 'transferred' | 'transferring'

class PreparedTargetOwnership {
  readonly #target: DownloadTarget
  #state: TargetOwnershipState = 'owned'

  constructor(target: DownloadTarget) {
    this.#target = target
  }

  get retained(): boolean {
    if (this.#state === 'transferring') {
      throw new Error('prepared output cannot settle during target transfer')
    }
    return this.#state === 'owned'
  }

  transfer<T>(receiver: (target: DownloadTarget) => T): T {
    if (this.#state !== 'owned') {
      throw new Error('prepared output target can only be transferred once')
    }
    this.#state = 'transferring'
    try {
      const result = receiver(this.#target)
      this.#state = 'transferred'
      return result
    } catch (error) {
      // A receiver that never materialized an owner must leave cleanup authority
      // with the prepared output so picker-created resources remain reachable.
      this.#state = 'owned'
      throw error
    }
  }

  requireTransferred(): void {
    if (this.#state !== 'transferred') {
      throw new Error('prepared output target has not been transferred')
    }
  }
}

function browserStorage(): OriginStorage {
  try {
    return (navigator.storage as OriginStorage | undefined) ?? {}
  } catch {
    return {}
  }
}

function directoryPicker(runtime: BrowserOutputRuntime): PickerWindow['showDirectoryPicker'] {
  try {
    const picker = runtime.browserWindow.showDirectoryPicker
    return typeof picker === 'function' ? picker : undefined
  } catch {
    return undefined
  }
}

function saveFilePicker(runtime: BrowserOutputRuntime): PickerWindow['showSaveFilePicker'] {
  try {
    const picker = runtime.browserWindow.showSaveFilePicker
    return typeof picker === 'function' ? picker : undefined
  } catch {
    return undefined
  }
}

function originDirectoryGetter(runtime: BrowserOutputRuntime): OriginStorage['getDirectory'] {
  try {
    const getDirectory = runtime.storage.getDirectory
    return typeof getDirectory === 'function' ? getDirectory : undefined
  } catch {
    return undefined
  }
}

function defaultRuntime(): BrowserOutputRuntime {
  const browserWindow = window as PickerWindow
  const present = createDownloadPresenter(browserWindow)
  return {
    browserWindow,
    storage: browserStorage(),
    randomId: () => crypto.randomUUID(),
    present,
  }
}

function createDownloadPresenter(browserWindow: PickerWindow): BrowserOutputRuntime['present'] {
  const retainedUrls = new Set<string>()
  const retainedReleases = new Set<() => Promise<void>>()
  let cleanupRegistered = false
  const release = async () => {
    for (const url of retainedUrls) URL.revokeObjectURL(url)
    retainedUrls.clear()
    const cleanups = [...retainedReleases]
    retainedReleases.clear()
    cleanupRegistered = false
    const results = await Promise.allSettled(
      cleanups.map((cleanup) => releaseBackingFile(cleanup)),
    )
    const failures: unknown[] = []
    for (const [index, result] of results.entries()) {
      if (result.status === 'rejected') {
        // Keep exact ownership if teardown is transient. A later presentation or
        // lifecycle boundary can retry without ever scanning unrelated OPFS data.
        const cleanup = cleanups[index]
        if (cleanup !== undefined) retainedReleases.add(cleanup)
        failures.push(result.reason)
      }
    }
    if (failures.length > 0) {
      // A persisted page can retry on its next teardown. Waiting for pageshow
      // avoids registering recursively while the current pagehide is dispatching.
      browserWindow.addEventListener('pageshow', registerCleanup, { once: true })
    }
    throwCleanupErrors(failures)
  }
  const registerCleanup = () => {
    if (cleanupRegistered) return
    browserWindow.addEventListener('pagehide', () => {
      // EventTarget cannot observe asynchronous listener failures. Exact ownership
      // remains registered for another lifecycle boundary when the page survives.
      release().catch(() => undefined)
    }, { once: true })
    cleanupRegistered = true
  }
  return (file, suggestedName, releaseTemporaryFile) => {
    const url = URL.createObjectURL(file)
    retainedUrls.add(url)
    retainedReleases.add(releaseTemporaryFile)
    if (!cleanupRegistered) {
      // A download click has no completion event. Page teardown is the first
      // browser-owned boundary at which revocation cannot race URL consumption.
      registerCleanup()
    }
    const anchor = document.createElement('a')
    anchor.href = url
    anchor.download = suggestedName
    anchor.hidden = true
    let presented = false
    try {
      document.body.append(anchor)
      anchor.click()
      presented = true
    } finally {
      anchor.remove()
      if (!presented) {
        retainedUrls.delete(url)
        retainedReleases.delete(releaseTemporaryFile)
        URL.revokeObjectURL(url)
      }
    }
  }
}

export async function releaseBackingFile(release: () => Promise<void>): Promise<void> {
  let lastFailure: unknown
  for (let attempt = 0; attempt < BACKING_FILE_RELEASE_ATTEMPTS; attempt += 1) {
    try {
      await release()
      return
    } catch (error) {
      lastFailure = error
    }
  }
  throw lastFailure
}

function selectedSingleFile(plan: TransferPlan): FileManifestEntry | undefined {
  if (plan.selectedEntries.length !== 1) {
    return undefined
  }
  const entry = plan.selectedEntries[0]
  return entry?.kind === 'file' ? entry : undefined
}

function suggestedName(plan: TransferPlan): string {
  const file = selectedSingleFile(plan)
  return file?.path.split('/').at(-1) ?? ARCHIVE_NAME
}

function streamTarget(
  plan: TransferPlan,
  output: WritableStream<Uint8Array>,
): DownloadTarget {
  return selectedSingleFile(plan) === undefined
    ? { kind: 'zip-stream', output }
    : { kind: 'single-file-stream', output }
}

function inertPreparedOutput(target: DownloadTarget): PreparedBrowserOutput {
  const ownership = new PreparedTargetOwnership(target)
  let settled = false
  return Object.freeze({
    transferTarget: <T>(receiver: (target: DownloadTarget) => T) => {
      if (settled) {
        throw new Error('prepared output is already settled')
      }
      return ownership.transfer(receiver)
    },
    commit: async () => {
      ownership.requireTransferred()
      settled = true
    },
    abort: async () => {
      settled = true
    },
  })
}

async function preparePickedFile(
  picked: Promise<FileSystemFileHandle>,
  plan: TransferPlan,
): Promise<PreparedBrowserOutput> {
  const handle = await picked
  const writable = await handle.createWritable()
  const ownership = new PreparedTargetOwnership(
    streamTarget(plan, writable as unknown as WritableStream<Uint8Array>),
  )
  let settled = false
  let cleanupStarted = false
  let cleanupTask: Promise<void> | undefined
  return Object.freeze({
    transferTarget: <T>(receiver: (target: DownloadTarget) => T) => {
      if (settled || cleanupStarted) {
        throw new Error('prepared output is already settling')
      }
      return ownership.transfer(receiver)
    },
    commit: async () => {
      ownership.requireTransferred()
      settled = true
    },
    abort: async (reason: unknown) => {
      if (settled) {
        return
      }
      cleanupStarted = true
      cleanupTask ??= (async () => {
        if (ownership.retained) {
          await writable.abort(reason)
        }
        settled = true
      })()
      try {
        await cleanupTask
      } catch (error) {
        cleanupTask = undefined
        throw error
      }
    },
  })
}

function errorNamed(error: unknown, name: string): boolean {
  return (
    typeof error === 'object' &&
    error !== null &&
    'name' in error &&
    (error as { readonly name?: unknown }).name === name
  )
}

async function removeTemporary(
  root: FileSystemDirectoryHandle,
  temporaryName: string,
): Promise<void> {
  try {
    await root.removeEntry(temporaryName)
  } catch (error) {
    if (!errorNamed(error, 'NotFoundError')) {
      throw error
    }
  }
}

class TemporaryFileOwnership {
  readonly #root: FileSystemDirectoryHandle
  readonly #name: string
  #removed = false
  #removalTask: Promise<void> | undefined

  constructor(root: FileSystemDirectoryHandle, name: string) {
    this.#root = root
    this.#name = name
  }

  get retained(): boolean {
    return !this.#removed
  }

  remove(): Promise<void> {
    if (this.#removed) {
      return Promise.resolve()
    }
    this.#removalTask ??= this.#removeOnce().catch((error: unknown) => {
      // A failed exact-name operation must remain retryable; clearing only the
      // task never broadens authority to any other origin-storage entry.
      this.#removalTask = undefined
      throw error
    })
    return this.#removalTask
  }

  async #removeOnce(): Promise<void> {
    await removeTemporary(this.#root, this.#name)
    this.#removed = true
  }
}

/**
 * Carries the operation failure and the only capability that can finish cleaning
 * the exact temporary file. A bounded retry task is shared by concurrent callers;
 * exhaustion leaves the same authority reachable for a later explicit retry.
 */
export class BrowserOutputPreparationFailure extends AggregateError {
  readonly primary: unknown
  readonly #cleanup: () => Promise<void>
  #cleanupTask: Promise<void> | undefined

  constructor(
    primary: unknown,
    cleanupError: unknown,
    cleanup: () => Promise<void>,
  ) {
    super(
      [primary, cleanupError],
      'Temporary output creation and cleanup both failed',
      { cause: primary },
    )
    this.name = 'BrowserOutputPreparationFailure'
    this.primary = primary
    this.#cleanup = cleanup
  }

  retryCleanup(): Promise<void> {
    this.#cleanupTask ??= releaseBackingFile(this.#cleanup).catch((error: unknown) => {
      this.#cleanupTask = undefined
      throw error
    })
    return this.#cleanupTask
  }

  withCleanupFailure(cleanupError: unknown): BrowserOutputPreparationFailure {
    return new BrowserOutputPreparationFailure(this.primary, cleanupError, this.#cleanup)
  }
}

async function allocateTemporaryFile(
  root: FileSystemDirectoryHandle,
  runtime: BrowserOutputRuntime,
): Promise<{ readonly name: string; readonly handle: FileSystemFileHandle }> {
  for (let attempt = 0; attempt < MAX_TEMPORARY_NAME_ATTEMPTS; attempt += 1) {
    const id = runtime.randomId()
    if (!TEMPORARY_ID_PATTERN.test(id)) {
      continue
    }
    const name = `${TEMPORARY_PREFIX}${id}`
    try {
      await root.getFileHandle(name)
      continue
    } catch (error) {
      if (!errorNamed(error, 'NotFoundError')) {
        throw error
      }
    }
    return {
      name,
      handle: await root.getFileHandle(name, { create: true }),
    }
  }
  throw new ReceiverPublicError(
    'output-unavailable',
    'A private temporary download file could not be allocated.',
  )
}

function throwCleanupErrors(errors: readonly unknown[]): void {
  if (errors.length === 1) {
    throw errors[0]
  }
  if (errors.length > 1) {
    throw new AggregateError(errors, 'Prepared browser output cleanup failed')
  }
}

async function prepareTemporaryOutput(
  rootRequest: Promise<FileSystemDirectoryHandle>,
  plan: TransferPlan,
  runtime: BrowserOutputRuntime,
): Promise<PreparedBrowserOutput> {
  const root = await rootRequest
  const temporary = await allocateTemporaryFile(root, runtime)
  const temporaryOwnership = new TemporaryFileOwnership(root, temporary.name)
  let writable: FileSystemWritableFileStream
  try {
    writable = await temporary.handle.createWritable()
  } catch (error) {
    try {
      await temporaryOwnership.remove()
    } catch (cleanupError) {
      throw new BrowserOutputPreparationFailure(
        error,
        cleanupError,
        () => temporaryOwnership.remove(),
      )
    }
    throw error
  }
  let streamSettled = false
  let presented = false
  let cleanupStarted = false
  let cleanupTask: Promise<void> | undefined
  const ownership = new PreparedTargetOwnership(
    streamTarget(plan, writable as unknown as WritableStream<Uint8Array>),
  )

  return Object.freeze({
    transferTarget: <T>(receiver: (target: DownloadTarget) => T) => {
      if (cleanupStarted || !temporaryOwnership.retained) {
        throw new Error('prepared output is already settling')
      }
      return ownership.transfer(receiver)
    },
    commit: async () => {
      if (!temporaryOwnership.retained) {
        return
      }
      ownership.requireTransferred()
      // Download sinks finalize their stream before committing presentation.
      streamSettled = true
      if (!presented) {
        runtime.present(
          await temporary.handle.getFile(),
          suggestedName(plan),
          () => temporaryOwnership.remove(),
        )
        presented = true
      }
    },
    abort: async (reason: unknown) => {
      if (!temporaryOwnership.retained) {
        return
      }
      cleanupStarted = true
      cleanupTask ??= (async () => {
        const errors: unknown[] = []
        if (!streamSettled && ownership.retained) {
          try {
            await writable.abort(reason)
            streamSettled = true
          } catch (error) {
            errors.push(error)
          }
        } else if (!ownership.retained) {
          // The sink owner has already settled or reported its stream failure;
          // this layer retains only the exact backing-file cleanup capability.
          streamSettled = true
        }
        try {
          await temporaryOwnership.remove()
        } catch (error) {
          errors.push(error)
        }
        throwCleanupErrors(errors)
      })()
      try {
        await cleanupTask
      } catch (error) {
        cleanupTask = undefined
        throw error
      }
    },
  })
}

export function browserOutputChoices(
  runtime: BrowserOutputRuntime = defaultRuntime(),
): readonly OutputChoice[] {
  const folderAvailable = directoryPicker(runtime) !== undefined
  const downloadAvailable =
    saveFilePicker(runtime) !== undefined || originDirectoryGetter(runtime) !== undefined
  return Object.freeze([
    Object.freeze({
      id: 'folder' as const,
      label: 'Files and folders',
      description: 'Save the selected tree into a folder you choose.',
      available: folderAvailable,
    }),
    Object.freeze({
      id: 'download' as const,
      label: 'Browser download',
      description: 'Save one file directly, or package multiple entries as a ZIP.',
      available: downloadAvailable,
    }),
  ])
}

/** Picker calls happen before this function returns its promise to preserve activation. */
export function prepareBrowserOutput(
  plan: TransferPlan,
  choice: OutputChoiceId,
  runtime: BrowserOutputRuntime = defaultRuntime(),
): Promise<PreparedBrowserOutput> {
  if (choice === 'folder') {
    const picker = directoryPicker(runtime)
    if (picker === undefined) {
      return Promise.reject(
        new ReceiverPublicError(
          'output-unavailable',
          'Folder access is not available in this browser.',
        ),
      )
    }
    const picked = picker.call(runtime.browserWindow)
    return picked.then((root) => inertPreparedOutput({ kind: 'file-system', root }))
  }
  if (choice !== 'download') {
    return Promise.reject(
      new ReceiverPublicError('output-unavailable', 'The selected output type is not supported.'),
    )
  }

  const picker = saveFilePicker(runtime)
  if (picker !== undefined) {
    const picked = picker.call(runtime.browserWindow, { suggestedName: suggestedName(plan) })
    return preparePickedFile(picked, plan)
  }
  const getDirectory = originDirectoryGetter(runtime)
  if (getDirectory !== undefined) {
    const rootRequest = getDirectory.call(runtime.storage)
    return prepareTemporaryOutput(rootRequest, plan, runtime)
  }
  return Promise.reject(
    new ReceiverPublicError(
      'output-unavailable',
      'Streaming downloads are not available in this browser.',
    ),
  )
}
