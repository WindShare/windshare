import { encodeBase64Url } from '../../crypto/bytes'
import { sha256 } from '../../crypto/digest'
import {
  decodeSuite02CapabilityKey,
  parseSuite02CapabilityLink,
  type Suite02CapabilityLink,
} from '../../crypto/suite02-link'
import { receiverRelayBases } from '../../receiver/relay-race'

const TEXT_ENCODER = new TextEncoder()

export async function capabilityFromInput(input: string, pageUrl: string): Promise<Suite02CapabilityLink> {
  const trimmed = input.trim()
  if (trimmed.includes('://')) return parseSuite02CapabilityLink(trimmed)
  const capability = await decodeSuite02CapabilityKey(trimmed)
  try {
    return Object.freeze({
      ...capability,
      relayHints: Object.freeze(new URL(pageUrl).searchParams.getAll('r')),
    })
  } catch (error) {
    capability.readSecret.fill(0)
    throw error
  }
}

/** Remember equality, not another retained copy of the capability's read secret. */
export async function capabilityInputFingerprint(input: string, pageUrl: string): Promise<string | undefined> {
  let capability: Suite02CapabilityLink | undefined
  let encoded: Uint8Array<ArrayBuffer> | undefined
  try {
    capability = await capabilityFromInput(input, pageUrl)
    const relays = receiverRelayBases(capability.relayHints.length > 0
      ? capability.relayHints : [new URL(pageUrl).origin])
    encoded = TEXT_ENCODER.encode(JSON.stringify([
      capability.suite, encodeBase64Url(capability.readSecret), encodeBase64Url(capability.pkHash), relays,
    ]))
    return encodeBase64Url(await sha256(encoded))
  } catch {
    // Invalid input still goes through the gateway's normal diagnostic boundary.
    return undefined
  } finally {
    capability?.readSecret.fill(0)
    encoded?.fill(0)
  }
}
