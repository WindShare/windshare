import { access, mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'

import { afterEach, describe, expect, it } from 'vitest'

import type { ChildEvidenceContext } from '../../scripts/browser-evidence/child-evidence.ts'
import {
  createNativeProcessGroupContainmentBackend,
  executeNativeProcessGroupCommand,
} from '../../scripts/browser-evidence/process/native-process-group-backend.ts'
import type { BrowserSampleContainmentRequest } from '../../scripts/browser-evidence/process/containment.ts'
import { inheritedSampleEnvironment } from '../../scripts/browser-evidence/process/sample-environment.ts'
import { browserRunPolicy } from '../../scripts/browser-evidence/run-policy.ts'

const workspaces: string[] = []
const SHA256 = 'a'.repeat(64)
const CHECKOUT_SHA = 'b'.repeat(40)

afterEach(async () => {
  await Promise.all(workspaces.splice(0).map((workspace) => rm(workspace, {
    force: true,
    recursive: true,
  })))
})

describe.skipIf(process.platform === 'win32')('native process-group containment', () => {
  it('owns a remainder-like command tree and reaps its descendant on timeout', async () => {
    const fixture = await createProcessTreeFixture('remain-alive')

    const execution = await executeNativeProcessGroupCommand({
      command: {
        executable: process.execPath,
        arguments: [
          fixture.rootScriptPath,
          fixture.mode,
          fixture.descendantScriptPath,
          fixture.markerPath,
        ],
        cwd: fixture.workspace,
      },
      environment: inheritedSampleEnvironment(),
      deadlineMs: 75,
      terminationGraceMs: 500,
      stdout: () => undefined,
      stderr: () => undefined,
      trace: () => undefined,
    })

    expect(execution.timedOut).toBe(true)
    expect(execution.processEvidence.terminal).toBe('signaled')
    await expectDescendantNeverRan(fixture.markerPath)
  })

  it('retires the whole group at its deadline even when the trace sink throws', async () => {
    const fixture = await createProcessTreeFixture('remain-alive')
    const request = containmentRequest(fixture, {
      deadlineMs: 75,
      trace: () => { throw new Error('hostile trace sink') },
    })

    const execution = await createNativeProcessGroupContainmentBackend().execute(request)

    expect(execution.timedOut).toBe(true)
    expect(execution.processEvidence.terminal).toBe('signaled')
    await expectDescendantNeverRan(fixture.markerPath)
  })

  it('reports a throwing byte sink only after the descendant group is retired', async () => {
    const fixture = await createProcessTreeFixture('remain-alive', true)

    await expect(createNativeProcessGroupContainmentBackend().execute(containmentRequest(
      fixture,
      {
        deadlineMs: 75,
        stdout: () => { throw new Error('hostile stdout sink') },
        stderr: () => { throw new Error('hostile stderr sink') },
      },
    ))).rejects.toThrow(/stdout sink failed|stderr sink failed/u)
    await expectDescendantNeverRan(fixture.markerPath)
  })

  it('treats parent abort as a distinct termination request and reaps descendants', async () => {
    const fixture = await createProcessTreeFixture('remain-alive')
    const controller = new AbortController()
    const events: string[] = []
    const executionPromise = createNativeProcessGroupContainmentBackend().execute(containmentRequest(
      fixture,
      {
        deadlineMs: 5_000,
        terminationSignal: controller.signal,
        trace: ({ milestone }) => events.push(milestone),
      },
    ))
    setTimeout(() => controller.abort(), 75)

    const execution = await executionPromise

    expect(execution.timedOut).toBe(false)
    expect(execution.processEvidence.terminal).toBe('signaled')
    expect(events).toContain('native-process-group-termination-requested')
    expect(events).toContain('native-process-group-tree-empty')
    await expectDescendantNeverRan(fixture.markerPath)
  })

  it('reaps a residual descendant after the group leader exits successfully', async () => {
    const fixture = await createProcessTreeFixture('exit-after-spawn')

    const execution = await createNativeProcessGroupContainmentBackend().execute(containmentRequest(
      fixture,
      { deadlineMs: 5_000 },
    ))

    expect(execution).toMatchObject({
      timedOut: false,
      processEvidence: { terminal: 'exited', exitCode: 0 },
    })
    await expectDescendantNeverRan(fixture.markerPath)
  })
})

interface ProcessTreeFixture {
  readonly workspace: string
  readonly rootScriptPath: string
  readonly descendantScriptPath: string
  readonly markerPath: string
  readonly profilePath: string
  readonly resolutionPath: string
  readonly sampleDirectory: string
  readonly attachmentRoot: string
  readonly evidencePath: string
  readonly mode: 'remain-alive' | 'exit-after-spawn'
}

async function createProcessTreeFixture(
  mode: ProcessTreeFixture['mode'],
  emitOutput = false,
): Promise<ProcessTreeFixture> {
  const workspace = resolve(await mkdtemp(join(tmpdir(), 'windshare-native-group-')))
  workspaces.push(workspace)
  const rootScriptPath = join(workspace, 'root.mjs')
  const descendantScriptPath = join(workspace, 'descendant.mjs')
  const markerPath = join(workspace, 'escaped.txt')
  const profilePath = join(workspace, 'profile.json')
  const resolutionPath = join(workspace, 'resolution.json')
  const sampleDirectory = join(workspace, 'sample')
  const attachmentRoot = join(sampleDirectory, 'attachments')
  const evidencePath = join(sampleDirectory, 'evidence.jsonl')
  await mkdir(attachmentRoot, { recursive: true })
  await Promise.all([
    writeFile(profilePath, '{}'),
    writeFile(resolutionPath, '{}'),
    writeFile(descendantScriptPath, [
      "import { writeFileSync } from 'node:fs'",
      'const markerPath = process.argv[2]',
      "setTimeout(() => writeFileSync(markerPath, 'escaped'), 400)",
      'setInterval(() => undefined, 1_000)',
    ].join('\n')),
    writeFile(rootScriptPath, [
      "import { spawn } from 'node:child_process'",
      'const [mode, descendantPath, markerPath] = process.argv.slice(2)',
      "spawn(process.execPath, [descendantPath, markerPath], { stdio: 'ignore' })",
      ...(emitOutput ? ["process.stdout.write('stdout-trigger\\n')", "process.stderr.write('stderr-trigger\\n')"] : []),
      "if (mode === 'exit-after-spawn') process.exit(0)",
      'setInterval(() => undefined, 1_000)',
    ].join('\n')),
  ])
  return {
    workspace,
    rootScriptPath,
    descendantScriptPath,
    markerPath,
    profilePath,
    resolutionPath,
    sampleDirectory,
    attachmentRoot,
    evidencePath,
    mode,
  }
}

function containmentRequest(
  fixture: ProcessTreeFixture,
  overrides: Partial<BrowserSampleContainmentRequest> = {},
): BrowserSampleContainmentRequest {
  const childContext: ChildEvidenceContext = {
    runId: 'native-group-test',
    runPolicy: browserRunPolicy('blocking'),
    suite: 'main',
    browser: 'chromium',
    sampleIndex: 1,
    checkoutSha: CHECKOUT_SHA,
    topologyProfileSha256: SHA256,
    topologyResolutionSha256: SHA256,
    topologyProfilePath: fixture.profilePath,
    topologyResolutionPath: fixture.resolutionPath,
    evidencePath: fixture.evidencePath,
    artifactRoot: fixture.attachmentRoot,
  }
  return {
    operationId: 'native-process-group-test',
    topologyProfilePath: fixture.profilePath,
    topologyProfileSha256: SHA256,
    topologyResolutionPath: fixture.resolutionPath,
    topologyResolutionSha256: SHA256,
    readOnlyInputRoots: [fixture.rootScriptPath, fixture.descendantScriptPath],
    command: {
      executable: process.execPath,
      arguments: [
        fixture.rootScriptPath,
        fixture.mode,
        fixture.descendantScriptPath,
        fixture.markerPath,
      ],
      cwd: fixture.workspace,
    },
    sampleDirectory: fixture.sampleDirectory,
    childAttachmentStagingRoot: fixture.attachmentRoot,
    childContext,
    deadlineMs: 1_000,
    terminationGraceMs: 500,
    stdout: () => undefined,
    stderr: () => undefined,
    trace: () => undefined,
    ...overrides,
  }
}

async function expectDescendantNeverRan(markerPath: string): Promise<void> {
  await new Promise((resolveWait) => setTimeout(resolveWait, 550))
  await expect(access(markerPath)).rejects.toThrow()
}
