import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createWindowBrowserHandoffPublisher,
  issuePortableArtifactAdmission,
  type BrowserHandoffStarted,
  type PortableArtifactAdmission,
  type PortableHandoffWindow,
} from '../../src/output/portable/browser-download'
import {
  createPackagedArtifactHandoffPublisher,
} from '../../src/output/portable/packaged-handoff'
import {
  createPublicationAttempt,
  sealPackagedArtifact,
} from '../../src/output/workspace/aggregate'
import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  createOriginalFileArtifact,
  createPortableBinding,
  createPortableHandoffPlan,
  createReceiveIntent,
  createSelectionSpec,
  type ReceiveIntent,
} from '../../src/transfer/intent'

const PACKAGED_RETRYABLE_UNTIL = 2_000_000_000_000

export interface PortableBrowserFixture {
  readonly intent: ReceiveIntent
  readonly admission: PortableArtifactAdmission
  readonly attemptId: string
}

export async function createPortableBrowserFixture(
  suggestedName: string,
  exactArtifactBytes: bigint,
): Promise<PortableBrowserFixture> {
  const selection = await createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const artifact = await createOriginalFileArtifact({
    fileId: identity(3),
    sourcePath: `root/${suggestedName}`,
    suggestedName,
  })
  const portable = await createPortableBinding({
    operationId: identity(4),
    portablePlanId: identity(5),
    artifact,
  })
  const plan = await createPortableHandoffPlan(artifact, portable)
  const intent = await createReceiveIntent({ selection, artifact, plan })
  return Object.freeze({
    intent,
    admission: issuePortableArtifactAdmission({
      receiveIntentDigest: intent.digest,
      artifactDigest: artifact.digest,
      sealedArtifact: {
        artifactKind: 'original-file',
        preparationManifestDigest: identity(7, 32),
      },
      exactArtifactBytes,
    }),
    attemptId: identity(6),
  })
}

type PackagedArtifact = Awaited<ReturnType<typeof sealPackagedArtifact>>

interface PackagedBrowserSession {
  readonly root: FileSystemDirectoryHandle
  readonly directoryName: string
  readonly handle: FileSystemFileHandle
  readonly artifact: PackagedArtifact
  readonly suggestedName: string
  readonly packageDigest: string
  readonly receiveIntentDigest: string
  readonly files: File[]
  readonly objectUrls: string[]
  nextAttemptSeed: number
}

export interface PackagedBrowserRetryProof {
  readonly started: BrowserHandoffStarted
  readonly packageDigest: string
  readonly receiveIntentDigest: string
  readonly packageIdentityUnchanged: boolean
  readonly sourceFileFresh: boolean
  readonly immutableFileSource: boolean
  readonly freshObjectUrl: boolean
  readonly retryableUntil: number
}

let packagedBrowserSession: PackagedBrowserSession | undefined

export async function preparePackagedFileRetries(
  suggestedName: string,
  bytes: readonly number[],
): Promise<void> {
  if (packagedBrowserSession !== undefined) await cleanupPackagedFileRetries()
  const operationId = identity(20)
  const receiveIntentDigest = identity(21, 32)
  const artifact = await sealPackagedArtifact({
    operationId,
    receiveIntentDigest,
    sealedMaterializationDigest: identity(22, 32),
    artifactSpecDigest: identity(23, 32),
    packageOwnedObjectId: identity(24, 32),
    exactBytes: BigInt(bytes.length),
    artifactReceiptDigest: identity(25, 32),
    layoutDigest: identity(26, 32),
  })
  const root = await navigator.storage.getDirectory()
  const directoryName = `windshare-package-handoff-${operationId}`
  const directory = await root.getDirectoryHandle(directoryName, { create: true })
  const handle = await directory.getFileHandle('sealed-package.bin', { create: true })
  const writable = await handle.createWritable()
  await writable.write(Uint8Array.from(bytes))
  await writable.close()
  packagedBrowserSession = {
    root,
    directoryName,
    handle,
    artifact,
    suggestedName,
    packageDigest: artifact.digest,
    receiveIntentDigest: artifact.receiveIntentDigest,
    files: [],
    objectUrls: [],
    nextAttemptSeed: 27,
  }
}

export async function handoffNextPackagedFileRetry(): Promise<PackagedBrowserRetryProof> {
  const session = packagedBrowserSession
  if (session === undefined) throw new TypeError('Packaged browser fixture is not prepared')
  const priorFile = session.files.at(-1)
  const priorObjectUrl = session.objectUrls.at(-1)
  const attempt = await packagedAttempt(
    session.artifact,
    session.nextAttemptSeed++,
    session.suggestedName,
  )
  const windowPort = currentPortableHandoffWindow()
  const trackedWindow: PortableHandoffWindow = {
    ...windowPort,
    URL: {
      createObjectURL: (source) => {
        const objectUrl = window.URL.createObjectURL(source)
        session.objectUrls.push(objectUrl)
        return objectUrl
      },
      revokeObjectURL: (objectUrl) => window.URL.revokeObjectURL(objectUrl),
    },
  }
  const publisher = createPackagedArtifactHandoffPublisher({
    packages: {
      async readPackagedArtifact(candidate) {
        if (candidate !== session.artifact) {
          throw new TypeError('Browser fixture received a different package authority')
        }
        const file = await session.handle.getFile()
        session.files.push(file)
        return file
      },
    },
    browser: createWindowBrowserHandoffPublisher(trackedWindow),
    File: window.File,
  })
  const started = await publisher.handoff({
    artifact: session.artifact,
    attempt,
    retryableUntil: PACKAGED_RETRYABLE_UNTIL,
  })
  const source = session.files.at(-1)
  const objectUrl = session.objectUrls.at(-1)
  return Object.freeze({
    started,
    packageDigest: session.packageDigest,
    receiveIntentDigest: session.receiveIntentDigest,
    packageIdentityUnchanged: session.artifact.digest === session.packageDigest &&
      session.artifact.receiveIntentDigest === session.receiveIntentDigest,
    sourceFileFresh: source instanceof window.File &&
      (priorFile === undefined || source !== priorFile),
    immutableFileSource: source instanceof window.File,
    freshObjectUrl: objectUrl !== undefined &&
      (priorObjectUrl === undefined || objectUrl !== priorObjectUrl),
    retryableUntil: PACKAGED_RETRYABLE_UNTIL,
  })
}

export async function cleanupPackagedFileRetries(): Promise<void> {
  const session = packagedBrowserSession
  packagedBrowserSession = undefined
  if (session === undefined) return
  await session.root.removeEntry(session.directoryName, { recursive: true })
}

async function packagedAttempt(
  artifact: Awaited<ReturnType<typeof sealPackagedArtifact>>,
  seed: number,
  suggestedName: string,
) {
  return createPublicationAttempt({
    publicationAttemptId: identity(seed),
    operationId: artifact.operationId,
    receiveIntentDigest: artifact.receiveIntentDigest,
    packagedArtifactDigest: artifact.digest,
    route: {
      kind: 'handoff',
      suggestedName,
      packagedFileSupported: true,
      objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
    },
  })
}

export function currentPortableHandoffWindow(): PortableHandoffWindow {
  return {
    Blob: window.Blob,
    WritableStream: window.WritableStream,
    URL: window.URL,
    document: window.document,
    setTimeout: window.setTimeout.bind(window),
    clearTimeout: window.clearTimeout.bind(window),
  }
}

function identity(seed: number, width = 16): string {
  const bytes = new Uint8Array(width)
  bytes[0] = seed
  bytes[bytes.length - 1] = seed ^ 0xff
  return encodeBase64Url(bytes)
}
