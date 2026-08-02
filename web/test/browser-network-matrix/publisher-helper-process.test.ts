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
import type { TestProcessOwnerExecution } from '../../scripts/browser-evidence/process/test-process-owner-client.mjs'
import { loadFrameworkProcessOwner } from '../browser-evidence/process-owner-fixtures.ts'

const REQUEST = Buffer.from('{"request":"bounded"}', 'utf8')
const NONCE = 'b'.repeat(32)
const cleanupRoots: string[] = []

interface TestHelperManifestEntry {
  readonly role: 'artifact-publisher' | 'test-process-owner'
  readonly path: string
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

describe('publisher helper process', () => {
  it('binds finite stdin to the selected publisher and reports an early child exit', async () => {
    const fixture = await executableFixture()
    const executeProcessOwner = vi.fn(async (options) => {
      expect(options.owner.path).toBe(fixture.processOwner)
      expect(options.command.executable).toBe(fixture.publisher)
      expect(options.environment).toEqual({})
      expect(Buffer.from(options.command.stdin ?? []).toString('utf8')).toContain(
        'windshare.artifact-publisher/v2',
      )
      return processOwnerExecutionFixture(23)
    })
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      processOwnerPath: fixture.processOwner,
      platform: 'win32',
      executeProcessOwner,
    })
    try {
      const client = networkMatrixPublisherHelperFromProcess(authority, () => NONCE)
      await expect(client.aggregatePublisher.publish(
        join(fixture.root, 'must-not-exist.json'),
        '{"aggregate":true}\n',
      )).rejects.toThrow(/response exceeds its byte authority/u)
      expect(executeProcessOwner).toHaveBeenCalledOnce()
    } finally {
      await authority.close()
    }
  })

  it.each([
    ['crash', async () => Promise.reject(new Error('test process owner supervisor crashed'))],
    ['timeout', async () => processOwnerExecutionFixture(137, true)],
  ] as const)('does not convert a helper %s into publication authority', async (_case, executeProcessOwner) => {
    const fixture = await executableFixture()
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      processOwnerPath: fixture.processOwner,
      platform: 'win32',
      executeProcessOwner,
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
    const executeProcessOwner = vi.fn()
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      processOwnerPath: fixture.processOwner,
      platform: 'win32',
      executeProcessOwner,
    })
    await rename(fixture.publisher, `${fixture.publisher}.held`)
    await writeFile(fixture.publisher, 'foreign replacement')
    try {
      await expect(authority.execute(REQUEST)).rejects.toThrow(/identity or revision changed/u)
      expect(executeProcessOwner).not.toHaveBeenCalled()
    } finally {
      await authority.close()
    }
  })

  it('requires the explicit publisher and test process owner paths to equal the held manifest', async () => {
    const fixture = await executableFixture()
    const foreignPublisher = join(fixture.root, 'foreign-publisher.exe')
    const foreignProcessOwner = join(fixture.root, 'foreign-testprocessowner.exe')
    await Promise.all([
      writeFile(foreignPublisher, fixture.publisherBytes),
      writeFile(foreignProcessOwner, fixture.processOwnerBytes),
    ])

    await expect(openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: foreignPublisher,
      processOwnerPath: fixture.processOwner,
      platform: 'win32',
    })).rejects.toThrow(/publisher helper explicit path differs from the held helper manifest/u)
    await expect(openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      processOwnerPath: foreignProcessOwner,
      platform: 'win32',
    })).rejects.toThrow(/test process owner explicit path differs from the held helper manifest/u)

    const publisherHardlink = join(fixture.root, 'publisher-hardlink.exe')
    await link(fixture.publisher, publisherHardlink)
    await expect(openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: publisherHardlink,
      processOwnerPath: fixture.processOwner,
      platform: 'win32',
    })).rejects.toThrow(/publisher helper explicit path differs from the held helper manifest/u)

    await expect(openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.processOwner,
      processOwnerPath: fixture.publisher,
      platform: 'win32',
    })).rejects.toThrow(/publisher helper explicit path differs from the held helper manifest/u)
  })

  it('rejects missing, extra, and cross-platform manifest roles before launch', async () => {
    const fixture = await executableFixture()
    const options = {
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      processOwnerPath: fixture.processOwner,
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

  it('rejects non-canonical manifest bytes', async () => {
    const fixture = await executableFixture()
    const options = {
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      processOwnerPath: fixture.processOwner,
      platform: 'win32' as const,
    }
    await writeFile(fixture.manifest, `${JSON.stringify(fixture.manifestValue, null, 2)}\n`, 'utf8')
    await expect(openPublisherHelperProcessAuthority(options)).rejects.toThrow(/canonical JSON/u)
  })
})

describe('publisher helper held-authority revalidation', () => {
  it('rejects a named manifest swap before any helper launch', async () => {
    const fixture = await executableFixture()
    const executeProcessOwner = vi.fn()
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      processOwnerPath: fixture.processOwner,
      platform: 'win32',
      executeProcessOwner,
    })
    await rename(fixture.manifest, `${fixture.manifest}.held`)
    await writeFile(fixture.manifest, fixture.manifestBytes, 'utf8')
    try {
      await expect(authority.execute(REQUEST)).rejects.toThrow(/manifest identity or revision changed/u)
      expect(executeProcessOwner).not.toHaveBeenCalled()
    } finally {
      await authority.close()
    }
  })

  it('rejects a manifest mutation during launch-time TOCTOU revalidation', async () => {
    const fixture = await executableFixture()
    const executeProcessOwner = vi.fn(async () => {
      await writeFile(fixture.manifest, `${fixture.manifestBytes} `, 'utf8')
      return processOwnerExecutionFixture(0)
    })
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      processOwnerPath: fixture.processOwner,
      platform: 'win32',
      executeProcessOwner,
    })
    try {
      await expect(authority.execute(REQUEST)).rejects.toThrow(/manifest identity or revision changed/u)
      expect(executeProcessOwner).toHaveBeenCalledOnce()
    } finally {
      await authority.close()
    }
  })

  it('rejects a test process owner supervisor swap before launch', async () => {
    const fixture = await executableFixture()
    const executeProcessOwner = vi.fn()
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      processOwnerPath: fixture.processOwner,
      platform: 'win32',
      executeProcessOwner,
    })
    await rename(fixture.processOwner, `${fixture.processOwner}.held`)
    await writeFile(fixture.processOwner, fixture.processOwnerBytes)
    try {
      await expect(authority.execute(REQUEST)).rejects.toThrow(
        /test process owner identity or revision changed/u,
      )
      expect(executeProcessOwner).not.toHaveBeenCalled()
    } finally {
      await authority.close()
    }
  })

  it('rejects publisher byte mutation before launch and during post-launch revalidation', async () => {
    const beforeFixture = await executableFixture()
    const beforeExecuteProcessOwner = vi.fn()
    const beforeAuthority = await openPublisherHelperProcessAuthority({
      helperManifestPath: beforeFixture.manifest,
      publisherHelperPath: beforeFixture.publisher,
      processOwnerPath: beforeFixture.processOwner,
      platform: 'win32',
      executeProcessOwner: beforeExecuteProcessOwner,
    })
    await writeFile(beforeFixture.publisher, 'mutated publisher bytes', 'utf8')
    try {
      await expect(beforeAuthority.execute(REQUEST)).rejects.toThrow(
        /publisher helper identity or revision changed/u,
      )
      expect(beforeExecuteProcessOwner).not.toHaveBeenCalled()
    } finally {
      await beforeAuthority.close()
    }

    const duringFixture = await executableFixture()
    const duringExecuteProcessOwner = vi.fn(async () => {
      await writeFile(duringFixture.publisher, 'mutated during execution', 'utf8')
      return processOwnerExecutionFixture(0)
    })
    const duringAuthority = await openPublisherHelperProcessAuthority({
      helperManifestPath: duringFixture.manifest,
      publisherHelperPath: duringFixture.publisher,
      processOwnerPath: duringFixture.processOwner,
      platform: 'win32',
      executeProcessOwner: duringExecuteProcessOwner,
    })
    try {
      await expect(duringAuthority.execute(REQUEST)).rejects.toThrow(
        /publisher helper identity or revision changed/u,
      )
      expect(duringExecuteProcessOwner).toHaveBeenCalledOnce()
    } finally {
      await duringAuthority.close()
    }
  })

  it('fails closed when the test process owner supervisor path changes during execution', async () => {
    const fixture = await executableFixture()
    const executeProcessOwner = vi.fn(async () => {
      await rename(fixture.processOwner, `${fixture.processOwner}.during`)
      await writeFile(fixture.processOwner, fixture.processOwnerBytes)
      return processOwnerExecutionFixture(0)
    })
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      processOwnerPath: fixture.processOwner,
      platform: 'win32',
      executeProcessOwner,
    })
    try {
      await expect(authority.execute(REQUEST)).rejects.toThrow(
        /test process owner identity or revision changed/u,
      )
      expect(executeProcessOwner).toHaveBeenCalledOnce()
    } finally {
      await authority.close()
    }
  })

  it('requires an explicit test process owner path on every supported platform', async () => {
    const windowsFixture = await executableFixture()
    await expect(openPublisherHelperProcessAuthority({
      helperManifestPath: windowsFixture.manifest,
      publisherHelperPath: windowsFixture.publisher,
      processOwnerPath: '',
      platform: 'win32',
    })).rejects.toThrow(/test process owner explicit path differs/u)

    const linuxFixture = await executableFixture('linux')
    await expect(openPublisherHelperProcessAuthority({
      helperManifestPath: linuxFixture.manifest,
      publisherHelperPath: linuxFixture.publisher,
      processOwnerPath: '',
      platform: 'linux',
    })).rejects.toThrow(/test process owner explicit path differs/u)
  })

  it('reuses long-lived executable handles without accumulating stream listeners', async () => {
    const fixture = await executableFixture()
    const executeProcessOwner = vi.fn(async () => processOwnerExecutionFixture(0))
    const emitWarning = vi.spyOn(process, 'emitWarning').mockImplementation(() => undefined)
    const authority = await openPublisherHelperProcessAuthority({
      helperManifestPath: fixture.manifest,
      publisherHelperPath: fixture.publisher,
      processOwnerPath: fixture.processOwner,
      platform: 'win32',
      executeProcessOwner,
    })
    try {
      for (let attempt = 0; attempt < 32; attempt += 1) {
        await expect(authority.execute(REQUEST)).resolves.toMatchObject({ exitCode: 0 })
      }
      expect(executeProcessOwner).toHaveBeenCalledTimes(32)
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
      processOwnerPath: fixture.processOwner,
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

function processOwnerExecutionFixture(
  exitCode: number,
  timedOut = false,
): TestProcessOwnerExecution {
  return Object.freeze({
    processEvidence: Object.freeze({ terminal: 'exited' as const, exitCode }),
    treeEmpty: true,
    cleanupOutcome: 'completed' as const,
    inputEvidence: Object.freeze({
      outcome: 'delivered' as const,
      failureCode: '',
      failureMessage: '',
    }),
    output: Object.freeze({
      stdout: byteSnapshot(),
      stderr: byteSnapshot(),
    }),
    events: Object.freeze({
      events: Object.freeze([]),
      observedEvents: 0,
      capturedEvents: 0,
      truncated: false,
      completed: true,
    }),
    ownershipEvidence: Object.freeze({
      kind: 'test-process-owner' as const,
      backend: 'windows_job' as const,
      terminationReason: timedOut ? 'deadline' as const : 'natural' as const,
      platform: Object.freeze({ kind: 'windows_job' }),
    }),
  })
}

function byteSnapshot(bytes = new Uint8Array()) {
  const retained = Uint8Array.from(bytes)
  return Object.freeze({
    observedBytes: retained.byteLength,
    capturedBytes: retained.byteLength,
    truncated: false,
    completed: true,
    bytes: () => Uint8Array.from(retained),
  })
}

async function executableFixture(platform: 'win32' | 'linux' = 'win32'): Promise<Readonly<{
  root: string
  manifest: string
  manifestBytes: string
  manifestValue: TestHelperManifest
  publisher: string
  publisherBytes: Buffer
  processOwner: string
  processOwnerBytes: Buffer
}>> {
  const root = await mkdtemp(join(tmpdir(), 'windshare-publisher-process-test-'))
  cleanupRoots.push(root)
  const executableSuffix = platform === 'win32' ? '.exe' : ''
  const publisher = join(root, `browsermatrixpublish${executableSuffix}`)
  const processOwner = join(root, `testprocessowner${executableSuffix}`)
  const manifest = join(root, 'helper-manifest.json')
  const publisherBytes = Buffer.from('publisher executable bytes')
  const processOwnerBytes = Buffer.from('test process owner executable bytes')
  await Promise.all([
    writeFile(publisher, publisherBytes),
    writeFile(processOwner, processOwnerBytes),
  ])
  if (platform === 'linux') await Promise.all([chmod(publisher, 0o700), chmod(processOwner, 0o700)])
  const manifestValue: TestHelperManifest = Object.freeze({
    schemaVersion: HELPER_BUILD_MANIFEST_SCHEMA_VERSION,
    platform,
    architecture: runtimeGoArchitecture(),
    helpers: Object.freeze([
      {
        role: 'artifact-publisher' as const,
        path: publisher,
      },
      {
        role: 'test-process-owner',
        path: processOwner,
      } as const,
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
    processOwner,
    processOwnerBytes,
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
  processOwner: string
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
  const processOwner = await loadFrameworkProcessOwner()
  await writeFile(manifest, `${JSON.stringify({
    schemaVersion: HELPER_BUILD_MANIFEST_SCHEMA_VERSION,
    platform: 'linux',
    architecture: runtimeGoArchitecture(),
    helpers: [
      {
        role: 'artifact-publisher',
        path: publisher,
      },
      {
        role: 'test-process-owner',
        path: processOwner.path,
      },
    ],
  })}\n`, 'utf8')
  return Object.freeze({ manifest, publisher, processOwner: processOwner.path })
}

function runtimeGoArchitecture(): 'amd64' | 'arm64' {
  if (process.arch === 'arm64') return 'arm64'
  return 'amd64'
}
