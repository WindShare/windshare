import type { V2ShareDescriptor } from '../../catalog/v2-records'
import type { V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import { decodeBase64Url, encodeBase64Url } from '../../crypto/bytes'
import {
  STABLE_IDENTITY_BYTES,
  createSelectionSpec,
  createTransferJobID,
  selectionRulesSpecFromPolicy,
  validateReceiveIntent,
  type ReceiveIntent,
} from '../intent'

export function descriptorShareInstanceId(descriptor: V2ShareDescriptor): string {
  return encodeBase64Url(descriptor.shareInstance)
}

export function descriptorRootId(descriptor: V2ShareDescriptor): string {
  return encodeBase64Url(descriptor.syntheticRoot)
}

export function createTransferJobId(): string {
  return createTransferJobID()
}

export function snapshotTransferJobId(value: string): string {
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== STABLE_IDENTITY_BYTES ||
      decoded.every(byte => byte === 0) || encodeBase64Url(decoded) !== value) {
    throw new TypeError('transfer job ID is not a canonical non-zero identity')
  }
  return value
}

export async function validateTransferJobIntent(
  input: ReceiveIntent,
  descriptor: V2ShareDescriptor,
  selection: V2FrozenSelectionPolicy,
): Promise<ReceiveIntent> {
  const intent = await validateReceiveIntent(input)
  const shareInstance = descriptorShareInstanceId(descriptor)
  const syntheticRoot = descriptorRootId(descriptor)
  if (intent.shareInstance !== shareInstance || intent.syntheticRoot !== syntheticRoot) {
    throw new TypeError('receive intent belongs to a different share descriptor')
  }
  const expectedSelection = await createSelectionSpec({
    shareInstance,
    syntheticRoot,
    rules: selectionRulesSpecFromPolicy(selection),
  })
  if (intent.selection.digest !== expectedSelection.digest) {
    throw new TypeError('receive intent selection differs from the frozen transfer selection')
  }
  return intent
}
