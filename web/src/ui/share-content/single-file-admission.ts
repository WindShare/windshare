import type { BrowserCapabilitiesState } from '../../application/receive/contracts'
import { singleFileNeedsPicker } from '../../application/receive/single-file'
import { presentReceiveReason } from '../experience/receive-reason'
import { receiverCopy } from '../locales/receiver'
import type { ReceiverSnapshot } from '../model'
import type { Language } from '../preferences/model'

export interface SingleFileDownloadReadiness {
  readonly startAdmission: ReceiverSnapshot['startAdmission']
  readonly capabilitiesState: BrowserCapabilitiesState
  readonly activeSingleFileId: string | null
  readonly singleFilePickerPendingFromBytes: bigint | null
  readonly language: Language
}

export interface SingleFileDownloadState {
  readonly downloading: boolean
  readonly disabledReason: string | null
}

/**
 * Resolves row and preview shortcut availability for a single file.
 * Capability probe failure stays clickable so a click retries probing cleanly.
 */
export function singleFileDownloadState(
  fileId: string,
  expectedSize: bigint | undefined,
  readiness: SingleFileDownloadReadiness,
): SingleFileDownloadState {
  const { startAdmission, capabilitiesState, activeSingleFileId, singleFilePickerPendingFromBytes, language } = readiness
  const copy = receiverCopy[language]
  if (activeSingleFileId === fileId) {
    return Object.freeze({ downloading: true, disabledReason: copy.browse.downloading })
  }
  if (!startAdmission.allowed && startAdmission.reason !== null) {
    return Object.freeze({ downloading: false, disabledReason: presentReceiveReason(startAdmission.reason, language) })
  }
  if (capabilitiesState === 'probing') {
    return Object.freeze({ downloading: false, disabledReason: copy.saving.preparingOptions })
  }
  if (capabilitiesState === 'failed') {
    return Object.freeze({ downloading: false, disabledReason: null })
  }
  if (singleFileNeedsPicker(expectedSize, singleFilePickerPendingFromBytes)) {
    return Object.freeze({ downloading: false, disabledReason: copy.saving.preparingOptions })
  }
  return Object.freeze({ downloading: false, disabledReason: null })
}
