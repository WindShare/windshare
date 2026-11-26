import { useCallback, useEffect, useRef, useState, useSyncExternalStore, type RefObject } from 'react'
import { canPreviewFile } from '../../preview/capability'
import type { ReceiverController } from '../controller'
import { DetailSheet } from '../controls/DetailSheet'
import type { BrowseRow, PreviewSnapshot, ReceiverFailure } from '../model'
import { MediaPreview, type PreviewActions, type PreviewDownloadControl, type PreviewNavigation } from '../share-content/MediaPreview'
import { PreviewTaskStrip } from '../share-content/PreviewTaskStrip'
import { ShareContent } from '../share-content/ShareContent'
import { singleFileDownloadState, type SingleFileDownloadReadiness } from '../share-content/single-file-admission'

const STARTED_FEEDBACK_MS = 1_500

type ActivePreviewSnapshot = Exclude<PreviewSnapshot, { state: 'idle' }>

type PreviewModalError =
  | Readonly<{ kind: 'file'; fileId: string; failure: ReceiverFailure }>
  | Readonly<{ kind: 'task'; failure: ReceiverFailure }>

/**
 * Owns ephemeral preview-modal feedback (`startedFeedback` and `modalError`) so
 * external preview closure and share switches unmount and reset it automatically,
 * while prev/next stepping inside an open modal preserves task-strip errors.
 */
function PreviewModal({
  controller,
  preview,
  rows,
  downloadReadiness,
  returnFocus,
  previewActions,
}: {
  readonly controller: ReceiverController
  readonly preview: ActivePreviewSnapshot
  readonly rows: readonly BrowseRow[]
  readonly downloadReadiness: SingleFileDownloadReadiness
  readonly returnFocus: RefObject<HTMLButtonElement | null>
  readonly previewActions: PreviewActions
}) {
  const [startedFeedback, setStartedFeedback] = useState<Readonly<{ fileId: string; nonce: number }> | null>(null)
  const [modalError, setModalError] = useState<PreviewModalError | null>(null)

  useEffect(() => {
    if (startedFeedback === null) return
    const timer = setTimeout(() => setStartedFeedback(null), STARTED_FEEDBACK_MS)
    return () => clearTimeout(timer)
  }, [startedFeedback])

  const handleTaskActionFailure = useCallback((failure: ReceiverFailure) => {
    setModalError({ kind: 'task', failure })
  }, [])

  const handleClearTaskActionFailure = useCallback(() => {
    setModalError(previous => (previous?.kind === 'task' ? null : previous))
  }, [])

  const showStartedFeedback = (fileId: string) => {
    setModalError(previous => (previous?.kind === 'file' && previous.fileId === fileId ? null : previous))
    setStartedFeedback(previous => ({ fileId, nonce: (previous?.nonce ?? 0) + 1 }))
  }

  const navigatePreviewItem = (id: string) => {
    setStartedFeedback(null)
    setModalError(previous => (previous?.kind === 'file' ? null : previous))
    controller.previewFile(id)
  }

  const previewableRows = rows.filter(row => row.kind === 'file' && canPreviewFile(row.name, row.expectedSize))
  const previewIndex = previewableRows.findIndex(row => row.id === preview.fileId)
  const navigation: PreviewNavigation | undefined = previewableRows.length > 1 && previewIndex >= 0
    ? {
        index: previewIndex,
        total: previewableRows.length,
        previous: () => {
          const target = previewableRows[previewIndex - 1]
          if (target !== undefined) navigatePreviewItem(target.id)
        },
        next: () => {
          const target = previewableRows[previewIndex + 1]
          if (target !== undefined) navigatePreviewItem(target.id)
        },
      }
    : undefined

  const fileId = preview.fileId
  const state = preview.state === 'image'
    ? { downloading: false, disabledReason: null }
    : singleFileDownloadState(fileId, downloadReadiness)
  const previewDownload: PreviewDownloadControl = {
    downloading: state.downloading,
    started: startedFeedback?.fileId === fileId,
    disabledReason: state.disabledReason,
    onDownload: () => {
      setModalError(null)
      const result = controller.downloadPreviewedFile({
        onAsyncFailure: failure => setModalError({ kind: 'file', fileId, failure }),
      })
      if (result.kind === 'saved-image') {
        showStartedFeedback(fileId)
      } else if (result.kind === 'failed') {
        setModalError({ kind: 'file', fileId, failure: result.failure })
      }
    },
  }

  const activeModalError = modalError !== null && (modalError.kind === 'task' || modalError.fileId === fileId)
    ? modalError.failure
    : null

  return <DetailSheet title={preview.name} onClose={previewActions.close} returnFocus={returnFocus} className="preview-sheet">
    <MediaPreview
      preview={preview}
      actions={previewActions}
      {...(navigation === undefined ? {} : { navigation })}
      download={previewDownload}
      downloadError={activeModalError}
      statusStrip={<PreviewTaskStrip
        controller={controller}
        onActionFailure={handleTaskActionFailure}
        onClearActionFailure={handleClearTaskActionFailure}
      />}
    />
  </DetailSheet>
}

export function ReceiverBrowsing({ controller }: { readonly controller: ReceiverController }) {
  const source = controller.presentation.browsing
  const snapshot = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const invoker = useRef<HTMLButtonElement>(null)

  const previewActions: PreviewActions = {
    close: () => controller.cancelPreview(),
    seek: seconds => controller.seekPreview(seconds),
    presented: id => controller.previewMediaPresented(id),
    failed: id => controller.previewMediaFailed(id),
  }

  const share = snapshot.share
  if (share === null) return null

  const downloadReadiness: SingleFileDownloadReadiness = {
    startAdmission: snapshot.startAdmission,
    capabilitiesState: snapshot.capabilitiesState,
    activeSingleFileId: snapshot.activeSingleFileId,
    language: snapshot.language,
  }

  const preview = snapshot.preview

  return <>
    <ShareContent share={share} preview={preview} previewActions={previewActions}
      explorer={{
        rows: snapshot.rows,
        breadcrumbs: snapshot.breadcrumbs,
        pageIndex: snapshot.pageIndex,
        pageCount: snapshot.pageCount,
        omittedCount: snapshot.omittedCount,
        browse: snapshot.browse,
        directoryRetryable: snapshot.directoryRetryable,
        draft: snapshot.draft,
        downloadReadiness,
        actions: {
          openDirectory: id => controller.openDirectory(id),
          openBreadcrumb: index => controller.openBreadcrumb(index),
          showPage: page => controller.showPage(page),
          preview: (id, element) => {
            invoker.current = element
            controller.previewFile(id)
          },
          download: id => { controller.downloadFile(id) },
          toggle: id => controller.toggleSelection(id),
          enterSelection: () => controller.enterSelectionMode(),
          exitSelection: () => controller.exitSelectionMode(),
          selectPage: () => controller.selectPage(),
          clearSelection: () => controller.clearSelection(),
          retry: () => controller.retryDirectory(),
        },
      }} />
    {share.kind === 'browser' && preview.state !== 'idle' &&
      <PreviewModal
        key={share.shareInstance}
        controller={controller}
        preview={preview}
        rows={snapshot.rows}
        downloadReadiness={downloadReadiness}
        returnFocus={invoker}
        previewActions={previewActions}
      />}
  </>
}
