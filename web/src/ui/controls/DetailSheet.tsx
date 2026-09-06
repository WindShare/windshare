import { useEffect, useId, useRef, type ReactNode } from 'react'

export interface DetailSheetProps {
  readonly title: string
  readonly children: ReactNode
  readonly onClose: () => void
  readonly className?: string
  readonly returnLabel?: string
}

/** Native modal ownership keeps keyboard focus and background interaction aligned. */
export function DetailSheet({ title, children, onClose, className = '', returnLabel = 'Back to share' }: DetailSheetProps) {
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
      onCancel={event => { event.preventDefault(); close.current() }}
      onClick={event => { if (event.target === event.currentTarget) close.current() }}>
      <div className="detail-sheet-body">
        <header className="detail-sheet-heading">
          <h2 id={titleId}>{title}</h2>
          <button type="button" onClick={onClose} aria-label={returnLabel}>{returnLabel}</button>
        </header>
        {children}
      </div>
    </dialog>
  )
}
