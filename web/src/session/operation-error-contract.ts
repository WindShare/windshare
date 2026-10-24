import {
  type MessageKind,
  MESSAGE_KIND,
  type OperationErrorControl,
} from './message'

export type OperationErrorScope = OperationErrorControl['scope']

const DIRECTORY_ERROR_SCOPES = Object.freeze(['directory'] as const)
const REVISION_ERROR_SCOPES = Object.freeze(['revision'] as const)
const BLOCK_REQUEST_ERROR_SCOPES = Object.freeze(['block', 'revision'] as const)
const PEER_ERROR_SCOPES = Object.freeze(['peer'] as const)
const NO_ERROR_SCOPES = Object.freeze([] as const)

/**
 * A block request is authorized by a revision lease, so the sender can reject
 * either the individual block or the revision authority that made it readable.
 * Keeping this matrix at the session boundary prevents routing and content
 * services from assigning different meanings to the same authenticated final.
 */
export function operationErrorScopesForRequest(
  request: MessageKind,
): readonly OperationErrorScope[] {
  switch (request) {
    case MESSAGE_KIND.listChildren:
      return DIRECTORY_ERROR_SCOPES
    case MESSAGE_KIND.openRevisions:
    case MESSAGE_KIND.renewLease:
    case MESSAGE_KIND.releaseLease:
      return REVISION_ERROR_SCOPES
    case MESSAGE_KIND.requestBlocks:
      return BLOCK_REQUEST_ERROR_SCOPES
    case MESSAGE_KIND.peerOffer:
      return PEER_ERROR_SCOPES
    default:
      return NO_ERROR_SCOPES
  }
}

export function operationErrorScopeAllowed(
  request: MessageKind,
  scope: OperationErrorScope,
): boolean {
  return operationErrorScopesForRequest(request).includes(scope)
}
