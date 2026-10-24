import {
  materializeClassifiedTransferFailure,
} from './failures'
import {
  type ClassifiedTransferFailure,
} from '../failure/classification'

export type TransferAdmissionFailureAuthority =
  | Readonly<{ readonly kind: 'canceled' }>
  | Readonly<{
      readonly kind: 'fault'
      readonly classification: ClassifiedTransferFailure
    }>

/** Keeps admission settlement independent from the native error that reached its classifier. */
export class TransferAdmissionFailureError extends Error {
  readonly authority: TransferAdmissionFailureAuthority

  constructor(authority: TransferAdmissionFailureAuthority) {
    super(authority.kind === 'canceled'
      ? 'Transfer admission was canceled'
      : 'Transfer failed before execution admission')
    this.name = 'TransferAdmissionFailureError'
    this.authority = authority.kind === 'canceled'
      ? Object.freeze({ kind: 'canceled' })
      : Object.freeze({
          kind: 'fault',
          classification: materializeClassifiedTransferFailure(
            authority.classification,
            undefined,
          ),
        })
  }
}
