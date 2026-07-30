import { createHash } from 'node:crypto'
import { mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  buildNetworkMatrixHelpers,
  HELPER_BUILD_MANIFEST_SCHEMA_VERSION,
  HelperBuildError,
  helperBuildPlan,
  runBuildHelpersCli,
} from '../../scripts/browser-network-matrix/cli/build-helpers.mjs'

const cleanupRoots: string[] = []

afterEach(async () => {
  await Promise.all(cleanupRoots.splice(0).map((root) =>
    rm(root, { recursive: true, force: true })))
})

describe('browser network matrix helper build contract', () => {
  it('uses deterministic platform-exact Go build operations', () => {
    const windows = helperBuildPlan('C:\\absolute\\new-helpers', 'win32', 'x64')
    expect(windows.map(({ operation, role }) => ({ operation, role }))).toEqual([
      { operation: 'artifact-publisher', role: 'artifact-publisher' },
      { operation: 'windows-job-supervisor', role: 'windows-job' },
    ])
    for (const operation of windows) {
      expect(operation.arguments).toEqual([
        'build',
        '-trimpath',
        '-buildvcs=false',
        '-ldflags=-buildid=',
        '-o',
        operation.outputPath,
        operation.packagePath,
      ])
      expect(operation.deadlineMs).toBeGreaterThan(0)
      expect(operation.maximumOutputBytes).toBeGreaterThan(0)
      expect(operation.outputPath).toMatch(/\.exe$/u)
    }

    const linux = helperBuildPlan('/absolute/new-helpers', 'linux', 'arm64')
    expect(linux).toHaveLength(1)
    expect(linux[0]).toMatchObject({
      role: 'artifact-publisher',
      platform: 'linux',
      goArchitecture: 'arm64',
      outputPath: join('/absolute/new-helpers', 'browsermatrixpublish'),
    })
  })

  it('emits one strict manifest bound to actual helper paths and bytes', async () => {
    const parent = await ownedParent()
    const output = join(parent, 'helpers')
    const helperBytes = new Map<string, Buffer>()
    const result = await buildNetworkMatrixHelpers(output, {
      platform: 'win32',
      architecture: 'x64',
      runOperation: async (operation) => {
        const bytes = Buffer.from(`native ${operation.role} bytes`)
        helperBytes.set(operation.role, bytes)
        await writeFile(operation.outputPath, bytes)
      },
    })

    expect(result.manifest).toMatchObject({
      schemaVersion: HELPER_BUILD_MANIFEST_SCHEMA_VERSION,
      platform: 'win32',
      architecture: 'amd64',
    })
    expect(result.manifest.helpers).toHaveLength(2)
    for (const helper of result.manifest.helpers) {
      const expected = helperBytes.get(helper.role)
      expect(expected).toBeDefined()
      expect(helper.path).toBe(join(output, helper.role === 'artifact-publisher'
        ? 'browsermatrixpublish.exe'
        : 'windowsjob.exe'))
      expect(helper.sha256).toBe(createHash('sha256').update(expected!).digest('hex'))
      expect(await readFile(helper.path)).toEqual(expected)
    }
    const manifestBytes = await readFile(result.manifestPath, 'utf8')
    expect(manifestBytes).toBe(`${JSON.stringify(result.manifest)}\n`)
    expect(Object.keys(JSON.parse(manifestBytes) as object)).toEqual([
      'schemaVersion', 'platform', 'architecture', 'helpers',
    ])
  })

  it('never enters an existing output path', async () => {
    const parent = await ownedParent()
    const output = join(parent, 'foreign')
    await writeFile(output, 'foreign bytes')
    const runOperation = vi.fn()
    await expect(buildNetworkMatrixHelpers(output, {
      platform: 'win32',
      architecture: 'x64',
      runOperation,
    })).rejects.toMatchObject({ outputOwned: false })
    expect(await readFile(output, 'utf8')).toBe('foreign bytes')
    expect(runOperation).not.toHaveBeenCalled()
  })

  it('retains and reports only its partial owned output on failure', async () => {
    const parent = await ownedParent()
    const output = join(parent, 'partial-helpers')
    let builds = 0
    let failure: unknown
    try {
      await buildNetworkMatrixHelpers(output, {
        platform: 'win32',
        architecture: 'x64',
        runOperation: async (operation) => {
          builds += 1
          if (builds === 1) {
            await writeFile(operation.outputPath, 'completed publisher')
            return
          }
          throw new Error('injected Job build failure')
        },
      })
    } catch (cause) {
      failure = cause
    }
    expect(failure).toBeInstanceOf(HelperBuildError)
    expect(failure).toMatchObject({
      operation: 'windows-job-supervisor',
      outputDirectory: output,
      outputOwned: true,
    })
    expect((failure as Error).message).toContain(`partial owned output is retained at ${output}`)
    expect((await stat(join(output, 'browsermatrixpublish.exe'))).isFile()).toBe(true)
    await expect(stat(join(output, 'helper-manifest.json'))).rejects.toMatchObject({ code: 'ENOENT' })

    const stderr: string[] = []
    const exitCode = await runBuildHelpersCli([output], {
      stdout: { write: () => true },
      stderr: { write: (value) => { stderr.push(String(value)); return true } },
      build: async () => { throw failure },
    })
    expect(exitCode).toBe(1)
    expect(JSON.parse(stderr.join(''))).toMatchObject({
      outcome: 'failed',
      operation: 'windows-job-supervisor',
      outputDirectory: output,
      partialOutputRetained: true,
    })
  })
})

async function ownedParent(): Promise<string> {
  const root = await mkdtemp(join(tmpdir(), 'windshare-helper-build-test-'))
  cleanupRoots.push(root)
  return root
}
