import { copyBytes } from './bytes'
import { CryptoError } from './errors'
import { type CryptoRuntime, defaultCryptoRuntime } from './webcrypto'

export async function sha256(
  bytes: Uint8Array,
  runtime: CryptoRuntime = defaultCryptoRuntime(),
): Promise<Uint8Array<ArrayBuffer>> {
  try {
    return new Uint8Array(await runtime.subtle.digest('SHA-256', copyBytes(bytes)))
  } catch (cause) {
    throw new CryptoError('digest-failed', 'SHA-256 digest failed', { cause })
  }
}
