import { encodeMessage, type MessageKind } from '../../src/session/message'

export async function senderControlKeyPair() {
  const pair = await globalThis.crypto.subtle.generateKey('Ed25519', true, ['sign', 'verify']) as CryptoKeyPair
  return Object.freeze({
    privateKey: pair.privateKey,
    publicKey: new Uint8Array(await globalThis.crypto.subtle.exportKey('raw', pair.publicKey)),
  })
}

export function senderOperationControl(options: {
  readonly kind: MessageKind
  readonly operationId: bigint
  readonly semanticBody: Uint8Array
}) {
  return { message: encodeMessage(options.kind, options.operationId, options.semanticBody) }
}
