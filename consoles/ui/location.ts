import { useLayoutEffect, useRef, useSyncExternalStore } from 'react'

export function takeFragment(name: string) {
  const params = new URLSearchParams(location.hash.slice(1))
  const value = params.get(name)
  if (value !== null) history.replaceState(null, '', location.pathname + location.search)
  return value
}

const subscribe = (listener: () => void) => {
  window.addEventListener('hashchange', listener)
  return () => window.removeEventListener('hashchange', listener)
}

export function usePage() {
  const page = useSyncExternalStore(subscribe, () => location.hash.slice(1))
  const previous = useRef(page)
  useLayoutEffect(() => {
    // The route owner has just committed the destination heading. Focus it so keyboard reading resumes there.
    if (previous.current === page) return
    previous.current = page
    const heading = document.querySelector<HTMLElement>('main h1')
    if (heading) {
      heading.tabIndex = -1
      heading.focus()
    }
  }, [page])
  return page
}
