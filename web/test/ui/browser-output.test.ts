import { describe, expect, it } from 'vitest'

import type { TransferPlan } from '../../src/contracts'
import {
  BrowserOutputPreparationFailure,
  browserOutputChoices,
  prepareBrowserOutput,
  releaseBackingFile,
  type BrowserOutputRuntime,
  type PreparedBrowserOutput,
} from '../../src/ui/browser-output'

const plan = {
  selectedEntries: [{ kind: 'file', path: 'report.txt', size: 0, mtime: 0 }],
} as unknown as TransferPlan
const BACKING_FILE_RELEASE_ATTEMPTS = 3

function transferStream(output: PreparedBrowserOutput): WritableStream<Uint8Array> {
  return output.transferTarget((target) => {
    if (target.kind === 'file-system') {
      throw new TypeError('expected a streaming browser output')
    }
    return target.output
  })
}

describe('prepared browser output ownership', () => {
  it('retains target cleanup when a receiver fails before accepting ownership', async () => {
    const abortReasons: unknown[] = []
    const writable = new WritableStream<Uint8Array>({
      abort: (reason) => {
        abortReasons.push(reason)
      },
    })
    const runtime = {
      browserWindow: {
        showSaveFilePicker: () => Promise.resolve({
          createWritable: () => Promise.resolve(writable),
        } as unknown as FileSystemFileHandle),
      },
      storage: {},
      randomId: () => 'fixed',
      present: () => undefined,
    } as unknown as BrowserOutputRuntime
    const output = await prepareBrowserOutput(plan, 'download', runtime)
    const receiverFailure = new Error('receiver construction failed')

    expect(() => output.transferTarget(() => {
      throw receiverFailure
    })).toThrow(receiverFailure)
    const cancellation = new DOMException('stop after receiver failure', 'AbortError')
    await output.abort(cancellation)

    expect(abortReasons).toEqual([cancellation])
    expect(writable.locked).toBe(false)
    expect(() => output.transferTarget((target) => target)).toThrow(
      'prepared output is already settling',
    )
  })
})

describe('failed browser output preparation ownership', () => {
  it('retains primary-first failure and shared exact-name cleanup authority', async () => {
    const creationError = new Error('create writable failed')
    const cleanupErrors = Array.from(
      { length: BACKING_FILE_RELEASE_ATTEMPTS + 1 },
      (_, index) => new Error(`remove failed ${index + 1}`),
    )
    const removedNames: string[] = []
    let removalAttempt = 0
    const root = {
      getFileHandle: (_name: string, options?: { readonly create?: boolean }) =>
        options?.create === true
          ? Promise.resolve({
              createWritable: () => Promise.reject(creationError),
            } as unknown as FileSystemFileHandle)
          : Promise.reject(new DOMException('missing', 'NotFoundError')),
      removeEntry: (name: string) => {
        removedNames.push(name)
        const cleanupError = cleanupErrors[removalAttempt]
        removalAttempt += 1
        return cleanupError === undefined
          ? Promise.resolve()
          : Promise.reject(cleanupError)
      },
    } as unknown as FileSystemDirectoryHandle
    const runtime = {
      browserWindow: {},
      storage: { getDirectory: () => Promise.resolve(root) },
      randomId: () => 'creation-failure',
      present: () => undefined,
    } as unknown as BrowserOutputRuntime

    const caught: unknown = await prepareBrowserOutput(plan, 'download', runtime).then(
      () => {
        throw new Error('expected output preparation to fail')
      },
      (error: unknown) => error,
    )
    expect(caught).toBeInstanceOf(BrowserOutputPreparationFailure)
    const failure = caught as BrowserOutputPreparationFailure
    expect(failure.errors).toEqual([creationError, cleanupErrors[0]])
    expect(failure.cause).toBe(creationError)
    expect(failure.primary).toBe(creationError)

    const firstRetry = failure.retryCleanup()
    const concurrentRetry = failure.retryCleanup()
    expect(concurrentRetry).toBe(firstRetry)
    const exhausted = await Promise.allSettled([firstRetry, concurrentRetry])
    expect(exhausted).toEqual([
      { status: 'rejected', reason: cleanupErrors.at(-1) },
      { status: 'rejected', reason: cleanupErrors.at(-1) },
    ])
    expect(removedNames).toEqual(
      Array.from(
        { length: BACKING_FILE_RELEASE_ATTEMPTS + 1 },
        () => '.windshare-download-creation-failure',
      ),
    )

    await failure.retryCleanup()
    await failure.retryCleanup()
    expect(removedNames).toEqual([
      ...Array.from(
        { length: BACKING_FILE_RELEASE_ATTEMPTS + 1 },
        () => '.windshare-download-creation-failure',
      ),
      '.windshare-download-creation-failure',
    ])
  })
})

describe('browser output preparation', () => {
  it('retries exact-owned backing-file cleanup within the lifecycle boundary', async () => {
    let attempts = 0
    await releaseBackingFile(async () => {
      attempts += 1
      if (attempts === 1) throw new Error('transient removal failure')
    })
    expect(attempts).toBe(2)

    const permanent = new Error('permanent removal failure')
    attempts = 0
    await expect(
      releaseBackingFile(async () => {
        attempts += 1
        throw permanent
      }),
    ).rejects.toBe(permanent)
    expect(attempts).toBe(BACKING_FILE_RELEASE_ATTEMPTS)
  })

  it('reports picker and origin-storage availability without guessing', () => {
    const available = browserOutputChoices({
      browserWindow: {
        showDirectoryPicker: () => Promise.reject(new Error('unused')),
        showSaveFilePicker: () => Promise.reject(new Error('unused')),
      },
      storage: {},
      randomId: () => 'fixed',
      present: () => undefined,
    } as unknown as BrowserOutputRuntime)
    const unavailable = browserOutputChoices({
      browserWindow: {},
      storage: {},
      randomId: () => 'fixed',
      present: () => undefined,
    } as unknown as BrowserOutputRuntime)

    expect(available.map((choice) => choice.available)).toEqual([true, true])
    expect(unavailable.map((choice) => choice.available)).toEqual([false, false])
  })

  it('treats hostile capability getters as unavailable instead of crashing boot', async () => {
    const browserWindow = {}
    const storage = {}
    Object.defineProperty(browserWindow, 'showDirectoryPicker', {
      get: () => {
        throw new Error('private browser detail')
      },
    })
    Object.defineProperty(browserWindow, 'showSaveFilePicker', {
      get: () => {
        throw new Error('private browser detail')
      },
    })
    Object.defineProperty(storage, 'getDirectory', {
      get: () => {
        throw new Error('private browser detail')
      },
    })
    const runtime = {
      browserWindow,
      storage,
      randomId: () => 'fixed',
      present: () => undefined,
    } as unknown as BrowserOutputRuntime

    expect(browserOutputChoices(runtime).map((choice) => choice.available)).toEqual([
      false,
      false,
    ])
    await expect(prepareBrowserOutput(plan, 'folder', runtime)).rejects.toMatchObject({
      code: 'output-unavailable',
    })
    await expect(prepareBrowserOutput(plan, 'download', runtime)).rejects.toMatchObject({
      code: 'output-unavailable',
    })
  })

  it('invokes the folder picker synchronously in the gesture turn', async () => {
    const events: string[] = []
    const root = {} as FileSystemDirectoryHandle
    const runtime = {
      browserWindow: {
        showDirectoryPicker: () => {
          events.push('picker')
          return Promise.resolve(root)
        },
      },
      storage: {},
      randomId: () => 'fixed',
      present: () => undefined,
    } as unknown as BrowserOutputRuntime

    const prepared = prepareBrowserOutput(plan, 'folder', runtime)
    events.push('returned')

    expect(events).toEqual(['picker', 'returned'])
    expect((await prepared).transferTarget((target) => target)).toEqual({
      kind: 'file-system',
      root,
    })
  })

  it('invokes the save-file picker synchronously and preserves the selected filename', async () => {
    const events: string[] = []
    const writable = new WritableStream<Uint8Array>({
      abort: () => {
        events.push('aborted')
      },
    })
    const handle = {
      createWritable: () => Promise.resolve(writable),
    } as unknown as FileSystemFileHandle
    const runtime = {
      browserWindow: {
        showSaveFilePicker: (options: { readonly suggestedName?: string }) => {
          events.push(`picker:${options.suggestedName}`)
          return Promise.resolve(handle)
        },
      },
      storage: {},
      randomId: () => 'fixed',
      present: () => undefined,
    } as unknown as BrowserOutputRuntime

    const prepared = prepareBrowserOutput(plan, 'download', runtime)
    events.push('returned')

    expect(events).toEqual(['picker:report.txt', 'returned'])
    const output = await prepared
    const stream = transferStream(output)
    expect(stream.locked).toBe(false)
    expect(() => output.transferTarget((target) => target)).toThrow(
      'prepared output target can only be transferred once',
    )
    const writer = stream.getWriter()
    await writer.abort(new Error('test cleanup'))
    writer.releaseLock()
    await output.abort(new Error('test cleanup'))
    expect(events.filter((event) => event === 'aborted')).toHaveLength(1)
  })

  it('rejects unavailable output choices with bounded public errors', async () => {
    const runtime = {
      browserWindow: {},
      storage: {},
      randomId: () => 'fixed',
      present: () => undefined,
    } as unknown as BrowserOutputRuntime

    await expect(prepareBrowserOutput(plan, 'folder', runtime)).rejects.toMatchObject({
      code: 'output-unavailable',
    })
    await expect(prepareBrowserOutput(plan, 'download', runtime)).rejects.toMatchObject({
      code: 'output-unavailable',
    })
  })

  it('stages fallback output in origin storage and removes it after presentation', async () => {
    const events: string[] = []
    let releaseTask = Promise.resolve()
    const writable = new WritableStream<Uint8Array>()
    const file = new Blob(['download']) as File
    const handle = {
      createWritable: () => Promise.resolve(writable),
      getFile: () => Promise.resolve(file),
    } as unknown as FileSystemFileHandle
    const root = {
      getFileHandle: (name: string, options?: { readonly create?: boolean }) => {
        events.push(`${options?.create === true ? 'create' : 'probe'}:${name}`)
        return options?.create === true
          ? Promise.resolve(handle)
          : Promise.reject(new DOMException('missing', 'NotFoundError'))
      },
      removeEntry: (name: string) => {
        events.push(`remove:${name}`)
        return Promise.resolve()
      },
    } as unknown as FileSystemDirectoryHandle
    const runtime = {
      browserWindow: {},
      storage: { getDirectory: () => Promise.resolve(root) },
      randomId: () => 'fixed',
      present: (_file: File, name: string, release: () => Promise<void>) => {
        events.push(`present:${name}`)
        releaseTask = release()
      },
    } as unknown as BrowserOutputRuntime

    const output = await prepareBrowserOutput(plan, 'download', runtime)
    const writer = transferStream(output).getWriter()
    await writer.close()
    writer.releaseLock()
    await output.commit()
    await releaseTask

    expect(events).toEqual([
      'probe:.windshare-download-fixed',
      'create:.windshare-download-fixed',
      'present:report.txt',
      'remove:.windshare-download-fixed',
    ])
  })

  it('never claims or removes a colliding origin-storage file', async () => {
    const events: string[] = []
    let releaseTask = Promise.resolve()
    const ids = ['collision', 'fresh'].values()
    const writable = new WritableStream<Uint8Array>()
    const handle = {
      createWritable: () => Promise.resolve(writable),
      getFile: () => Promise.resolve(new Blob(['download']) as File),
    } as unknown as FileSystemFileHandle
    const root = {
      getFileHandle: (name: string, options?: { readonly create?: boolean }) => {
        events.push(`${options?.create === true ? 'create' : 'probe'}:${name}`)
        if (name.endsWith('collision') || options?.create === true) {
          return Promise.resolve(handle)
        }
        return Promise.reject(new DOMException('missing', 'NotFoundError'))
      },
      removeEntry: (name: string) => {
        events.push(`remove:${name}`)
        return Promise.resolve()
      },
    } as unknown as FileSystemDirectoryHandle
    const runtime = {
      browserWindow: {},
      storage: { getDirectory: () => Promise.resolve(root) },
      randomId: () => ids.next().value ?? 'fresh',
      present: (_file: File, _name: string, release: () => Promise<void>) => {
        releaseTask = release()
      },
    } as unknown as BrowserOutputRuntime

    const output = await prepareBrowserOutput(plan, 'download', runtime)
    const writer = transferStream(output).getWriter()
    await writer.close()
    writer.releaseLock()
    await output.commit()
    await releaseTask

    expect(events).toContain('probe:.windshare-download-collision')
    expect(events).not.toContain('remove:.windshare-download-collision')
    expect(events).toContain('remove:.windshare-download-fresh')
  })

  it('removes an owned temporary file when stream creation fails', async () => {
    const events: string[] = []
    const root = {
      getFileHandle: (_name: string, options?: { readonly create?: boolean }) =>
        options?.create === true
          ? Promise.resolve({
              createWritable: () => Promise.reject(new Error('create failed')),
            } as unknown as FileSystemFileHandle)
          : Promise.reject(new DOMException('missing', 'NotFoundError')),
      removeEntry: (name: string) => {
        events.push(`remove:${name}`)
        return Promise.resolve()
      },
    } as unknown as FileSystemDirectoryHandle
    const runtime = {
      browserWindow: {},
      storage: { getDirectory: () => Promise.resolve(root) },
      randomId: () => 'owned',
      present: () => undefined,
    } as unknown as BrowserOutputRuntime

    await expect(prepareBrowserOutput(plan, 'download', runtime)).rejects.toThrow(
      'create failed',
    )
    expect(events).toEqual(['remove:.windshare-download-owned'])
  })

  it('reports stream cleanup failure while still removing exact-owned temporary output', async () => {
    const events: string[] = []
    const writable = new WritableStream<Uint8Array>({
      abort: () => Promise.reject(new Error('abort failed')),
    })
    const handle = {
      createWritable: () => Promise.resolve(writable),
    } as unknown as FileSystemFileHandle
    const root = {
      getFileHandle: (_name: string, options?: { readonly create?: boolean }) =>
        options?.create === true
          ? Promise.resolve(handle)
          : Promise.reject(new DOMException('missing', 'NotFoundError')),
      removeEntry: (name: string) => {
        events.push(`remove:${name}`)
        return Promise.resolve()
      },
    } as unknown as FileSystemDirectoryHandle
    const runtime = {
      browserWindow: {},
      storage: { getDirectory: () => Promise.resolve(root) },
      randomId: () => 'cleanup',
      present: () => undefined,
    } as unknown as BrowserOutputRuntime

    const output = await prepareBrowserOutput(plan, 'download', runtime)
    await expect(output.abort(new Error('stop'))).rejects.toThrow('abort failed')
    expect(events).toEqual(['remove:.windshare-download-cleanup'])
  })

  it('retries exact-owned removal after deferred presentation cleanup fails', async () => {
    const events: string[] = []
    let releaseTask = Promise.resolve()
    let removals = 0
    const writable = new WritableStream<Uint8Array>()
    const handle = {
      createWritable: () => Promise.resolve(writable),
      getFile: () => Promise.resolve(new Blob(['download']) as File),
    } as unknown as FileSystemFileHandle
    const root = {
      getFileHandle: (_name: string, options?: { readonly create?: boolean }) =>
        options?.create === true
          ? Promise.resolve(handle)
          : Promise.reject(new DOMException('missing', 'NotFoundError')),
      removeEntry: () => {
        removals += 1
        return removals === 1
          ? Promise.reject(new Error('transient removal failure'))
          : Promise.resolve()
      },
    } as unknown as FileSystemDirectoryHandle
    const runtime = {
      browserWindow: {},
      storage: { getDirectory: () => Promise.resolve(root) },
      randomId: () => 'retry',
      present: (_file: File, _name: string, release: () => Promise<void>) => {
        events.push('present')
        releaseTask = release()
      },
    } as unknown as BrowserOutputRuntime
    const output = await prepareBrowserOutput(plan, 'download', runtime)
    const writer = transferStream(output).getWriter()
    await writer.close()
    writer.releaseLock()

    await output.commit()
    await expect(releaseTask).rejects.toThrow('transient removal failure')
    await output.abort(new Error('commit failed'))
    expect(events).toEqual(['present'])
    expect(removals).toBe(2)
  })

  it('rejects forged output choices instead of falling through to download', async () => {
    const runtime = {
      browserWindow: { showSaveFilePicker: () => Promise.reject(new Error('must not run')) },
      storage: {},
      randomId: () => 'fixed',
      present: () => undefined,
    } as unknown as BrowserOutputRuntime

    await expect(
      prepareBrowserOutput(plan, 'forged' as 'download', runtime),
    ).rejects.toMatchObject({ code: 'output-unavailable' })
  })
})
