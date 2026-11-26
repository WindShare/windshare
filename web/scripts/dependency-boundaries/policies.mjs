export const TRANSFER_BOUNDARY = Object.freeze({
  name: 'transfer contracts',
  roots: ['transfer'],
  forbidden: ['output', 'application', 'ui'],
})

const OUTPUT_PUBLIC_SURFACE = Object.freeze({ root: 'output', publicSubtree: 'output/model' })
// Pure wire/endpoint/verification contracts live under network/contracts; all
// other network/ files (directory fetch, IndexedDB cache) are runtime I/O.
const NETWORK_CONTRACTS_SURFACE = Object.freeze({ root: 'network', publicSubtree: 'network/contracts' })

const PURE_FOUNDATION_NON_NETWORK = Object.freeze([
  'protocol', 'crypto', 'contracts', 'unicode', 'security',
])
const PURE_CONTRACTS_AND_SECURITY = Object.freeze([
  ...PURE_FOUNDATION_NON_NETWORK, 'network/contracts',
])
const DOMAIN_AND_RUNTIME_NON_NETWORK = Object.freeze([
  'catalog', 'connectivity', 'content', 'output',
  'preview', 'receiver', 'session', 'transfer', 'transport',
])
const DOMAIN_AND_RUNTIME = Object.freeze([
  ...DOMAIN_AND_RUNTIME_NON_NETWORK, 'network',
])

export const BOUNDARY_POLICIES = Object.freeze([
  TRANSFER_BOUNDARY,
  {
    name: 'application owns orchestration',
    roots: ['application'],
    forbidden: ['ui', 'package:react', 'package:react-dom'],
  },
  {
    name: 'reusable modules do not depend on the application or presentation',
    roots: [
      ...DOMAIN_AND_RUNTIME,
      ...PURE_FOUNDATION_NON_NETWORK,
      'diagnostics',
    ],
    forbidden: ['application', 'ui', 'package:react', 'package:react-dom'],
  },
  {
    name: 'pure contracts and security primitives do not depend on runtime implementations',
    roots: PURE_CONTRACTS_AND_SECURITY,
    forbidden: [
      ...DOMAIN_AND_RUNTIME_NON_NETWORK,
      'diagnostics', 'application', 'ui',
    ],
  },
  {
    name: 'domain and runtime layers do not depend on diagnostics',
    roots: DOMAIN_AND_RUNTIME,
    forbidden: ['diagnostics'],
  },
  {
    name: 'diagnostics stays decoupled from domain and runtime layers',
    roots: ['diagnostics'],
    forbidden: DOMAIN_AND_RUNTIME,
  },
  {
    name: 'reusable layers outside network only depend on network contracts',
    roots: [
      ...DOMAIN_AND_RUNTIME_NON_NETWORK,
      ...PURE_CONTRACTS_AND_SECURITY,
    ],
    publicSurfaces: [NETWORK_CONTRACTS_SURFACE],
    forbidden: [],
  },
  {
    name: 'protocol sessions do not depend on connectivity or storage implementations',
    roots: ['session'],
    forbidden: ['connectivity', 'receiver', 'transport', 'output', 'application', 'ui'],
  },
  {
    name: 'output models expose facts and pure queries',
    roots: ['output/model'],
    publicSurfaces: [OUTPUT_PUBLIC_SURFACE],
    forbidden: ['application', 'ui', 'package:react', 'package:react-dom', 'connectivity', 'receiver', 'transport'],
  },
  {
    name: 'presentation delegates output and network runtime ownership to the application',
    roots: ['ui'],
    publicSurfaces: [OUTPUT_PUBLIC_SURFACE, NETWORK_CONTRACTS_SURFACE],
    forbidden: [],
    // Commands and diagnostic services own their effects. Public models and
    // network contracts remain traversable so forwarding modules cannot conceal
    // private output or network runtime access.
    stopAt: ['application', 'diagnostics'],
  },
])
