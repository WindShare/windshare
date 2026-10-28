import { useEffect, useRef } from 'react'
import { usePreferences } from '../../preferences/use-preferences'
import { createSpiritMotion, pointerPose, type SpiritPose } from './motion'

const MILLISECONDS_PER_SECOND = 1000
const REDUCED_MOTION_QUERY = '(prefers-reduced-motion: reduce)'

export function useSpiritMotion() {
  const { motion: motionPreference } = usePreferences()
  const scene = useRef<HTMLElement>(null)
  const mascot = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    const element = scene.current
    const button = mascot.current
    if (!element || !button) return
    const motion = createSpiritMotion()
    const reducedMotion = window.matchMedia(REDUCED_MOTION_QUERY)
    let visible = false
    let frame: number | null = null
    let lastTime = 0
    const available = () => visible && !document.hidden && !reducedMotion.matches && motionPreference !== 'reduced'
    const paint = (pose: SpiritPose) => {
      element.style.setProperty('--spirit-x', `${pose.x}px`)
      element.style.setProperty('--spirit-y', `${pose.y}px`)
      element.style.setProperty('--spirit-tilt', `${pose.tilt}deg`)
    }
    const tick = (now: number) => {
      frame = null
      const elapsed = Math.max(0, now - lastTime) / MILLISECONDS_PER_SECOND
      const next = motion.advance(elapsed, now)
      lastTime = now
      paint(next.pose)
      if (next.moving) frame = window.requestAnimationFrame(tick)
    }
    const wake = () => {
      if (frame !== null || !available()) return
      lastTime = performance.now()
      frame = window.requestAnimationFrame(tick)
    }
    const reset = () => {
      if (frame !== null) window.cancelAnimationFrame(frame)
      frame = null
      paint(motion.reset())
    }
    const updateAvailability = () => {
      element.dataset.active = String(available())
      if (!available()) reset()
    }
    const follow = (event: PointerEvent) => {
      if (event.pointerType !== 'mouse' || !available()) return
      if (event.buttons !== 0) {
        motion.release()
        return
      }
      // Measure the stationary hit target, never the animated artwork, to prevent
      // motion feeding back into pointer geometry or making the button evade a click.
      motion.follow(pointerPose(event.clientX, event.clientY, button.getBoundingClientRect()), performance.now())
      wake()
    }
    const release = () => {
      motion.release()
      wake()
    }
    const tap = () => {
      if (!available()) return
      motion.tap()
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
  }, [motionPreference])

  return { scene, mascot }
}
