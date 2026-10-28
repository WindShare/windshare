const ARTWORK_VIEW_BOX = '26.713 11.755 468.811 468.811'
const ARTWORK_URL = `${import.meta.env.BASE_URL}brand/windshare.svg`

export function SpiritArtwork({ size }: { readonly size: number }) {
  return <svg className="portal-spirit-artwork" viewBox={ARTWORK_VIEW_BOX}
    width={size} height={size} fill="none" role="img" aria-hidden="true" focusable="false">
    <use href={`${ARTWORK_URL}#windshare-body`} />
    <g transform="rotate(-7 300 276)">
      <g className="portal-spirit-open-eyes"><use href={`${ARTWORK_URL}#windshare-eyes-open`} /></g>
      <g className="portal-spirit-smile-eyes"><use href={`${ARTWORK_URL}#windshare-eyes-smile`} /></g>
    </g>
  </svg>
}
