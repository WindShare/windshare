import { BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS } from '../../transfer/intent'
import type {
  PackagedArtifactV1,
  PublicationAttemptV1,
} from '../workspace/aggregate'
import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MS,
  BrowserHandoffNotStartedError,
  browserSupportsObjectUrlHandoff,
  type BrowserHandoffStarted,
  type PortableHandoffWindow,
  type TimedBrowserHandoffPublisher,
} from './browser-download'

const PACKAGED_FILE_CAPABILITY_PROBE_NAME = 'windshare-package-capability-probe'

export interface PackagedArtifactReadPort {
  readPackagedArtifact(artifact: PackagedArtifactV1): Promise<Blob>
  acquireReader?(artifact: PackagedArtifactV1): Promise<{ release(): void }>
}

export interface PackagedArtifactHandoffRequest {
  readonly artifact: PackagedArtifactV1
  readonly attempt: PublicationAttemptV1
  readonly signal?: AbortSignal
}

export interface PackagedArtifactHandoffPublisher {
  handoff(request: PackagedArtifactHandoffRequest): Promise<BrowserHandoffStarted>
}

export interface PackagedArtifactHandoffPorts {
  readonly packages: PackagedArtifactReadPort
  readonly browser: TimedBrowserHandoffPublisher
  readonly File: typeof File
}

export type BrowserHandoffCapabilityRuntime =
  Partial<PortableHandoffWindow> &
  Readonly<{ File?: typeof File }>

export interface BrowserHandoffCapabilityFacts {
  readonly supportsWorkspacePackage: boolean
  readonly supportsPortableArtifact: boolean
}

export function createPackagedArtifactHandoffPublisher(
  ports: PackagedArtifactHandoffPorts,
): PackagedArtifactHandoffPublisher {
  return Object.freeze({
    async handoff(
      request: PackagedArtifactHandoffRequest,
    ): Promise<BrowserHandoffStarted> {
      assertPackagedHandoffRequest(request)
      if (request.attempt.route.kind !== 'handoff' ||
          !request.attempt.route.packagedFileSupported) {
        throw new DOMException(
          'Packaged browser handoff is unavailable in this engine',
          'NotSupportedError',
        )
      }
      if (typeof ports.File !== 'function') {
        throw new DOMException(
          'Packaged browser handoff requires immutable File support',
          'NotSupportedError',
        )
      }

      if (request.signal?.aborted) throw new BrowserHandoffNotStartedError()
      const reader = await ports.packages.acquireReader?.(request.artifact)
      let handedOff = false
      try {
        const source = await ports.packages.readPackagedArtifact(request.artifact)
        if (!(source instanceof ports.File)) {
          throw new DOMException(
            'The packaged artifact reader did not return an immutable File',
            'NotSupportedError',
          )
        }
        if (BigInt(source.size) !== request.artifact.exactBytes) {
          throw new TypeError('Packaged artifact File length changed after seal')
        }

        // Revocation is still provable before the synchronous browser handoff boundary.
        if (request.signal?.aborted) throw new BrowserHandoffNotStartedError()
        const started = ports.browser.handoffWithLease({
          context: {
            attemptKind: 'workspace',
            operationId: request.artifact.operationId,
            attemptId: request.attempt.publicationAttemptId,
            packageDigest: request.artifact.digest,
          },
          source,
          exactBytes: request.artifact.exactBytes,
          suggestedName: request.attempt.route.suggestedName,
          objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MS,
          ...(reader === undefined ? {} : { onSourceReleased: () => reader.release() }),
        })
        handedOff = true
        assertPackagedHandoffStarted(started, request)
        return started
      } catch (error) {
        if (!handedOff) reader?.release()
        throw error
      }
    },
  })
}

/**
 * The File and Blob probes are independent so an engine that cannot hand a packaged
 * File to createObjectURL loses workspace redownload without losing bounded portable.
 */
export function probeBrowserHandoffCapabilities(
  runtime: BrowserHandoffCapabilityRuntime,
): BrowserHandoffCapabilityFacts {
  if (!browserSupportsObjectUrlHandoff(runtime) || runtime.URL === undefined) {
    return unsupportedHandoffCapabilities()
  }

  const supportsWorkspacePackage = probePackagedFile(runtime, runtime.URL)
  const supportsPortableArtifact = probePortableBlob(runtime, runtime.URL)
  return Object.freeze({
    supportsWorkspacePackage,
    supportsPortableArtifact,
  })
}

function assertPackagedHandoffRequest(request: PackagedArtifactHandoffRequest): void {
  const route = request.attempt.route
  if (request.attempt.operationId !== request.artifact.operationId ||
      request.attempt.receiveIntentDigest !== request.artifact.receiveIntentDigest ||
      request.attempt.packagedArtifactDigest !== request.artifact.digest) {
    throw new TypeError('Packaged handoff attempt does not bind the sealed artifact')
  }
  if (route.kind !== 'handoff' ||
      route.objectUrlLeaseMilliseconds !== BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS) {
    throw new TypeError('Packaged handoff attempt does not use the finite browser route')
  }
}

function assertPackagedHandoffStarted(
  started: BrowserHandoffStarted,
  request: PackagedArtifactHandoffRequest,
): void {
  const route = request.attempt.route
  if (route.kind !== 'handoff') {
    throw new TypeError('Packaged handoff evidence requires a browser route')
  }
  if (started.result.kind !== 'download-started' ||
      started.result.suggestedName !== route.suggestedName ||
      !Number.isSafeInteger(started.urlLeaseStartedAt) ||
      started.urlLeaseStartedAt < 0 ||
      !Number.isSafeInteger(started.urlLeaseEndsAt) ||
      started.urlLeaseEndsAt !==
        started.urlLeaseStartedAt + BROWSER_HANDOFF_OBJECT_URL_LEASE_MS) {
    throw new TypeError('Packaged browser publisher returned invalid handoff evidence')
  }
}

function probePackagedFile(
  runtime: BrowserHandoffCapabilityRuntime,
  url: Pick<typeof URL, 'createObjectURL' | 'revokeObjectURL'>,
): boolean {
  if (typeof runtime.File !== 'function') return false
  try {
    return probeObjectUrl(
      url,
      new runtime.File([], PACKAGED_FILE_CAPABILITY_PROBE_NAME),
    )
  } catch {
    return false
  }
}

function probePortableBlob(
  runtime: BrowserHandoffCapabilityRuntime,
  url: Pick<typeof URL, 'createObjectURL' | 'revokeObjectURL'>,
): boolean {
  if (typeof runtime.Blob !== 'function' ||
      typeof runtime.WritableStream !== 'function') return false
  try {
    return probeObjectUrl(url, new runtime.Blob())
  } catch {
    return false
  }
}

function probeObjectUrl(
  url: Pick<typeof URL, 'createObjectURL' | 'revokeObjectURL'>,
  source: Blob,
): boolean {
  let objectUrl: string | undefined
  let revoked = false
  try {
    objectUrl = url.createObjectURL(source)
    if (typeof objectUrl !== 'string' || objectUrl.length === 0) return false
    url.revokeObjectURL(objectUrl)
    revoked = true
    return true
  } catch {
    return false
  } finally {
    if (objectUrl !== undefined && !revoked) {
      try {
        url.revokeObjectURL(objectUrl)
      } catch {
        // A failed immediate cleanup makes the finite handoff capability unsupported.
      }
    }
  }
}

function unsupportedHandoffCapabilities(): BrowserHandoffCapabilityFacts {
  return Object.freeze({
    supportsWorkspacePackage: false,
    supportsPortableArtifact: false,
  })
}
