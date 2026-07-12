import {
  ALL_SELECTION,
  MAX_SEALED_MANIFEST_BYTES,
  PLAN_ID_DOMAIN,
  createPathSelection,
  type ByteLength,
  type PathSelection,
  type PlanId,
  type Selection,
  type TransferPlan,
  type ValidatedManifestV1,
} from '../contracts'
import { sha256 } from '../crypto/digest'
import {
  type CryptoRuntime,
  defaultCryptoRuntime,
} from '../crypto/webcrypto'
import { PackedLayout } from './layout'
import { canonicalizePath, compareCanonicalPaths } from './path-policy'

const encoder = new TextEncoder()
const PLAN_ID_DOMAIN_BYTES = encoder.encode(PLAN_ID_DOMAIN)
const PLAN_ID_PATH_LENGTH_BYTES = 8

function utf8ByteLength(value: string): number {
  let bytes = 0
  for (let offset = 0; offset < value.length; offset += 1) {
    const scalar = value.codePointAt(offset) ?? 0
    if (scalar <= 0x7f) {
      bytes += 1
    } else if (scalar <= 0x7ff) {
      bytes += 2
    } else if (scalar <= 0xffff) {
      bytes += 3
    } else {
      bytes += 4
      offset += 1
    }
  }
  return bytes
}

function planIdPreimage(paths: readonly string[]): Uint8Array<ArrayBuffer> {
  const ordered = [...paths].sort(compareCanonicalPaths)
  let preimageBytes = PLAN_ID_DOMAIN_BYTES.byteLength
  for (const path of ordered) {
    preimageBytes += PLAN_ID_PATH_LENGTH_BYTES + utf8ByteLength(path)
    if (preimageBytes > MAX_SEALED_MANIFEST_BYTES) {
      throw new RangeError('Transfer-plan identity exceeds the authenticated manifest budget')
    }
  }

  const preimage = new Uint8Array(preimageBytes)
  const view = new DataView(preimage.buffer)
  preimage.set(PLAN_ID_DOMAIN_BYTES)
  let offset = PLAN_ID_DOMAIN_BYTES.byteLength
  for (const path of ordered) {
    const pathBytes = utf8ByteLength(path)
    view.setBigUint64(offset, BigInt(pathBytes), false)
    offset += PLAN_ID_PATH_LENGTH_BYTES
    const encoded = encoder.encodeInto(path, preimage.subarray(offset, offset + pathBytes))
    if (encoded.read !== path.length || encoded.written !== pathBytes) {
      throw new Error('Transfer-plan path encoding did not consume the validated path')
    }
    offset += pathBytes
  }
  return preimage
}

export function createSelection(selectors: readonly string[] | null): Selection {
  if (selectors === null) {
    return ALL_SELECTION
  }
  const canonicalPaths = selectors.map(canonicalizePath)
  return createPathSelection(canonicalPaths)
}

export function createSelectedPathSelection(selectors: readonly string[]): PathSelection {
  return createPathSelection(selectors.map(canonicalizePath))
}

async function computePlanId(
  paths: readonly string[],
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<PlanId> {
  // WebCrypto requires one digest input. Building it directly avoids retaining two
  // typed arrays per selected entry at the maximum manifest cardinality.
  return (await sha256(planIdPreimage(paths), runtime)) as PlanId
}

export async function compileTransferPlan(
  layout: PackedLayout,
  selection: Selection,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<TransferPlan> {
  const resolved = layout.resolve(selection)
  let selectedBytes = 0
  const paths: string[] = []
  for (const entry of resolved.selectedEntries) {
    paths.push(entry.path)
    if (entry.kind === 'file') {
      selectedBytes += entry.size
    }
  }
  const planId = await computePlanId(paths, runtime)
  return Object.freeze({
    planId,
    selectedEntries: resolved.selectedEntries,
    selectedBytes: selectedBytes as ByteLength,
    chunks: resolved.chunks,
  }) as TransferPlan
}

export async function createTransferPlan(
  manifest: ValidatedManifestV1,
  selectors: readonly string[] | null,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<TransferPlan> {
  return compileTransferPlan(new PackedLayout(manifest), createSelection(selectors), runtime)
}
