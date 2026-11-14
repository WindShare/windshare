import type { BrowserReceiveWindow } from '../../../output/browser/environment'
import { deferArtifactAuthority } from '../deferred-authority'
import { browserDirectZipCapabilities } from './capabilities'
import type { BrowserDirectZipCompositionPort } from './contracts'
import type { BrowserDirectZipProductionOptions } from './production'

export function createLazyBrowserDirectZipComposition(windowPort: BrowserReceiveWindow, options: BrowserDirectZipProductionOptions): BrowserDirectZipCompositionPort {
  let loaded: Promise<BrowserDirectZipCompositionPort> | undefined
  const load = () => loaded ??= import('./production').then(module =>
    module.createBrowserDirectZipComposition(windowPort, options)).catch((error: unknown) => {
      loaded = undefined
      throw error
    })
  const capabilities = browserDirectZipCapabilities(options.openJournal)
  return {
    capabilities: {
      read: async signal => {
        const facts = await capabilities.read(signal)
        if (facts.authority.kind === 'owned-target-session-v1' && typeof windowPort.showDirectoryPicker === 'function') {
          void load().catch(() => undefined)
        }
        return facts
      },
    },
    runtime: {
      startFresh: input => {
        // The acquisition adapter already invoked the picker synchronously.
        // Observe its rejection even if loading the implementation fails first.
        input.pickedParent.catch(() => undefined)
        return deferArtifactAuthority(load().then(value => value.runtime.startFresh(input)))
      },
      dispatchBootstrapCandidate: async (candidate, signal) => {
        const value = await load()
        signal.throwIfAborted()
        return value.runtime.dispatchBootstrapCandidate(candidate, signal)
      },
      resume: async (operation, signal, failures) => {
        const value = await load()
        signal.throwIfAborted()
        return value.runtime.resume(operation, signal, failures)
      },
      deleteRetained: async (operation, signal, failures) => {
        const value = await load()
        signal.throwIfAborted()
        return value.runtime.deleteRetained(operation, signal, failures)
      },
    },
  }
}
