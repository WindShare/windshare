export const TRANSFER_BOUNDARY = Object.freeze({
  name: 'transfer contracts',
  roots: ['transfer'],
  forbidden: ['output', 'application', 'ui'],
})

const OUTPUT_PUBLIC_SURFACE = Object.freeze({ root: 'output', publicSubtree: 'output/model' })

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
      'output', 'diagnostics', 'security', 'preview',
    ],
    forbidden: ['application', 'ui', 'package:react', 'package:react-dom'],
  },
  {
    name: 'wire contracts do not depend on runtime implementations',
    roots: ['protocol', 'crypto', 'contracts', 'unicode'],
    forbidden: [
      'catalog', 'content', 'session', 'connectivity', 'receiver', 'transport',
      'transfer', 'output', 'diagnostics', 'application', 'ui',
    ],
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
    name: 'presentation delegates output ownership to the application',
    roots: ['ui'],
    publicSurfaces: [OUTPUT_PUBLIC_SURFACE],
    forbidden: ['application/browser-receive'],
    // Commands and diagnostic services own their effects. Public models remain
    // traversable so a forwarding module cannot conceal private output access.
    stopAt: ['application', 'diagnostics'],
  },
])
