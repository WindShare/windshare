import { createEnvironmentOffers } from '../../src/output/planning'
import type { ReceiveCompositionPort } from '../../src/application/receive/contracts'

/** Browse-only controller tests install an explicit non-executable output boundary. */
export const INERT_TEST_RECEIVE_COMPOSITION: ReceiveCompositionPort = Object.freeze({
  retained: Object.freeze({
    list: () => Promise.resolve(Object.freeze({
      operations: Object.freeze([]),
      presentationFailures: Object.freeze([]),
      act: () => Promise.reject(new DOMException('No retained operation', 'InvalidStateError')),
      close: () => undefined,
    })),
  }),
  environment: () => createEnvironmentOffers({ targets: [] }),
  startArtifactAuthority: () => {
    throw new DOMException('Browse-only test has no output authority', 'NotSupportedError')
  },
})
