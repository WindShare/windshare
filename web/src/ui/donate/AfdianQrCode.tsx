import { AFDIAN_QR_PATH, AFDIAN_QR_VIEW_SIZE } from './afdian-qr'

export function AfdianQrCode({ label }: { readonly label: string }) {
  return (
    <svg
      className="donate-qr-svg"
      viewBox={`0 0 ${AFDIAN_QR_VIEW_SIZE} ${AFDIAN_QR_VIEW_SIZE}`}
      role="img"
      aria-label={label}
      shapeRendering="crispEdges"
    >
      <rect width={AFDIAN_QR_VIEW_SIZE} height={AFDIAN_QR_VIEW_SIZE} fill="#ffffff" />
      <path d={AFDIAN_QR_PATH} fill="#142921" />
    </svg>
  )
}
