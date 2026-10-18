import { access, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  buildDirectProductBinaries,
  DIRECT_PRODUCT_BINARY_DIRECTORY_ENV,
  preparedDirectBinaryPaths,
  type DirectBinaryPaths,
} from '../../e2e/fixtures/direct-product-binaries'
import prepareDirectProductBinaries from '../../e2e/fixtures/direct-product-setup'
import { DirectProductStack } from '../../e2e/fixtures/direct-product-stack'

vi.mock('../../e2e/fixtures/direct-product-binaries', async (importOriginal) => {
  const original = await importOriginal<typeof import('../../e2e/fixtures/direct-product-binaries')>()
  return { ...original, buildDirectProductBinaries: vi.fn() }
})

const buildBinaries = vi.mocked(buildDirectProductBinaries)

beforeEach(() => {
  buildBinaries.mockReset()
  vi.stubEnv(DIRECT_PRODUCT_BINARY_DIRECTORY_ENV, undefined)
  vi.spyOn(console, 'info').mockImplementation(() => undefined)
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllEnvs()
  vi.restoreAllMocks()
})

describe('direct product suite preparation', () => {
  it('waits for a cold build beyond one minute before publishing a complete pair to workers', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] })
    const started = deferred<DirectBinaryPaths>()
    buildBinaries.mockImplementation(async (binaries) => {
      await writeFile(binaries.wind, 'wind')
      started.resolve(binaries)
      await new Promise<void>((resolveBuild) => setTimeout(resolveBuild, 61_000))
      await writeFile(binaries.relay, 'relay-native')
    })

    const preparing = prepareDirectProductBinaries()
    const binaries = await started.promise
    try {
      await vi.advanceTimersByTimeAsync(60_000)
      expect(() => preparedDirectBinaryPaths()).toThrow(/not prepared/u)
      await expect(access(binaries.relay)).rejects.toMatchObject({ code: 'ENOENT' })
      await vi.advanceTimersByTimeAsync(1_000)
      await preparing
      expect(preparedDirectBinaryPaths()).toEqual(binaries)
      // Independent scenarios and replacement workers receive the same completed
      // paths; neither lookup is permitted to start another build.
      expect(preparedDirectBinaryPaths()).toEqual(binaries)
      await expect(readFile(binaries.wind, 'utf8')).resolves.toBe('wind')
      await expect(readFile(binaries.relay, 'utf8')).resolves.toBe('relay-native')
      expect(buildBinaries).toHaveBeenCalledTimes(1)
    } finally {
      await vi.runAllTimersAsync()
      const teardown = await preparing
      await teardown()
    }
    expect(() => preparedDirectBinaryPaths()).toThrow(/not prepared/u)
    await expect(access(binaries.directory)).rejects.toMatchObject({ code: 'ENOENT' })
  })

  it('removes partially built output and preserves the original build failure', async () => {
    const failure = new Error('compiler failed')
    const started = deferred<DirectBinaryPaths>()
    buildBinaries.mockImplementation(async (binaries) => {
      started.resolve(binaries)
      await writeFile(binaries.wind, 'partial executable')
      throw failure
    })

    const preparing = prepareDirectProductBinaries()
    await expect(preparing).rejects.toBe(failure)
    const binaries = await started.promise
    expect(() => preparedDirectBinaryPaths()).toThrow(/not prepared/u)
    await expect(access(binaries.directory)).rejects.toMatchObject({ code: 'ENOENT' })
  })

  it('owns a fresh directory for each invocation and leaves inherited artifacts intact', async () => {
    const inheritedDirectory = await mkdtemp(join(tmpdir(), 'windshare-browser-inherited-test-'))
    const marker = join(inheritedDirectory, 'marker')
    await writeFile(marker, 'belongs to another owner')
    vi.stubEnv(DIRECT_PRODUCT_BINARY_DIRECTORY_ENV, inheritedDirectory)
    buildBinaries.mockImplementation(async (binaries) => {
      await writeFile(binaries.wind, 'wind')
      await writeFile(binaries.relay, 'relay-native')
    })

    const directories = new Set<string>()
    try {
      for (let invocation = 0; invocation < 2; invocation += 1) {
        const teardown = await prepareDirectProductBinaries()
        const binaries = preparedDirectBinaryPaths()
        try {
          expect(binaries.directory).not.toBe(inheritedDirectory)
          expect(directories.has(binaries.directory)).toBe(false)
          directories.add(binaries.directory)
        } finally {
          await teardown()
        }
        expect(process.env[DIRECT_PRODUCT_BINARY_DIRECTORY_ENV]).toBe(inheritedDirectory)
        await expect(access(binaries.directory)).rejects.toMatchObject({ code: 'ENOENT' })
      }
      await expect(readFile(marker, 'utf8')).resolves.toBe('belongs to another owner')
    } finally {
      await rm(inheritedDirectory, { recursive: true, force: true })
    }
  })

  it('rejects scenario startup without suite preparation instead of compiling inside the test', async () => {
    const stack = new DirectProductStack('missing-suite-preparation', () => undefined)
    try {
      await expect(stack.start()).rejects.toThrow(/binaries are not prepared/u)
      expect(buildBinaries).not.toHaveBeenCalled()
      expect(stack.diagnostic()).toEqual([])
    } finally {
      await stack.dispose()
    }
  })
})

function deferred<T>(): { readonly promise: Promise<T>; readonly resolve: (value: T) => void } {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((resolveValue) => { resolve = resolveValue })
  return { promise, resolve }
}
