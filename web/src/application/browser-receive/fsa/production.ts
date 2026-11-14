import { FSAArtifactPresentationAuthority, type FSAArtifactPresentationAuthorityOptions } from './route'
import { BrowserFolderDeliveryProgress } from './folder-delivery'
import { inspectBrowserStagingStorage } from '../../../output/capability/staging-storage'
import type { BrowserReceiveWindow } from '../../../output/browser/environment'
import type { BrowserRecoveryPreference } from '../../../output/model/browser-delivery'

export function createFSAArtifactAuthority(
  options: FSAArtifactPresentationAuthorityOptions,
  windowPort: BrowserReceiveWindow,
  nativeObjectSupported: boolean,
  preference: BrowserRecoveryPreference,
) {
  return new FSAArtifactPresentationAuthority({
    ...options,
    folderDelivery: {
      preference, storage: windowPort.navigator.storage,
      storageFacts: () => inspectBrowserStagingStorage(windowPort.navigator.storage,
        nativeObjectSupported && typeof windowPort.navigator.storage?.getDirectory === 'function'),
      progress: new BrowserFolderDeliveryProgress(),
    },
  })
}
