import { useState } from 'react'
import { useAction } from '../ui/activity'
import type { RelayClient, ServiceCredential } from './api'

type ServiceKeyState =
  | { phase: 'idle' }
  | { phase: 'creating' }
  | { phase: 'ready'; credential: ServiceCredential }

export type ServiceKeyPhase = ServiceKeyState['phase']

export function useServiceKeyCreation(api: RelayClient) {
  const action = useAction()
  const [credential, setCredential] = useState<ServiceCredential | null>(null)

  let state: ServiceKeyState = { phase: 'idle' }
  if (credential) state = { phase: 'ready', credential }
  else if (action.busy) state = { phase: 'creating' }

  function create(name: string) {
    if (credential) return
    void action.run(async () => setCredential(await api.createKey(name)))
  }

  return { state, feedback: action.feedback, create, finish: () => setCredential(null) }
}
