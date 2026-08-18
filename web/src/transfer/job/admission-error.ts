export class V2TransferAdmissionFailureError extends Error {
  readonly transferFailure: unknown

  constructor(transferFailure: unknown) {
    super(transferFailure instanceof Error && transferFailure.message.trim().length > 0
      ? transferFailure.message
      : 'Transfer failed before execution admission', { cause: transferFailure })
    this.name = 'V2TransferAdmissionFailureError'
    this.transferFailure = transferFailure
  }
}
