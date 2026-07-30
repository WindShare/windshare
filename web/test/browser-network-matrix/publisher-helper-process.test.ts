import { createHash } from 'node:crypto'
import { chmod, link, mkdtemp, rename, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { Writable } from 'node:stream'

import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  openPublisherHelperProcessAuthority,
  writePublisherHelperRequestAndClose,
} from '../../scripts/browser-network-matrix/cli/publisher-helper-process.ts'
import { networkMatrixPublisherHelperFromProcess } from '../../scripts/browser-network-matrix/cli/publisher-helper.ts'
import { HELPER_BUILD_MANIFEST_SCHEMA_VERSION } from '../../scripts/browser-network-matrix/cli/build-helpers.mjs'
import type { WindowsJobExecution } from '../../scripts/browser-evidence/process/windows-job-client.ts'

const REQUEST = Buffer.from('{"request":"bounded"}', 'utf8')
const NONCE = 'b'.repeat(32)
const cleanupRoots: string[] = []

interface TestHelperManifestEntry {
  readonly role: 'artifact-publisher' | 'windows-job'
  readonly path: string
  readonly sha256: string
}

interface TestHelperManifest {
  readonly schemaVersion: typeof HELPER_BUILD_MANIFEST_SCHEMA_VERSION
  readonly platform: 'win32' | 'linux'
  readonly architecture: 'amd64' | 'arm64'
  readonly helpers: readonly TestHelperManifestEntry[]
}

afterEach(async () => {
  vi.unstubAllEnvs()
  await Promise.all(cleanupRoots.splice(0).map((root) =>
    rm(root, { recursive: true, force: true })))
})

describe('publisher helper process authority', () => {
  it('binds finite stdin to the held publisher digest and reports an early child exit', async () => {
    const fixture = await executableFixture()
    const executeWindowsJob = vi.fn(async (options) => {
      expect(options.helperPath).toBe(fixture.windowsJob)
      expect(options.command.executable).toBe(fixture.publisher)
      expect(options.inheritedEnvironment).toEqual({})
      expect(options.command.executableSha256).toBe(
        createHash('sha256').update(fixture.publisherBytes).digest('hex'),
      )
      expect(Buffer.from(options.command.stdin ?? []).toString('utf8')).toContain(
        'windshare.artifact-publisher/v2',
      )
      return windowsJobExecutionFixture(23)
    })
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      windowsJobHelperPath: fixture.windowsJob,
      platform: 'win32',
      executeWindowsJob,
    })
    try {
      const client = networkMatrixPublisherHelperFromProcess(authority, () => NONCE)
      await expect(client.aggregatePublisher.publish(
        join(fixture.root, 'must-not-exist.json'),
        '{"aggregate":true}\n',
      )).rejects.toThrow(/response exceeds its byte authority/u)
      expect(executeWindowsJob).toHaveBeenCalledOnce()
    } finally {
      await authority.close()
    }
  })

  it.each([
    ['crash', async () => Promise.reject(new Error('Windows Job supervisor crashed'))],
    ['timeout', async () => windowsJobExecutionFixture(137, true)],
  ] as const)('does not convert a helper %s into publication authority', async (_case, executeWindowsJob) => {
    const fixture = await executableFixture()
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      windowsJobHelperPath: fixture.windowsJob,
      platform: 'win32',
      executeWindowsJob,
    })
    try {
      await expect(authority.execute(REQUEST)).rejects.toThrow(
        _case === 'crash' ? /supervisor crashed/u : /response deadline exceeded/u,
      )
    } finally {
      await authority.close()
    }
  })

  it('rejects a named executable path swap while retaining the opened identity', async () => {
    const fixture = await executableFixture()
    const executeWindowsJob = vi.fn()
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      windowsJobHelperPath: fixture.windowsJob,
      platform: 'win32',
      executeWindowsJob,
    })
    await rename(fixture.publisher, `${fixture.publisher}.held`)
    await writeFile(fixture.publisher, 'foreign replacement')
    try {
      await expect(authority.execute(REQUEST)).rejects.toThrow(/identity or revision changed/u)
      expect(executeWindowsJob).not.toHaveBeenCalled()
    } finally {
      await authority.close()
    }
  })

  it('requires the explicit publisher and Windows Job paths to equal the held manifest', async () => {
    const fixture = await executableFixture()
    const foreignPublisher = join(fixture.root, 'foreign-publisher.exe')
    const foreignWindowsJob = join(fixture.root, 'foreign-windowsjob.exe')
    await Promise.all([
      writeFile(foreignPublisher, fixture.publisherBytes),
      writeFile(foreignWindowsJob, fixture.windowsJobBytes),
    ])

    await expect(openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: foreignPublisher,
      windowsJobHelperPath: fixture.windowsJob,
      platform: 'win32',
    })).rejects.toThrow(/publisher helper explicit path differs from the held helper manifest/u)
    await expect(openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      windowsJobHelperPath: foreignWindowsJob,
      platform: 'win32',
    })).rejects.toThrow(/Windows Job helper explicit path differs from the held helper manifest/u)

    const publisherHardlink = join(fixture.root, 'publisher-hardlink.exe')
    await link(fixture.publisher, publisherHardlink)
    await expect(openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: publisherHardlink,
      windowsJobHelperPath: fixture.windowsJob,
      platform: 'win32',
    })).rejects.toThrow(/publisher helper explicit path differs from the held helper manifest/u)

    await expect(openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.windowsJob,
      windowsJobHelperPath: fixture.publisher,
      platform: 'win32',
    })).rejects.toThrow(/publisher helper explicit path differs from the held helper manifest/u)
  })

  it('rejects missing, extra, and cross-platform manifest roles before launch', async () => {
    const fixture = await executableFixture()
    const options = {
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      windowsJobHelperPath: fixture.windowsJob,
      platform: 'win32' as const,
    }
    await writeCanonicalManifest(fixture.manifest, {
      ...fixture.manifestValue,
      helpers: [fixture.manifestValue.helpers[0] as TestHelperManifestEntry],
    })
    await expect(openPublisherHelperProcessAuthority(options)).rejects.toThrow(
      /missing or extra platform helper/u,
    )

    await writeCanonicalManifest(fixture.manifest, {
      ...fixture.manifestValue,
      helpers: [...fixture.manifestValue.helpers, fixture.manifestValue.helpers[1] as TestHelperManifestEntry],
    })
    await expect(openPublisherHelperProcessAuthority(options)).rejects.toThrow(
      /missing or extra platform helper/u,
    )

    await writeCanonicalManifest(fixture.manifest, {
      ...fixture.manifestValue,
      platform: 'linux',
      helpers: [fixture.manifestValue.helpers[0] as TestHelperManifestEntry],
    })
    await expect(openPublisherHelperProcessAuthority(options)).rejects.toThrow(
      /platform differs from runtime/u,
    )
  })

  it('rejects non-canonical manifest bytes and helper SHA-256 mismatches', async () => {
    const fixture = await executableFixture()
    const options = {
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      windowsJobHelperPath: fixture.windowsJob,
      platform: 'win32' as const,
    }
    await writeFile(fixture.manifest, `${JSON.stringify(fixture.manifestValue, null, 2)}\n`, 'utf8')
    await expect(openPublisherHelperProcessAuthority(options)).rejects.toThrow(/canonical JSON/u)

    await writeCanonicalManifest(fixture.manifest, {
      ...fixture.manifestValue,
      helpers: [
        { ...(fixture.manifestValue.helpers[0] as TestHelperManifestEntry), sha256: '0'.repeat(64) },
        fixture.manifestValue.helpers[1] as TestHelperManifestEntry,
      ],
    })
    await expect(openPublisherHelperProcessAuthority(options)).rejects.toThrow(
      /publisher helper bytes differ from the held helper manifest/u,
    )

    await writeCanonicalManifest(fixture.manifest, {
      ...fixture.manifestValue,
      helpers: [
        fixture.manifestValue.helpers[0] as TestHelperManifestEntry,
        { ...(fixture.manifestValue.helpers[1] as TestHelperManifestEntry), sha256: '0'.repeat(64) },
      ],
    })
    await expect(openPublisherHelperProcessAuthority(options)).rejects.toThrow(
      /Windows Job helper bytes differ from the held helper manifest/u,
    )
  })
})

describe('publisher helper held-authority revalidation', () => {
  it('rejects a named manifest swap before any helper launch', async () => {
    const fixture = await executableFixture()
    const executeWindowsJob = vi.fn()
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      windowsJobHelperPath: fixture.windowsJob,
      platform: 'win32',
      executeWindowsJob,
    })
    await rename(fixture.manifest, `${fixture.manifest}.held`)
    await writeFile(fixture.manifest, fixture.manifestBytes, 'utf8')
    try {
      await expect(authority.execute(REQUEST)).rejects.toThrow(/manifest identity or revision changed/u)
      expect(executeWindowsJob).not.toHaveBeenCalled()
    } finally {
      await authority.close()
    }
  })

  it('rejects a manifest mutation during launch-time TOCTOU revalidation', async () => {
    const fixture = await executableFixture()
    const executeWindowsJob = vi.fn(async () => {
      await writeFile(fixture.manifest, `${fixture.manifestBytes} `, 'utf8')
      return windowsJobExecutionFixture(0)
    })
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      windowsJobHelperPath: fixture.windowsJob,
      platform: 'win32',
      executeWindowsJob,
    })
    try {
      await expect(authority.execute(REQUEST)).rejects.toThrow(/manifest identity or revision changed/u)
      expect(executeWindowsJob).toHaveBeenCalledOnce()
    } finally {
      await authority.close()
    }
  })

  it('rejects a Windows Job supervisor swap before launch', async () => {
    const fixture = await executableFixture()
    const executeWindowsJob = vi.fn()
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      windowsJobHelperPath: fixture.windowsJob,
      platform: 'win32',
      executeWindowsJob,
    })
    await rename(fixture.windowsJob, `${fixture.windowsJob}.held`)
    await writeFile(fixture.windowsJob, fixture.windowsJobBytes)
    try {
      await expect(authority.execute(REQUEST)).rejects.toThrow(
        /Windows Job helper identity or revision changed/u,
      )
      expect(executeWindowsJob).not.toHaveBeenCalled()
    } finally {
      await authority.close()
    }
  })

  it('rejects publisher byte mutation before launch and during post-launch revalidation', async () => {
    const beforeFixture = await executableFixture()
    const beforeExecuteWindowsJob = vi.fn()
    const beforeAuthority = await openPublisherHelperProcessAuthority({
      helperManifestPath: beforeFixture.manifest,
      publisherHelperPath: beforeFixture.publisher,
      windowsJobHelperPath: beforeFixture.windowsJob,
      platform: 'win32',
      executeWindowsJob: beforeExecuteWindowsJob,
    })
    await writeFile(beforeFixture.publisher, 'mutated publisher bytes', 'utf8')
    try {
      await expect(beforeAuthority.execute(REQUEST)).rejects.toThrow(
        /publisher helper identity or revision changed/u,
      )
      expect(beforeExecuteWindowsJob).not.toHaveBeenCalled()
    } finally {
      await beforeAuthority.close()
    }

    const duringFixture = await executableFixture()
    const duringExecuteWindowsJob = vi.fn(async () => {
      await writeFile(duringFixture.publisher, 'mutated during execution', 'utf8')
      return windowsJobExecutionFixture(0)
    })
    const duringAuthority = await openPublisherHelperProcessAuthority({
      helperManifestPath: duringFixture.manifest,
      publisherHelperPath: duringFixture.publisher,
      windowsJobHelperPath: duringFixture.windowsJob,
      platform: 'win32',
      executeWindowsJob: duringExecuteWindowsJob,
    })
    try {
      await expect(duringAuthority.execute(REQUEST)).rejects.toThrow(
        /publisher helper identity or revision changed/u,
      )
      expect(duringExecuteWindowsJob).toHaveBeenCalledOnce()
    } finally {
      await duringAuthority.close()
    }
  })

  it('fails closed when the Windows Job supervisor path changes during execution', async () => {
    const fixture = await executableFixture()
    const executeWindowsJob = vi.fn(async () => {
      await rename(fixture.windowsJob, `${fixture.windowsJob}.during`)
      await writeFile(fixture.windowsJob, fixture.windowsJobBytes)
      return windowsJobExecutionFixture(0)
    })
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      windowsJobHelperPath: fixture.windowsJob,
      platform: 'win32',
      executeWindowsJob,
    })
    try {
      await expect(authority.execute(REQUEST)).rejects.toThrow(
        /Windows Job helper identity or revision changed/u,
      )
      expect(executeWindowsJob).toHaveBeenCalledOnce()
    } finally {
      await authority.close()
    }
  })

  it('requires an explicit Windows Job path and rejects that role on Linux', async () => {
    const windowsFixture = await executableFixture()
    await expect(openPublisherHelperProcessAuthority({
      helperManifestPath: windowsFixture.manifest,
      publisherHelperPath: windowsFixture.publisher,
      platform: 'win32',
    })).rejects.toThrow(/requires an explicit Windows Job helper path/u)

    const linuxFixture = await executableFixture('linux')
    await expect(openPublisherHelperProcessAuthority({
      helperManifestPath: linuxFixture.manifest,
      publisherHelperPath: linuxFixture.publisher,
      windowsJobHelperPath: linuxFixture.windowsJob,
      platform: 'linux',
    })).rejects.toThrow(/Linux must not receive a Windows Job helper path/u)
  })

  it('reuses long-lived executable handles without accumulating stream listeners', async () => {
    const fixture = await executableFixture()
    const executeWindowsJob = vi.fn(async () => windowsJobExecutionFixture(0))
    const emitWarning = vi.spyOn(process, 'emitWarning').mockImplementation(() => undefined)
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      windowsJobHelperPath: fixture.windowsJob,
      platform: 'win32',
      executeWindowsJob,
    })
    try {
      for (let attempt = 0; attempt < 32; attempt += 1) {
        await expect(authority.execute(REQUEST)).resolves.toMatchObject({ exitCode: 0 })
      }
      expect(executeWindowsJob).toHaveBeenCalledTimes(32)
      expect(emitWarning.mock.calls.map(([warning]) => String(warning)).join('\n'))
        .not.toMatch(/MaxListenersExceeded/u)
    } finally {
      await authority.close()
      emitWarning.mockRestore()
    }
  })

  it.runIf(process.platform === 'linux')('does not pass ambient secrets to the Linux publisher', async () => {
    const fixture = await linuxEnvironmentFixture()
    vi.stubEnv('WINDSHARE_PUBLISHER_SECRET_SENTINEL', 'must-not-cross-publisher-boundary')
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      platform: 'linux',
    })
    try {
      const result = await authority.execute(REQUEST)
      expect(result.exitCode).toBe(0)
      expect(result.stdout).toHaveLength(0)
      expect(result.stderr).toHaveLength(0)
    } finally {
      await authority.close()
    }
  })

  it('rejects an EPIPE after a partial request write', async () => {
    const observed: Buffer[] = []
    const stream = new Writable({
      write(chunk: Buffer, _encoding, callback) {
        observed.push(Buffer.from(chunk.subarray(0, 4)))
        callback(Object.assign(new Error('publisher child exited during request'), { code: 'EPIPE' }))
      },
    })

    await expect(writePublisherHelperRequestAndClose(stream, REQUEST)).rejects.toMatchObject({
      code: 'EPIPE',
    })
    expect(Buffer.concat(observed)).toEqual(REQUEST.subarray(0, 4))
  })
})

const WINDOWS_JOB_FIXTURE_ROOT_PID = 4_242

function windowsJobExecutionFixture(
  exitCode: number,
  timedOut = false,
): WindowsJobExecution {
  return Object.freeze({
    processEvidence: Object.freeze({ terminal: 'exited' as const, exitCode }),
    timedOut,
    launched: true,
    treeEmpty: true,
    inputEvidence: Object.freeze({
      outcome: 'delivered' as const,
      failureCode: '',
      failureMessage: '',
    }),
    clientIoEvidence: Object.freeze({
      requestOutcome: 'delivered' as const,
      rawInputOutcome: 'delivered' as const,
      controlOutcome: 'not-requested' as const,
      outputOutcome: 'delivered' as const,
      failureCode: '',
      failureMessage: '',
    }),
    ownershipEvidence: Object.freeze({
      supervisionOutcome: 'tree-empty' as const,
      terminationReason: timedOut ? 'deadline' as const : 'natural' as const,
      activeProcessCount: 0 as const,
      root: Object.freeze({ pid: WINDOWS_JOB_FIXTURE_ROOT_PID, exitCode }),
      spawnFailure: null,
    }),
  })
}

async function executableFixture(platform: 'win32' | 'linux' = 'win32'): Promise<Readonly<{
  root: string
  manifest: string
  manifestBytes: string
  manifestValue: TestHelperManifest
  publisher: string
  publisherBytes: Buffer
  windowsJob: string
  windowsJobBytes: Buffer
}>> {
  const root = await mkdtemp(join(tmpdir(), 'windshare-publisher-process-test-'))
  cleanupRoots.push(root)
  const publisher = join(root, 'browsermatrixpublish.exe')
  const windowsJob = join(root, 'windowsjob.exe')
  const manifest = join(root, 'helper-manifest.json')
  const publisherBytes = Buffer.from('publisher executable bytes')
  const windowsJobBytes = Buffer.from('Windows Job executable bytes')
  await Promise.all([
    writeFile(publisher, publisherBytes),
    writeFile(windowsJob, windowsJobBytes),
  ])
  const manifestValue: TestHelperManifest = Object.freeze({
    schemaVersion: HELPER_BUILD_MANIFEST_SCHEMA_VERSION,
    platform,
    architecture: runtimeGoArchitecture(),
    helpers: Object.freeze([
      {
        role: 'artifact-publisher' as const,
        path: publisher,
        sha256: createHash('sha256').update(publisherBytes).digest('hex'),
      },
      ...(platform === 'win32' ? [{
        role: 'windows-job',
        path: windowsJob,
        sha256: createHash('sha256').update(windowsJobBytes).digest('hex'),
      } as const] : []),
    ]),
  })
  const manifestBytes = await writeCanonicalManifest(manifest, manifestValue)
  return Object.freeze({
    root,
    manifest,
    manifestBytes,
    manifestValue,
    publisher,
    publisherBytes,
    windowsJob,
    windowsJobBytes,
  })
}

async function writeCanonicalManifest(path: string, value: TestHelperManifest): Promise<string> {
  const encoded = `${JSON.stringify(value)}\n`
  await writeFile(path, encoded, 'utf8')
  return encoded
}

async function linuxEnvironmentFixture(): Promise<Readonly<{
  manifest: string
  publisher: string
}>> {
  const root = await mkdtemp(join(tmpdir(), 'windshare-linux-publisher-environment-test-'))
  cleanupRoots.push(root)
  const publisher = join(root, 'browsermatrixpublish')
  const manifest = join(root, 'helper-manifest.json')
  const publisherBytes = Buffer.from([
    '#!/bin/sh',
    'if [ "${WINDSHARE_PUBLISHER_SECRET_SENTINEL+x}" = "x" ]; then',
    '  printf "ambient environment leaked" >&2',
    '  exit 97',
    'fi',
    'while IFS= read -r line || [ -n "$line" ]; do :; done',
    'exit 0',
    '',
  ].join('\n'), 'utf8')
  await writeFile(publisher, publisherBytes)
  await chmod(publisher, 0o700)
  await writeFile(manifest, `${JSON.stringify({
    schemaVersion: HELPER_BUILD_MANIFEST_SCHEMA_VERSION,
    platform: 'linux',
    architecture: runtimeGoArchitecture(),
    helpers: [{
      role: 'artifact-publisher',
      path: publisher,
      sha256: createHash('sha256').update(publisherBytes).digest('hex'),
    }],
  })}\n`, 'utf8')
  return Object.freeze({ manifest, publisher })
}

function runtimeGoArchitecture(): 'amd64' | 'arm64' {
  if (process.arch === 'arm64') return 'arm64'
  return 'amd64'
}
