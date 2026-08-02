import type { Page } from '@playwright/test'

const GATEWAY_MODULE_PATH = '/src/ui/v2-gateway.ts'

type GatewayModule = typeof import('../../src/ui/v2-gateway')
type JoinedShare = Awaited<ReturnType<
  InstanceType<GatewayModule['V2BrowserReceiverGateway']>['join']
>>
type BrowseDirectory = ReturnType<JoinedShare['rootDirectory']>

interface ProgressiveCatalogWindow extends Window {
  __windshareProgressiveCatalog?: {
    readonly joined: JoinedShare
    readonly selected: BrowseDirectory
  }
}

export interface ProgressiveDescriptorEvidence {
  readonly descriptorOpened: true
  readonly wireVersion: 2
  readonly suite: 2
  readonly shareInstanceId: string
  readonly syntheticRootId: string
  readonly selectedName: string
}

export async function openProgressiveCatalogDescriptor(
  page: Page,
  input: {
    readonly key: string
    readonly receiverLink: string
  },
): Promise<ProgressiveDescriptorEvidence> {
  return page.evaluate(async ({ gatewayModulePath, key, receiverLink }) => {
    const gatewayModule = await import(gatewayModulePath) as GatewayModule
    const joined = await new gatewayModule.V2BrowserReceiverGateway().join(
      key,
      receiverLink,
    )
    try {
      const root = joined.rootDirectory()
      const rootPage = await joined.page(root, 0)
      if (rootPage.pageCount !== 1 || rootPage.entryCount !== 1 ||
          rootPage.omittedCount !== 0n || rootPage.entries.length !== 1) {
        throw new Error('Authenticated synthetic root does not contain exactly one selected object')
      }
      const selectedEntry = rootPage.entries[0]
      if (selectedEntry === undefined || selectedEntry.kind !== 'directory') {
        throw new Error('Authenticated selected object is not a directory')
      }
      const selected = joined.childDirectory(root, selectedEntry)
      const progressiveWindow = window as ProgressiveCatalogWindow
      if (progressiveWindow.__windshareProgressiveCatalog !== undefined) {
        throw new Error('Progressive catalog browser state already exists')
      }
      progressiveWindow.__windshareProgressiveCatalog = { joined, selected }
      return Object.freeze({
        descriptorOpened: true as const,
        wireVersion: joined.descriptor.wireVersion,
        suite: joined.descriptor.suite,
        shareInstanceId: joined.descriptor.shareInstanceId,
        syntheticRootId: joined.descriptor.syntheticRootId,
        selectedName: selectedEntry.name,
      })
    } catch (error) {
      await joined.close().catch(() => undefined)
      throw error
    }
  }, {
    gatewayModulePath: GATEWAY_MODULE_PATH,
    key: input.key,
    receiverLink: input.receiverLink,
  })
}

export async function enumerateProgressiveCatalog(page: Page): Promise<readonly string[]> {
  return page.evaluate(async () => {
    const state = (window as ProgressiveCatalogWindow).__windshareProgressiveCatalog
    if (state === undefined) throw new Error('Progressive catalog browser state is unavailable')

    const inventory: string[] = []
    const loadEntries = async (directory: BrowseDirectory) => {
      const first = await state.joined.page(directory, 0)
      const entries = [...first.entries]
      for (let pageIndex = 1; pageIndex < first.pageCount; pageIndex += 1) {
        const page = await state.joined.page(directory, pageIndex)
        if (page.pageCount !== first.pageCount || page.entryCount !== first.entryCount ||
            page.omittedCount !== first.omittedCount) {
          throw new Error('Authenticated catalog page metadata changed within one generation')
        }
        entries.push(...page.entries)
      }
      if (entries.length !== first.entryCount || first.omittedCount !== 0n) {
        throw new Error('Authenticated directory inventory is incomplete')
      }
      return entries
    }
    const visit = async (directory: BrowseDirectory): Promise<void> => {
      for (const entry of await loadEntries(directory)) {
        const path = [...directory.path, entry.name].join('/')
        if (entry.kind === 'file') {
          inventory.push('file:' + path + ':' + entry.expectedSize.toString())
          continue
        }
        inventory.push('directory:' + path)
        await visit(state.joined.childDirectory(directory, entry))
      }
    }

    inventory.push('directory:' + state.selected.path.join('/'))
    await visit(state.selected)
    return Object.freeze(inventory.sort())
  })
}

export async function closeProgressiveCatalog(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const progressiveWindow = window as ProgressiveCatalogWindow
    const state = progressiveWindow.__windshareProgressiveCatalog
    delete progressiveWindow.__windshareProgressiveCatalog
    await state?.joined.close()
  })
}
