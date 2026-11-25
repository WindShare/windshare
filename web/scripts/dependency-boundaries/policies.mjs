export const TRANSFER_BOUNDARY = Object.freeze({
  name: 'transfer contracts',
  roots: ['transfer'],
  forbidden: ['output', 'application', 'ui'],
})

const OUTPUT_PUBLIC_SURFACE = Object.freeze({ root: 'output', publicSubtree: 'output/model' })
// Pure wire/endpoint/verification contracts live under network/contracts; all
// other network/ files (directory fetch, IndexedDB cache) are runtime I/O.
const NETWORK_CONTRACTS_SURFACE = Object.freeze({ root: 'network', publicSubtree: 'network/contracts' })

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
      'catalog', 'content', 'contracts', 'crypto', 'protocol', 'unicode',
      'session', 'connectivity', 'receiver', 'transport', 'transfer',
      'output', 'diagnostics', 'security', 'preview', 'network',
    ],
    forbidden: ['application', 'ui', 'package:react', 'package:react-dom'],
  },
  {
    name: 'wire contracts do not depend on runtime implementations',
    roots: ['protocol', 'crypto', 'contracts', 'unicode', 'network/contracts'],
    publicSurfaces: [NETWORK_CONTRACTS_SURFACE],
    forbidden: [
      'catalog', 'content', 'session', 'connectivity', 'receiver', 'transport',
      'transfer', 'output', 'diagnostics', 'preview', 'application', 'ui',
    ],
  },
  {
    name: 'domain and runtime layers do not depend on diagnostics',
    roots: [
      'catalog', 'connectivity', 'content', 'network', 'output',
      'preview', 'receiver', 'session', 'transfer', 'transport',
    ],
    forbidden: ['diagnostics'],
  },
  {
    name: 'diagnostics stays decoupled from domain and runtime layers',
    roots: ['diagnostics'],
    forbidden: [
      'catalog', 'connectivity', 'content', 'network', 'output',
      'preview', 'receiver', 'session', 'transfer', 'transport',
    ],
  },
  {
    name: 'protocol sessions do not depend on connectivity or storage implementations',
    roots: ['session'],
    publicSurfaces: [NETWORK_CONTRACTS_SURFACE],
    forbidden: ['connectivity', 'receiver', 'transport', 'output', 'application', 'ui'],
  },
  {
    name: 'output models expose facts and pure queries',
    roots: ['output/model'],
    publicSurfaces: [OUTPUT_PUBLIC_SURFACE, NETWORK_CONTRACTS_SURFACE],
    forbidden: ['application', 'ui', 'package:react', 'package:react-dom', 'connectivity', 'receiver', 'transport'],
  },
  {
    name: 'presentation delegates output ownership to the application',
    roots: ['ui'],
    publicSurfaces: [OUTPUT_PUBLIC_SURFACE],
    forbidden: [],
    // Commands and diagnostic services own their effects. Public models remain
    // traversable so a forwarding module cannot conceal private output access.
    stopAt: ['application', 'diagnostics'],
  },
])
