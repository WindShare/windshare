/** Display identity is deliberately excluded from receive, destination and deletion authority. */
export interface ReceiveOperationDisplay {
  readonly objectLabel: string
  readonly destinationLabel?: string
  readonly createdAtMilliseconds: number
}

const DISPLAY_LABEL_MAXIMUM_LENGTH = 4096

export function snapshotReceiveOperationDisplay(input: unknown): ReceiveOperationDisplay | undefined {
  if (typeof input !== 'object' || input === null) return undefined
  const value = input as Partial<ReceiveOperationDisplay>
  if (!validLabel(value.objectLabel) ||
      !Number.isSafeInteger(value.createdAtMilliseconds) || value.createdAtMilliseconds! < 0) {
    return undefined
  }
  return Object.freeze({
    objectLabel: value.objectLabel,
    createdAtMilliseconds: value.createdAtMilliseconds!,
    ...(validLabel(value.destinationLabel) ? { destinationLabel: value.destinationLabel } : {}),
  })
}

export function receiveOperationDisplayFields(
  display: ReceiveOperationDisplay | undefined,
  destinationLabel?: string,
): Readonly<{ display?: ReceiveOperationDisplay }> {
  if (display === undefined) return Object.freeze({})
  const snapshot = snapshotReceiveOperationDisplay({ ...display,
    ...(destinationLabel === undefined ? {} : { destinationLabel }) })
  return snapshot === undefined ? Object.freeze({}) : Object.freeze({ display: snapshot })
}

function validLabel(value: unknown): value is string {
  return typeof value === 'string' && value.trim().length > 0 &&
    value.length <= DISPLAY_LABEL_MAXIMUM_LENGTH
}
