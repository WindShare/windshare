import { describe, expect, it } from 'vitest'

import {
  V2RelayProtocolError,
  decodeV2Challenge,
  decodeV2DescriptorUpload,
  decodeV2RegisterInit,
  decodeV2RegisterProof,
  decodeV2Registered,
  decodeV2RelayError,
  decodeV2ResumeCredential,
  decodeV2StopInit,
  decodeV2StopProof,
  decodeV2Stopped,
} from '../../src/transport/relay/v2-protocol'
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
  WS2R: (encoded) => decodeV2RegisterInit(encoded),
  WS2Q: decodeV2Challenge,
  WS2P: decodeV2RegisterProof,
  WS2T: decodeV2ResumeCredential,
  WS2U: decodeV2DescriptorUpload,
  WS2K: decodeV2Registered,
  WS2X: (encoded) => decodeV2StopInit(encoded),
  WS2V: decodeV2StopProof,
  WS2Y: decodeV2Stopped,
  WS2E: decodeV2RelayError,
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
        await expect(decoded).rejects.toBeInstanceOf(V2RelayProtocolError)
      }
    })
  }
})
