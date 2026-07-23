import {
  createDecipheriv,
  createHash,
  createHmac,
  createPrivateKey,
  createPublicKey,
  hkdfSync,
  verify,
} from 'node:crypto'

import { decode, encode, rfc8949EncodeOptions } from 'cborg'
import { expect } from 'vitest'

import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'

const textEncoder = new TextEncoder()
const ED25519_SPKI_PREFIX = Buffer.from('302a300506032b6570032100', 'hex')
const X25519_SPKI_PREFIX = Buffer.from('302a300506032b656e032100', 'hex')
const X25519_PKCS8_PREFIX = Buffer.from('302e020100300506032b656e04220420', 'hex')
const AES_GCM_TAG_BYTES = 16
const SENDER_OBJECT_HEADER_BYTES = 8
const SENDER_OBJECT_NONCE_BYTES = 12
const SENDER_OBJECT_SIGNATURE_BYTES = 64
const OPERATION_HEADER_BYTES = 16
const OPERATION_NONCE_BYTES = 12
const V2_RELAY_WEBSOCKET_PATH = '/v2/ws'
const OBJECT_DOMAIN_PREFIX = 'windshare/v2 object/'
const CONTROL_OPERATION_DOMAIN = 'windshare/v2 control/operation'
const CONTROL_TERMINAL_DOMAIN = 'windshare/v2 control/session-terminal'
const CONTROL_LANE_ATTACH_DOMAIN = 'windshare/v2 control/lane-attach'

interface IdentityVector extends VectorCase {
  readonly suite: number
  readonly readSecretB64: string
  readonly senderPublicKeyB64: string
  readonly pkHashB64: string
  readonly shareInstanceB64: string
  readonly shareIdRawB64: string
  readonly shareId: string
  readonly keyString: string
  readonly descriptorKeyB64: string
  readonly catalogKeyB64: string
  readonly fileIdB64: string
  readonly fileObjectKeyB64: string
  readonly fileRevisionB64: string
  readonly revisionKeyB64: string
  readonly segment: string
  readonly fileSegmentKeyB64: string
}

interface SenderObjectVector extends VectorCase {
  readonly domain: string
  readonly contextB64: string
  readonly contextHashB64: string
  readonly keyB64: string
  readonly nonceB64: string
  readonly canonicalCborB64: string
  readonly aadB64: string
  readonly signaturePreimageB64: string
  readonly objectB64: string
}

export interface SenderControlVector {
  readonly shareInstanceB64: string
  readonly protocolSessionIdB64: string
  readonly laneId: number
  readonly laneEpoch: number
  readonly sequence: string
  readonly trafficKeyB64: string
  readonly semanticBodyCborB64: string
  readonly unsignedControlCborB64: string
  readonly signedControlCborB64: string
  readonly controlPreimageB64: string
  readonly controlSignatureB64: string
  readonly plaintextB64: string
  readonly aadB64: string
  readonly envelopeB64: string
}

export interface OpenedControl {
  readonly plaintext: Uint8Array
  readonly messageKind: number
  readonly operationId: Uint8Array | null
  readonly body: Map<number, unknown>
}

export const identity = loadVectorFile(
  new URL('../../../core/testvectors/v2-identity.json', import.meta.url),
).cases[0] as IdentityVector
export const senderObjects = loadVectorFile(
  new URL('../../../core/testvectors/v2-sender-objects.json', import.meta.url),
).cases as SenderObjectVector[]

export function bytes(value: string): Uint8Array {
  return b64ToBytes(value)
}

export function namedCase<T extends VectorCase>(cases: readonly VectorCase[], name: string): T {
  const found = cases.find((value) => value.name === name)
  if (found === undefined) {
    throw new Error(`missing vector case ${name}`)
  }
  return found as T
}

export function utf8(value: string): Uint8Array {
  return textEncoder.encode(value)
}

export function concat(...parts: readonly Uint8Array[]): Uint8Array {
  return Uint8Array.from(Buffer.concat(parts.map((part) => Buffer.from(part))))
}

export function sha256(value: Uint8Array): Uint8Array {
  return Uint8Array.from(createHash('sha256').update(value).digest())
}

export function hmacSha256(key: Uint8Array, value: Uint8Array): Uint8Array {
  return Uint8Array.from(createHmac('sha256', key).update(value).digest())
}

export function u32(value: number): Uint8Array {
  const encoded = Buffer.alloc(4)
  encoded.writeUInt32BE(value)
  return encoded
}

export function u64(value: bigint): Uint8Array {
  const encoded = Buffer.alloc(8)
  encoded.writeBigUInt64BE(value)
  return encoded
}

export function derive(secret: Uint8Array, label: string, context: Uint8Array): Uint8Array {
  const info = concat(utf8(label), Uint8Array.of(0), context)
  return Uint8Array.from(Buffer.from(hkdfSync('sha256', secret, new Uint8Array(), info, 32)))
}

export function ed25519Public(raw: Uint8Array) {
  return createPublicKey({
    key: Buffer.concat([ED25519_SPKI_PREFIX, Buffer.from(raw)]),
    format: 'der',
    type: 'spki',
  })
}

export function x25519Public(raw: Uint8Array) {
  return createPublicKey({
    key: Buffer.concat([X25519_SPKI_PREFIX, Buffer.from(raw)]),
    format: 'der',
    type: 'spki',
  })
}

export function x25519Private(raw: Uint8Array) {
  return createPrivateKey({
    key: Buffer.concat([X25519_PKCS8_PREFIX, Buffer.from(raw)]),
    format: 'der',
    type: 'pkcs8',
  })
}

function opensGCM(
  key: Uint8Array,
  nonce: Uint8Array,
  ciphertextAndTag: Uint8Array,
  aad: Uint8Array,
): Uint8Array {
  const tagOffset = ciphertextAndTag.byteLength - AES_GCM_TAG_BYTES
  const decipher = createDecipheriv('aes-256-gcm', key, nonce)
  decipher.setAAD(aad)
  decipher.setAuthTag(ciphertextAndTag.subarray(tagOffset))
  return Uint8Array.from(
    Buffer.concat([
      decipher.update(ciphertextAndTag.subarray(0, tagOffset)),
      decipher.final(),
    ]),
  )
}

export function expectCanonicalCBOR(encoded: Uint8Array): void {
  const decoded = decode(encoded, { useMaps: true })
  expect(Uint8Array.from(encode(decoded, rfc8949EncodeOptions))).toEqual(encoded)
}

export function offlineMerkleRoot(objectDigests: readonly Uint8Array[]): Uint8Array {
  let nodes = objectDigests
    .map((digest) => digest.slice())
    .sort((left, right) => Buffer.compare(left, right))
    .map((digest) => sha256(concat(Uint8Array.of(0), digest)))
  while (nodes.length > 1) {
    const next: Uint8Array[] = []
    for (let index = 0; index < nodes.length; index += 2) {
      const left = nodes[index]!
      const right = nodes[index + 1] ?? left
      next.push(sha256(concat(Uint8Array.of(1), left, right)))
    }
    nodes = next
  }
  return nodes[0]!
}

function requireMap(value: unknown, label: string): Map<number, unknown> {
  if (!(value instanceof Map)) throw new TypeError(`${label} must be a CBOR map`)
  return value as Map<number, unknown>
}

function requireBytes(value: unknown, label: string, length?: number): Uint8Array {
  if (!(value instanceof Uint8Array)) throw new TypeError(`${label} must be bytes`)
  if (length !== undefined && value.byteLength !== length) {
    throw new TypeError(`${label} must be ${length} bytes`)
  }
  return value
}

function requireNumber(value: unknown, label: string): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) {
    throw new TypeError(`${label} must be a non-negative safe integer`)
  }
  return value
}

interface ContextAxis {
  readonly name: string
  readonly offset: number
}

interface SenderObjectBinding {
  readonly context: Uint8Array
  readonly key: Uint8Array
  readonly axes: readonly ContextAxis[]
}

function bindContext(parts: readonly (readonly [string, Uint8Array])[]): {
  readonly context: Uint8Array
  readonly axes: readonly ContextAxis[]
} {
  let offset = 0
  const axes: ContextAxis[] = []
  for (const [name, value] of parts) {
    axes.push({ name, offset })
    offset += value.byteLength
  }
  return { context: concat(...parts.map(([, value]) => value)), axes }
}

function descriptorSemantics(): Map<number, unknown> {
  return requireMap(
    decode(bytes(senderObjects[0]!.canonicalCborB64), { useMaps: true }),
    'descriptor',
  )
}

function senderIdentityFromDescriptor(): {
  readonly pkHash: Uint8Array
  readonly shareIdRaw: Uint8Array
} {
  const descriptor = descriptorSemantics()
  const senderPublicKey = requireBytes(descriptor.get(7), 'descriptor sender key', 32)
  const pkHash = sha256(concat(utf8('windshare/v2 sender-key\0'), senderPublicKey)).subarray(0, 16)
  const shareIdRaw = sha256(concat(utf8('windshare/v2 share-id\0'), pkHash)).subarray(0, 12)
  return { pkHash, shareIdRaw }
}

function senderObjectBinding(
  domain: string,
  semanticBody: Map<number, unknown>,
): SenderObjectBinding {
  const readSecret = bytes(identity.readSecretB64)
  const { pkHash, shareIdRaw } = senderIdentityFromDescriptor()
  if (domain === `${OBJECT_DOMAIN_PREFIX}descriptor`) {
    const suite = requireNumber(semanticBody.get(2), 'descriptor suite')
    const bound = bindContext([
      ['suite', Uint8Array.of(suite)],
      ['pk-hash', pkHash],
      ['share-id', shareIdRaw],
    ])
    return { ...bound, key: derive(readSecret, 'windshare/v2 descriptor', pkHash) }
  }
  const shareInstance = requireBytes(semanticBody.get(1), `${domain} ShareInstance`, 16)
  if (domain === `${OBJECT_DOMAIN_PREFIX}catalog-page`) {
    const directoryId = requireBytes(semanticBody.get(2), 'catalog DirectoryID', 16)
    const pageIndex = requireNumber(semanticBody.get(4), 'catalog page index')
    const bound = bindContext([
      ['share-instance', shareInstance],
      ['directory-id', directoryId],
      ['page-index', u32(pageIndex)],
    ])
    return { ...bound, key: derive(readSecret, 'windshare/v2 catalog', shareInstance) }
  }
  if (domain === `${OBJECT_DOMAIN_PREFIX}directory-error`) {
    const directoryId = requireBytes(semanticBody.get(2), 'directory error DirectoryID', 16)
    const bound = bindContext([
      ['share-instance', shareInstance],
      ['directory-id', directoryId],
    ])
    return { ...bound, key: derive(readSecret, 'windshare/v2 catalog', shareInstance) }
  }
  if (domain === `${OBJECT_DOMAIN_PREFIX}file-revision`) {
    const fileId = requireBytes(semanticBody.get(2), 'revision FileID', 16)
    const bound = bindContext([
      ['share-instance', shareInstance],
      ['file-id', fileId],
    ])
    return {
      ...bound,
      key: derive(readSecret, 'windshare/v2 file-object', concat(shareInstance, fileId)),
    }
  }
  if (domain === `${OBJECT_DOMAIN_PREFIX}block-record`) {
    const fileId = requireBytes(semanticBody.get(2), 'block FileID', 16)
    const fileRevision = requireBytes(semanticBody.get(3), 'block FileRevision', 16)
    const localBlockIndex = BigInt(requireNumber(semanticBody.get(4), 'local block index'))
    const data = requireBytes(semanticBody.get(5), 'block data')
    const bound = bindContext([
      ['share-instance', shareInstance],
      ['file-id', fileId],
      ['file-revision', fileRevision],
      ['local-block-index', u64(localBlockIndex)],
      ['data-length', u32(data.byteLength)],
    ])
    const fileObjectKey = derive(
      readSecret,
      'windshare/v2 file-object',
      concat(shareInstance, fileId),
    )
    const revisionKey = derive(fileObjectKey, 'windshare/v2 file-revision', fileRevision)
    const chunkSize = BigInt(requireNumber(descriptorSemantics().get(5), 'descriptor chunk size'))
    const segment = (localBlockIndex * chunkSize) / (16n << 30n)
    return {
      ...bound,
      key: derive(revisionKey, 'windshare/v2 file-segment', u64(segment)),
    }
  }
  if (domain === `${OBJECT_DOMAIN_PREFIX}offline-commit`) {
    const bound = bindContext([['share-instance', shareInstance]])
    return { ...bound, key: derive(readSecret, 'windshare/v2 descriptor', pkHash) }
  }
  throw new TypeError(`unknown sender object domain ${domain}`)
}

function flipped(value: Uint8Array): Uint8Array {
  const result = value.slice()
  result[0] = result[0]! ^ 1
  return result
}

export function expectSenderObject(
  vector: SenderObjectVector,
  senderPublicKey: ReturnType<typeof ed25519Public>,
): void {
  const object = bytes(vector.objectB64)
  const semanticBytes = bytes(vector.canonicalCborB64)
  const semanticBody = requireMap(decode(semanticBytes, { useMaps: true }), vector.domain)
  const binding = senderObjectBinding(vector.domain, semanticBody)
  const contextHash = sha256(binding.context)
  const cipherLength = Buffer.from(object).readUInt32BE(4)
  const cipherEnd = SENDER_OBJECT_HEADER_BYTES + SENDER_OBJECT_NONCE_BYTES + cipherLength
  const prefix = object.subarray(0, cipherEnd)
  const nonce = object.subarray(
    SENDER_OBJECT_HEADER_BYTES,
    SENDER_OBJECT_HEADER_BYTES + SENDER_OBJECT_NONCE_BYTES,
  )
  const ciphertext = object.subarray(SENDER_OBJECT_HEADER_BYTES + SENDER_OBJECT_NONCE_BYTES, cipherEnd)
  const signature = object.subarray(cipherEnd)
  const aad = concat(utf8(vector.domain), Uint8Array.of(0), contextHash, object.subarray(0, 8))
  const preimage = concat(utf8(vector.domain), Uint8Array.of(0), contextHash, prefix)

  expect(object[0]).toBe(2)
  expect(object.subarray(1, 4)).toEqual(Uint8Array.of(0, 0, 0))
  expect(object.byteLength).toBe(cipherEnd + SENDER_OBJECT_SIGNATURE_BYTES)
  expect(binding.context).toEqual(bytes(vector.contextB64))
  expect(contextHash).toEqual(bytes(vector.contextHashB64))
  expect(binding.key).toEqual(bytes(vector.keyB64))
  expect(nonce).toEqual(bytes(vector.nonceB64))
  expect(aad).toEqual(bytes(vector.aadB64))
  expect(preimage).toEqual(bytes(vector.signaturePreimageB64))
  expect(verify(null, preimage, senderPublicKey, signature)).toBe(true)

  const plaintext = opensGCM(binding.key, nonce, ciphertext, aad)
  expect(plaintext).toEqual(semanticBytes)
  expectCanonicalCBOR(plaintext)

  for (const axis of binding.axes) {
    const hostileContext = binding.context.slice()
    hostileContext[axis.offset] = hostileContext[axis.offset]! ^ 1
    const hostileHash = sha256(hostileContext)
    const hostileAAD = concat(
      utf8(vector.domain),
      Uint8Array.of(0),
      hostileHash,
      object.subarray(0, 8),
    )
    expect(() => opensGCM(binding.key, nonce, ciphertext, hostileAAD), axis.name).toThrow()
    const hostilePreimage = concat(
      utf8(vector.domain),
      Uint8Array.of(0),
      hostileHash,
      prefix,
    )
    expect(verify(null, hostilePreimage, senderPublicKey, signature), axis.name).toBe(false)
  }
  expect(() => opensGCM(flipped(binding.key), nonce, ciphertext, aad)).toThrow()
  const hostileCiphertext = ciphertext.slice()
  hostileCiphertext[0] = hostileCiphertext[0]! ^ 1
  expect(() => opensGCM(binding.key, nonce, hostileCiphertext, aad)).toThrow()
  const hostilePrefix = concat(object.subarray(0, 20), hostileCiphertext)
  const hostilePreimage = concat(
    utf8(vector.domain),
    Uint8Array.of(0),
    contextHash,
    hostilePrefix,
  )
  expect(verify(null, hostilePreimage, senderPublicKey, signature)).toBe(false)
  const hostileSignature = signature.slice()
  hostileSignature[0] = hostileSignature[0]! ^ 1
  expect(verify(null, preimage, senderPublicKey, hostileSignature)).toBe(false)
}

interface OperationAxes {
  readonly wire: number
  readonly direction: number
  readonly shareInstance: Uint8Array
  readonly protocolSessionId: Uint8Array
  readonly laneId: number
  readonly laneEpoch: number
  readonly sequence: bigint
  readonly ciphertextLength: number
}

function operationAAD(axes: OperationAxes): Uint8Array {
  return concat(
    utf8('windshare/v2 operation-envelope\0'),
    Uint8Array.of(axes.wire, axes.direction),
    axes.shareInstance,
    axes.protocolSessionId,
    u32(axes.laneId),
    u32(axes.laneEpoch),
    u64(axes.sequence),
    u32(axes.ciphertextLength),
  )
}

function controlDomain(messageKind: number): string {
  if (messageKind === 11) return CONTROL_TERMINAL_DOMAIN
  if (messageKind === 12) return CONTROL_LANE_ATTACH_DOMAIN
  return CONTROL_OPERATION_DOMAIN
}

interface ControlAxes {
  readonly domain: string
  readonly shareInstance: Uint8Array
  readonly protocolSessionId: Uint8Array
  readonly laneId: number
  readonly laneEpoch: number
  readonly direction: number
  readonly sequence: bigint
  readonly messageKind: number
  readonly operationId: Uint8Array
  readonly unsignedBody: Uint8Array
}

function controlPreimage(axes: ControlAxes): Uint8Array {
  return concat(
    utf8(axes.domain),
    Uint8Array.of(0),
    axes.shareInstance,
    axes.protocolSessionId,
    u32(axes.laneId),
    u32(axes.laneEpoch),
    Uint8Array.of(axes.direction),
    u64(axes.sequence),
    Uint8Array.of(axes.messageKind),
    axes.operationId,
    sha256(axes.unsignedBody),
  )
}

export function expectSenderControl(
  vector: SenderControlVector,
  senderPublicKey: ReturnType<typeof ed25519Public>,
): OpenedControl {
  const envelope = bytes(vector.envelopeB64)
  const cipherLength = Buffer.from(envelope).readUInt32BE(12)
  const baseAxes: OperationAxes = {
    wire: 2,
    direction: 1,
    shareInstance: bytes(vector.shareInstanceB64),
    protocolSessionId: bytes(vector.protocolSessionIdB64),
    laneId: vector.laneId,
    laneEpoch: vector.laneEpoch,
    sequence: BigInt(vector.sequence),
    ciphertextLength: cipherLength,
  }
  const aad = operationAAD(baseAxes)
  const nonce = envelope.subarray(
    OPERATION_HEADER_BYTES,
    OPERATION_HEADER_BYTES + OPERATION_NONCE_BYTES,
  )
  const ciphertext = envelope.subarray(OPERATION_HEADER_BYTES + OPERATION_NONCE_BYTES)
  const trafficKey = bytes(vector.trafficKeyB64)
  expect(envelope.subarray(0, 2)).toEqual(Uint8Array.of(2, 1))
  expect(envelope.subarray(2, 4)).toEqual(Uint8Array.of(0, 0))
  expect(Buffer.from(envelope).readBigUInt64BE(4)).toBe(baseAxes.sequence)
  expect(envelope.byteLength).toBe(
    OPERATION_HEADER_BYTES + OPERATION_NONCE_BYTES + cipherLength,
  )
  expect(aad).toEqual(bytes(vector.aadB64))
  const plaintext = opensGCM(trafficKey, nonce, ciphertext, aad)
  expect(plaintext).toEqual(bytes(vector.plaintextB64))
  expectCanonicalCBOR(plaintext)

  const envelopeMutations: readonly (readonly [string, OperationAxes])[] = [
    ['wire', { ...baseAxes, wire: 3 }],
    ['direction', { ...baseAxes, direction: 0 }],
    ['share-instance', { ...baseAxes, shareInstance: flipped(baseAxes.shareInstance) }],
    ['protocol-session', { ...baseAxes, protocolSessionId: flipped(baseAxes.protocolSessionId) }],
    ['lane-id', { ...baseAxes, laneId: baseAxes.laneId + 1 }],
    ['lane-epoch', { ...baseAxes, laneEpoch: baseAxes.laneEpoch + 1 }],
    ['sequence', { ...baseAxes, sequence: baseAxes.sequence + 1n }],
    ['ciphertext-length', { ...baseAxes, ciphertextLength: baseAxes.ciphertextLength + 1 }],
  ]
  for (const [name, candidate] of envelopeMutations) {
    expect(() => opensGCM(trafficKey, nonce, ciphertext, operationAAD(candidate)), name).toThrow()
  }
  expect(() => opensGCM(flipped(trafficKey), nonce, ciphertext, aad), 'traffic-key').toThrow()

  const message = requireMap(decode(plaintext, { useMaps: true }), 'control message')
  const messageKind = requireNumber(message.get(0), 'message kind')
  const rawOperationId = message.get(1)
  const operationId = rawOperationId === null
    ? null
    : requireBytes(rawOperationId, 'operation ID', 16)
  const wrapper = requireMap(message.get(2), 'signed control wrapper')
  expect([...wrapper.keys()].sort((left, right) => Number(left) - Number(right))).toEqual([0, 1, 255])
  expect(requireNumber(wrapper.get(0), 'signed control wrapper schema')).toBe(1)
  expect(Uint8Array.from(encode(wrapper, rfc8949EncodeOptions))).toEqual(
    bytes(vector.signedControlCborB64),
  )
  expect(Uint8Array.from(encode(wrapper.get(1), rfc8949EncodeOptions))).toEqual(
    bytes(vector.semanticBodyCborB64),
  )
  const signature = requireBytes(wrapper.get(255), 'control signature', 64)
  const unsigned = new Map(wrapper)
  unsigned.delete(255)
  const unsignedBody = Uint8Array.from(encode(unsigned, rfc8949EncodeOptions))
  const baseControlAxes: ControlAxes = {
    domain: controlDomain(messageKind),
    shareInstance: baseAxes.shareInstance,
    protocolSessionId: baseAxes.protocolSessionId,
    laneId: baseAxes.laneId,
    laneEpoch: baseAxes.laneEpoch,
    direction: baseAxes.direction,
    sequence: baseAxes.sequence,
    messageKind,
    operationId: operationId ?? new Uint8Array(16),
    unsignedBody,
  }
  const preimage = controlPreimage(baseControlAxes)
  expect(unsignedBody).toEqual(bytes(vector.unsignedControlCborB64))
  expect(preimage).toEqual(bytes(vector.controlPreimageB64))
  expect(signature).toEqual(bytes(vector.controlSignatureB64))
  expect(verify(null, preimage, senderPublicKey, signature)).toBe(true)

  const alternateDomain = baseControlAxes.domain === CONTROL_OPERATION_DOMAIN
    ? CONTROL_TERMINAL_DOMAIN
    : CONTROL_OPERATION_DOMAIN
  const controlMutations: readonly (readonly [string, ControlAxes])[] = [
    ['domain', { ...baseControlAxes, domain: alternateDomain }],
    ['share-instance', { ...baseControlAxes, shareInstance: flipped(baseControlAxes.shareInstance) }],
    ['protocol-session', { ...baseControlAxes, protocolSessionId: flipped(baseControlAxes.protocolSessionId) }],
    ['lane-id', { ...baseControlAxes, laneId: baseControlAxes.laneId + 1 }],
    ['lane-epoch', { ...baseControlAxes, laneEpoch: baseControlAxes.laneEpoch + 1 }],
    ['direction', { ...baseControlAxes, direction: baseControlAxes.direction ^ 1 }],
    ['sequence', { ...baseControlAxes, sequence: baseControlAxes.sequence + 1n }],
    ['message-kind', { ...baseControlAxes, messageKind: baseControlAxes.messageKind + 1 }],
    ['operation-id', { ...baseControlAxes, operationId: flipped(baseControlAxes.operationId) }],
    ['body', { ...baseControlAxes, unsignedBody: flipped(baseControlAxes.unsignedBody) }],
  ]
  for (const [name, candidate] of controlMutations) {
    expect(verify(null, controlPreimage(candidate), senderPublicKey, signature), name).toBe(false)
  }
  return {
    plaintext,
    messageKind,
    operationId,
    body: requireMap(wrapper.get(1), 'sender control semantic body'),
  }
}

function rawRelayPathHasDotSegment(raw: string): boolean {
  const authorityStart = raw.indexOf('://') + 3
  const pathStart = raw.indexOf('/', authorityStart)
  if (pathStart < 0) return false
  const queryStart = raw.indexOf('?', pathStart)
  const path = raw.slice(pathStart, queryStart < 0 ? raw.length : queryStart)
  return path.split('/').some((segment) => {
    try {
      const decoded = decodeURIComponent(segment)
      return decoded === '.' || decoded === '..'
    } catch {
      return true
    }
  })
}

function validRelayQuery(raw: string): boolean {
  const queryStart = raw.indexOf('?')
  if (queryStart < 0) return true
  const query = raw.slice(queryStart + 1)
  for (let index = 0; index < query.length; index += 1) {
    const code = query.charCodeAt(index)
    if (code > 0x7f) return false
    const character = query[index]!
    if (character === '%') {
      if (index + 2 >= query.length || !/^[\dA-Fa-f]{2}$/u.test(query.slice(index + 1, index + 3))) {
        return false
      }
      index += 2
      continue
    }
    if (!/^[A-Za-z\d]$/u.test(character) && !'-._~!$&()*+,;=:@/?'.includes(character)) {
      return false
    }
  }
  return true
}

function isRelayBoundaryWhitespace(codePoint: number): boolean {
  return (codePoint >= 0x09 && codePoint <= 0x0d) || codePoint === 0x20 ||
    codePoint === 0x85 || codePoint === 0xa0 || codePoint === 0x1680 ||
    (codePoint >= 0x2000 && codePoint <= 0x200a) || codePoint === 0x2028 ||
    codePoint === 0x2029 || codePoint === 0x202f || codePoint === 0x205f ||
    codePoint === 0x3000 || codePoint === 0xfeff
}

function hasInvalidRelaySpelling(raw: string): boolean {
  const first = raw.codePointAt(0)
  const last = raw.codePointAt(raw.length - 1)
  if (first === undefined || last === undefined ||
      isRelayBoundaryWhitespace(first) || isRelayBoundaryWhitespace(last)) return true
  for (const character of raw) {
    const codePoint = character.codePointAt(0)!
    if (codePoint <= 0x1f || codePoint === 0x7f || character === '\\') return true
  }
  return false
}

function isASCIIText(value: string): boolean {
  for (const character of value) {
    if (character.codePointAt(0)! > 0x7f) return false
  }
  return true
}

export function canonicalV2RelayEndpoints(raw: string): {
  readonly dialEndpoint: string
  readonly relayIdentityEndpoint: string
} {
  if (raw === '' || hasInvalidRelaySpelling(raw) || raw.includes('#')) {
    throw new TypeError('invalid v2 relay base spelling')
  }
  const schemeSeparator = raw.indexOf('://')
  if (schemeSeparator <= 0) throw new TypeError('relay base must be absolute')
  const authorityEndMatch = /[/?#]/u.exec(raw.slice(schemeSeparator + 3))
  const authorityEnd = authorityEndMatch === null
    ? raw.length
    : schemeSeparator + 3 + authorityEndMatch.index
  if (raw.slice(schemeSeparator + 3, authorityEnd).includes('@')) {
    throw new TypeError('v2 relay base forbids userinfo')
  }
  if (rawRelayPathHasDotSegment(raw) || !validRelayQuery(raw)) {
    throw new TypeError('invalid v2 relay base path or query')
  }
  const endpoint = new URL(raw)
  if (endpoint.protocol === 'http:' || endpoint.protocol === 'ws:') endpoint.protocol = 'ws:'
  else if (endpoint.protocol === 'https:' || endpoint.protocol === 'wss:') endpoint.protocol = 'wss:'
  else throw new TypeError('unsupported v2 relay scheme')
  if (endpoint.hostname === '' || !isASCIIText(endpoint.hostname)) {
    throw new TypeError('invalid v2 relay host')
  }
  const basePath = endpoint.pathname.endsWith('/')
    ? endpoint.pathname.slice(0, -1)
    : endpoint.pathname
  endpoint.pathname = `${basePath}${V2_RELAY_WEBSOCKET_PATH}`
  const dialEndpoint = endpoint.toString()
  endpoint.search = ''
  const relayIdentityEndpoint = endpoint.toString()
  return { dialEndpoint, relayIdentityEndpoint }
}

export function validSingleFragment(
  candidate: Uint8Array,
  expectedOperationId: Uint8Array,
  expectedRecordId: Uint8Array,
  maxPayload: number,
  maxRecord: number,
): boolean {
  if (candidate.byteLength < 52) return false
  const header = Buffer.from(candidate.subarray(0, 52))
  const payload = candidate.subarray(52)
  if (
    header[0] !== 1 || header[1] !== 8 || header[2] !== 1 || header[3] !== 0 ||
    !header.subarray(4, 20).equals(Buffer.from(expectedOperationId)) ||
    !header.subarray(20, 36).equals(Buffer.from(expectedRecordId)) ||
    header.readUInt32BE(36) !== 0 || header.readUInt32BE(40) !== 1
  ) return false
  const totalLength = header.readUInt32BE(44)
  const payloadLength = header.readUInt32BE(48)
  return totalLength > 0 && totalLength <= maxRecord && payloadLength <= maxPayload &&
    totalLength === payloadLength && payloadLength === payload.byteLength &&
    Buffer.from(sha256(payload).subarray(0, 16)).equals(Buffer.from(expectedRecordId))
}

export function fragmentDuplicateConflicts(
  previous: Uint8Array,
  candidate: Uint8Array,
): boolean {
  if (previous.byteLength < 52 || candidate.byteLength < 52) return false
  const sameIdentityAndIndex = Buffer.from(previous.subarray(4, 40)).equals(
    Buffer.from(candidate.subarray(4, 40)),
  )
  return sameIdentityAndIndex && !Buffer.from(previous).equals(Buffer.from(candidate))
}
