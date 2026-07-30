import type { RunnerProcessEvidence } from '../../execution-evidence.ts'
import type { ContainedSampleCommand } from '../containment.ts'

export const LINUX_PROCESS_OWNER_REQUEST_SCHEMA_VERSION =
  'windshare.linux-process-owner-request/v1' as const
export const LINUX_PROCESS_OWNER_STATUS_SCHEMA_VERSION =
  'windshare.linux-process-owner-status/v2' as const

export interface LinuxProcessOwnerArtifact {
  readonly path: string
  readonly byteLength: number
  readonly sha256: string
}

export interface LinuxProcessOwnerRequest {
  readonly helper: LinuxProcessOwnerArtifact
  readonly operationId: string
  readonly command: ContainedSampleCommand & {
    readonly executableSha256?: string
    readonly executableByteLength?: number
    readonly stdinAuthority?: LinuxProcessOwnerStdinAuthority
  }
  readonly environment: Readonly<Record<string, string>>
  readonly deadlineMs: number
  readonly terminationGraceMs: number
  readonly terminationSignal?: AbortSignal
  readonly stdout: (chunk: Uint8Array) => void
  readonly stderr: (chunk: Uint8Array) => void
  readonly trace?: (event: {
    readonly milestone: string
    readonly context?: Readonly<Record<string, unknown>>
  }) => void
}

export interface LinuxProcessOwnerStdinAuthority {
  readonly channelId: string
  readonly runId: string
  readonly profileId: string
  readonly attemptId: string
}

export interface LinuxProcessOwnershipEvidence {
  readonly ownerPid: number
  readonly rootPid: number | null
  readonly rootStartTimeTicks: string
  readonly inventoryScans: number
  readonly maximumObservedDescendants: number
  readonly quietInventoryCount: number
  readonly controlOutcome: string
  readonly cleanupOutcome: 'completed' | 'failed'
  readonly failureCode: string
  readonly failureMessage: string
}

export interface LinuxProcessInputEvidence {
  readonly outcome: 'not-started' | 'not-requested' | 'delivered' | 'failed'
  readonly failureCode: string
  readonly failureMessage: string
}

export interface LinuxProcessClientIoEvidence {
  readonly requestOutcome: 'delivered' | 'failed'
  readonly rawInputOutcome: 'not-requested' | 'delivered' | 'failed'
  readonly controlOutcome: 'not-requested' | 'delivered' | 'failed'
  readonly outputOutcome: 'delivered' | 'failed'
  readonly failureCode: string
  readonly failureMessage: string
}

export interface LinuxProcessOwnerExecution {
  readonly processEvidence: RunnerProcessEvidence
  readonly timedOut: boolean
  readonly launched: boolean
  readonly treeEmpty: boolean
  readonly inputEvidence: LinuxProcessInputEvidence
  readonly clientIoEvidence: LinuxProcessClientIoEvidence
  readonly ownershipEvidence: LinuxProcessOwnershipEvidence
}
