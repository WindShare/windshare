import type { ExternalFixtureProbeResult } from '../../runtime-authority.ts'
import { RemotePionControlClient } from '../remote-pion.ts'
import type {
  ContainedBrowserPionControl,
  ContainedBrowserSampleSecret,
} from './contracts.ts'

export function createContainedBrowserPionControl(
  secret: ContainedBrowserSampleSecret,
): ContainedBrowserPionControl {
  const client = new RemotePionControlClient({
    controllerOrigin: secret.control.controllerOrigin,
    tlsCertificateAuthority: secret.control.tlsCertificateAuthority,
    tlsCertificateSha256: secret.control.tlsCertificateSha256,
    attestationPublicKey: secret.control.attestationPublicKey,
    controlCredential: secret.control.credential,
    controlLease: secret.control.controlLease,
  })
  return Object.freeze({
    probeFixture: async (signal: AbortSignal): Promise<ExternalFixtureProbeResult> => {
      const operation = client.probe({
        sampleAuthority: secret.control.controlLease.controlAuthority.sampleAuthority,
        signal,
      })
      try {
        return await operation.result
      } catch (primaryFailure) {
        try {
          await operation.forceTerminateAndWait('sample-execute')
        } catch (cleanupFailure) {
          throw new AggregateError(
            [primaryFailure, cleanupFailure],
            'contained browser external fixture probe cleanup failed',
            { cause: cleanupFailure },
          )
        }
        throw primaryFailure
      }
    },
    createAttempt: (requestId: string, leaseMillis: number, signal: AbortSignal) =>
      client.createAttempt(requestId, leaseMillis, signal),
    offer: (attemptId: string, sdp: string, signal: AbortSignal) =>
      client.offer(attemptId, sdp, signal),
    result: (attemptId: string, signal: AbortSignal) => client.result(attemptId, signal),
    deleteAttempt: (attemptId: string, signal: AbortSignal) =>
      client.deleteAttempt(attemptId, signal),
  })
}
