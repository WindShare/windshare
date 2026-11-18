import type { ReactNode } from 'react'
import { Icon, type IconName } from '../Icon'

interface Destination {
  href: string
  label: string
  icon: IconName
  current: boolean
}

export function WorkspaceNav({ label, destinations, action }: {
  label: string; destinations: Destination[]; action?: ReactNode
}) {
  return <div className="workspace-bar"><nav aria-label={label} className="workspace-nav">
    {destinations.map(item => <a key={item.href} href={item.href} aria-current={item.current ? 'page' : undefined}>
      <Icon name={item.icon} size={17} />{item.label}</a>)}
  </nav>{action}</div>
}
