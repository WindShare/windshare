import { useRef, useState } from 'react'
import type { SavingActionPresentation, SavingChoice } from './index'
import type { ArtifactActivationPresentation } from '../v2-artifact-presentation'
import { DetailSheet } from '../controls/DetailSheet'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'

function Choice({ choice, choose }: { readonly choice: SavingChoice; readonly choose: (choice: SavingChoice) => void }) {
  return <div className="saving-choice">
    <p>{choice.description}</p>
    {choice.consequences.map(line => <small key={line}>{line}</small>)}
    <button type="button" disabled={choice.disabledReason !== null} onClick={() => choose(choice)}>
      <ReceiverIcon name="download" />{choice.label}
    </button>
    {choice.disabledReason !== null && <p className="action-reason">{choice.disabledReason}</p>}
  </div>
}

export function SavingControls({ model, activation, currentTaskContext, actionLabel, choose, retry, cancel, onIntent }: {
  readonly model: SavingActionPresentation
  readonly activation: ArtifactActivationPresentation | null
  readonly currentTaskContext: Readonly<{ operationId: string; reason: string }> | null
  readonly actionLabel: string
  readonly choose: (choice: SavingChoice) => void
  readonly retry: () => void
  readonly cancel: () => void
  readonly onIntent: (action: string) => void
}) {
  const [alternatives, setAlternatives] = useState(false)
  const alternativesButton = useRef<HTMLButtonElement>(null)
  // A disabled, non-empty draft alongside current work defers route costs to the available alternatives.
  const deferConsequences = currentTaskContext !== null && model.primary !== null && model.primary.disabledReason !== null
  const disabledReason = model.disabledReason
  const commit = (choice: SavingChoice) => {
    // The destination picker must run synchronously inside this trusted click.
    choose(choice)
    setAlternatives(false)
  }
  return <section className="saving-controls" aria-label="Download action"
    data-current-operation-id={deferConsequences ? currentTaskContext.operationId : undefined}>
    {activation !== null ? <div className="preparing-task">
      <div className="saving-context">
        <strong className="saving-heading" role="status"><ReceiverIcon name="clock" />Preparing · {activation.title}</strong>
        <p>{activation.description}</p>
      </div>
      <div className="task-actions">
        {activation.kind === 'retry' && <button className="primary-action" type="button" onClick={retry}>{activation.label}</button>}
        {activation.kind !== 'committing' && <button className="quiet-action" type="button" onClick={cancel}>Cancel preparation</button>}
      </div>
    </div> : <div className="saving-decision">
      <div className="saving-context">
        <strong className="saving-heading"><ReceiverIcon name="download" />{actionLabel}</strong>
        {disabledReason !== null && <p className="action-reason" role="status">{disabledReason}</p>}
        {deferConsequences && currentTaskContext.reason !== disabledReason && <p className="saving-consequence">{currentTaskContext.reason}</p>}
        {!deferConsequences && model.primary?.consequences.map(line => <p className="saving-consequence" key={line}>{line}</p>)}
        {model.desktopGuidance !== null && <p className="saving-consequence">{model.desktopGuidance}</p>}
      </div>
      <div className="saving-primary-row">
        <button className="primary-action" type="button" disabled={model.primary === null || model.primary.disabledReason !== null}
          onClick={() => { if (model.primary !== null) commit(model.primary) }}><ReceiverIcon name="download" />{model.primary?.label ?? actionLabel}</button>
        {model.alternatives.length > 0 && <button ref={alternativesButton} className="quiet-action" type="button" onClick={() => { onIntent('open-saving-alternatives'); setAlternatives(true) }}>Other ways to save<ReceiverIcon name="chevron-right" /></button>}
        {model.retry && <button className="quiet-action" type="button" onClick={retry}>Retry confirmation</button>}
      </div>
    </div>}
    {alternatives && <DetailSheet returnFocus={alternativesButton} title="Other ways to save" onClose={() => { onIntent('close-saving-alternatives'); setAlternatives(false) }}>
      {model.recommendation !== null && <p className="saving-recommendation">{model.recommendation}</p>}
      {model.alternatives.map(outcome => <section className="saving-outcome" key={outcome.kind}>
        <h3>{outcome.label}</h3><Choice choice={outcome.primary} choose={commit} />
        {outcome.alternatives.length > 0 && <details><summary>Saving and recovery options</summary>
          {outcome.alternatives.map(choice => <Choice key={choice.offered.choice.choiceId} choice={choice} choose={commit} />)}
        </details>}
      </section>)}
    </DetailSheet>}
  </section>
}
