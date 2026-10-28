import { useId, useRef, type ReactNode, type RefObject } from 'react'
import { PortalIcon, type PortalIconName } from './PortalIcon'

export function PortalPopover({ label, icon, className, children, panelRef }: {
  readonly label: string
  readonly icon: PortalIconName
  readonly className: string
  readonly children: ReactNode
  readonly panelRef?: RefObject<HTMLDivElement | null>
}) {
  const id = useId()
  const ownPanel = useRef<HTMLDivElement>(null)
  const panel = panelRef ?? ownPanel
  const dismiss = () => panel.current?.hidePopover()

  // Native popovers own light dismissal, Escape, focus return, and mutual exclusion.
  return <div className={`portal-popover ${className}`} onBlur={event => {
    if (event.relatedTarget instanceof Node && !event.currentTarget.contains(event.relatedTarget)) dismiss()
  }}>
    <button className="portal-popover-trigger" type="button" popoverTarget={id}
      aria-label={label} title={label}><PortalIcon name={icon} size={19} /></button>
    <div ref={panel} id={id} popover="auto" className="portal-popover-panel">
      {children}
    </div>
  </div>
}
