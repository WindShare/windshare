import type {
  CompatibleNameEntryKind,
  CompatibleNameFooterState,
  CompatibleNamePairPlacement,
} from './model'

export const COMPATIBLE_NAME_SIDECAR_FORMAT_VERSION =
  'windshare-name-restoration/v2' as const

const TEXT_ENCODER = new TextEncoder()
const TEXT_DECODER = new TextDecoder('utf-8', { fatal: true })
const CANONICAL_BASE64_PATTERN = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/u
const WINDOWS_INVALID_COMPONENT_PATTERN = /["*/:<>?\\|]/u
const CANONICAL_NON_NEGATIVE_INTEGER_PATTERN = /^(?:0|[1-9]\d*)$/u
const MAX_WINDOWS_POWERSHELL_INTEGER = 2_147_483_647

export type CompatibleNameSidecarPlacement = 'inside' | 'beside'

export interface CompatibleNameSidecarHeaderV1 {
  readonly formatVersion: typeof COMPATIBLE_NAME_SIDECAR_FORMAT_VERSION
  readonly operationId: string
  readonly placement: CompatibleNameSidecarPlacement
}

export interface CompatibleNameSidecarMappingV1 {
  readonly ordinal: number
  readonly entryKind: CompatibleNameEntryKind
  readonly logicalPath: readonly string[]
  readonly physicalComponent: string
}

export interface CompatibleNameSidecarFooterV1 {
  readonly committedCount: number
  readonly state: CompatibleNameFooterState
}

export interface CompatibleNameSidecarCheckpointV1 {
  readonly header: CompatibleNameSidecarHeaderV1
  readonly mappings: readonly CompatibleNameSidecarMappingV1[]
  readonly footer: CompatibleNameSidecarFooterV1
  /** Byte boundary through the selected footer's LF, suitable for owned-sidecar truncation. */
  readonly checkpointByteLength: number
  readonly trailingByteLength: number
}

export class CompatibleNameSidecarError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'CompatibleNameSidecarError'
  }
}

export function compatibleNameSidecarPlacement(
  placement: CompatibleNamePairPlacement,
): CompatibleNameSidecarPlacement {
  switch (placement) {
    case 'inside-logical-root': return 'inside'
    case 'beside-mapped-root': return 'beside'
    default: throw new CompatibleNameSidecarError('compatible-name pair placement is unsupported')
  }
}

export function encodeCompatibleNameSidecarHeader(
  input: Readonly<{ operationId: string; placement: CompatibleNameSidecarPlacement }>,
): string {
  const operationId = validateOperationId(input.operationId)
  const placement = validatePlacement(input.placement)
  return `H\t${COMPATIBLE_NAME_SIDECAR_FORMAT_VERSION}\t${encodeBase64Utf8(operationId)}\t${placement}\n`
}

export function encodeCompatibleNameSidecarMapping(
  input: CompatibleNameSidecarMappingV1,
): string {
  const ordinal = canonicalInteger(input.ordinal, 'mapping ordinal', true)
  const entryKind = validateEntryKind(input.entryKind)
  const logicalPath = validateLogicalPath(input.logicalPath)
  const physicalComponent = validatePathComponent(
    input.physicalComponent,
    'physical component',
  )
  assertDistinctPhysicalComponent(logicalPath, physicalComponent)
  return `M\t${ordinal}\t${entryKind}\t${encodeBase64Utf8(logicalPath.join('/'))}\t${
    encodeBase64Utf8(physicalComponent)}\n`
}

export function encodeCompatibleNameSidecarFooter(
  input: CompatibleNameSidecarFooterV1,
): string {
  const committedCount = canonicalInteger(input.committedCount, 'committed count', false)
  const state = validateFooterState(input.state)
  return `F\t${committedCount}\t${state}\n`
}

/**
 * A closed footer is the completeness authority. A writer crash may leave any later
 * bytes behind, so candidate validation deliberately starts again at every footer.
 */
export function decodeCompatibleNameSidecar(
  encoded: Uint8Array,
): CompatibleNameSidecarCheckpointV1 {
  const text = decodeStrictUtf8(encoded)
  const completeLines = splitCompleteLines(text)
  let selected: CompatibleNameSidecarCheckpointV1 | undefined
  let lastCandidateError: unknown

  for (let footerIndex = 1; footerIndex < completeLines.length; footerIndex += 1) {
    const footerLine = completeLines[footerIndex]
    if (footerLine === undefined || !footerLine.value.startsWith('F\t')) continue
    try {
      const candidate = decodeCheckpointCandidate(completeLines, footerIndex)
      const checkpointByteLength = TEXT_ENCODER.encode(
        text.slice(0, footerLine.textEndOffset),
      ).byteLength
      selected = Object.freeze({
        ...candidate,
        checkpointByteLength,
        trailingByteLength: encoded.byteLength - checkpointByteLength,
      })
    } catch (error) {
      lastCandidateError = error
    }
  }

  if (selected === undefined) {
    throw new CompatibleNameSidecarError(
      'compatible-name sidecar contains no structurally valid complete checkpoint',
      lastCandidateError === undefined ? undefined : { cause: lastCandidateError },
    )
  }
  return selected
}

interface CompleteSidecarLine {
  readonly value: string
  readonly textEndOffset: number
}

function splitCompleteLines(text: string): readonly CompleteSidecarLine[] {
  const lines: CompleteSidecarLine[] = []
  let start = 0
  while (start < text.length) {
    const lineFeed = text.indexOf('\n', start)
    if (lineFeed === -1) break
    let value = text.slice(start, lineFeed)
    if (value.endsWith('\r')) value = value.slice(0, -1)
    lines.push(Object.freeze({ value, textEndOffset: lineFeed + 1 }))
    start = lineFeed + 1
  }
  return Object.freeze(lines)
}

function decodeCheckpointCandidate(
  lines: readonly CompleteSidecarLine[],
  footerIndex: number,
): Pick<CompatibleNameSidecarCheckpointV1, 'header' | 'mappings' | 'footer'> {
  const header = decodeHeader(requiredLine(lines, 0))
  const mappings: CompatibleNameSidecarMappingV1[] = []
  const paths = new Map<string, CompatibleNameSidecarMappingV1>()
  let footer: CompatibleNameSidecarFooterV1 | undefined

  for (let lineIndex = 1; lineIndex <= footerIndex; lineIndex += 1) {
    const line = requiredLine(lines, lineIndex)
    const recordType = line.split('\t', 1)[0]
    if (recordType === 'M') {
      const mapping = decodeMapping(line, mappings.length + 1)
      const logicalPathKey = windowsOrdinalIgnoreCasePathKey(mapping.logicalPath)
      if (paths.has(logicalPathKey)) {
        throw new CompatibleNameSidecarError('compatible-name sidecar repeats a logical path')
      }
      mappings.push(mapping)
      paths.set(logicalPathKey, mapping)
      continue
    }
    if (recordType !== 'F') {
      throw new CompatibleNameSidecarError('compatible-name sidecar record type is unsupported')
    }
    footer = decodeFooter(line, mappings.length)
    if (lineIndex !== footerIndex && footer.state !== 'active') {
      throw new CompatibleNameSidecarError(
        'compatible-name sidecar has records after a terminal footer',
      )
    }
  }

  if (footer === undefined) {
    throw new CompatibleNameSidecarError('compatible-name checkpoint has no footer')
  }
  assertMappedAncestorsAreDirectories(mappings, paths)
  return Object.freeze({
    header,
    mappings: Object.freeze(mappings),
    footer,
  })
}

function decodeHeader(line: string): CompatibleNameSidecarHeaderV1 {
  const fields = line.split('\t')
  if (fields.length !== 4 || fields[0] !== 'H' ||
      fields[1] !== COMPATIBLE_NAME_SIDECAR_FORMAT_VERSION) {
    throw new CompatibleNameSidecarError('compatible-name sidecar header is malformed or unsupported')
  }
  return Object.freeze({
    formatVersion: COMPATIBLE_NAME_SIDECAR_FORMAT_VERSION,
    operationId: validateOperationId(decodeBase64Utf8(requiredField(fields, 2, 'operation ID'))),
    placement: validatePlacement(requiredField(fields, 3, 'placement')),
  })
}

function decodeMapping(line: string, expectedOrdinal: number): CompatibleNameSidecarMappingV1 {
  const fields = line.split('\t')
  if (fields.length !== 5 || fields[0] !== 'M') {
    throw new CompatibleNameSidecarError('compatible-name sidecar mapping is malformed')
  }
  const ordinal = decodeCanonicalInteger(requiredField(fields, 1, 'mapping ordinal'))
  if (ordinal !== expectedOrdinal) {
    throw new CompatibleNameSidecarError('compatible-name sidecar mapping ordinals are not contiguous')
  }
  const entryKind = validateEntryKind(requiredField(fields, 2, 'entry kind'))
  const logicalPath = decodeLogicalPath(decodeBase64Utf8(
    requiredField(fields, 3, 'logical path'),
  ))
  const physicalComponent = validatePathComponent(
    decodeBase64Utf8(requiredField(fields, 4, 'physical component')),
    'physical component',
  )
  assertDistinctPhysicalComponent(logicalPath, physicalComponent)
  return Object.freeze({ ordinal, entryKind, logicalPath, physicalComponent })
}

function decodeFooter(line: string, mappingCount: number): CompatibleNameSidecarFooterV1 {
  const fields = line.split('\t')
  if (fields.length !== 3 || fields[0] !== 'F') {
    throw new CompatibleNameSidecarError('compatible-name sidecar footer is malformed')
  }
  const committedCount = decodeCanonicalInteger(requiredField(fields, 1, 'committed count'))
  if (committedCount !== mappingCount) {
    throw new CompatibleNameSidecarError('compatible-name sidecar footer count disagrees with mappings')
  }
  return Object.freeze({
    committedCount,
    state: validateFooterState(requiredField(fields, 2, 'footer state')),
  })
}

function decodeLogicalPath(value: string): readonly string[] {
  if (value.length === 0 || value.startsWith('/') || value.includes('\\')) {
    throw new CompatibleNameSidecarError('compatible-name logical path is not confined')
  }
  return validateLogicalPath(value.split('/'))
}

function validateLogicalPath(path: readonly string[]): readonly string[] {
  if (!Array.isArray(path) || path.length === 0) {
    throw new CompatibleNameSidecarError('compatible-name logical path is empty')
  }
  const validated: string[] = []
  for (const component of path) {
    validated.push(validatePathComponent(component, 'logical path'))
  }
  return Object.freeze(validated)
}

function validatePathComponent(value: string, label: string): string {
  if (typeof value !== 'string' || value.length === 0 || value === '.' || value === '..' ||
      WINDOWS_INVALID_COMPONENT_PATTERN.test(value) || containsControlCharacter(value) ||
      !isStrictUtf8Text(value)) {
    throw new CompatibleNameSidecarError(`${label} contains an invalid path component`)
  }
  return value
}

function validateOperationId(value: string): string {
  if (typeof value !== 'string' || value.length === 0 || containsControlCharacter(value) ||
      !isStrictUtf8Text(value)) {
    throw new CompatibleNameSidecarError('compatible-name operation ID is empty or contains controls')
  }
  return value
}

function validatePlacement(value: string): CompatibleNameSidecarPlacement {
  if (value !== 'inside' && value !== 'beside') {
    throw new CompatibleNameSidecarError('compatible-name sidecar placement is unsupported')
  }
  return value
}

function validateEntryKind(value: string): CompatibleNameEntryKind {
  if (value !== 'file' && value !== 'directory') {
    throw new CompatibleNameSidecarError('compatible-name mapping kind is unsupported')
  }
  return value
}

function validateFooterState(value: string): CompatibleNameFooterState {
  if (value !== 'active' && value !== 'completed' && value !== 'stopped' && value !== 'failed') {
    throw new CompatibleNameSidecarError('compatible-name footer state is unsupported')
  }
  return value
}

function decodeCanonicalInteger(value: string): number {
  if (!CANONICAL_NON_NEGATIVE_INTEGER_PATTERN.test(value)) {
    throw new CompatibleNameSidecarError('compatible-name sidecar integer is not canonical')
  }
  return canonicalInteger(Number(value), 'sidecar integer', false)
}

function canonicalInteger(value: number, label: string, positive: boolean): number {
  if (!Number.isSafeInteger(value) || value < (positive ? 1 : 0) ||
      value > MAX_WINDOWS_POWERSHELL_INTEGER) {
    throw new CompatibleNameSidecarError(`${label} is outside the sidecar integer domain`)
  }
  return value
}

function decodeBase64Utf8(value: string): string {
  if (!CANONICAL_BASE64_PATTERN.test(value)) {
    throw new CompatibleNameSidecarError('compatible-name sidecar field is not canonical Base64')
  }
  try {
    const binary = atob(value)
    const bytes = new Uint8Array(binary.length)
    for (let index = 0; index < binary.length; index += 1) {
      bytes[index] = binary.charCodeAt(index)
    }
    const decoded = TEXT_DECODER.decode(bytes)
    if (encodeBase64Utf8(decoded) !== value) {
      throw new CompatibleNameSidecarError('compatible-name sidecar field is not canonical Base64')
    }
    return decoded
  } catch (error) {
    if (error instanceof CompatibleNameSidecarError) throw error
    throw new CompatibleNameSidecarError(
      'compatible-name sidecar field is not strict Base64-encoded UTF-8',
      { cause: error },
    )
  }
}

function encodeBase64Utf8(value: string): string {
  if (!isStrictUtf8Text(value)) {
    throw new CompatibleNameSidecarError('compatible-name sidecar text is not strict UTF-8')
  }
  let binary = ''
  for (const byte of TEXT_ENCODER.encode(value)) binary += String.fromCharCode(byte)
  return btoa(binary)
}

function decodeStrictUtf8(value: Uint8Array): string {
  try {
    return TEXT_DECODER.decode(value)
  } catch (error) {
    throw new CompatibleNameSidecarError('compatible-name sidecar is not strict UTF-8', {
      cause: error,
    })
  }
}

function isStrictUtf8Text(value: string): boolean {
  try {
    return TEXT_DECODER.decode(TEXT_ENCODER.encode(value)) === value
  } catch {
    return false
  }
}

function containsControlCharacter(value: string): boolean {
  for (const character of value) {
    const scalar = character.codePointAt(0) ?? 0
    if (scalar <= 0x1f || (scalar >= 0x7f && scalar <= 0x9f)) return true
  }
  return false
}

function assertDistinctPhysicalComponent(
  logicalPath: readonly string[],
  physicalComponent: string,
): void {
  const logicalLeaf = logicalPath.at(-1)
  if (logicalLeaf !== undefined &&
      windowsOrdinalIgnoreCaseComponentKey(logicalLeaf) ===
      windowsOrdinalIgnoreCaseComponentKey(physicalComponent)) {
    throw new CompatibleNameSidecarError(
      'compatible-name mapping does not name a distinct physical component',
    )
  }
}

function assertMappedAncestorsAreDirectories(
  mappings: readonly CompatibleNameSidecarMappingV1[],
  paths: ReadonlyMap<string, CompatibleNameSidecarMappingV1>,
): void {
  for (const mapping of mappings) {
    for (let depth = 1; depth < mapping.logicalPath.length; depth += 1) {
      const ancestor = paths.get(windowsOrdinalIgnoreCasePathKey(mapping.logicalPath.slice(0, depth)))
      if (ancestor !== undefined && ancestor.entryKind !== 'directory') {
        throw new CompatibleNameSidecarError(
          'compatible-name sidecar maps a file as an ancestor of another mapping',
        )
      }
    }
  }
}

function windowsOrdinalIgnoreCasePathKey(path: readonly string[]): string {
  return path.map(windowsOrdinalIgnoreCaseComponentKey).join('/')
}

function windowsOrdinalIgnoreCaseComponentKey(value: string): string {
  // The immutable Windows template compares path names without case. Unicode
  // uppercasing can conservatively merge extra spellings, which only fails closed.
  return value.toUpperCase()
}

function requiredLine(lines: readonly CompleteSidecarLine[], index: number): string {
  const line = lines[index]
  if (line === undefined) throw new CompatibleNameSidecarError('compatible-name sidecar is truncated')
  return line.value
}

function requiredField(fields: readonly string[], index: number, label: string): string {
  const value = fields[index]
  if (value === undefined) throw new CompatibleNameSidecarError(`${label} is missing`)
  return value
}
