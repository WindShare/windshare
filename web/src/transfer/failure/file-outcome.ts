export type TransferFileOutcomeEvidence =
  | Readonly<{ readonly kind: 'authenticated-source-drift' }>
  | Readonly<{
      readonly kind: 'checkpoint-decision'
      readonly decision: 'revision-conflict' | 'ownership-conflict' | 'invalid'
    }>
  | Readonly<{ readonly kind: 'occupied-unbound-destination' }>
  | Readonly<{ readonly kind: 'residual-failure' }>
