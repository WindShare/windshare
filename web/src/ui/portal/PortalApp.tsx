import { useEffect, useState, type ReactNode } from 'react'
import type { PortalCommands } from './commands'
import { PortalHeader } from './PortalHeader'
import { HeroSection } from './HeroSection'
import { FeatureGrid } from './FeatureGrid'
import { HowItWorksSection } from './HowItWorksSection'
import { SelfHostSection } from './SelfHostSection'
import { PortalFooter } from './PortalFooter'
import type { PortalTab } from './console/tabs'
import './portal.css'

export function PortalApp({ controller, downloads }: { readonly controller: PortalCommands; readonly downloads: ReactNode }) {
  const [activeTab, setActiveTab] = useState<PortalTab>(() => window.location.hash === '#cli' ? 'cli' : 'receive')
  useEffect(() => {
    const followShareAnchor = () => {
      if (window.location.hash === '#cli') setActiveTab('cli')
    }
    window.addEventListener('hashchange', followShareAnchor)
    return () => window.removeEventListener('hashchange', followShareAnchor)
  }, [])
  return <div className="portal-root">
    <PortalHeader onShare={() => setActiveTab('cli')} />
    <main>
      <HeroSection controller={controller} activeTab={activeTab} onTabChange={setActiveTab} downloads={downloads} />
      <FeatureGrid />
      <HowItWorksSection />
      <SelfHostSection />
    </main>
    <PortalFooter />
  </div>
}
