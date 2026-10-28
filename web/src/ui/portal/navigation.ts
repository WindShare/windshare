const REPOSITORY_URL = 'https://github.com/WindShare/windshare'

export const PORTAL_LINKS = Object.freeze({
  repository: REPOSITORY_URL,
  releases: `${REPOSITORY_URL}/releases`,
  installation: `${REPOSITORY_URL}/blob/main/docs/installation.md`,
  deployment: `${REPOSITORY_URL}/blob/main/scripts/deploy/wsrelay/README.md`,
  cloudflare: `${REPOSITORY_URL}/blob/main/docs/deploy/cloudflare-relay.md`,
  protocol: `${REPOSITORY_URL}/blob/main/docs/protocol.md`,
  issues: `${REPOSITORY_URL}/issues`,
})

export const PORTAL_SECTIONS = Object.freeze({
  features: { id: 'features', labelKey: 'navFeatures' },
  howItWorks: { id: 'how-it-works', labelKey: 'navWorkflow' },
  cli: { id: 'cli', labelKey: 'navShare' },
  selfHost: { id: 'self-host', labelKey: 'navSelfHost' },
} as const)

export function isPortalFragment(fragment: string): boolean {
  return Object.values(PORTAL_SECTIONS).some(section => fragment === `#${section.id}`)
}
