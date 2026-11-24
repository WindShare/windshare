import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
import { useRef, useState } from 'react'
import type { SavingActionPresentation, SavingChoice, SavingOutcome } from './index'
import type { ArtifactActivationPresentation } from '../artifact-presentation'
import { DetailSheet } from '../controls/DetailSheet'
import { ReceiverIcon } from '../receiver-presentation/ReceiverIcon'

function isSameChoice(left: SavingChoice | null, right: SavingChoice): boolean {
  return left !== null && left.choice.choiceId === right.choice.choiceId
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

function Choice({ choice, recommended, sharedDisabledReason, choose }: {
  readonly choice: SavingChoice
  readonly recommended: boolean
  readonly sharedDisabledReason: string | null
  readonly choose: (choice: SavingChoice) => void
}) {
  const t = useCopy(receiverCopy).saving
  const uniqueReason = choice.disabledReason !== null && choice.disabledReason !== sharedDisabledReason
    ? choice.disabledReason : null
  return <div className="saving-choice-wrap">
    <button type="button"
      className={`saving-choice${recommended ? ' saving-choice-recommended' : ''}`}
      disabled={choice.disabledReason !== null}
      onClick={() => choose(choice)}>
      <span className="saving-choice-copy">
        <span className="saving-choice-header">
          <strong className="saving-choice-title">{choice.label}</strong>
          {recommended && <span className="saving-badge saving-badge-accent">{t.recommended}</span>}
          {choice.badges.map(badge => <span className="saving-badge" key={badge}>{badge}</span>)}
        </span>
        <span className="saving-choice-summary">{choice.detail}</span>
      </span>
      <span className="saving-choice-arrow" aria-hidden="true">
        <ReceiverIcon name="download" />
      </span>
    </button>
    {uniqueReason !== null && <p className="action-reason" role="status">{uniqueReason}</p>}
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
  const t = useCopy(receiverCopy).saving
  const [alternatives, setAlternatives] = useState(false)
  const alternativesButton = useRef<HTMLButtonElement>(null)
  const deferSummary = currentTaskContext !== null && model.primary !== null && model.primary.disabledReason !== null
  const disabledReason = model.disabledReason
  const isBlocked = model.primary === null || model.primary.disabledReason !== null
  const notes = alternatives ? collectSheetNotes(model.alternatives) : []
  const commit = (choice: SavingChoice) => {
    // The destination picker must run synchronously inside this trusted click.
    choose(choice)
    setAlternatives(false)
  }
  return <section className="saving-controls" aria-label={t.downloadAction}
    data-saving-state={isBlocked ? 'blocked' : 'ready'}
    data-current-operation-id={deferSummary ? currentTaskContext.operationId : undefined}>
    {activation !== null ? <div className="preparing-task">
      <div className="saving-context">
        <strong className="saving-heading" role="status"><ReceiverIcon name="clock" />{t.preparing(activation.title)}</strong>
        <p>{activation.description}</p>
      </div>
      <div className="task-actions">
        {activation.kind !== 'committing' && <button className="quiet-action" type="button" onClick={cancel}>{t.cancel}</button>}
      </div>
    </div> : <div className="saving-decision">
      <div className="saving-actions-row">
        <div className={`saving-split-group${model.hasAlternatives ? ' saving-split-has-menu' : ''}`}>
          <button className="primary-action" type="button" disabled={isBlocked}
            onClick={() => { if (model.primary !== null) commit(model.primary) }}>
            <ReceiverIcon name="download" />{model.primary?.label ?? actionLabel}
          </button>
          {model.hasAlternatives && <button ref={alternativesButton} className="saving-alternatives-trigger" type="button"
            aria-label={t.otherWays} title={t.otherWays} aria-haspopup="dialog"
            onClick={() => { onIntent('open-saving-alternatives'); setAlternatives(true) }}>
            <ReceiverIcon name="chevron-down" />
          </button>}
        </div>
        {model.retry && <button className="quiet-action" type="button" onClick={retry}>{t.retryConfirmation}</button>}
      </div>
      {disabledReason !== null && <p className="saving-note action-reason" role="status">{disabledReason}</p>}
      {deferSummary && currentTaskContext.reason !== disabledReason && <p className="saving-note saving-summary">{currentTaskContext.reason}</p>}
      {!deferSummary && disabledReason === null && model.primary !== null && <>
        <p className="saving-note saving-summary">{model.primary.summary}</p>
        {model.recoveryNotice !== null && <p className="saving-note saving-caution">{model.recoveryNotice}</p>}
      </>}
      {model.desktopGuidance !== null && <p className="saving-note saving-summary">{model.desktopGuidance}</p>}
    </div>}
    {alternatives && <DetailSheet returnFocus={alternativesButton} title={t.otherWays}
      onClose={() => { onIntent('close-saving-alternatives'); setAlternatives(false) }}>
      {disabledReason !== null && <p className="saving-sheet-notice" role="status">{disabledReason}</p>}
      {model.recommendation !== null && <p className="saving-recommendation">{model.recommendation}</p>}
      {model.alternatives.map(outcome => <section className="saving-outcome" key={outcome.kind}>
        <h3>{outcome.label}</h3>
        <Choice choice={outcome.primary} recommended={isSameChoice(model.primary, outcome.primary)}
          sharedDisabledReason={disabledReason} choose={commit} />
        {outcome.alternatives.length > 0 && <details className="saving-advanced">
          <summary>{t.moreOptions}</summary>
          {outcome.alternatives.map(choice => <Choice
            key={choice.choice.choiceId}
            choice={choice} recommended={isSameChoice(model.primary, choice)}
            sharedDisabledReason={disabledReason} choose={commit} />)}
        </details>}
      </section>)}
      {notes.length > 0 && <details className="saving-notes">
        <summary>{t.goodToKnow}</summary>
        <ul>{notes.map(note => <li key={note}>{note}</li>)}</ul>
      </details>}
    </DetailSheet>}
  </section>
}
