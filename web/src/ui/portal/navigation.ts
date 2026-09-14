export const PORTAL_SECTIONS = Object.freeze({
  features: { id: 'features', label: '核心优势' },
  howItWorks: { id: 'how-it-works', label: '工作原理' },
  cli: { id: 'cli', label: 'CLI & 客户端' },
  selfHost: { id: 'self-host', label: '自建中转' },
})

export function isPortalFragment(fragment: string): boolean {
  return Object.values(PORTAL_SECTIONS).some(section => fragment === `#${section.id}`)
}
