import { describe, expect, it } from 'vitest'

import {
  RelayProtocolError,
  decodeChallenge,
  decodeDescriptorUpload,
  decodeJoin,
  decodeRegisterInit,
  decodeRegisterProof,
  decodeRegistered,
  decodeRelayError,
  decodeResumeCredential,
  decodeStopInit,
  decodeStopProof,
  decodeStopped,
} from '../../src/transport/relay/protocol'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'

interface DecodingCase extends VectorCase {
  readonly frame: string
  readonly encodedB64: string
  readonly accepted: boolean
}

interface DecodingVector extends VectorCase {
  readonly cases: readonly DecodingCase[]
}

// The Go relay codec and the Rust relay kernel consume the same refusals.
const decoders: Readonly<Record<string, (encoded: Uint8Array) => unknown>> = {
  WS2R: (encoded) => decodeRegisterInit(encoded),
  WS2Q: decodeChallenge,
  WS2P: decodeRegisterProof,
  WS2T: decodeResumeCredential,
  WS2U: decodeDescriptorUpload,
  WS2K: decodeRegistered,
  WS2J: decodeJoin,
  WS2X: (encoded) => decodeStopInit(encoded),
  WS2V: decodeStopProof,
  WS2Y: decodeStopped,
  WS2E: decodeRelayError,
}

const vector = loadVectorFile(
  new URL('../../../core/testvectors/v2-session.json', import.meta.url),
).cases.find((candidate) => candidate.name === 'relay-control-frame-decoding') as DecodingVector

describe('relay control frame decoding vectors', () => {
  for (const test of vector.cases) {
    it(test.name, async () => {
      const decode = decoders[test.frame]
      if (decode === undefined) throw new Error(`no browser decoder owns ${test.frame}`)
      const decoded = Promise.resolve().then(() => decode(b64ToBytes(test.encodedB64)))
      if (test.accepted) {
        await expect(decoded).resolves.toBeDefined()
      } else {
        await expect(decoded).rejects.toBeInstanceOf(RelayProtocolError)
      }
    })
  }
})
