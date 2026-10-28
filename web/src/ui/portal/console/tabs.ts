import type { PortalIconName } from '../PortalIcon'
import type { PortalMessageKey } from '../locales/use-portal-copy'

export type PortalTab = 'receive' | 'cli' | 'download'

export const CONSOLE_TABS: readonly { id: PortalTab; label: PortalMessageKey; icon: PortalIconName }[] = [
  { id: 'receive', label: 'tabReceive', icon: 'download' },
  { id: 'cli', label: 'tabCli', icon: 'terminal' },
  { id: 'download', label: 'tabDownload', icon: 'monitor' },
]
