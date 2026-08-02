import { fileURLToPath } from 'node:url'
import { isAbsolute, join, resolve } from 'node:path'

import { readStableRegularFileSnapshot } from './filesystem/snapshot.ts'
import { startBrowserSample } from './sample-runner.ts'
import { parseBrowserRunPolicy } from './run-policy.ts'
import { parseCanonicalJsonText } from './contract/strict-json.ts'
import {
  parseTestIceTopologyJson,
  parseTestIceTopologyResolutionJson,
  TEST_ICE_TOPOLOGY_MAXIMUM_FILE_BYTES,
  verifyTestIceTopologyLock,
} from './test-ice-topology.ts'
import { BROWSER_ENGINES, BROWSER_SUITES } from './vocabulary.ts'

export const BROWSER_SAMPLE_DRIVER_SCHEMA_VERSION =
  'windshare.browser-sample-driver/v4' as const
const MAXIMUM_DRIVER_REQUEST_BYTES = 1_048_576
const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const CHECKOUT_SHA_PATTERN = /^[a-f0-9]{40}$/u
const PORTABLE_TOKEN_PATTERN = /^[A-Za-z0-9._-]{1,256}$/u

export async function runBrowserSampleDriver(encoded: Uint8Array): Promise<unknown> {
  if (encoded.byteLength === 0 || encoded.byteLength > MAXIMUM_DRIVER_REQUEST_BYTES) {
    throw new Error('browser sample driver request is empty or exceeds its byte limit')
  }
  const request = parseDriverRequest(parseCanonicalJsonText(
    decodeUtf8(encoded, 'browser sample driver request'),
    'browser sample driver request',
  ))
  const [profileSnapshot, resolutionSnapshot] = await Promise.all([
    readStableRegularFileSnapshot(
      request.topology.profilePath,
      TEST_ICE_TOPOLOGY_MAXIMUM_FILE_BYTES,
      'browser sample driver topology profile',
    ),
    readStableRegularFileSnapshot(
      request.topology.resolutionPath,
      TEST_ICE_TOPOLOGY_MAXIMUM_FILE_BYTES,
      'browser sample driver topology resolution',
    ),
  ])
  if (
    profileSnapshot.sha256 !== request.topology.profileSha256 ||
    resolutionSnapshot.sha256 !== request.topology.resolutionSha256
  ) throw new Error('browser sample driver topology bytes differ from their authority')
  const profile = parseTestIceTopologyJson(decodeUtf8(profileSnapshot.bytes, 'topology profile'))
  const resolution = parseTestIceTopologyResolutionJson(
    decodeUtf8(resolutionSnapshot.bytes, 'topology resolution'),
    profile,
    profileSnapshot.sha256,
  )
  const topologyLock = await verifyTestIceTopologyLock(
    profile,
    resolution,
    profileSnapshot.sha256,
    resolutionSnapshot.sha256,
  )
  const execution = startBrowserSample({
    ...request.identity,
    sampleDirectory: request.output.sampleDirectory,
    topologyLock,
    topologyProfilePath: request.topology.profilePath,
    topologyResolutionPath: request.topology.resolutionPath,
    command: Object.freeze({
      executable: request.leaf.executable,
      arguments: request.leaf.arguments,
      cwd: request.leaf.cwd,
      environment: request.leaf.environment,
    }),
    processDeadlineMs: request.ownership.childDeadlineMs,
    ownershipMode: 'inherited',
    outerProcessAuthority: request.ownership.outerAuthority,
  })
  const outcome = await execution.result
  const traces = execution.traces.snapshot()
  if (
    !traces.completed ||
    traces.truncated ||
    traces.observedEvents !== traces.capturedEvents
  ) throw new Error('browser sample driver received incomplete lifecycle trace evidence')
  return Object.freeze({
    schemaVersion: BROWSER_SAMPLE_DRIVER_SCHEMA_VERSION,
    runId: request.identity.runId,
    operationId: request.identity.operationId,
    scenario: request.identity.scenario,
    outcome: outcome.acceptedBeforeGuard ? 'succeeded' : 'failed',
    resultPath: outcome.resultPath,
    artifactRoot: outcome.artifactRoot,
    candidate: outcome.result,
    acceptedBeforeGuard: outcome.acceptedBeforeGuard,
    traces,
  })
}

function parseDriverRequest(value: unknown) {
  const record = requireRecord(value, 'browser sample driver request')
  exactKeys(record, [
    'schemaVersion',
    'identity',
    'output',
    'topology',
    'ownership',
    'leaf',
  ], 'browser sample driver request')
  if (record.schemaVersion !== BROWSER_SAMPLE_DRIVER_SCHEMA_VERSION) {
    throw new Error('browser sample driver schema is unsupported')
  }
  const identity = parseIdentity(record.identity)
  const output = parseOutput(record.output, identity)
  return Object.freeze({
    schemaVersion: BROWSER_SAMPLE_DRIVER_SCHEMA_VERSION,
    identity,
    output,
    topology: parseTopology(record.topology),
    ownership: parseOwnership(record.ownership, identity),
    leaf: parseLeaf(record.leaf),
  })
}

function parseIdentity(value: unknown) {
  const record = requireRecord(value, 'browser sample driver identity')
  exactKeys(record, [
    'runId', 'operationId', 'scenario', 'runPolicy', 'suite', 'browser', 'sampleIndex', 'checkoutSha',
  ], 'browser sample driver identity')
  if (typeof record.runId !== 'string' || !PORTABLE_TOKEN_PATTERN.test(record.runId)) {
    throw new Error('browser sample driver run ID is invalid')
  }
  if (typeof record.operationId !== 'string' || !PORTABLE_TOKEN_PATTERN.test(record.operationId)) {
    throw new Error('browser sample driver operation ID is invalid')
  }
  if (typeof record.scenario !== 'string' || !PORTABLE_TOKEN_PATTERN.test(record.scenario)) {
    throw new Error('browser sample driver scenario is invalid')
  }
  const runPolicy = parseBrowserRunPolicy(record.runPolicy, 'browser sample driver run policy')
  if (!BROWSER_SUITES.includes(record.suite as never)) {
    throw new Error('browser sample driver suite is invalid')
  }
  if (!BROWSER_ENGINES.includes(record.browser as never)) {
    throw new Error('browser sample driver browser is invalid')
  }
  if (
    !Number.isSafeInteger(record.sampleIndex) || (record.sampleIndex as number) < 1 ||
    (record.sampleIndex as number) > runPolicy.sampleCount
  ) throw new Error('browser sample driver index exceeds its run policy')
  if (typeof record.checkoutSha !== 'string' || !CHECKOUT_SHA_PATTERN.test(record.checkoutSha)) {
    throw new Error('browser sample driver checkout SHA is invalid')
  }
  return Object.freeze({
    runId: record.runId,
    operationId: record.operationId,
    scenario: record.scenario,
    runPolicy,
    suite: record.suite as (typeof BROWSER_SUITES)[number],
    browser: record.browser as (typeof BROWSER_ENGINES)[number],
    sampleIndex: record.sampleIndex as number,
    checkoutSha: record.checkoutSha,
  })
}

function parseOutput(value: unknown, identity: ReturnType<typeof parseIdentity>) {
  const record = requireRecord(value, 'browser sample driver output')
  exactKeys(record, ['root', 'sampleDirectory', 'resultPath'], 'browser sample driver output')
  const root = canonicalAbsolutePath(record.root, 'browser sample driver output root')
  const sampleDirectory = canonicalAbsolutePath(
    record.sampleDirectory,
    'browser sample driver sample directory',
  )
  const expected = join(root, identity.suite, identity.browser, `sample-${identity.sampleIndex}`)
  if (sampleDirectory !== expected || record.resultPath !== join(expected, 'result.json')) {
    throw new Error('browser sample driver output slot differs from its identity')
  }
  return Object.freeze({ root, sampleDirectory, resultPath: record.resultPath as string })
}

function parseTopology(value: unknown) {
  const record = requireRecord(value, 'browser sample driver topology')
  exactKeys(record, [
    'profilePath', 'profileSha256', 'resolutionPath', 'resolutionSha256',
  ], 'browser sample driver topology')
  return Object.freeze({
    profilePath: canonicalAbsolutePath(record.profilePath, 'browser sample driver topology profile'),
    profileSha256: sha256(record.profileSha256, 'browser sample driver topology profile'),
    resolutionPath: canonicalAbsolutePath(
      record.resolutionPath,
      'browser sample driver topology resolution',
    ),
    resolutionSha256: sha256(
      record.resolutionSha256,
      'browser sample driver topology resolution',
    ),
  })
}

function parseOwnership(value: unknown, identity: ReturnType<typeof parseIdentity>) {
  const record = requireRecord(value, 'browser sample driver ownership')
  exactKeys(record, ['outerAuthority', 'childDeadlineMs'], 'browser sample driver ownership')
  const outer = requireRecord(record.outerAuthority, 'browser sample driver outer authority')
  exactKeys(outer, ['kind', 'backend', 'operationId'], 'browser sample driver outer authority')
  if (
    outer.kind !== 'test-process-owner' ||
    !['windows_job', 'linux_subreaper'].includes(outer.backend as string) ||
    typeof outer.operationId !== 'string' || !PORTABLE_TOKEN_PATTERN.test(outer.operationId) ||
    outer.operationId !== identity.operationId
  ) throw new Error('browser sample driver outer process authority is invalid')
  if (!Number.isSafeInteger(record.childDeadlineMs) || (record.childDeadlineMs as number) < 1) {
    throw new Error('browser sample driver child deadline is invalid')
  }
  return Object.freeze({
    outerAuthority: Object.freeze({
      kind: 'test-process-owner' as const,
      backend: outer.backend as 'windows_job' | 'linux_subreaper',
      operationId: outer.operationId,
    }),
    childDeadlineMs: record.childDeadlineMs as number,
  })
}

function parseLeaf(value: unknown) {
  const record = requireRecord(value, 'browser sample driver leaf')
  exactKeys(record, ['executable', 'arguments', 'cwd', 'environment'], 'browser sample driver leaf')
  if (!Array.isArray(record.arguments) || record.arguments.some((entry) =>
    typeof entry !== 'string' || entry.includes('\0'))) {
    throw new Error('browser sample driver leaf arguments are invalid')
  }
  const environmentRecord = requireRecord(record.environment, 'browser sample driver leaf environment')
  const environment: Record<string, string> = {}
  for (const name of Object.keys(environmentRecord).sort()) {
    const entry = environmentRecord[name]
    if (!/^[A-Za-z_]\w*$/u.test(name) || typeof entry !== 'string' || entry.includes('\0')) {
      throw new Error('browser sample driver leaf environment is invalid')
    }
    environment[name] = entry
  }
  return Object.freeze({
    executable: canonicalAbsolutePath(record.executable, 'browser sample driver leaf executable'),
    arguments: Object.freeze([...record.arguments] as string[]),
    cwd: canonicalAbsolutePath(record.cwd, 'browser sample driver leaf working directory'),
    environment: Object.freeze(environment),
  })
}

function canonicalAbsolutePath(value: unknown, label: string): string {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(`${label} must be absolute and canonical`)
  }
  return value
}

function sha256(value: unknown, label: string): string {
  if (typeof value !== 'string' || !SHA256_PATTERN.test(value)) {
    throw new Error(`${label} digest must be lowercase 64-hex`)
  }
  return value
}

function requireRecord(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function exactKeys(record: Record<string, unknown>, keys: readonly string[], label: string): void {
  const actual = Object.keys(record)
  if (actual.length !== keys.length || keys.some((key) => !Object.hasOwn(record, key))) {
    throw new Error(`${label} does not have exact keys`)
  }
}

function decodeUtf8(bytes: Uint8Array, label: string): string {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch (cause) {
    throw new Error(`${label} is not valid UTF-8`, { cause })
  }
}

async function readStandardInput(): Promise<Uint8Array> {
  const chunks: Buffer[] = []
  let byteLength = 0
  for await (const chunk of process.stdin) {
    const bytes = Buffer.from(chunk)
    byteLength += bytes.byteLength
    if (byteLength > MAXIMUM_DRIVER_REQUEST_BYTES) {
      throw new Error('browser sample driver request exceeds its byte limit')
    }
    chunks.push(bytes)
  }
  return Buffer.concat(chunks)
}

const invokedPath = process.argv[1]
if (invokedPath !== undefined && resolve(invokedPath) === fileURLToPath(import.meta.url)) {
  if (process.argv.length !== 2) {
    process.stderr.write('{"component":"browser-sample-driver","outcome":"failed","error":"arguments are forbidden"}\n')
    process.exitCode = 1
  } else {
    readStandardInput().then(runBrowserSampleDriver).then(
      (record) => {
        process.stdout.write(`${JSON.stringify(record)}\n`)
        process.exitCode = (record as { acceptedBeforeGuard: boolean }).acceptedBeforeGuard ? 0 : 1
      },
      (cause: unknown) => {
        process.stderr.write(`${JSON.stringify({
          component: 'browser-sample-driver',
          outcome: 'failed',
          error: cause instanceof Error ? cause.message : String(cause),
        })}\n`)
        process.exitCode = 1
      },
    )
  }
}
