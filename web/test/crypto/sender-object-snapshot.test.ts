import { createCipheriv, createHash, generateKeyPairSync, sign } from 'node:crypto'
import { describe, expect, it, vi } from 'vitest'
import { createEd25519Verifier } from '../../src/crypto/ed25519'
import {
  createBlockRecordObjectBinding, createDescriptorObjectBinding,
  openDescriptorObjectBootstrap, openSenderObject, type SenderObjectBinding,
} from '../../src/crypto/sender-object'
import { suite02SenderKeyHash } from '../../src/crypto/suite02-link'
import type { CryptoRuntime } from '../../src/crypto/webcrypto'

const keys = generateKeyPairSync('ed25519')
const publicKey = new Uint8Array(keys.publicKey.export({ format: 'der', type: 'spki' }).subarray(-32))
const content = new Uint8Array(1024).fill(0x5a)
const key = new Uint8Array(32).fill(0x42)
const binding = createBlockRecordObjectBinding(
  new Uint8Array(16).fill(1), new Uint8Array(16).fill(2), new Uint8Array(16).fill(3), 0n, content.length,
)

describe('sender object operation snapshots', () => {
  it('decrypts precisely the bytes verified even if caller buffers change during verification', async () => {
    const object = seal(binding)
    const callerKey = key.slice()
    const realSender = createEd25519Verifier(publicKey)
    let release!: () => void
    const gate = new Promise<void>(resolve => { release = resolve })
    const verify = vi.fn(async (message: Uint8Array, signature: Uint8Array) => {
      await gate
      return realSender.verify(message, signature)
    })
    const digest = vi.fn(crypto.subtle.digest.bind(crypto.subtle))
    const pending = openSenderObject(binding, callerKey, { publicKey, verify }, object, withDigest(digest))
    await vi.waitFor(() => expect(verify).toHaveBeenCalledOnce())
    object.fill(0)
    callerKey.fill(0)
    release()
    await expect(pending).resolves.toEqual(content)
    expect(digest).toHaveBeenCalledTimes(2)
    await expect(openSenderObject(binding, key, realSender, object)).rejects.toMatchObject({ kind: 'malformed' })
  })

  it('keeps descriptor decryption and bootstrap authentication on the same snapshot', async () => {
    const pkHash = await suite02SenderKeyHash(publicKey)
    const shareId = createHash('sha256').update(Buffer.concat([
      Buffer.from('windshare/v2 share-id\0'), pkHash,
    ])).digest().subarray(0, 12)
    const descriptorBinding = await createDescriptorObjectBinding(pkHash, shareId)
    const object = seal(descriptorBinding)
    const result = await openDescriptorObjectBootstrap(descriptorBinding, key, object, plaintext => {
      expect(plaintext).toEqual(content)
      object.fill(0)
      return createEd25519Verifier(publicKey)
    })
    expect(result).toEqual(content)
  })

  it('does not decrypt or release plaintext when signature verification fails', async () => {
    const decrypt = vi.fn(crypto.subtle.decrypt.bind(crypto.subtle))
    const runtime = { subtle: new Proxy(crypto.subtle, {
      get(target, property) {
        if (property === 'decrypt') return decrypt
        const value = Reflect.get(target, property) as unknown
        return typeof value === 'function' ? value.bind(target) : value
      },
    }) }
    await expect(openSenderObject(binding, key, {
      publicKey, verify: async () => false,
    }, seal(binding), runtime)).rejects.toMatchObject({ kind: 'signature' })
    expect(decrypt).not.toHaveBeenCalled()
  })
})

function seal(bound: SenderObjectBinding): Uint8Array<ArrayBuffer> {
  const header = Buffer.alloc(8)
  header[0] = 3
  header.writeUInt32BE(content.length + 16, 4)
  const nonce = Buffer.alloc(12, 0x17)
  const domain = Buffer.from(bound.domain + '\0')
  const contextHash = createHash('sha256').update(bound.context).digest()
  const cipher = createCipheriv('aes-256-gcm', key, nonce)
  cipher.setAAD(Buffer.concat([domain, contextHash, header]))
  const prefix = Buffer.concat([header, nonce, cipher.update(content), cipher.final(), cipher.getAuthTag()])
  const preimage = Buffer.concat([domain, contextHash, createHash('sha256').update(prefix).digest()])
  return new Uint8Array(Buffer.concat([prefix, sign(null, preimage, keys.privateKey)]))
}

function withDigest(digest: SubtleCrypto['digest']): CryptoRuntime {
  return { subtle: new Proxy(crypto.subtle, {
    get(target, property) {
      if (property === 'digest') return digest
      const value = Reflect.get(target, property) as unknown
      return typeof value === 'function' ? value.bind(target) : value
    },
  }) }
}
