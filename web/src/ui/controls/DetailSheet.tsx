import { useEffect, useId, useRef, type ReactNode } from 'react'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'

export interface DetailSheetProps {
  readonly title: string
  readonly children: ReactNode
  readonly onClose: () => void
  readonly className?: string
  readonly returnLabel?: string
  readonly subtitle?: string
  readonly navigation?: ReactNode
}

/** Native modal ownership keeps keyboard focus and background interaction aligned. */
export function DetailSheet({ title, children, onClose, className = '', returnLabel = 'Back to share', subtitle, navigation }: DetailSheetProps) {
  const dialog = useRef<HTMLDialogElement>(null)
  const titleId = useId()
  const close = useRef(onClose)
  useEffect(() => { close.current = onClose }, [onClose])
  useEffect(() => {
    const element = dialog.current
    const origin = document.activeElement instanceof HTMLElement ? document.activeElement : null
    element?.showModal()
    return () => {
      element?.close()
      if (origin?.isConnected) origin.focus()
    }
  }, [])
  return (
    <dialog ref={dialog} className={`detail-sheet ${className}`} aria-labelledby={titleId}
      onCancel={event => {
        // Nested native dialogs still share React's event tree; Escape belongs to the topmost sheet.
        event.preventDefault()
        event.stopPropagation()
        close.current()
      }}
      onClick={event => { if (event.target === event.currentTarget) close.current() }}>
      <div className="detail-sheet-body">
        <header className="detail-sheet-heading">
          <div className="detail-sheet-titles">
            {navigation}
            <h2 id={titleId}>{title}</h2>
            {subtitle !== undefined && <p className="detail-sheet-subtitle">{subtitle}</p>}
          </div>
          <button className="quiet-action detail-sheet-return" type="button" onClick={onClose} aria-label={returnLabel} title={returnLabel}>
            <ReceiverIcon name="close" />
          </button>
        </header>
        {children}
      </div>
    </dialog>
  )
}
