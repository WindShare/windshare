import { readFileSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import { descriptorObjectVector } from '../protocol/delivery-vectors'
import { loadVectorFile } from '../vectors'
import { BROWSER_CONTRACT_HOST_PATH } from './contract-host'

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

test('authenticates the bootstrap descriptor even when native Ed25519 rejects valid signatures', async ({ page }) => {
  const identity = loadVectorFile(new URL('../../../core/testvectors/v2-identity.json', import.meta.url)).cases[0]
  const keyString = identity?.keyString
  const senderPublicKeyB64 = identity?.senderPublicKeyB64
  if (typeof keyString !== 'string' || typeof senderPublicKeyB64 !== 'string') {
    throw new Error('missing descriptor capability vector')
  }

  await page.goto(BROWSER_CONTRACT_HOST_PATH)
  const result = await page.evaluate(async ({ encodedKey, encodedDescriptor }) => {
    const linkPath = '/src/crypto/suite02-link.ts'
    const links = await import(linkPath) as typeof import('../../src/crypto/suite02-link')
    const recordsPath = '/src/catalog/records.ts'
    const records = await import(recordsPath) as typeof import('../../src/catalog/records')
    const objectPath = '/src/crypto/sender-object.ts'
    const objects = await import(objectPath) as typeof import('../../src/crypto/sender-object')
    const capability = await links.decodeSuite02CapabilityKey(encodedKey)
    const descriptor = Uint8Array.from(atob(encodedDescriptor), character => character.charCodeAt(0))
    let nativeEd25519Calls = 0
    const runtime = { subtle: new Proxy(crypto.subtle, {
      get(target, property) {
        if (property === 'verify') return async () => { nativeEd25519Calls += 1; return false }
        const value = Reflect.get(target, property) as unknown
        return typeof value === 'function' ? value.bind(target) : value
      },
    }) }
    const opened = await records.openShareDescriptor(descriptor, capability, { runtime })
    const validDescriptorNativeCalls = nativeEd25519Calls

    const changedContent = descriptor.slice()
    changedContent[changedContent.length - 65] = changedContent[changedContent.length - 65]! ^ 1
    const changedSignature = descriptor.slice()
    changedSignature[changedSignature.length - 1] = changedSignature[changedSignature.length - 1]! ^ 1
    const wrongRoute = { ...capability, shareIdRaw: capability.shareIdRaw.slice() }
    wrongRoute.shareIdRaw[0] = wrongRoute.shareIdRaw[0]! ^ 1
    const rejectionKinds: string[] = []
    for (const [input, route] of [
      [changedContent, capability], [changedSignature, capability], [descriptor, wrongRoute],
    ] as const) {
      try {
        await records.openShareDescriptor(input, route, { runtime })
        rejectionKinds.push('accepted')
      } catch (cause) {
        if (!(cause instanceof objects.SenderObjectError)) throw cause
        rejectionKinds.push(cause.kind)
      }
    }
    return {
      publicKey: btoa(String.fromCharCode(...opened.sender.publicKey)),
      rejectionKinds,
      validDescriptorNativeCalls,
    }
  }, { encodedKey: keyString, encodedDescriptor: descriptorObjectVector.objectB64 })

  expect(result.publicKey).toBe(senderPublicKeyB64)
  expect(result.rejectionKinds).toEqual(['authentication', 'signature', 'binding'])
  expect(result.validDescriptorNativeCalls).toBeLessThanOrEqual(1)
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
