import { useState } from 'react'
import type { SavingActionPresentation, SavingChoice } from './index'
import type { ArtifactActivationPresentation } from '../v2-artifact-presentation'
import { DetailSheet } from '../controls/DetailSheet'

function Choice({ choice, choose }: { readonly choice: SavingChoice; readonly choose: (choice: SavingChoice) => void }) {
  return <div className="saving-choice">
    <button type="button" disabled={choice.disabledReason !== null} onClick={() => choose(choice)}>{choice.label}</button>
    <p>{choice.description}</p>
    {choice.consequences.map(line => <small key={line}>{line}</small>)}
    {choice.disabledReason !== null && <p>{choice.disabledReason}</p>}
  </div>
}

export function SavingControls({ model, activation, actionLabel, choose, retry, cancel, onIntent }: {
  readonly model: SavingActionPresentation
  readonly activation: ArtifactActivationPresentation | null
  readonly actionLabel: string
  readonly choose: (choice: SavingChoice) => void
  readonly retry: () => void
  readonly cancel: () => void
  readonly onIntent: (action: string) => void
}) {
  const [alternatives, setAlternatives] = useState(false)
  const commit = (choice: SavingChoice) => {
    // The destination picker must run synchronously inside this trusted click.
    choose(choice)
    setAlternatives(false)
  }
  return <section className="saving-controls" aria-label="Download action">
    {activation !== null ? <div className="preparing-task">
      <strong role="status">Preparing · {activation.title}</strong>
      <p>{activation.description}</p>
      <div className="task-actions">
        {activation.kind === 'retry' && <button type="button" onClick={retry}>{activation.label}</button>}
        {activation.kind !== 'committing' && <button type="button" onClick={cancel}>Cancel preparation</button>}
      </div>
    </div> : <>
      <div className="saving-primary-row">
        <button className="primary-action" type="button" disabled={model.primary === null || model.primary.disabledReason !== null}
          onClick={() => { if (model.primary !== null) commit(model.primary) }}>{model.primary?.label ?? actionLabel}</button>
        {model.alternatives.length > 0 && <button type="button" onClick={() => { onIntent('open-saving-alternatives'); setAlternatives(true) }}>Other ways to save</button>}
        {model.retry && <button type="button" onClick={retry}>Retry confirmation</button>}
      </div>
      {model.disabledReason !== null && <p className="action-reason" role="status">{model.disabledReason}</p>}
      {model.primary?.consequences.map(line => <p className="saving-consequence" key={line}>{line}</p>)}
      {model.desktopGuidance !== null && <p>{model.desktopGuidance}</p>}
    </>}
    {alternatives && <DetailSheet title="Other ways to save" onClose={() => { onIntent('close-saving-alternatives'); setAlternatives(false) }}>
      {model.recommendation !== null && <p>{model.recommendation}</p>}
      {model.alternatives.map(outcome => <section className="saving-outcome" key={outcome.kind}>
        <h3>{outcome.label}</h3><Choice choice={outcome.primary} choose={commit} />
        {outcome.alternatives.length > 0 && <details><summary>Saving and recovery options</summary>
          {outcome.alternatives.map(choice => <Choice key={choice.offered.choice.choiceId} choice={choice} choose={commit} />)}
        </details>}
      </section>)}
    </DetailSheet>}
  </section>
}
