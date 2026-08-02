import { chmod, cp, mkdir, readFile, readdir, writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { afterEach, beforeAll, describe, expect, it } from 'vitest'

import {
  ArtifactGuardRecordedError,
  guardArtifactSuite,
  startGuardArtifactSuite,
  type GuardArtifactSuiteOptions,
  type GuardArtifactSuiteSample,
} from '../../scripts/browser-evidence/artifact/guard.ts'
import {
  GUARD_UPLOAD_MANIFEST_FILENAME,
  resolveGuardUpload,
} from '../../scripts/browser-evidence/artifact/sealed-suite.ts'
import type {
  GuardUploadDirectoryPublisher,
} from '../../scripts/browser-evidence/artifact/directory-publisher.ts'
import { browserRunPolicy } from '../../scripts/browser-evidence/run-policy.ts'
import {
  BROWSER_ENGINES,
  type BrowserEngine,
  type BrowserSuite,
} from '../../scripts/browser-evidence/vocabulary.ts'
import { evaluateBrowserGate } from '../../../scripts/ci/browsergate/verdict.mjs'
import {
  artifactEnvironment,
  artifactRootForOutcome,
  createFrameworkWorkspace,
  createFrameworkGuardAuthority,
  loadFrameworkTopology,
  removeFrameworkWorkspace,
  runSyntheticSample,
  type FrameworkTopology,
  type FrameworkGuardAuthority,
} from './framework-fixtures.ts'

let topology: FrameworkTopology
const workspaces: string[] = []
const blockingPolicy = browserRunPolicy('blocking')

beforeAll(async () => { topology = await loadFrameworkTopology() })
afterEach(async () => {
  await Promise.all(workspaces.splice(0).map(removeFrameworkWorkspace))
})

describe('suite-owned guard upload transaction', () => {
  it('seals one content-addressed exact-inventory directory for the complete suite', async () => {
    const fixture = await prepareSuite()
    const resultBytesBefore = await Promise.all(fixture.samples.map(({ sample }) =>
      readFile(join(fixture.workspace, 'main', sample.browser, 'sample-1', 'result.json'))))
    const execution = startGuardArtifactSuite({
      ...fixture.identity,
      ...fixture.authority,
      samples: fixture.samples,
      uploadParent: fixture.uploadParent,
      explicitSecrets: [],
    })
    const consumer = execution.traces[Symbol.asyncIterator]()
    await expect(consumer.next()).resolves.toMatchObject({
      done: false,
      value: { milestone: 'suite-started' },
    })
    const outcome = await execution.result
    const guarded = Object.freeze({ ...outcome, traces: execution.traces.snapshot() })

    expect(guarded.guards).toHaveLength(BROWSER_ENGINES.length)
    expect(guarded.traces.events.filter(({ milestone }) => milestone === 'suite-started'))
      .toHaveLength(1)
    expect(guarded.traces.events.filter(({ milestone }) => milestone === 'suite-terminal'))
      .toHaveLength(1)
    expect(guarded.traces.events.filter(({ milestone }) => milestone === 'scan-started'))
      .toHaveLength(BROWSER_ENGINES.length)
    expect(guarded.traces.events.filter(({ milestone }) => milestone === 'scan-terminal'))
      .toHaveLength(BROWSER_ENGINES.length)
    expect(guarded.traces.events.at(-1)).toMatchObject({
      milestone: 'suite-terminal',
      outcome: 'succeeded',
      context: {
        cleanupOutcome: 'completed',
        lastMilestone: 'suite-upload-sealed',
      },
    })
    expect(guarded.traces).toMatchObject({
      completed: true,
      truncated: false,
      failure: null,
    })
    expect(
      guarded.guards.every(({ guardOutcome }) => guardOutcome === 'passed'),
      JSON.stringify(guarded.guards, null, 2),
    ).toBe(true)
    if (guarded.upload === null) throw new Error('passed suite did not produce an upload authority')
    expect((await readdir(guarded.upload.uploadDirectory)).sort()).toEqual([
      GUARD_UPLOAD_MANIFEST_FILENAME,
      'samples',
      'topology',
    ])

    const resolved = await resolveGuardUpload({
      uploadDirectory: guarded.upload.uploadDirectory,
      manifestSha256: guarded.upload.manifestSha256,
      manifestByteLength: guarded.upload.manifestByteLength,
      directoryPublisher: fixture.authority.directoryPublisher,
      suite: 'main',
    })
    expect(resolved.manifest).toMatchObject({
      ...fixture.identity,
      samples: BROWSER_ENGINES.map((browser) => ({ browser, sampleIndex: 1 })),
    })
    expect(resolved.guards.every(({ guardOutcome }) => guardOutcome === 'passed')).toBe(true)
    for (const [index, browser] of BROWSER_ENGINES.entries()) {
      const sealedResult = await readFile(join(
        resolved.uploadDirectory,
        'samples',
        browser,
        'sample-1',
        'result.json',
      ))
      expect(sealedResult).toEqual(resultBytesBefore[index])
    }
  })

  it('feeds actual sealed-v2 suites through the production standard-library verdict', async () => {
    const main = await prepareSuite({ suite: 'main' })
    const pion = await prepareSuite({ suite: 'pion' })
    const mainUpload = await sealSuite(main)
    const pionUpload = await sealSuite(pion)
    const options = {
      runId: main.identity.runId,
      checkoutSha: main.identity.checkoutSha,
      suites: {
        main: verdictSuiteInput(mainUpload),
        pion: verdictSuiteInput(pionUpload),
      },
    } as const

    const verdict = await evaluateBrowserGate(options)
    expect(verdict.verdict, JSON.stringify(verdict.violations, null, 2)).toBe('passed')
    expect(verdict.samples).toHaveLength(BROWSER_ENGINES.length * 2)

    const mutableMain = join(main.workspace, 'verdict-main-copy')
    await cp(mainUpload.uploadDirectory, mutableMain, { recursive: true })
    const topologyProfile = join(mutableMain, 'topology', 'profile.json')
    await chmod(topologyProfile, 0o600)
    await writeFile(topologyProfile, '{"tampered":true}', 'utf8')
    const tampered = await evaluateBrowserGate({
      ...options,
      suites: {
        ...options.suites,
        main: { ...options.suites.main, root: mutableMain },
      },
    })
    expect(tampered.verdict).toBe('failed')
    expect(tampered.violations.some((violation) =>
      violation.includes('main sealed upload is invalid'))).toBe(true)
  })

  it('returns no upload authority when one sample is quarantined and preserves every result byte', async () => {
    const secret = 'suite-guard-secret-value'
    const fixture = await prepareSuite({ secretBrowser: 'chromium', secret })
    const resultsBefore = new Map(await Promise.all(fixture.samples.map(async ({ sample }) => [
      sample.browser,
      await readFile(join(fixture.workspace, 'main', sample.browser, 'sample-1', 'result.json')),
    ] as const)))

    const guarded = await guardArtifactSuite({
      ...fixture.identity,
      ...fixture.authority,
      samples: fixture.samples,
      uploadParent: fixture.uploadParent,
      explicitSecrets: [{ value: secret }],
      })

    expect(guarded.upload).toBeNull()
    expect(guarded.guards.find(({ browser }) => browser === 'chromium')?.guardOutcome)
      .toBe('quarantined')
    expect(await readdir(fixture.uploadParent)).toEqual([])
    for (const { sample } of fixture.samples) {
      expect(await readFile(join(fixture.workspace, 'main', sample.browser, 'sample-1', 'result.json')))
        .toEqual(resultsBefore.get(sample.browser))
    }
    const chromium = fixture.samples.find(({ sample }) => sample.browser === 'chromium')
    if (chromium === undefined) throw new Error('chromium fixture is missing')
    await expect(readFile(join(chromium.artifactRoot, 'playwright', 'diagnostic.txt'), 'utf8'))
      .resolves.toContain(secret)
  })

  it('rejects split object/byte inputs before creating private staging', async () => {
    const fixture = await prepareSuite()
    const first = fixture.samples[0]
    const second = fixture.samples[1]
    if (first === undefined || second === undefined) throw new Error('suite fixture is incomplete')

    await expectGuardSuiteFailure({
      ...fixture.identity,
      ...fixture.authority,
      uploadParent: fixture.uploadParent,
      samples: [
        { ...first, sampleResultBytes: second.sampleResultBytes },
        ...fixture.samples.slice(1),
      ],
      explicitSecrets: [],
    }, /process settlement identity differs/u)
    expect(await readdir(fixture.uploadParent)).toEqual([])
  })

  it('rejects forged settlement before invoking the directory publisher capability', async () => {
    const fixture = await prepareSuite()
    const first = fixture.samples[0]
    if (first === undefined) throw new Error('suite fixture is incomplete')
    const settlementAttestation = first.settlementAttestation as {
      readonly payload: unknown
      readonly signatureBase64: string
    }
    await expectGuardSuiteFailure({
      ...fixture.identity,
      ...fixture.authority,
      directoryPublisher: {
        invoke: async () => {
          throw new Error('directory publisher must not run before settlement authentication')
        },
      },
      uploadParent: fixture.uploadParent,
      samples: [
        {
          ...first,
          settlementAttestation: {
            payload: settlementAttestation.payload,
            signatureBase64: Buffer.alloc(64).toString('base64'),
          },
        },
        ...fixture.samples.slice(1),
      ],
      explicitSecrets: [],
    }, /signature is invalid/u)
    expect(await readdir(fixture.uploadParent)).toEqual([])
  })

  it('detects post-seal manifest and inventory tampering', async () => {
    const fixture = await prepareSuite()
    const guarded = await guardArtifactSuite({
      ...fixture.identity,
      ...fixture.authority,
      samples: fixture.samples,
      uploadParent: fixture.uploadParent,
      explicitSecrets: [],
      })
    if (guarded.upload === null) throw new Error('passed suite did not produce an upload authority')
    let injected = false
    try {
      await chmod(guarded.upload.uploadDirectory, 0o700)
      await writeFile(join(guarded.upload.uploadDirectory, 'injected.txt'), 'not authorized', 'utf8')
      injected = true
    } catch {
      // Native publication deliberately denies mutation on platforms that can enforce it.
    }
    const verification = resolveGuardUpload({
      uploadDirectory: guarded.upload.uploadDirectory,
      manifestSha256: guarded.upload.manifestSha256,
      manifestByteLength: guarded.upload.manifestByteLength,
      directoryPublisher: fixture.authority.directoryPublisher,
      suite: 'main',
    })
    if (injected) await expect(verification).rejects.toThrow(/publication-unsafe/u)
    else await expect(verification).resolves.toMatchObject({ manifestSha256: guarded.upload.manifestSha256 })
  })

  it('fails every guard and never seals foreign staging entries that cleanup cannot own', async () => {
    const fixture = await prepareSuite()
    const guarded = await guardArtifactSuite({
      ...fixture.identity,
      ...fixture.authority,
      samples: fixture.samples,
      uploadParent: fixture.uploadParent,
      explicitSecrets: [],
      uploadFaultCuts: Object.freeze([
        Object.freeze({ action: 'add-foreign-file-before-publication' as const }),
      ]),
      })

    expect(guarded.upload).toBeNull()
    expect(guarded.guards.every(({ guardOutcome, failureCode }) =>
      guardOutcome === 'failed' && failureCode === 'unexpected')).toBe(true)
    const entries = await readdir(fixture.uploadParent)
    expect(entries).not.toContain('sealed')
    expect(entries.every((entry) => entry.startsWith('.browser-evidence-upload-'))).toBe(true)
  })

  it('rejects an incomplete policy slot set before scanning or staging', async () => {
    const fixture = await prepareSuite()
    await expectGuardSuiteFailure({
      ...fixture.identity,
      ...fixture.authority,
      samples: fixture.samples.slice(1),
      uploadParent: fixture.uploadParent,
      explicitSecrets: [],
    }, /every policy browser\/sample slot/u)
    expect(await readdir(fixture.uploadParent)).toEqual([])
  })

  it('cleans receipt-bound staging after a declarative artifact-copy failure cut', async () => {
    const fixture = await prepareSuite()
    const first = fixture.samples[0]
    if (first === undefined) throw new Error('suite fixture is incomplete')
    const artifact = first.sample.artifacts[0]
    if (artifact === undefined) throw new Error('suite fixture has no artifact fault target')
    const guarded = await guardArtifactSuite({
      ...fixture.identity,
      ...fixture.authority,
      samples: fixture.samples,
      uploadParent: fixture.uploadParent,
      explicitSecrets: [],
      uploadFaultCuts: Object.freeze([Object.freeze({
        action: 'fail-before-artifact-copy' as const,
        browser: first.sample.browser,
        sampleIndex: first.sample.sampleIndex,
        relativePath: artifact.relativePath,
      })]),
      })

    expect(guarded.upload).toBeNull()
    expect(await readdir(fixture.uploadParent)).toEqual([])
  })

  it('rejects a structurally smuggled lifecycle callback without invoking it', async () => {
    const fixture = await prepareSuite()
    let invoked = false
    const hostileOptions: GuardArtifactSuiteOptions & {
      readonly hooks: { readonly upload: { readonly beforeSeal: () => Promise<void> } }
    } = {
      ...fixture.identity,
      ...fixture.authority,
      samples: fixture.samples,
      uploadParent: fixture.uploadParent,
      explicitSecrets: [],
      hooks: {
        upload: {
          beforeSeal: () => {
            invoked = true
            return new Promise<void>(() => undefined)
          },
        },
      },
    }
    let recordedFailure: unknown
    try {
      await guardArtifactSuite(hostileOptions)
    } catch (cause) {
      recordedFailure = cause
    }
    expect(recordedFailure).toBeInstanceOf(ArtifactGuardRecordedError)
    expect((recordedFailure as ArtifactGuardRecordedError).traces).toMatchObject({
      completed: true,
      truncated: false,
      failure: null,
    })
    expect(invoked).toBe(false)
    expect(await readdir(fixture.uploadParent)).toEqual([])
  })

  it.each([
    'message-getter',
    'to-string',
    'proxy',
  ] as const)('settles cleanup and traces without inspecting a hostile %s rejection', async (kind) => {
    const fixture = await prepareSuite()
    let inspected = false
    let hostile: unknown
    if (kind === 'message-getter') {
      const error = new Error('placeholder')
      Object.defineProperty(error, 'message', {
        enumerable: false,
        get: () => {
          inspected = true
          throw new Error('hostile message getter executed')
        },
      })
      hostile = error
    } else if (kind === 'to-string') {
      hostile = Object.freeze({
        toString: () => {
          inspected = true
          throw new Error('hostile toString executed')
        },
      })
    } else {
      hostile = new Proxy(Object.create(null), {
        get: () => {
          inspected = true
          throw new Error('hostile proxy getter executed')
        },
        getPrototypeOf: () => {
          inspected = true
          throw new Error('hostile proxy prototype trap executed')
        },
      })
    }
    const directoryPublisher: GuardUploadDirectoryPublisher = Object.freeze({
      invoke: async () => { throw hostile },
    }) as GuardUploadDirectoryPublisher

    const guarded = await guardArtifactSuite({
      ...fixture.identity,
      ...fixture.authority,
      directoryPublisher,
      samples: fixture.samples,
      uploadParent: fixture.uploadParent,
      explicitSecrets: [],
    })

    expect(inspected).toBe(false)
    expect(guarded.upload).toBeNull()
    expect(guarded.guards.every(({ failureMessage }) =>
      failureMessage === 'guard suite upload sealing failed')).toBe(true)
    expect(guarded.traces).toMatchObject({ completed: true, truncated: false, failure: null })
    expect(guarded.traces.events.at(-1)).toMatchObject({
      milestone: 'suite-terminal',
      outcome: 'failed',
      context: {
        cleanupOutcome: 'completed',
        lastMilestone: 'suite-upload-failed',
      },
    })
    expect(await readdir(fixture.uploadParent)).toEqual([])
  })
})

async function expectGuardSuiteFailure(
  options: GuardArtifactSuiteOptions,
  expectedMessage: RegExp,
): Promise<void> {
  const execution = startGuardArtifactSuite(options)
  await expect(execution.result).rejects.toThrow(expectedMessage)
  expect(execution.traces.snapshot()).toMatchObject({
    completed: true,
    truncated: false,
    failure: null,
  })
}

interface SuiteFixture {
  readonly workspace: string
  readonly uploadParent: string
  readonly identity: {
    readonly runId: string
    readonly runPolicy: typeof blockingPolicy
    readonly suite: BrowserSuite
    readonly checkoutSha: string
  }
  readonly authority: Pick<
    FrameworkGuardAuthority,
    'topology' | 'directoryPublisher' | 'settlementTrust'
  >
  readonly samples: readonly GuardArtifactSuiteSample[]
}

async function prepareSuite(options: {
  readonly secretBrowser?: BrowserEngine
  readonly secret?: string
  readonly suite?: BrowserSuite
} = {}): Promise<SuiteFixture> {
  const suite = options.suite ?? 'main'
  const workspace = await createFrameworkWorkspace()
  workspaces.push(workspace)
  const uploadParent = join(workspace, 'suite-upload')
  await mkdir(uploadParent)
  const authority = await createFrameworkGuardAuthority(topology)
  const samples: GuardArtifactSuiteSample[] = []
  for (const browser of BROWSER_ENGINES) {
    const source = join(workspace, `${browser}-diagnostic.txt`)
    const bytes = browser === options.secretBrowser ? options.secret ?? '' : `safe ${browser} diagnostics`
    await writeFile(source, bytes, 'utf8')
    const outcome = await runSyntheticSample({
      workspace,
      topology,
      suite,
      browser,
      sampleIndex: 1,
      runPolicy: blockingPolicy,
      mode: suite === 'main' ? 'main-unavailable' : 'pion-unavailable',
      environment: artifactEnvironment(source, 'playwright/diagnostic.txt', 'process-log', 'text/plain'),
    })
    const sampleResultBytes = await readFile(outcome.resultPath)
    samples.push(Object.freeze({
      sample: outcome.result,
      sampleResultBytes,
      artifactRoot: artifactRootForOutcome(outcome),
      commandSha256: authority.commandSha256,
      settlementAttestation: authority.settlementAttestation(outcome, sampleResultBytes),
    }))
  }
  const first = samples[0]?.sample
  if (first === undefined) throw new Error('suite fixture did not produce a sample')
  return Object.freeze({
    workspace,
    uploadParent,
    identity: Object.freeze({
      runId: first.runId,
      runPolicy: blockingPolicy,
      suite,
      checkoutSha: first.checkoutSha,
    }),
    authority: Object.freeze({
      topology: authority.topology,
      directoryPublisher: authority.directoryPublisher,
      settlementTrust: authority.settlementTrust,
    }),
    samples: Object.freeze(samples),
  })
}

async function sealSuite(fixture: SuiteFixture) {
  const guarded = await guardArtifactSuite({
    ...fixture.identity,
    ...fixture.authority,
    samples: fixture.samples,
    uploadParent: fixture.uploadParent,
    explicitSecrets: [],
  })
  if (guarded.upload === null) throw new Error('passed suite did not produce an upload authority')
  return guarded.upload
}

function verdictSuiteInput(upload: Awaited<ReturnType<typeof sealSuite>>) {
  return Object.freeze({
    root: upload.uploadDirectory,
    jobOutcome: 'success',
    guardOutcome: 'passed',
    downloadOutcome: 'success',
    manifestSha256: upload.manifestSha256,
    manifestByteLength: upload.manifestByteLength,
  })
}
