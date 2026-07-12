import { expect, test } from '@playwright/test'

import { b64ToBytes, loadVectorFile } from '../vectors'

type C1CryptoModule = typeof import('../../src/crypto/index')
type C1ManifestModule = typeof import('../../src/manifest/index')

interface BrowserManifestVector {
  readonly readSecretB64: string
  readonly sealedManifestB64: string
  readonly manifest: {
    readonly entries: readonly { readonly path: string }[]
  }
}

interface BrowserLinkVector {
  readonly shareId: string
  readonly keyString: string
}

interface BrowserChunkVector {
  readonly streamKeyB64: string
  readonly chunkSize: number
  readonly index: string
  readonly plaintextB64: string
  readonly blockCTB64: string
}

interface BrowserTransferPlanVector {
  readonly selectors: readonly string[] | null
  readonly selectedPaths: readonly string[]
  readonly planId: string
}

const manifestVector = loadVectorFile(
  new URL('../../../testvectors/manifest-seal.json', import.meta.url),
).cases[0] as unknown as BrowserManifestVector

const linkVector = loadVectorFile(
  new URL('../../../testvectors/link.json', import.meta.url),
).cases[0] as unknown as BrowserLinkVector

const chunkVector = loadVectorFile(
  new URL('../../../testvectors/chunk-seal.json', import.meta.url),
).cases[0] as unknown as BrowserChunkVector

const transferPlanVector = loadVectorFile(
  new URL('../../../testvectors/transfer-plan.json', import.meta.url),
).cases[0] as unknown as BrowserTransferPlanVector

test('derives the Go manifest key through real browser WebCrypto', async ({ page }) => {
  await page.goto('/')

  const result = await page.evaluate(async () => {
    const modulePath = '/src/crypto/index.ts'
    const c1 = (await import(modulePath)) as C1CryptoModule
    const readSecret = Uint8Array.from({ length: 16 }, (_, index) => index)
    const manifestKey = await c1.deriveManifestKey(readSecret)
    return {
      available: globalThis.crypto?.subtle !== undefined,
      keyHex: c1.bytesToHex(manifestKey),
    }
  })

  expect(result.available).toBe(true)
  expect(result.keyHex).toBe(
    '4a04eafaa40ee9e993dc254e3ddb1cd1e75e167b9645c671a55f8b728f5f842c',
  )
})

test('authenticates, decodes, and plans the Go manifest in Chromium', async ({ page }) => {
  await page.goto('/')

  const result = await page.evaluate(async ({ manifest, plan }) => {
    const modulePath = '/src/manifest/index.ts'
    const c1 = (await import(modulePath)) as C1ManifestModule
    const decodeBase64 = (value: string): Uint8Array => {
      const binary = atob(value)
      return Uint8Array.from(binary, (character) => character.charCodeAt(0))
    }
    const opened = await c1.openSealedManifest(
      1,
      decodeBase64(manifest.readSecretB64),
      decodeBase64(manifest.sealedManifestB64),
    )
    const transferPlan = await c1.createTransferPlan(opened.manifest, plan.selectors)
    const planId = Array.from(transferPlan.planId, (byte) =>
      byte.toString(16).padStart(2, '0'),
    ).join('')
    return {
      paths: opened.manifest.entries.map((entry) => entry.path),
      selectedPaths: transferPlan.selectedEntries.map((entry) => entry.path),
      planId,
    }
  }, { manifest: manifestVector, plan: transferPlanVector })

  expect(result.paths).toEqual(manifestVector.manifest.entries.map((entry) => entry.path))
  expect(result.selectedPaths).toEqual(transferPlanVector.selectedPaths)
  expect(result.planId).toBe(transferPlanVector.planId)
})

test('opens the Go sealed chunk through real browser WebCrypto', async ({ page }) => {
  await page.goto('/')

  const plaintext = await page.evaluate(async (vector) => {
    const modulePath = '/src/crypto/index.ts'
    const c1 = (await import(modulePath)) as C1CryptoModule
    const decodeBase64 = (value: string): Uint8Array => {
      const binary = atob(value)
      return Uint8Array.from(binary, (character) => character.charCodeAt(0))
    }
    const opener = c1.createChunkOpenerFromStreamKey(
      1,
      decodeBase64(vector.streamKeyB64),
      vector.chunkSize,
    )
    return Array.from(await opener.open(Number(vector.index), decodeBase64(vector.blockCTB64)))
  }, chunkVector)

  expect(plaintext).toEqual(Array.from(b64ToBytes(chunkVector.plaintextB64)))
})

test('keeps WHATWG URL repair outside capability-link semantics', async ({ page }) => {
  await page.goto('/')

  const result = await page.evaluate(async (vector) => {
    const modulePath = '/src/crypto/index.ts'
    const c1 = (await import(modulePath)) as C1CryptoModule
    const escapedSeparator = c1.parseCapabilityLink(
      `https://windshare.example/prefix%2F${vector.shareId}#${vector.keyString}`,
    )
    const rejected = [
      `https:windshare.example/${vector.shareId}#${vector.keyString}`,
      `https:////windshare.example/${vector.shareId}#${vector.keyString}`,
      `https://windshare.example\\${vector.shareId}#${vector.keyString}`,
      `https://windshare.example/ignored\n/${vector.shareId}#${vector.keyString}`,
      `https://windshare.example/%zz/${vector.shareId}#${vector.keyString}`,
    ].map((value) => {
      try {
        c1.parseCapabilityLink(value)
        return { code: 'accepted', message: '' }
      } catch (error) {
        return {
          code: error instanceof c1.CryptoError ? error.code : 'wrong-error-type',
          message: error instanceof Error ? error.message : String(error),
        }
      }
    })
    return { escapedShareId: escapedSeparator.shareId, rejected }
  }, linkVector)

  expect(result.escapedShareId).toBe(linkVector.shareId)
  expect(result.rejected.map((error) => error.code)).toEqual([
    'malformed-link',
    'malformed-link',
    'malformed-link',
    'malformed-link',
    'malformed-link',
  ])
  for (const error of result.rejected) {
    expect(error.message).not.toContain(linkVector.keyString)
  }
})
