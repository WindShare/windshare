/** The fold is receiver decoration, independent of task state and the portal brand. */
export function ReceiverFold({ compact = false }: { readonly compact?: boolean }) {
  return <div className={`receiver-fold${compact ? ' receiver-fold-compact' : ''}`} aria-hidden="true">
    <div className="receiver-fold-airfoil">
      <span className="receiver-fold-vane receiver-fold-vane-a" />
      <span className="receiver-fold-vane receiver-fold-vane-b" />
      <span className="receiver-fold-vane receiver-fold-vane-c" />
    </div>
  </div>
}
