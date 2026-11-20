import { useLayoutEffect, useRef, useState } from 'react'

export function useListNavigation(showList: boolean) {
  const [visitedList, setVisitedList] = useState(showList)
  const scroll = useRef<number | null>(null)
  if (showList && !visitedList) setVisitedList(true)
  useLayoutEffect(() => {
    if (!showList || scroll.current === null) return
    const top = scroll.current
    const frame = requestAnimationFrame(() => {
      scroll.current = null
      window.scrollTo(0, top)
    })
    return () => cancelAnimationFrame(frame)
  }, [showList])
  // Deep links fetch only their detail. A previously opened list retains its pages and position.
  return { visitedList, leaving: () => { scroll.current = window.scrollY } }
}
