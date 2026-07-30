import { spawnSync } from 'node:child_process'
import { access, mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import type {
  BrowserSampleContainmentPreflight,
} from '../../scripts/browser-evidence/process/containment.ts'
import {
  createWindowsJobContainmentBackend,
} from '../../scripts/browser-evidence/process/windows-job-backend.ts'
import { executeWindowsJob } from '../../scripts/browser-evidence/process/windows-job-client.ts'
import { inheritedSampleEnvironment } from '../../scripts/browser-evidence/process/sample-environment.ts'

describe('Windows Job network authority boundary', () => {
  it('accepts an external signal only as an owner-controlled termination request', async () => {
    const workspace = await mkdtemp(join(tmpdir(), 'windshare-job-preflight-'))
    try {
      const helperPath = join(workspace, 'windowsjob.exe')
      await writeFile(helperPath, 'fixture')
      const backend = createWindowsJobContainmentBackend(helperPath)
      const preflight: BrowserSampleContainmentPreflight = Object.freeze({
        operationId: 'windows-owner-controlled-termination',
        topologyProfilePath: join(tmpdir(), 'profile.json'),
        topologyProfileSha256: '4'.repeat(64),
        topologyResolutionPath: join(tmpdir(), 'resolution.json'),
        topologyResolutionSha256: '5'.repeat(64),
        readOnlyInputRoots: Object.freeze([tmpdir()]),
        terminationSignal: new AbortController().signal,
      })

      await expect(backend.preflight(preflight)).resolves.toBeUndefined()
    } finally {
      await rm(workspace, { force: true, recursive: true })
    }
  })
})

describe.skipIf(process.platform !== 'win32')('Windows Job output failure ownership', () => {
  let workspace = ''
  let helperPath = ''
  let rootScriptPath = ''
  let descendantScriptPath = ''
  let markerPath = ''

  beforeAll(async () => {
    workspace = resolve(await mkdtemp(join(tmpdir(), 'windshare-windows-job-sink-')))
    helperPath = join(workspace, 'windowsjob.exe')
    rootScriptPath = join(workspace, 'root.mjs')
    descendantScriptPath = join(workspace, 'descendant.mjs')
    markerPath = join(workspace, 'escaped.txt')
    const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..')
    // This integration intentionally uses the runner's configured Go toolchain;
    // production helper launch remains authenticated and absolute.
    // eslint-disable-next-line sonarjs/no-os-command-from-path
    const build = spawnSync('go', [
      'build',
      '-o',
      helperPath,
      './web/scripts/browser-evidence/windowsjob',
    ], { cwd: repositoryRoot, shell: false, stdio: 'pipe' })
    if (build.status !== 0) {
      throw new Error(`cannot build Windows Job test helper: ${String(build.stderr)}`)
    }
    await Promise.all([
      writeFile(descendantScriptPath, [
        "import { writeFileSync } from 'node:fs'",
        'const markerPath = process.argv[2]',
        "setTimeout(() => writeFileSync(markerPath, 'escaped'), 400)",
        'setInterval(() => undefined, 1_000)',
      ].join('\n')),
      writeFile(rootScriptPath, [
        "import { spawn } from 'node:child_process'",
        'const [descendantPath, markerPath] = process.argv.slice(2)',
        "spawn(process.execPath, [descendantPath, markerPath], { stdio: 'ignore' })",
        "process.stdout.write('stdout-trigger\\n')",
        "process.stderr.write('stderr-trigger\\n')",
        'setInterval(() => undefined, 1_000)',
      ].join('\n')),
    ])
  })

  afterAll(async () => {
    if (workspace !== '') await rm(workspace, { force: true, recursive: true })
  })

  it('reports byte-sink failure only after the authenticated Job is empty', async () => {
    const controller = new AbortController()
    const observedChunks: Uint8Array[] = []
    const execution = await executeWindowsJob({
      helperPath,
      operationId: 'windows-job-hostile-output-sink',
      command: {
        executable: process.execPath,
        arguments: [rootScriptPath, descendantScriptPath, markerPath],
        cwd: workspace,
      },
      inheritedEnvironment: inheritedSampleEnvironment(),
      injectedEnvironment: Object.freeze({}),
      deadlineMs: 5_000,
      terminationGraceMs: 500,
      terminationSignal: controller.signal,
      stdout: (chunk) => {
        observedChunks.push(chunk)
        controller.abort()
        throw new Error('hostile stdout sink')
      },
      stderr: (chunk) => {
        observedChunks.push(chunk)
        throw new Error('hostile stderr sink')
      },
    })
    expect(execution.treeEmpty).toBe(true)
    expect(execution.timedOut).toBe(false)
    expect(execution.ownershipEvidence.terminationReason).toBe('parent-request')
    expect(execution.clientIoEvidence.controlOutcome).toBe('delivered')
    expect(execution.clientIoEvidence.outputOutcome).toBe('failed')
    expect(execution.clientIoEvidence.failureCode).toBe('CLIENT_IO_FAILED')
    expect(observedChunks.length).toBeGreaterThan(0)
    expect(observedChunks.every((chunk) => chunk.every((byte) => byte === 0))).toBe(true)

    await new Promise((resolveWait) => setTimeout(resolveWait, 550))
    await expect(access(markerPath)).rejects.toThrow()
  })
})
