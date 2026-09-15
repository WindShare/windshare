import { generateKeyPairSync, sign } from 'node:crypto'
import { expect, test } from '@playwright/test'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'

const BLOCK_SIGNATURE_MESSAGE_BYTES = 1024 * 1024
const BLOCK_SIGNATURE_MESSAGE_FILL = 0x5a

const RFC8032_EMPTY_MESSAGE_PUBLIC_KEY =
  'd75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a'
const RFC8032_EMPTY_MESSAGE_SIGNATURE =
  'e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e06522490155' +
  '5fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b'

test('production curve boundary works with the active browser capabilities', async ({ page }) => {
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const result = await page.evaluate(
    async ({ publicKeyHex, signatureHex }) => {
      const curvePath = '/src/crypto/curve25519.ts'
      const curves = await import(curvePath) as typeof import('../../src/crypto/curve25519')
      const left = await curves.createX25519KeyAgreement()
      const right = await curves.createX25519KeyAgreement()
      const leftPublic = left.publicKey
      const rightPublic = right.publicKey
      const leftSecret = await left.deriveSharedSecret(rightPublic)
      const rightSecret = await right.deriveSharedSecret(leftPublic)
      const sharedSecretMatches = leftSecret.every((byte, index) => byte === rightSecret[index])
      leftSecret.fill(0)
      rightSecret.fill(0)

      const publicKey = fromHex(publicKeyHex)
      const signature = fromHex(signatureHex)
      const validSignature = await curves.verifyEd25519Signature(
        publicKey,
        new Uint8Array(),
        signature,
      )
      signature[0] = signature[0]! ^ 1
      const mutatedSignature = await curves.verifyEd25519Signature(
        publicKey,
        new Uint8Array(),
        signature,
      )
      return { sharedSecretMatches, validSignature, mutatedSignature }

      function fromHex(encoded: string): Uint8Array {
        const bytes = new Uint8Array(encoded.length / 2)
        for (let index = 0; index < bytes.length; index += 1) {
          bytes[index] = Number.parseInt(encoded.slice(index * 2, index * 2 + 2), 16)
        }
        return bytes
      }
    },
    {
      publicKeyHex: RFC8032_EMPTY_MESSAGE_PUBLIC_KEY,
      signatureHex: RFC8032_EMPTY_MESSAGE_SIGNATURE,
    },
  )

  expect(result).toEqual({
    sharedSecretMatches: true,
    validSignature: true,
    mutatedSignature: false,
  })
})

test('authenticates production-size signed blocks and rejects changed content and signatures', async ({ page }) => {
  // Independent signing catches browser backends that accept Ed25519 but reject
  // valid block objects larger than their small capability-test messages.
  const { publicKey, privateKey } = generateKeyPairSync('ed25519')
  const publicKeyJwk = publicKey.export({ format: 'jwk' })
  if (publicKeyJwk.x === undefined) throw new Error('Ed25519 public key has no raw coordinate')
  const message = Buffer.alloc(BLOCK_SIGNATURE_MESSAGE_BYTES, BLOCK_SIGNATURE_MESSAGE_FILL)
  const signature = sign(null, message, privateKey)
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const result = await page.evaluate(async ({ publicKey, signature, messageBytes, fill }) => {
    const curvePath = '/src/crypto/curve25519.ts'
    const curves = await import(curvePath) as typeof import('../../src/crypto/curve25519')
    const key = Uint8Array.from(publicKey)
    const proof = Uint8Array.from(signature)
    const content = new Uint8Array(messageBytes).fill(fill)
    const valid = await curves.verifyEd25519Signature(key, content, proof)
    content[content.length - 1] = content[content.length - 1]! ^ 1
    const changedContent = await curves.verifyEd25519Signature(key, content, proof)
    content[content.length - 1] = content[content.length - 1]! ^ 1
    proof[0] = proof[0]! ^ 1
    const changedSignature = await curves.verifyEd25519Signature(key, content, proof)
    return { valid, changedContent, changedSignature }
  }, {
    publicKey: [...Buffer.from(publicKeyJwk.x, 'base64url')],
    signature: [...signature],
    messageBytes: BLOCK_SIGNATURE_MESSAGE_BYTES,
    fill: BLOCK_SIGNATURE_MESSAGE_FILL,
  })
  expect(result).toEqual({ valid: true, changedContent: false, changedSignature: false })
})
