import { execFile } from 'node:child_process'
import { mkdtemp, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

import type { ChildEvidenceContext } from '../../scripts/browser-evidence/child-evidence'
import {
  browserRtcConfiguration,
  parseTestIceTopologyJson,
  parseTestIceTopologyResolutionJson,
  testIceTopologyResolutionSha256,
  testIceTopologySha256,
  verifyTestIceTopologyLock,
  type VerifiedTestIceTopologyLock,
} from '../../scripts/browser-evidence/test-ice-topology'

const execFileAsync = promisify(execFile)
const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const CANONICAL_TOPOLOGY_PROFILE = resolve(
  REPOSITORY_ROOT,
  'testdata/test-ice-topology/pr-same-host-kernel-route-ipv4.json',
)
const TOPOLOGY_MATERIALIZER = './web/scripts/browser-evidence/topology-resolution'
const TOPOLOGY_MATERIALIZATION_TIMEOUT_MS = 30_000
const MAXIMUM_MATERIALIZER_OUTPUT_BYTES = 64 * 1024
const SHA256_PATTERN = /^[0-9a-f]{64}$/u
const TOPOLOGY_PROFILE_ENV = 'WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE'
const TOPOLOGY_RESOLUTION_ENV = 'WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION'
const TOPOLOGY_PROFILE_SHA256_ENV = 'WINDSHARE_TEST_ICE_TOPOLOGY_PROFILE_SHA256'
const TOPOLOGY_RESOLUTION_SHA256_ENV = 'WINDSHARE_TEST_ICE_TOPOLOGY_RESOLUTION_SHA256'

interface TopologyMaterializationRecord {
  readonly component: 'browser-evidence-topology-resolution'
  readonly outcome: 'materialized'
  readonly profilePath: string
  readonly resolutionPath: string
  readonly topologyProfileSha256: string
  readonly topologyResolutionSha256: string
}

export interface AcquiredTestIceTopology {
  readonly profilePath: string
  readonly resolutionPath: string
  readonly lock: VerifiedTestIceTopologyLock
  readonly rtcConfiguration: RTCConfiguration
  release(): Promise<void>
}

type PublishedTestIceTopology = Pick<
  ChildEvidenceContext,
  | 'topologyProfilePath'
  | 'topologyResolutionPath'
  | 'topologyProfileSha256'
  | 'topologyResolutionSha256'
>

/**
 * A parent-published child context or full-suite environment is authoritative.
 * Direct Playwright runs use the same one-shot materializer, so no path can fall
 * back to the checked-in example resolution or a JavaScript-only interface guess.
 */
export async function acquireTestIceTopology(
  context?: PublishedTestIceTopology,
  environment: Readonly<Record<string, string | undefined>> = process.env,
  signal?: AbortSignal,
): Promise<AcquiredTestIceTopology> {
  signal?.throwIfAborted()
  const environmentLock = publishedTopologyFromEnvironment(environment)
  if (context !== undefined && environmentLock !== undefined) {
    requireSamePublishedTopology(context, environmentLock)
  }
  const published = context ?? environmentLock
  if (published !== undefined) {
    const lock = await loadVerifiedLock(
      published.topologyProfilePath,
      published.topologyResolutionPath,
      published.topologyProfileSha256,
      published.topologyResolutionSha256,
      signal,
    )
    return frozenTopology(
      published.topologyProfilePath,
      published.topologyResolutionPath,
      lock,
    )
  }

  const directory = await mkdtemp(join(tmpdir(), 'windshare-test-ice-topology-'))
  const resolutionPath = join(directory, 'resolution.json')
  try {
    const { stdout } = await execFileAsync(
      process.env.WINDSHARE_GO_EXECUTABLE ?? 'go',
      [
        'run',
        TOPOLOGY_MATERIALIZER,
        '--profile',
        CANONICAL_TOPOLOGY_PROFILE,
        '--output',
        resolutionPath,
      ],
      {
        cwd: REPOSITORY_ROOT,
        env: { ...process.env, GOWORK: 'auto' },
        encoding: 'utf8',
        timeout: TOPOLOGY_MATERIALIZATION_TIMEOUT_MS,
        signal,
        windowsHide: true,
        maxBuffer: MAXIMUM_MATERIALIZER_OUTPUT_BYTES,
      },
    )
    signal?.throwIfAborted()
    const materialized = parseMaterializationRecord(stdout)
    if (
      !samePath(materialized.profilePath, CANONICAL_TOPOLOGY_PROFILE) ||
      !samePath(materialized.resolutionPath, resolutionPath)
    ) {
      throw new Error('Topology materializer published paths outside this sample')
    }
    const lock = await loadVerifiedLock(
      materialized.profilePath,
      materialized.resolutionPath,
      materialized.topologyProfileSha256,
      materialized.topologyResolutionSha256,
      signal,
    )
    const acquired = frozenTopology(materialized.profilePath, materialized.resolutionPath, lock)
    let releaseTask: Promise<void> | undefined
    return Object.freeze({
      ...acquired,
      release: async () => {
        const attempt = releaseTask ?? rm(directory, { recursive: true, force: true })
        releaseTask = attempt
        try {
          await attempt
        } catch (error) {
          // Concurrent callers share one removal, but a transient handle race
          // must not poison ownership and prevent a later teardown retry.
          if (releaseTask === attempt) releaseTask = undefined
          throw error
        }
      },
    })
  } catch (error) {
    try {
      await rm(directory, { recursive: true, force: true })
    } catch (cleanupError) {
      throw new AggregateError(
        [error, cleanupError],
        'Topology acquisition failed and its private directory could not be removed',
        { cause: cleanupError },
      )
    }
    throw error
  }
}

function publishedTopologyFromEnvironment(
  environment: Readonly<Record<string, string | undefined>>,
): PublishedTestIceTopology | undefined {
  const values = {
    topologyProfilePath: environment[TOPOLOGY_PROFILE_ENV],
    topologyResolutionPath: environment[TOPOLOGY_RESOLUTION_ENV],
    topologyProfileSha256: environment[TOPOLOGY_PROFILE_SHA256_ENV],
    topologyResolutionSha256: environment[TOPOLOGY_RESOLUTION_SHA256_ENV],
  }
  const defined = Object.values(values).filter((value) => value !== undefined)
  if (defined.length === 0) return undefined
  if (defined.length !== Object.keys(values).length || defined.some((value) => value === '')) {
    throw new Error('Published test ICE topology environment must provide all four lock values')
  }
  const published = values as Record<keyof typeof values, string>
  requireAbsolutePath(published.topologyProfilePath, 'Published topology profile')
  requireAbsolutePath(published.topologyResolutionPath, 'Published topology resolution')
  if (published.topologyProfilePath === published.topologyResolutionPath) {
    throw new Error('Published topology profile and resolution paths must be distinct')
  }
  requireSha256(published.topologyProfileSha256, 'Published topology profile')
  requireSha256(published.topologyResolutionSha256, 'Published topology resolution')
  return Object.freeze(published)
}

function requireSamePublishedTopology(
  context: PublishedTestIceTopology,
  environment: PublishedTestIceTopology,
): void {
  if (
    context.topologyProfilePath !== environment.topologyProfilePath ||
    context.topologyResolutionPath !== environment.topologyResolutionPath ||
    context.topologyProfileSha256 !== environment.topologyProfileSha256 ||
    context.topologyResolutionSha256 !== environment.topologyResolutionSha256
  ) {
    throw new Error('Child evidence context and topology environment identify different locks')
  }
}

async function loadVerifiedLock(
  profilePath: string,
  resolutionPath: string,
  expectedProfileSha256: string,
  expectedResolutionSha256: string,
  signal?: AbortSignal,
): Promise<VerifiedTestIceTopologyLock> {
  signal?.throwIfAborted()
  const [profileBytes, resolutionBytes, canonicalProfileBytes] = await Promise.all([
    readFile(profilePath, { signal }),
    readFile(resolutionPath, { signal }),
    readFile(CANONICAL_TOPOLOGY_PROFILE, { signal }),
  ])
  signal?.throwIfAborted()
  if (!profileBytes.equals(canonicalProfileBytes)) {
    throw new Error('Current sample topology profile is not the canonical PR profile')
  }
  const encodedProfile = profileBytes.toString('utf8')
  const encodedResolution = resolutionBytes.toString('utf8')
  const profile = parseTestIceTopologyJson(encodedProfile)
  const actualProfileSha256 = await testIceTopologySha256(profile)
  signal?.throwIfAborted()
  if (actualProfileSha256 !== expectedProfileSha256) {
    throw new Error('Current sample topology profile differs from its locked digest')
  }
  const resolution = parseTestIceTopologyResolutionJson(
    encodedResolution,
    profile,
    expectedProfileSha256,
  )
  const actualResolutionSha256 = await testIceTopologyResolutionSha256(
    resolution,
    profile,
    expectedProfileSha256,
  )
  signal?.throwIfAborted()
  if (actualResolutionSha256 !== expectedResolutionSha256) {
    throw new Error('Current sample topology resolution differs from its locked digest')
  }
  return verifyTestIceTopologyLock(
    profile,
    resolution,
    expectedProfileSha256,
    expectedResolutionSha256,
  )
}

function frozenTopology(
  profilePath: string,
  resolutionPath: string,
  lock: VerifiedTestIceTopologyLock,
): AcquiredTestIceTopology {
  return Object.freeze({
    profilePath,
    resolutionPath,
    lock,
    rtcConfiguration: Object.freeze(browserRtcConfiguration(lock.profile)),
    release: async () => undefined,
  })
}

function parseMaterializationRecord(encoded: string): TopologyMaterializationRecord {
  let value: unknown
  try {
    value = JSON.parse(encoded) as unknown
  } catch (cause) {
    throw new Error('Topology materializer did not emit one JSON record', { cause })
  }
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('Topology materializer record has an invalid shape')
  }
  const record = value as Partial<TopologyMaterializationRecord>
  if (
    Object.keys(record).sort().join(',') !== [
      'component',
      'outcome',
      'profilePath',
      'resolutionPath',
      'topologyProfileSha256',
      'topologyResolutionSha256',
    ].sort().join(',') ||
    record.component !== 'browser-evidence-topology-resolution' ||
    record.outcome !== 'materialized' ||
    typeof record.profilePath !== 'string' ||
    typeof record.resolutionPath !== 'string' ||
    typeof record.topologyProfileSha256 !== 'string' ||
    !SHA256_PATTERN.test(record.topologyProfileSha256) ||
    typeof record.topologyResolutionSha256 !== 'string' ||
    !SHA256_PATTERN.test(record.topologyResolutionSha256)
  ) {
    throw new Error('Topology materializer record has an invalid shape')
  }
  return Object.freeze(record as TopologyMaterializationRecord)
}

function samePath(left: string, right: string): boolean {
  return process.platform === 'win32'
    ? resolve(left).toLowerCase() === resolve(right).toLowerCase()
    : resolve(left) === resolve(right)
}

function requireAbsolutePath(path: string, label: string): void {
  if (resolve(path) !== path) throw new Error(`${label} path must be absolute and canonical`)
}

function requireSha256(digest: string, label: string): void {
  if (!SHA256_PATTERN.test(digest)) throw new Error(`${label} digest must be lowercase SHA-256`)
}
