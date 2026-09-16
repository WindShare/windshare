import { NATIVE_ED25519_PROBES } from './ed25519-native-vectors'

export const ED25519_NATIVE_DEADLINE_MS = 2_000
export type NativeEd25519Qualification = 'qualified' | 'unsupported' | 'qualification-failed' | 'timeout'

// Weak ownership shares the capability check across senders without retaining
// public keys or letting one injected runtime select another runtime's backend.
const qualifications = new WeakMap<SubtleCrypto, Promise<NativeEd25519Qualification>>()

export function qualifyNativeEd25519(subtle: SubtleCrypto): Promise<NativeEd25519Qualification> {
  let pending = qualifications.get(subtle)
  if (pending === undefined) {
    pending = qualify(subtle)
    qualifications.set(subtle, pending)
  }
  return pending
}

async function qualify(subtle: SubtleCrypto): Promise<NativeEd25519Qualification> {
  try {
    return await withEd25519Deadline((async () => {
      const key = await subtle.importKey(
        'raw', fromHex(NATIVE_ED25519_PROBES[0].publicKeyHex), 'Ed25519', false, ['verify'],
      )
      for (const probe of NATIVE_ED25519_PROBES) {
        const accepted = await subtle.verify(
          'Ed25519', key, fromHex(probe.signatureHex), fromHex(probe.messageHex),
        )
        if (accepted !== probe.accepted) return 'qualification-failed'
      }
      return 'qualified'
    })())
  } catch (cause) {
    const reason = nativeEd25519Failure(cause)
    if (reason === 'unsupported' || reason === 'timeout') return reason
    return 'qualification-failed'
  }
}

export async function withEd25519Deadline<T>(operation: Promise<T>): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined
  try {
    return await Promise.race([
      operation,
      new Promise<never>((_, reject) => {
        timer = setTimeout(
          () => reject(new DOMException('Native Ed25519 did not settle', 'TimeoutError')),
          ED25519_NATIVE_DEADLINE_MS,
        )
      }),
    ])
  } finally {
    clearTimeout(timer)
  }
}

export function nativeEd25519Failure(cause: unknown): 'unsupported' | 'timeout' | 'operation-failed' | undefined {
  if (typeof cause !== 'object' || cause === null || !('name' in cause)) return undefined
  switch (cause.name) {
    case 'NotSupportedError': return 'unsupported'
    case 'TimeoutError': return 'timeout'
    case 'OperationError': return 'operation-failed'
    default: return undefined
  }
}

function fromHex(value: string): Uint8Array<ArrayBuffer> {
  return Uint8Array.from(value.match(/../g) ?? [], byte => Number.parseInt(byte, 16))
}
