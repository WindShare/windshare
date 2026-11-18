import { useCallback, useEffect, useRef, useState } from 'react'
import { messageOf } from './request'

export function useResource<T>(read: (signal: AbortSignal) => Promise<T>) {
  const active = useRef<AbortController | null>(null)
  const [value, setValue] = useState<T>()
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(true)
  const start = useCallback(() => {
    active.current?.abort()
    const controller = new AbortController()
    active.current = controller
    void read(controller.signal).then(result => {
      if (!controller.signal.aborted) {
        setValue(result)
        setError('')
      }
    }).catch(reason => {
      if (!controller.signal.aborted) setError(messageOf(reason))
    }).finally(() => {
      if (!controller.signal.aborted) setBusy(false)
    })
  }, [read])
  const reload = useCallback(() => {
    setBusy(true)
    setError('')
    start()
  }, [start])
  useEffect(() => {
    start()
    return () => active.current?.abort()
  }, [start])

  // A read started before a committed write must never overwrite the receipt.
  function update(merge: (previous: T) => T) {
    active.current?.abort()
    setValue(previous => previous === undefined ? previous : merge(previous))
    setBusy(false)
    setError('')
  }
  return { value, busy, error, reload, update }
}

export interface Feedback {
  kind: 'success' | 'error'
  message: string
}

export function useAction() {
  const [busy, setBusy] = useState(false)
  const [feedback, setFeedback] = useState<Feedback | null>(null)
  const running = useRef(false)
  async function run(operation: () => Promise<string | void>) {
    if (running.current) return
    running.current = true
    setBusy(true)
    setFeedback(null)
    try {
      const message = await operation()
      if (message) setFeedback({ kind: 'success', message })
    } catch (error) {
      setFeedback({ kind: 'error', message: messageOf(error) })
    } finally {
      running.current = false
      setBusy(false)
    }
  }
  return { busy, feedback, run }
}
