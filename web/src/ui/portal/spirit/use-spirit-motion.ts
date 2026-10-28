import { useEffect, useRef } from 'react'
import { createSpiritBehavior, type SpiritFrame } from './behavior'
import { pointerPose } from './motion'

const MILLISECONDS_PER_SECOND = 1000
const REDUCED_MOTION_QUERY = '(prefers-reduced-motion: reduce)'

export function useSpiritMotion() {
  const scene = useRef<HTMLElement>(null)
  const mascot = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    const element = scene.current
    const button = mascot.current
    if (!element || !button) return
    const behavior = createSpiritBehavior(Math.random)
    const reducedMotion = window.matchMedia(REDUCED_MOTION_QUERY)
    let visible = false
    let active = false
    let frame: number | null = null
    let idleTimer: number | null = null
    let lastTime = 0
    const paint = ({ pose, face }: SpiritFrame) => {
      element.style.setProperty('--spirit-x', `${pose.x}px`)
      element.style.setProperty('--spirit-y', `${pose.y}px`)
      element.style.setProperty('--spirit-tilt', `${pose.tilt}deg`)
      element.style.setProperty('--spirit-eye-openness', String(face.openness))
      element.style.setProperty('--spirit-smile', String(face.smile))
    }
    const clearIdleTimer = () => {
      if (idleTimer !== null) window.clearTimeout(idleTimer)
      idleTimer = null
    }
    const tick = (now: number) => {
      frame = null
      const elapsed = Math.max(0, now - lastTime) / MILLISECONDS_PER_SECOND
      const next = behavior.advance(elapsed, now)
      lastTime = now
      paint(next)
      if (next.animating) frame = window.requestAnimationFrame(tick)
      else if (next.wakeAt !== null) {
        // Quiet intervals need one deadline, not a continuously running animation loop.
        idleTimer = window.setTimeout(wake, Math.max(0, next.wakeAt - performance.now()))
      }
    }
    const wake = () => {
      clearIdleTimer()
      if (frame !== null || !active) return
      lastTime = performance.now()
      frame = window.requestAnimationFrame(tick)
    }
    const reset = () => {
      if (frame !== null) window.cancelAnimationFrame(frame)
      frame = null
      clearIdleTimer()
      paint(behavior.suspend())
    }
    const updateAvailability = () => {
      const available = visible && !document.hidden && !reducedMotion.matches
      if (active === available) return
      active = available
      element.dataset.active = String(active)
      reset()
      if (active) {
        behavior.resume(performance.now())
        wake()
      }
    }
    const follow = (event: PointerEvent) => {
      if (event.pointerType !== 'mouse' || !active) return
      if (event.buttons !== 0) {
        behavior.release()
        return
      }
      // Measure the stationary hit target so animated artwork cannot evade a click.
      behavior.follow(pointerPose(event.clientX, event.clientY, button.getBoundingClientRect()), performance.now())
      wake()
    }
    const release = () => {
      behavior.release()
      wake()
    }
    const tap = () => {
      if (!active) return
      behavior.tap(performance.now())
      wake()
    }
    const observer = new IntersectionObserver(entries => {
      visible = entries[0]?.isIntersecting ?? false
      updateAvailability()
    })
    observer.observe(element)
    element.addEventListener('pointermove', follow)
    element.addEventListener('pointerleave', release)
    element.addEventListener('pointercancel', release)
    button.addEventListener('click', tap)
    document.addEventListener('visibilitychange', updateAvailability)
    reducedMotion.addEventListener('change', updateAvailability)
    window.addEventListener('blur', release)
    window.addEventListener('resize', release)
    window.addEventListener('scroll', release, { passive: true, capture: true })
    return () => {
      observer.disconnect()
      element.removeEventListener('pointermove', follow)
      element.removeEventListener('pointerleave', release)
      element.removeEventListener('pointercancel', release)
      button.removeEventListener('click', tap)
      document.removeEventListener('visibilitychange', updateAvailability)
      reducedMotion.removeEventListener('change', updateAvailability)
      window.removeEventListener('blur', release)
      window.removeEventListener('resize', release)
      window.removeEventListener('scroll', release, true)
      reset()
    }
  }, [])

  return { scene, mascot }
}
