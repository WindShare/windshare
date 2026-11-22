import { useMemo } from 'react'
import {
  DEFAULT_QR_QUIET_ZONE,
  encodeQrMatrix,
  qrMatrixToSvgPath,
  type QrErrorCorrectionLevel,
} from './qr-encode'

interface QrCodeSvgProps {
  readonly value: string
  readonly label: string
  readonly level?: QrErrorCorrectionLevel
  readonly quietZone?: number
}

export function QrCodeSvg({
  value,
  label,
  level = 'M',
  quietZone = DEFAULT_QR_QUIET_ZONE,
}: QrCodeSvgProps) {
  const { viewSize, path } = useMemo(() => {
    const matrix = encodeQrMatrix(value, level)
    return {
      viewSize: matrix.size + quietZone * 2,
      path: qrMatrixToSvgPath(matrix, quietZone),
    }
  }, [value, level, quietZone])

  return (
    <svg
      className="donate-qr-svg"
      viewBox={`0 0 ${viewSize} ${viewSize}`}
      role="img"
      aria-label={label}
      shapeRendering="crispEdges"
    >
      <rect width={viewSize} height={viewSize} fill="#ffffff" />
      <path d={path} fill="#142921" />
    </svg>
  )
}
