import { useEffect, useId, useRef, type ReactNode, type RefObject } from 'react'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'

export interface DetailSheetProps {
  readonly title: string
  readonly children: ReactNode
  readonly onClose: () => void
  readonly returnFocus: RefObject<HTMLElement | null>
  readonly className?: string
  readonly returnLabel?: string
  readonly subtitle?: string
  readonly navigation?: ReactNode
}

/** Native modal ownership keeps keyboard focus and background interaction aligned. */
export function DetailSheet({ title, children, onClose, returnFocus, className = '', returnLabel = 'Back to share', subtitle, navigation }: DetailSheetProps) {
  const dialog = useRef<HTMLDialogElement>(null)
  const titleId = useId()
  const close = useRef(onClose)
  useEffect(() => { close.current = onClose }, [onClose])
  useEffect(() => {
    const element = dialog.current
    // Pointer activation does not focus buttons in every browser. The caller owns
    // the return target; snapshot it before nested sheets or later actions can change it.
    const origin = returnFocus.current
    element?.showModal()
    return () => {
      element?.close()
      if (origin?.isConnected) origin.focus()
    }
  }, [returnFocus])
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
