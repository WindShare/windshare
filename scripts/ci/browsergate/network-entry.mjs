import { closeSync, constants as fsConstants, fstatSync, readSync } from 'node:fs'
import { resolve } from 'node:path'
import { pathToFileURL } from 'node:url'
import { types as nodeTypes } from 'node:util'

const LOCAL_NETWORK_ENTRY_SCHEMA = 'windshare.browser-network-matrix.local-entry/v1'
const MINTED_OIDC_PROTOCOL = 'windshare.browser-network-matrix.minted-oidc/v1'
const MINTED_OIDC_DESCRIPTOR = 3
const RUNTIME_CONFIG_DESCRIPTOR = 4
const MAXIMUM_MINTED_OIDC_BYTES = 131_072
const MAXIMUM_RUNTIME_CONFIG_BYTES = 1_048_576
const OIDC_ASSERTION_PATTERN = /^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/u
const OIDC_AUDIENCE_PATTERN = /^[A-Za-z0-9._:/-]{8,512}$/u

export function browserNetworkEntryFailureRecord() {
  return Object.freeze({
    schemaVersion: LOCAL_NETWORK_ENTRY_SCHEMA,
    component: 'browser-network-entry',
    outcome: 'failed',
    blocking: true,
    failureCode: 'scheduled-network-execution-failed',
  })
}

export async function runBrowserNetworkEntry(
  arguments_,
  composition = {},
) {
  if (!Array.isArray(arguments_) || arguments_.some((argument) => typeof argument !== 'string')) {
    throw new Error('browser network entry arguments must be strings')
  }
  if (arguments_.length === 0) {
    throw new Error('full browser network authority requires an explicit scheduled execution')
  }
  if (arguments_[0] !== 'execute') {
    throw new Error('full browser network authority accepts only scheduled execution')
  }
  // The matrix CLI requires explicit manifest, publisher, and common process-owner
  // artifacts. Forwarding exact operands preserves that authority;
  // this gate never infers a helper or silently provisions a topology.
  const runMatrixCli = composition.runMatrixCli ?? defaultMatrixCli
  return runMatrixCli(Object.freeze([...arguments_]))
}

async function defaultMatrixCli(arguments_, mintedOidc, runtimeConfigBytes) {
  const [{ browserNetworkMatrixCli }, { GitHubActionsOidcBootstrapLease }] = await Promise.all([
    import('../../../web/scripts/browser-network-matrix/cli/main.ts'),
    import('../../../web/scripts/browser-network-matrix/linux-topology/parent-workload-identity.ts'),
  ])
  const workloadIdentityBootstrap = GitHubActionsOidcBootstrapLease.fromMintedEnvelope(mintedOidc)
  let result
  let primaryFailure
  try {
    result = await browserNetworkMatrixCli(arguments_, {
      workloadIdentityBootstrap,
      productionRuntimeConfigBytes: runtimeConfigBytes,
    })
  } catch (cause) {
    primaryFailure = cause
  }
  try {
    await workloadIdentityBootstrap.forceTerminateAndWait()
  } catch (cleanupFailure) {
    if (primaryFailure !== undefined) {
      throw new AggregateError(
        [primaryFailure, cleanupFailure],
        'browser network matrix execution and minted OIDC cleanup both failed',
        { cause: primaryFailure },
      )
    }
    throw cleanupFailure
  }
  if (primaryFailure !== undefined) throw primaryFailure
  return result
}

export function readRetainedRuntimeConfig(descriptor = RUNTIME_CONFIG_DESCRIPTOR) {
  const chunks = []
  let length = 0
  let descriptorOwned = false
  try {
    const metadata = fstatSync(descriptor)
    descriptorOwned = true
    if (!metadata.isFile() || metadata.size <= 0 || metadata.size > MAXIMUM_RUNTIME_CONFIG_BYTES) {
      throw new Error('runtime config descriptor is not a bounded retained file')
    }
    while (true) {
      const chunk = Buffer.alloc(4096)
      const count = readSync(descriptor, chunk, 0, chunk.byteLength, null)
      if (count === 0) {
        chunk.fill(0)
        break
      }
      length += count
      if (length > MAXIMUM_RUNTIME_CONFIG_BYTES) {
        chunk.fill(0)
        throw new Error('runtime config descriptor exceeded its capacity')
      }
      chunks.push(count === chunk.byteLength ? chunk : chunk.subarray(0, count))
    }
    if (length === 0) throw new Error('runtime config descriptor is empty')
    return Buffer.concat(chunks, length)
  } finally {
    for (const chunk of chunks) chunk.fill(0)
    if (descriptorOwned) closeSync(descriptor)
  }
}

export function readMintedOidcEnvelope(descriptor = MINTED_OIDC_DESCRIPTOR) {
  const chunks = []
  let length = 0
  let descriptorOwned = false
  try {
    const metadata = fstatSync(descriptor)
    descriptorOwned = true
    if (!isAnonymousDescriptorMetadata(metadata)) {
      throw new Error('minted OIDC authority must arrive through an anonymous descriptor')
    }
    while (true) {
      const chunk = Buffer.alloc(4096)
      const count = readSync(descriptor, chunk, 0, chunk.byteLength, null)
      if (count === 0) {
        chunk.fill(0)
        break
      }
      length += count
      if (length > MAXIMUM_MINTED_OIDC_BYTES) {
        chunk.fill(0)
        throw new Error('minted OIDC authority exceeded its descriptor capacity')
      }
      chunks.push(count === chunk.byteLength ? chunk : chunk.subarray(0, count))
    }
    const encoded = Buffer.concat(chunks, length)
    let value
    try {
      value = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(encoded))
    } finally {
      encoded.fill(0)
    }
    if (
      typeof value !== 'object' || value === null || nodeTypes.isProxy(value) || Array.isArray(value) ||
      Object.getPrototypeOf(value) !== Object.prototype
    ) throw new Error('minted OIDC descriptor envelope is invalid')
    const keys = Object.keys(value)
    const expected = [
      'protocolVersion', 'audience', 'requestOrigin', 'requestPath', 'requestQuery', 'assertion',
    ]
    if (keys.length !== expected.length || !expected.every((key) => keys.includes(key))) {
      throw new Error('minted OIDC descriptor envelope fields are invalid')
    }
    if (
      value.protocolVersion !== MINTED_OIDC_PROTOCOL ||
      typeof value.audience !== 'string' || !OIDC_AUDIENCE_PATTERN.test(value.audience) ||
      typeof value.requestOrigin !== 'string' || typeof value.requestPath !== 'string' ||
      typeof value.requestQuery !== 'string' || typeof value.assertion !== 'string' ||
      !OIDC_ASSERTION_PATTERN.test(value.assertion)
    ) throw new Error('minted OIDC descriptor envelope values are invalid')
    const assertion = Buffer.from(value.assertion, 'ascii')
    return Object.freeze({
      protocolVersion: MINTED_OIDC_PROTOCOL,
      audience: value.audience,
      requestOrigin: value.requestOrigin,
      requestPath: value.requestPath,
      requestQuery: value.requestQuery,
      assertion,
    })
  } finally {
    for (const chunk of chunks) chunk.fill(0)
    if (descriptorOwned) closeSync(descriptor)
  }
}

export function isAnonymousDescriptorMetadata(metadata) {
  const descriptorKind = metadata.mode & fsConstants.S_IFMT
  // Node's Windows Stats predicates report false for anonymous pipes even
  // though the stable file-type bits identify S_IFIFO. Inspecting both forms
  // keeps the authority transport-neutral without admitting regular files.
  return metadata.isFIFO() || metadata.isSocket() ||
    descriptorKind === fsConstants.S_IFIFO || descriptorKind === fsConstants.S_IFSOCK
}

/* c8 ignore start -- exercised by the process contract. */
async function invokedMatrixCli(arguments_) {
  const mintedOidc = readMintedOidcEnvelope()
  let runtimeConfigBytes
  try {
    // Both one-use descriptors are consumed and closed before any repository
    // module is dynamically imported or any descendant can exist.
    runtimeConfigBytes = readRetainedRuntimeConfig()
    return await runBrowserNetworkEntry(arguments_, {
      runMatrixCli: (ownedArguments) => defaultMatrixCli(
        ownedArguments,
        mintedOidc,
        runtimeConfigBytes,
      ),
    })
  } finally {
    mintedOidc.assertion.fill(0)
    runtimeConfigBytes?.fill(0)
  }
}
/* c8 ignore stop */

const invokedPath = process.argv[1]
if (invokedPath !== undefined && pathToFileURL(resolve(invokedPath)).href === import.meta.url) {
  try {
    process.exitCode = await invokedMatrixCli(process.argv.slice(2))
  } catch {
    // Dependency causes are opaque so hostile message/toString hooks cannot
    // suppress the one blocking terminal record.
    process.stderr.write(`${JSON.stringify(browserNetworkEntryFailureRecord())}\n`)
    process.exitCode = 1
  }
}
