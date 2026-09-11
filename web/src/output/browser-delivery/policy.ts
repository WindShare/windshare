import {
  FILE_CHECKPOINT_ID_BYTES,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE,
  OPERATION_ID_BYTES,
} from '../persistence/checkpoint'
import { durableCheckpointNamespaceIdentity } from '../persistence/namespace'
import { snapshotIdentity } from '../workspace/canonical'
import { deliveryDigest, namespaceFields } from './codec'
import { BROWSER_SAVE_POLICY_VERSION, type BrowserSavePolicyV1 } from './model'

// Only deeply immutable snapshots made here can reuse their canonical validation.
const canonicalPolicies = new WeakSet<BrowserSavePolicyV1>()

export function createBrowserSavePolicy(
  input: Omit<BrowserSavePolicyV1, 'schemaVersion' | 'digest'>,
): BrowserSavePolicyV1 {
  const operationId = snapshotIdentity(input.operationId, OPERATION_ID_BYTES, 'operation ID')
  const receiveIntentDigest = snapshotIdentity(input.receiveIntentDigest, FILE_CHECKPOINT_ID_BYTES, 'receive intent digest')
  if (input.preference !== 'automatic' && input.preference !== 'direct') {
    throw new TypeError('Unknown browser recovery preference')
  }
  const target = durableCheckpointNamespaceIdentity(input.target)
  const staging = input.staging === undefined ? undefined : durableCheckpointNamespaceIdentity(input.staging)
  if (target.operationId !== operationId || target.receiveIntentDigest !== receiveIntentDigest ||
      target.materializerKind !== FILE_CHECKPOINT_MATERIALIZER_FSA_TREE) {
    throw new TypeError('Browser save target must bind the original FSA receive intent')
  }
  if (staging !== undefined && (staging.operationId === operationId ||
      staging.materializerKind !== FILE_CHECKPOINT_MATERIALIZER_ORIGIN_PRIVATE ||
      staging.authorityRef === target.authorityRef)) {
    throw new TypeError('Browser staging requires distinct owned OPFS namespace authority')
  }
  if (input.preference === 'direct' && staging !== undefined) {
    throw new TypeError('Explicit direct preference cannot acquire staging authority')
  }
  const value = {
    schemaVersion: BROWSER_SAVE_POLICY_VERSION,
    operationId,
    receiveIntentDigest,
    preference: input.preference,
    target,
    ...(staging === undefined ? {} : { staging }),
  } as const
  const policy = Object.freeze({
    ...value,
    digest: deliveryDigest('windshare/browser-save-policy/v1', [
      value.schemaVersion, operationId, receiveIntentDigest, value.preference,
      namespaceFields(target), staging === undefined ? null : namespaceFields(staging),
    ]),
  })
  canonicalPolicies.add(policy)
  return policy
}

export function validateBrowserSavePolicy(input: BrowserSavePolicyV1): BrowserSavePolicyV1 {
  if (canonicalPolicies.has(input)) return input
  if (input.schemaVersion !== BROWSER_SAVE_POLICY_VERSION) throw new TypeError('Unsupported browser save policy')
  const value = createBrowserSavePolicy(input)
  if (input.digest !== value.digest) throw new TypeError('Browser save policy digest mismatch')
  return value
}
