import { PortalIcon } from './PortalIcon'
import { PORTAL_SECTIONS } from './navigation'
import { usePortalCopy, type PortalMessageKey } from './locales/use-portal-copy'

const STEPS: readonly { num: string; title: PortalMessageKey; description: PortalMessageKey }[] = [
  { num: '01', title: 'stepOneTitle', description: 'stepOneDescription' },
  { num: '02', title: 'stepTwoTitle', description: 'stepTwoDescription' },
  { num: '03', title: 'stepThreeTitle', description: 'stepThreeDescription' },
]

export function HowItWorksSection() {
  const t = usePortalCopy()
  return (
    <section id={PORTAL_SECTIONS.howItWorks.id} className="portal-section portal-workflow" aria-labelledby="portal-workflow-title">
      <div className="portal-container">
        <div className="portal-section-header">
          <div><span className="portal-section-tag">{t.workflowTag}</span><h2 id="portal-workflow-title">{t.workflowTitle}</h2></div>
          <p className="portal-online-note"><PortalIcon name="monitor" size={18} />{t.stayOnline}</p>
        </div>
        <ol className="portal-steps-grid">
          {STEPS.map(step => <li key={step.num} className="portal-step">
            <span className="portal-step-num">{step.num}</span>
            <h3>{t[step.title]}</h3><p>{t[step.description]}</p>
          </li>)}
        </ol>
      </div>
    </section>
  )
}
