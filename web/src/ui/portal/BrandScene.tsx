import { useEffect, useRef, useState, type PointerEvent } from 'react'
import { WindShareMark } from '../brand/WindShareMark'
import { usePreferences } from '../preferences/use-preferences'
import { PortalIcon } from './PortalIcon'
import { usePortalCopy } from './locales/use-portal-copy'

const GREETING_DURATION_MS = 2400
const POINTER_TRAVEL_PX = 7

export function BrandScene() {
  const scene = useRef<HTMLElement>(null)
  const [greeting, setGreeting] = useState(false)
  const { motion } = usePreferences()
  const t = usePortalCopy()

  useEffect(() => {
    const element = scene.current
    if (!element) return
    let visible = false
    const update = () => { element.dataset.active = String(visible && !document.hidden) }
    const observer = new IntersectionObserver(entries => {
      visible = entries[0]?.isIntersecting ?? false
      update()
    })
    observer.observe(element)
    document.addEventListener('visibilitychange', update)
    return () => {
      observer.disconnect()
      document.removeEventListener('visibilitychange', update)
    }
  }, [])

  useEffect(() => {
    if (!greeting) return
    const timer = window.setTimeout(() => setGreeting(false), GREETING_DURATION_MS)
    return () => window.clearTimeout(timer)
  }, [greeting])

  const followPointer = (event: PointerEvent<HTMLElement>) => {
    if (event.pointerType !== 'mouse' || motion === 'reduced' || matchMedia('(prefers-reduced-motion: reduce)').matches) return
    const bounds = event.currentTarget.getBoundingClientRect()
    event.currentTarget.style.setProperty('--spirit-look-x', `${((event.clientX - bounds.left) / bounds.width - 0.5) * POINTER_TRAVEL_PX}px`)
    event.currentTarget.style.setProperty('--spirit-look-y', `${((event.clientY - bounds.top) / bounds.height - 0.5) * POINTER_TRAVEL_PX}px`)
  }

  return <figure ref={scene} className="portal-brand-scene" aria-label={t.sceneLabel}
    data-active="false" data-greeting={greeting} onPointerMove={followPointer} onPointerLeave={event => {
      event.currentTarget.style.setProperty('--spirit-look-x', '0px')
      event.currentTarget.style.setProperty('--spirit-look-y', '0px')
    }}>
    <div className="portal-spirit-halo" aria-hidden="true" />
    <svg className="portal-breeze-lines" viewBox="0 0 480 180" fill="none" aria-hidden="true">
      <path d="M8 98C75 98 73 42 132 42h58m112 74h42c44 0 41-58 91-58h37M39 127h91m205 22h71" />
      <path className="portal-breeze-travel" d="M8 98C75 98 73 42 132 42h58m112 74h42c44 0 41-58 91-58h37" />
    </svg>
    <div className="portal-share-chip" aria-hidden="true"><PortalIcon name="folder" size={22} /><span>{t.routeFiles}</span></div>
    <div className="portal-browser-chip" aria-hidden="true"><PortalIcon name="monitor" size={23} /><span>{t.routeBrowser}</span></div>
    <div className="portal-mascot-parallax">
      <button type="button" className="portal-mascot-button" aria-label={t.mascotLabel}
        title={t.mascotLabel} onClick={() => setGreeting(true)}>
        <span className="portal-spirit-float"><WindShareMark size={176} /></span>
      </button>
    </div>
    <span className="portal-spirit-shadow" aria-hidden="true" />
    <span className="portal-greeting" role="status">{greeting ? t.mascotHello : ''}</span>
    <figcaption>{t.mascotCaption}<PortalIcon name="lock" size={11} /></figcaption>
  </figure>
}
