import { useRef, useSyncExternalStore } from 'react'
import type { ReceiverController } from '../controller'
import { ShareContent } from '../share-content/ShareContent'
import { MediaPreview, type PreviewActions } from '../share-content/MediaPreview'
import { DetailSheet } from '../controls/DetailSheet'

export function ReceiverBrowsing({ controller }: { readonly controller: ReceiverController }) {
  const source = controller.presentation.browsing
  const snapshot = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const invoker = useRef<HTMLButtonElement>(null)
  const previewActions: PreviewActions = {
    close: () => controller.cancelPreview(), seek: seconds => controller.seekPreview(seconds),
    presented: id => controller.previewMediaPresented(id), failed: id => controller.previewMediaFailed(id),
  }
  const share = snapshot.share
  if (share === null && snapshot.breadcrumbs.length === 0) return null
  return <>
    <ShareContent share={share} preview={snapshot.preview} previewActions={previewActions}
      explorer={{ rows: snapshot.rows, breadcrumbs: snapshot.breadcrumbs,
        pageIndex: snapshot.pageIndex, pageCount: snapshot.pageCount, omittedCount: snapshot.omittedCount,
        browse: snapshot.browse, draft: snapshot.draft, actions: {
          openDirectory: id => controller.openDirectory(id), openBreadcrumb: index => controller.openBreadcrumb(index),
          showPage: page => controller.showPage(page), preview: (id, element) => { invoker.current = element; controller.previewFile(id) },
          toggle: id => controller.toggleSelection(id), enterSelection: () => controller.enterSelectionMode(),
          exitSelection: () => controller.exitSelectionMode(), selectPage: () => controller.selectPage(),
          clearSelection: () => controller.clearSelection(), retry: () => controller.retryDirectory(),
        } }} />
    {(share === null || share.kind === 'browser') && snapshot.preview.state !== 'idle' &&
      <DetailSheet title={snapshot.preview.name} onClose={previewActions.close} returnFocus={invoker} className="preview-sheet">
        <MediaPreview preview={snapshot.preview} actions={previewActions} />
      </DetailSheet>}
  </>
}
