import { useEffect, useState, useSyncExternalStore } from 'react'
import type { ReceiverController } from '../controller'
import { PortalHeader } from './PortalHeader'
import { HeroSection } from './HeroSection'
import { FeatureGrid } from './FeatureGrid'
import { HowItWorksSection } from './HowItWorksSection'
import { SelfHostSection } from './SelfHostSection'
import { PortalFooter } from './PortalFooter'
import type { PortalTab } from './console/tabs'
import { composeTasks } from '../experience/task-composition'
import { TaskDownloads } from '../experience/TaskDownloads'
import { usePortalCopy } from './locales/use-portal-copy'
import './portal.css'

export function PortalApp({ controller }: { readonly controller: ReceiverController }) {
  const t = usePortalCopy()
  const snapshot = useSyncExternalStore(controller.subscribe, controller.getSnapshot, controller.getSnapshot)
  const [activeTab, setActiveTab] = useState<PortalTab>(() => window.location.hash === '#cli' ? 'cli' : 'receive')

  useEffect(() => {
    const followShareAnchor = () => {
      if (window.location.hash === '#cli') setActiveTab('cli')
    }
    window.addEventListener('hashchange', followShareAnchor)
    return () => window.removeEventListener('hashchange', followShareAnchor)
  }, [])

  return (
    <div className="portal-root">
      <PortalHeader onShare={() => setActiveTab('cli')} />
      <main>
        <HeroSection controller={controller} activeTab={activeTab} onTabChange={setActiveTab}
          downloads={<TaskDownloads
            tasks={composeTasks(snapshot, (operation, action) => controller.retainedActionAdmission(operation, action), action => controller.activeLifecycleActionAdmission(action)).tasks}
            snapshot={snapshot} controller={controller} entryLabel={t.downloads}
            entryDescription={t.downloadsDescription} />}
        />
        <FeatureGrid />
        <HowItWorksSection />
        <SelfHostSection />
      </main>
      <PortalFooter />
    </div>
  )
}
