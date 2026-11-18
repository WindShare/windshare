import type { ReactNode } from 'react'
import { RouteMap } from './artwork/RouteMap'
import { Icon, type IconName } from './Icon'
import './focus.css'

interface FocusProps {
  label?: string
  title: string
  icon: IconName
  children?: ReactNode
}

function FocusHeading({ label, title, icon }: FocusProps) {
  return <header className="focus-heading">
    <div className="focus-context"><span className="context-icon"><Icon name={icon} size={19} /></span>{label}</div>
    <h1>{title}</h1>
  </header>
}

export function FocusPage({ children, ...heading }: FocusProps) {
  return <div className="focus-page"><section className="focus-card">
    <FocusHeading {...heading} /><div className="focus-body">{children}</div>
  </section></div>
}

export function EntryPage({ children, ...heading }: FocusProps) {
  return <div className="focus-page"><section className="entry-card">
    <div className="entry-map">
      <RouteMap />
      <div className="map-host"><Icon name="globe" size={15} /><bdi>{window.location.host}</bdi></div>
    </div>
    <div className="entry-content"><FocusHeading {...heading} /><div className="focus-body">{children}</div></div>
  </section></div>
}
