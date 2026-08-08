import type { V2ShareDescriptor } from '../../catalog/v2-records'
import type { V2FrozenSelectionPolicy } from '../../catalog/v2-selection'
import { encodeBase64Url } from '../../crypto/bytes'
import {
  createTransferIntentDraft,
  selectionRulesFromPolicy,
  validateFinalTransferIntent,
  validateTransferIntentDraft,
  type TransferIntent,
  type TransferIntentDraft,
} from '../intent'
import type { TransferJobOptions } from './contract'

export { createTransferJobId } from '../intent'

export function descriptorShareInstanceId(descriptor: V2ShareDescriptor): string {
  return encodeBase64Url(descriptor.shareInstance)
}

export function descriptorRootId(descriptor: V2ShareDescriptor): string {
  return encodeBase64Url(descriptor.syntheticRoot)
}

export function transferTimestampMilliseconds(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now()
}

export async function transferIntentAuthority(
  options: TransferJobOptions,
  selection: V2FrozenSelectionPolicy,
): Promise<{
  readonly expected: TransferIntentDraft
  readonly input: TransferIntent | TransferIntentDraft
}> {
  const expected = createTransferIntentDraft({
    shareInstance: descriptorShareInstanceId(options.descriptor),
    syntheticRoot: descriptorRootId(options.descriptor),
    selection: selectionRulesFromPolicy(selection),
  })
  let input: TransferIntent | TransferIntentDraft
  if (options.intent === undefined) {
    input = expected
  } else if ('state' in options.intent) {
    input = validateTransferIntentDraft(options.intent, expected)
  } else {
    input = await validateFinalTransferIntent(options.intent, expected)
  }
  return Object.freeze({ expected, input })
}
