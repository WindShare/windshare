import { createContext, useContext } from 'react'

export const appearances = ['system', 'light', 'dark'] as const
type Appearance = typeof appearances[number]

export function parseAppearance(value: string | null): Appearance {
  return appearances.find(appearance => appearance === value) ?? 'system'
}

export const AppearanceContext = createContext<{
  appearance: Appearance; selectAppearance: (next: Appearance) => void
} | null>(null)

export function useAppearance() {
  const value = useContext(AppearanceContext)
  if (!value) throw new Error('Missing console appearance provider')
  return value
}
