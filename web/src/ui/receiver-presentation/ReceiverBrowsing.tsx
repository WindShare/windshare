import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react'
import { canPreviewFile } from '../../preview/capability'
import type { ReceiverController } from '../controller'
import { DetailSheet } from '../controls/DetailSheet'
import type { ReceiverFailure } from '../model'
import { MediaPreview, type PreviewActions, type PreviewDownloadControl, type PreviewNavigation } from '../share-content/MediaPreview'
import { PreviewTaskStrip } from '../share-content/PreviewTaskStrip'
import { ShareContent } from '../share-content/ShareContent'
import { singleFileDownloadState, type SingleFileDownloadReadiness } from '../share-content/single-file-admission'

const STARTED_FEEDBACK_MS = 1_500

type PreviewModalError =
  | Readonly<{ kind: 'file'; fileId: string; failure: ReceiverFailure }>
  | Readonly<{ kind: 'task'; failure: ReceiverFailure }>

export function ReceiverBrowsing({ controller }: { readonly controller: ReceiverController }) {
  const source = controller.presentation.browsing
  const snapshot = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const invoker = useRef<HTMLButtonElement>(null)
  const [startedFeedback, setStartedFeedback] = useState<Readonly<{ fileId: string; nonce: number }> | null>(null)
  const [modalError, setModalError] = useState<PreviewModalError | null>(null)

  useEffect(() => {
    if (startedFeedback === null) return
    const timer = setTimeout(() => setStartedFeedback(null), STARTED_FEEDBACK_MS)
    return () => clearTimeout(timer)
  }, [startedFeedback])

  const clearPreviewFeedback = () => {
    setStartedFeedback(null)
    setModalError(null)
  }

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

  const openPreviewItem = (id: string) => {
    setStartedFeedback(null)
    setModalError(previous => (previous?.kind === 'file' ? null : previous))
    controller.previewFile(id)
  }

  const previewActions: PreviewActions = {
    close: () => {
      clearPreviewFeedback()
      controller.cancelPreview()
    },
    seek: seconds => controller.seekPreview(seconds),
    presented: id => controller.previewMediaPresented(id),
    failed: id => controller.previewMediaFailed(id),
  }

  const share = snapshot.share
  if (share === null && snapshot.breadcrumbs.length === 0) return null

  const downloadReadiness: SingleFileDownloadReadiness = {
    startAdmission: snapshot.startAdmission,
    capabilitiesState: snapshot.capabilitiesState,
    activeSingleFileId: snapshot.activeSingleFileId,
    singleFilePickerPendingFromBytes: snapshot.singleFilePickerPendingFromBytes,
    language: snapshot.language,
  }

  const preview = snapshot.preview
  const previewableRows = snapshot.rows.filter(row => row.kind === 'file' && canPreviewFile(row.name, row.expectedSize))
  const previewIndex = preview.state === 'idle'
    ? -1
    : previewableRows.findIndex(row => row.id === preview.fileId)
  const navigation: PreviewNavigation | undefined = previewableRows.length > 1 && previewIndex >= 0
    ? {
        index: previewIndex,
        total: previewableRows.length,
        previous: () => {
          const target = previewableRows[previewIndex - 1]
          if (target !== undefined) openPreviewItem(target.id)
        },
        next: () => {
          const target = previewableRows[previewIndex + 1]
          if (target !== undefined) openPreviewItem(target.id)
        },
      }
    : undefined

  let previewDownload: PreviewDownloadControl | undefined
  if (preview.state !== 'idle') {
    const fileId = preview.fileId
    const row = snapshot.rows.find(candidate => candidate.id === fileId)
    const state = preview.state === 'image'
      ? { downloading: false, disabledReason: null }
      : singleFileDownloadState(fileId, row?.expectedSize, downloadReadiness)
    previewDownload = {
      downloading: state.downloading,
      started: startedFeedback?.fileId === fileId,
      disabledReason: state.disabledReason,
      onDownload: () => {
        setModalError(null)
        const result = controller.downloadPreviewedFile({
          onExported: () => showStartedFeedback(fileId),
          onAsyncFailure: failure => setModalError({ kind: 'file', fileId, failure }),
        })
        if (result.kind === 'saved-image') {
          showStartedFeedback(fileId)
        } else if (result.kind === 'failed') {
          setModalError({ kind: 'file', fileId, failure: result.failure })
        }
      },
    }
  }

  let activeModalError: ReceiverFailure | null = null
  if (preview.state !== 'idle' && modalError !== null) {
    if (modalError.kind === 'task' || modalError.fileId === preview.fileId) {
      activeModalError = modalError.failure
    }
  }

  return <>
    <ShareContent share={share} preview={preview} previewActions={previewActions}
      explorer={{
        rows: snapshot.rows,
        breadcrumbs: snapshot.breadcrumbs,
        pageIndex: snapshot.pageIndex,
        pageCount: snapshot.pageCount,
        omittedCount: snapshot.omittedCount,
        browse: snapshot.browse,
        draft: snapshot.draft,
        downloadReadiness,
        actions: {
          openDirectory: id => controller.openDirectory(id),
          openBreadcrumb: index => controller.openBreadcrumb(index),
          showPage: page => controller.showPage(page),
          preview: (id, element) => {
            invoker.current = element
            openPreviewItem(id)
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
    {(share === null || share.kind === 'browser') && preview.state !== 'idle' &&
      <DetailSheet title={preview.name} onClose={previewActions.close} returnFocus={invoker} className="preview-sheet">
        <MediaPreview
          preview={preview}
          actions={previewActions}
          {...(navigation === undefined ? {} : { navigation })}
          {...(previewDownload === undefined ? {} : { download: previewDownload })}
          downloadError={activeModalError}
          statusStrip={<PreviewTaskStrip
            controller={controller}
            onActionFailure={handleTaskActionFailure}
            onClearActionFailure={handleClearTaskActionFailure}
          />}
        />
      </DetailSheet>}
  </>
}
