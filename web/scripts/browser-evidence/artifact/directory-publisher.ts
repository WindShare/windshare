import type {
  ExistingDirectoryPublisherRequest,
  ExistingDirectoryPublisherResponseFor,
} from '../../browser-network-matrix/cli/publisher-helper-protocol.ts'
import type { GuardExecutionWindow } from '../execution/guard-execution-lease.ts'

/**
 * Guard sealing depends on publication semantics, not on how an executable is
 * discovered or launched. Keeping that capability at the consumer boundary
 * lets contract tests exercise the complete transaction without host authority.
 */
export interface GuardUploadDirectoryPublisher {
  invoke<Request extends ExistingDirectoryPublisherRequest>(
    request: Request,
    executionWindow: GuardExecutionWindow,
  ): Promise<ExistingDirectoryPublisherResponseFor<Request['operation']>>
}

export function requireGuardUploadDirectoryPublisher(
  value: unknown,
): asserts value is GuardUploadDirectoryPublisher {
  if (
    typeof value !== 'object' || value === null ||
    Object.keys(value).length !== 1 || !Object.hasOwn(value, 'invoke') ||
    typeof (value as { readonly invoke?: unknown }).invoke !== 'function'
  ) {
    throw new Error('guard upload requires one explicit directory publisher capability')
  }
}

const OWNED_UNSETTLED_PUBLISHER_FAILURES = new WeakSet<object>()

export class GuardUploadDirectoryPublisherUnsettledError extends Error {
  constructor(cause: Error) {
    super('guard upload directory publisher did not settle after forced termination', { cause })
    this.name = 'GuardUploadDirectoryPublisherUnsettledError'
    OWNED_UNSETTLED_PUBLISHER_FAILURES.add(this)
    Object.freeze(this)
  }
}

export function isGuardUploadDirectoryPublisherUnsettledError(
  value: unknown,
): value is GuardUploadDirectoryPublisherUnsettledError {
  return (typeof value === 'object' || typeof value === 'function') &&
    value !== null &&
    OWNED_UNSETTLED_PUBLISHER_FAILURES.has(value)
}
