export type ReceiveObjectLabel = string | Readonly<{ kind: 'shared-files' | 'all-selected' }> |
  Readonly<{ kind: 'selection'; folders: number; files: number }>

export type ReceiveDestinationLabel = string | Readonly<{ kind: 'browser-downloads' }>

/** Display identity is deliberately excluded from receive, destination and deletion authority. */
export interface ReceiveOperationDisplay {
  readonly objectLabel: ReceiveObjectLabel
  readonly destinationLabel?: ReceiveDestinationLabel
  readonly createdAtMilliseconds: number
}

const DISPLAY_LABEL_MAXIMUM_LENGTH = 4096

export function snapshotReceiveOperationDisplay(input: unknown): ReceiveOperationDisplay | undefined {
  if (typeof input !== 'object' || input === null) return undefined
  const value = input as Partial<ReceiveOperationDisplay>
  const label = snapshotObjectLabel(value.objectLabel)
  if (label === undefined ||
      !Number.isSafeInteger(value.createdAtMilliseconds) || value.createdAtMilliseconds! < 0) {
    return undefined
  }
  const destinationLabel = snapshotDestinationLabel(value.destinationLabel)
  return Object.freeze({
    objectLabel: label,
    createdAtMilliseconds: value.createdAtMilliseconds!,
    ...(destinationLabel === undefined ? {} : { destinationLabel }),
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

function snapshotObjectLabel(label: ReceiveObjectLabel | undefined): ReceiveObjectLabel | undefined {
  if (validLabel(label)) return label
  if (typeof label !== 'object' || label === null) return undefined
  if (label.kind === 'shared-files' || label.kind === 'all-selected') return Object.freeze({ kind: label.kind })
  if (label.kind !== 'selection' || !Number.isSafeInteger(label.folders) || label.folders < 0 ||
      !Number.isSafeInteger(label.files) || label.files < 0) return undefined
  return Object.freeze({ kind: label.kind, folders: label.folders, files: label.files })
}

function snapshotDestinationLabel(label: ReceiveDestinationLabel | undefined): ReceiveDestinationLabel | undefined {
  if (validLabel(label)) return label
  return typeof label === 'object' && label?.kind === 'browser-downloads'
    ? Object.freeze({ kind: 'browser-downloads' }) : undefined
}
