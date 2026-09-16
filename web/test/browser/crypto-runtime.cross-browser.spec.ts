import { readFileSync } from 'node:fs'
import { createCipheriv, createHash, generateKeyPairSync, sign } from 'node:crypto'
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
      const verifierPath = '/src/crypto/ed25519.ts'
      const verifiers = await import(verifierPath) as typeof import('../../src/crypto/ed25519')
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
      const sender = verifiers.createEd25519Verifier(publicKey)
      const validSignature = await sender.verify(
        new Uint8Array(),
        signature,
      )
      signature[0] = signature[0]! ^ 1
      const mutatedSignature = await sender.verify(
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

test('opens committed blocks even when native Ed25519 rejects valid signatures', async ({ page }) => {
  // Node builds the complete object independently so a shared browser encoder
  // cannot hide omitted identity, nonce, ciphertext, or tag commitments.
  const { publicKey, privateKey } = generateKeyPairSync('ed25519')
  const publicKeyJwk = publicKey.export({ format: 'jwk' })
  if (publicKeyJwk.x === undefined) throw new Error('Ed25519 public key has no raw coordinate')
  const domain = Buffer.from('windshare/v2 object/block-record\0')
  const context = Buffer.concat([
    Buffer.alloc(16, 1), Buffer.alloc(16, 2), Buffer.alloc(16, 3), Buffer.alloc(12),
  ])
  context.writeBigUInt64BE(9n, 48)
  context.writeUInt32BE(BLOCK_SIGNATURE_MESSAGE_BYTES, 56)
  const contextHash = createHash('sha256').update(context).digest()
  const header = Buffer.alloc(8)
  header[0] = 3
  header.writeUInt32BE(BLOCK_SIGNATURE_MESSAGE_BYTES + 16, 4)
  const key = Buffer.alloc(32, 0x41)
  const nonce = Buffer.alloc(12, 0x61)
  const cipher = createCipheriv('aes-256-gcm', key, nonce)
  cipher.setAAD(Buffer.concat([domain, contextHash, header]))
  const message = Buffer.alloc(BLOCK_SIGNATURE_MESSAGE_BYTES, BLOCK_SIGNATURE_MESSAGE_FILL)
  const prefix = Buffer.concat([header, nonce, cipher.update(message), cipher.final(), cipher.getAuthTag()])
  const preimage = Buffer.concat([domain, contextHash, createHash('sha256').update(prefix).digest()])
  const object = Buffer.concat([prefix, sign(null, preimage, privateKey)])

  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const result = await page.evaluate(async ({ publicKey, key, encodedObject, messageBytes, fill }) => {
    const objectPath = '/src/crypto/sender-object.ts'
    const objects = await import(objectPath) as typeof import('../../src/crypto/sender-object')
    const verifierPath = '/src/crypto/ed25519.ts'
    const verifiers = await import(verifierPath) as typeof import('../../src/crypto/ed25519')
    const publicBytes = Uint8Array.from(publicKey)
    const content = Uint8Array.from(atob(encodedObject), character => character.charCodeAt(0))
    const share = new Uint8Array(16).fill(1)
    const file = new Uint8Array(16).fill(2)
    const revision = new Uint8Array(16).fill(3)
    const binding = objects.createBlockRecordObjectBinding(share, file, revision, 9n, messageBytes)
    let nativeEd25519Calls = 0
    const runtime = { subtle: new Proxy(crypto.subtle, {
      get(target, property) {
        if (property === 'verify') return async () => { nativeEd25519Calls += 1; return false }
        const value = Reflect.get(target, property) as unknown
        return typeof value === 'function' ? value.bind(target) : value
      },
    }) }
    const sender = verifiers.createEd25519Verifier(publicBytes, { runtime })
    {
      const opened = await objects.openSenderObject(binding, Uint8Array.from(key), sender, content)
      const signatureInput = await objects.senderObjectSignaturePreimage(binding, content.subarray(0, -64))
      const changedContent = content.slice()
      changedContent[changedContent.length - 65] = changedContent[changedContent.length - 65]! ^ 1
      const changedSignature = content.slice()
      changedSignature[changedSignature.length - 1] = changedSignature[changedSignature.length - 1]! ^ 1
      const wrongIdentity = objects.createBlockRecordObjectBinding(share, file, revision, 10n, messageBytes)
      const rejectionKinds: string[] = []
      for (const [bound, encoded] of [
        [binding, changedContent], [binding, changedSignature], [wrongIdentity, content],
      ] as const) {
        try {
          await objects.verifySenderObject(bound, sender, encoded)
          rejectionKinds.push('accepted')
        } catch (cause) {
          if (!(cause instanceof objects.SenderObjectError)) throw cause
          rejectionKinds.push(cause.kind)
        }
      }
      return {
        valid: opened.byteLength === messageBytes && opened.every(byte => byte === fill),
        signatureInput: [...signatureInput],
        rejectionKinds,
        nativeEd25519Calls,
      }
    }
  }, {
    publicKey: [...Buffer.from(publicKeyJwk.x, 'base64url')],
    key: [...key],
    encodedObject: object.toString('base64'),
    messageBytes: BLOCK_SIGNATURE_MESSAGE_BYTES,
    fill: BLOCK_SIGNATURE_MESSAGE_FILL,
  })
  expect(result.nativeEd25519Calls).toBeLessThanOrEqual(1)
  expect(result).toEqual({
    valid: true,
    signatureInput: [...preimage],
    rejectionKinds: ['signature', 'signature', 'signature'],
    nativeEd25519Calls: expect.any(Number),
  })
})

test('native and portable backends enforce the same frozen sender acceptance rules', async ({ page }) => {
  const fixture = JSON.parse(readFileSync(
    new URL('../../../core/testvectors/ed25519-acceptance.json', import.meta.url), 'utf8',
  )) as { cases: Array<{ name: string; publicKeyHex: string; messageHex: string; signatureHex: string; accepted: boolean }> }
  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const results = await page.evaluate(async cases => {
    const modulePath = '/src/crypto/ed25519.ts'
    const { createEd25519Verifier } = await import(modulePath) as typeof import('../../src/crypto/ed25519')
    const portable = { subtle: new Proxy(crypto.subtle, {
      get(target, property) {
        if (property === 'importKey') return async () => { throw new DOMException('', 'NotSupportedError') }
        const value = Reflect.get(target, property) as unknown
        return typeof value === 'function' ? value.bind(target) : value
      },
    }) }
    const fromHex = (hex: string) => Uint8Array.from(hex.match(/../g) ?? [], byte => Number.parseInt(byte, 16))
    const results = []
    for (const row of cases) {
      for (const runtime of [{ subtle: crypto.subtle }, portable]) {
        let accepted = false
        try {
          accepted = await createEd25519Verifier(fromHex(row.publicKeyHex), { runtime })
            .verify(fromHex(row.messageHex), fromHex(row.signatureHex))
        } catch { /* Malformed identities fail before a backend can authenticate them. */ }
        results.push({ name: row.name, accepted })
      }
    }
    return results
  }, fixture.cases)
  expect(results).toEqual(fixture.cases.flatMap(row => [
    { name: row.name, accepted: row.accepted }, { name: row.name, accepted: row.accepted },
  ]))
})
