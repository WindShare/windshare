import {
  materializeClassifiedTransferFailure,
  type ClassifiedTransferFailure,
} from './failures'

export type V2TransferAdmissionFailureAuthority =
  | Readonly<{ readonly kind: 'canceled' }>
  | Readonly<{
      readonly kind: 'fault'
      readonly classification: ClassifiedTransferFailure
    }>

/** Keeps admission settlement independent from the native error that reached its classifier. */
export class V2TransferAdmissionFailureError extends Error {
  readonly authority: V2TransferAdmissionFailureAuthority

  constructor(authority: V2TransferAdmissionFailureAuthority) {
    super(authority.kind === 'canceled'
      ? 'Transfer admission was canceled'
      : 'Transfer failed before execution admission')
    this.name = 'V2TransferAdmissionFailureError'
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
