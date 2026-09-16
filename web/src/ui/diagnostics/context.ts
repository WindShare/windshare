import { createContext } from 'react'

export const DiagnosticsPanelContext = createContext<((invoker: HTMLElement) => void) | null>(null)
