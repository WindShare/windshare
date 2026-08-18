import {
  type V2MessageKind,
  V2_MESSAGE_KIND,
  type V2OperationErrorControl,
} from './v2-message'

export type V2OperationErrorScope = V2OperationErrorControl['scope']

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
export function v2OperationErrorScopesForRequest(
  request: V2MessageKind,
): readonly V2OperationErrorScope[] {
  switch (request) {
    case V2_MESSAGE_KIND.listChildren:
      return DIRECTORY_ERROR_SCOPES
    case V2_MESSAGE_KIND.openRevisions:
    case V2_MESSAGE_KIND.renewLease:
    case V2_MESSAGE_KIND.releaseLease:
      return REVISION_ERROR_SCOPES
    case V2_MESSAGE_KIND.requestBlocks:
      return BLOCK_REQUEST_ERROR_SCOPES
    case V2_MESSAGE_KIND.peerOffer:
      return PEER_ERROR_SCOPES
    default:
      return NO_ERROR_SCOPES
  }
}

export function v2OperationErrorScopeAllowed(
  request: V2MessageKind,
  scope: V2OperationErrorScope,
): boolean {
  return v2OperationErrorScopesForRequest(request).includes(scope)
}
