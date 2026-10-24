export const TRANSFER_BOUNDARY = Object.freeze({
  name: 'transfer contracts',
  roots: ['transfer'],
  forbidden: ['output', 'application', 'ui'],
})

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
    name: 'presentation delegates output ownership to the application',
    roots: ['ui'],
    forbidden: [
      'application/browser-receive',
      'output/browser', 'output/origin-private', 'output/execution', 'output/persistent-tree',
      'output/portable', 'output/direct-zip/browser', 'output/direct-zip/browser-journal',
      'output/direct-zip/target', 'output/direct-zip/writer', 'output/direct-zip/journal',
      'output/file-system-access/execution', 'output/file-system-access/session.ts',
      'output/browser-delivery/assembly.ts', 'output/browser-delivery/engine.ts',
      'output/browser-delivery/session.ts', 'output/browser-delivery/repository.ts',
      'output/browser-delivery/indexeddb', 'output/browser-delivery/indexeddb.ts',
      'output/browser-delivery/staging.ts', 'output/browser-delivery/staging-root.ts',
      'output/browser-delivery/retained-authority.ts', 'output/browser-delivery/recovery/authority.ts',
      'output/browser-delivery/recovery/inventory.ts', 'output/browser-delivery/recovery/mutation-journal.ts',
      'output/resume/reopen-authority.ts',
      'output/workspace/repository.ts', 'output/workspace/stages',
    ],
    // Application commands and diagnostic services encapsulate their effects;
    // presentation can still consume pure domain models without duplicate types.
    stopAt: ['application', 'diagnostics'],
  },
])
