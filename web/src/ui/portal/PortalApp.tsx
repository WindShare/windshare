import { useSyncExternalStore } from 'react'
import type { V2ReceiverController } from '../v2-controller'
import { P2PMeshBackground } from './background/P2PMeshBackground'
import { PortalHeader } from './PortalHeader'
import { HeroSection } from './HeroSection'
import { FeatureGrid } from './FeatureGrid'
import { HowItWorksSection } from './HowItWorksSection'
import { SelfHostSection } from './SelfHostSection'
import { PortalFooter } from './PortalFooter'
import { composeTasks } from '../experience/task-composition'
import { TaskDownloads } from '../experience/TaskDownloads'
import './portal.css'

export function PortalApp({ controller }: { readonly controller: V2ReceiverController }) {
  const snapshot = useSyncExternalStore(
    controller.subscribe,
    controller.getSnapshot,
    controller.getSnapshot,
  )

  return (
    <div className="portal-root">
      <P2PMeshBackground />
      <div className="portal-content-layer">
        <PortalHeader />
        <main>
          <HeroSection
            controller={controller}
            downloads={<TaskDownloads
              tasks={composeTasks(snapshot, (operation, action) => controller.retainedActionAdmission(operation, action), action => controller.activeLifecycleActionAdmission(action)).tasks}
              snapshot={snapshot}
              controller={controller}
              entryLabel="下载记录"
              entryDescription="当前浏览器的下载任务与记录"
            />}
          />
          <FeatureGrid />
          <HowItWorksSection />
          <SelfHostSection />
        </main>
        <PortalFooter />
      </div>
    </div>
  )
}
