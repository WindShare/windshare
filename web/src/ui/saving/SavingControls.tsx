import { useRef, useState } from 'react'
import type { SavingActionPresentation, SavingChoice, SavingOutcome } from './index'
import type { ArtifactActivationPresentation } from '../artifact-presentation'
import { DetailSheet } from '../controls/DetailSheet'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'

function isSameChoice(left: SavingChoice | null, right: SavingChoice): boolean {
  return left !== null &&
    left.offered.choice.choiceId === right.offered.choice.choiceId &&
    left.recoveryPreference === right.recoveryPreference
}

function collectSheetNotes(outcomes: readonly SavingOutcome[]): readonly string[] {
  const seen = new Set<string>()
  const result: string[] = []
  for (const group of outcomes) {
    for (const choice of [group.primary, ...group.alternatives]) {
      for (const note of choice.notes) {
        if (!seen.has(note)) {
          seen.add(note)
          result.push(note)
        }
      }
    }
  }
  return result
}

function Choice({ choice, recommended, choose }: {
  readonly choice: SavingChoice
  readonly recommended: boolean
  readonly choose: (choice: SavingChoice) => void
}) {
  return <div className="saving-choice-wrap">
    <button type="button"
      className={`saving-choice${recommended ? ' saving-choice-recommended' : ''}`}
      disabled={choice.disabledReason !== null}
      onClick={() => choose(choice)}>
      <span className="saving-choice-copy">
        <span className="saving-choice-header">
          <strong className="saving-choice-title">{choice.label}</strong>
          {recommended && <span className="saving-badge saving-badge-accent">Recommended</span>}
          {choice.badges.map(badge => <span className="saving-badge" key={badge}>{badge}</span>)}
        </span>
        <span className="saving-choice-summary">{choice.summary}</span>
      </span>
      <span className="saving-choice-arrow" aria-hidden="true">
        <ReceiverIcon name="download" />
      </span>
    </button>
    {choice.disabledReason !== null && <p className="action-reason" role="status">{choice.disabledReason}</p>}
  </div>
}

export function SavingControls({ model, activation, currentTaskContext, actionLabel, scopeLabel, choose, retry, cancel, onIntent }: {
  readonly model: SavingActionPresentation
  readonly activation: ArtifactActivationPresentation | null
  readonly currentTaskContext: Readonly<{ operationId: string; reason: string }> | null
  readonly actionLabel: string
  readonly scopeLabel?: string
  readonly choose: (choice: SavingChoice) => void
  readonly retry: () => void
  readonly cancel: () => void
  readonly onIntent: (action: string) => void
}) {
  const [alternatives, setAlternatives] = useState(false)
  const alternativesButton = useRef<HTMLButtonElement>(null)
  const deferSummary = currentTaskContext !== null && model.primary !== null && model.primary.disabledReason !== null
  const disabledReason = model.disabledReason
  const heading = scopeLabel ?? actionLabel
  const notes = alternatives ? collectSheetNotes(model.alternatives) : []
  const commit = (choice: SavingChoice) => {
    // The destination picker must run synchronously inside this trusted click.
    choose(choice)
    setAlternatives(false)
  }
  return <section className="saving-controls" aria-label="Download action"
    data-current-operation-id={deferSummary ? currentTaskContext.operationId : undefined}>
    {activation !== null ? <div className="preparing-task">
      <div className="saving-context">
        <strong className="saving-heading" role="status"><ReceiverIcon name="clock" />Preparing · {activation.title}</strong>
        <p>{activation.description}</p>
      </div>
      <div className="task-actions">
        {activation.kind === 'retry' && <button className="primary-action" type="button" onClick={retry}>{activation.label}</button>}
        {activation.kind !== 'committing' && <button className="quiet-action" type="button" onClick={cancel}>Cancel</button>}
      </div>
    </div> : <div className="saving-decision">
      <div className="saving-context">
        <strong className="saving-heading">{heading}</strong>
        {disabledReason !== null && <p className="action-reason" role="status">{disabledReason}</p>}
        {deferSummary && currentTaskContext.reason !== disabledReason && <p className="saving-summary">{currentTaskContext.reason}</p>}
        {!deferSummary && disabledReason === null && model.primary !== null && <p className="saving-summary">{model.primary.summary}</p>}
        {model.desktopGuidance !== null && <p className="saving-summary">{model.desktopGuidance}</p>}
      </div>
      <div className="saving-primary-row">
        <button className="primary-action" type="button" disabled={model.primary === null || model.primary.disabledReason !== null}
          onClick={() => { if (model.primary !== null) commit(model.primary) }}><ReceiverIcon name="download" />{model.primary?.label ?? actionLabel}</button>
        {model.hasAlternatives && <button ref={alternativesButton} className="quiet-action" type="button"
          onClick={() => { onIntent('open-saving-alternatives'); setAlternatives(true) }}>Other ways to save<ReceiverIcon name="chevron-right" /></button>}
        {model.retry && <button className="quiet-action" type="button" onClick={retry}>Retry confirmation</button>}
      </div>
    </div>}
    {alternatives && <DetailSheet returnFocus={alternativesButton} title="Other ways to save"
      onClose={() => { onIntent('close-saving-alternatives'); setAlternatives(false) }}>
      {model.recommendation !== null && <p className="saving-recommendation">{model.recommendation}</p>}
      {model.alternatives.map(outcome => <section className="saving-outcome" key={outcome.kind}>
        <h3>{outcome.label}</h3>
        <Choice choice={outcome.primary} recommended={isSameChoice(model.primary, outcome.primary)} choose={commit} />
        {outcome.alternatives.length > 0 && <details className="saving-advanced">
          <summary>More options</summary>
          {outcome.alternatives.map(choice => <Choice
            key={`${choice.offered.choice.choiceId}:${choice.recoveryPreference ?? ''}`}
            choice={choice} recommended={isSameChoice(model.primary, choice)} choose={commit} />)}
        </details>}
      </section>)}
      {notes.length > 0 && <details className="saving-notes">
        <summary>Good to know</summary>
        <ul>{notes.map(note => <li key={note}>{note}</li>)}</ul>
      </details>}
    </DetailSheet>}
  </section>
}
