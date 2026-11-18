// Contours and routes scale together so the relay diagram can be cropped like a paper map.
const hill = 'M-99-13c7-28 42-27 55-47s32-47 61-34 29 39 49 51 46 16 45 45-27 33-45 54-34 35-63 28-49-22-71-38-37-35-31-59Z'
const contourScales = [2.38, 2.13, 1.88, 1.64, 1.41, 1.2, 1, .81, .62, .44]

function Contours({ transform }: { transform: string }) {
  return <g transform={transform}>
    {contourScales.map(scale => <path key={scale} d={hill} transform={'scale(' + scale + ')'}
      vectorEffect="non-scaling-stroke" />)}
  </g>
}

function RelayStation({ x, y, small = false }: { x: number; y: number; small?: boolean }) {
  return <g transform={`translate(${x} ${y}) scale(${small ? .56 : 1})`} strokeLinejoin="round">
    <ellipse cy="12" rx="40" ry="15" className="station-shadow" />
    <g stroke="var(--map-ink)" strokeWidth="1.4">
      {[0, -10, -20].map(offset => <g key={offset} transform={'translate(0 ' + offset + ')'}>
        <path d="m-30-2 29-14 31 14L1 13Z" fill="var(--map-top)" />
        <path d="M-30-2 1 13v8L-30 6Z" fill="var(--map-front)" />
        <path d="M1 13 30-2v8L1 21Z" fill="var(--map-side)" />
        <path d="m-22 4 9 4m31-3 3-1.5" fill="none" strokeLinecap="round" />
      </g>)}
      <path d="m-3-25 9 4m-13-2 9 4" fill="none" strokeLinecap="round" />
    </g>
  </g>
}

export function RouteMap() {
  return <svg className="route-map" viewBox="0 0 440 420" fill="none" preserveAspectRatio="xMidYMid slice"
    aria-hidden="true" focusable="false">
    <g className="contours">
      <Contours transform="translate(12 -14) rotate(-19) scale(1.02 .94)" />
      <Contours transform="translate(425 384) rotate(-34) scale(1.08 .89)" />
    </g>
    <g className="survey-marks">
      <path d="M54 139h6m-3-3v6m156-75h6m-3-3v6m127 64h6m-3-3v6M91 331h6m-3-3v6m171 33h6m-3-3v6" />
    </g>
    <g className="map-routes">
      <path d="M-8 312c56 10 56-30 97-31 57-2 76-50 135-70 61-20 69-66 119-73 49-6 65-50 106-53" />
      <path d="M224 211c49 5 55 46 98 52" />
    </g>
    <RelayStation x={89} y={277} small /><RelayStation x={343} y={134} small />
    <g fill="var(--map-paper)" stroke="var(--map-ink)" strokeWidth="1.5">
      <circle cx="322" cy="263" r="5" /><circle cx="322" cy="263" r="1.2" fill="var(--map-ink)" />
    </g>
    <RelayStation x={224} y={203} />
  </svg>
}
