import { svgEl } from './dom'

const ICON_PATHS = {
  shield: 'm12 3 8 3v6c0 5-8 9-8 9s-8-4-8-9V6l8-3Zm-4 9 3 3 5-6',
  globe: 'M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0ZM3 12h18M12 3c5 5 5 13 0 18-5-5-5-13 0-18Z',
  system: 'M3 4h18v13H3zM8 21h8m-4-4v4',
  light: 'M16 12a4 4 0 1 1-8 0 4 4 0 0 1 8 0ZM12 2v2m0 16v2M2 12h2m16 0h2M5 5l1.5 1.5m11 11L19 19M5 19l1.5-1.5m11-11L19 5',
  dark: 'M20.5 14A9 9 0 0 1 10 3.5 9 9 0 1 0 20.5 14Z',
  down: 'm7 10 5 5 5-5',
  arrow: 'M4 12h15m-6-6 6 6-6 6',
  logout: 'M10 4H4v16h6m-1-8h12m-5-5 5 5-5 5',
  refresh: 'M20 9a8 8 0 0 0-14-3L3 9m0-5v5h5m-4 6a8 8 0 0 0 14 3l3-3m0 5v-5h-5',
  sync: 'M4 12a8 8 0 0 1 14-5.3L20 9m0-5v5h-5M20 12a8 8 0 0 1-14 5.3L4 15m0 5v-5h5',
  clock: 'M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0ZM12 7v5l3 2',
  check: 'm4 12 5 5L20 6',
  alert: 'M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0ZM12 7v6m0 3v.2',
  eye: 'M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12Zm13 0a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z',
  eyeOff: 'm3 3 18 18M9 5.5a11 11 0 0 1 3-.5c6 0 10 7 10 7a21 21 0 0 1-3 3.5M6 6.5A22 22 0 0 0 2 12s4 7 10 7c1.6 0 3-.4 4.5-1.2',
  copy: 'M8 8h12v13H8zM16 8V3H3v13h5',
  search: 'M17 10a7 7 0 1 1-14 0 7 7 0 0 1 14 0Zm-2 5 6 6',
  heart: 'M12 20.5s-7.5-4.7-9.2-9C1.5 8.1 3.7 5 7 5c2 0 3.7 1.1 5 2.7C13.3 6.1 15 5 17 5c3.3 0 5.5 3.1 4.2 6.5-1.7 4.3-9.2 9-9.2 9Z',
  sliders: 'M4 6h8m4 0h4M4 12h2m4 0h10M4 18h10m4 0h2M12 3v6m-6 0v6m8 0v6',
  lock: 'M6 11h12v9H6zm3 0V8a3 3 0 1 1 6 0v3',
  close: 'M6 6l12 12M18 6 6 18',
  undo: 'M9 14 4 9l5-5M4 9h10.5a5.5 5.5 0 0 1 0 11H11',
  loader: 'M20.5 12A8.5 8.5 0 1 1 12 3.5',
} as const

export type IconName = keyof typeof ICON_PATHS

export function icon(name: IconName, size = 17): SVGElement {
  return svgEl(
    'svg',
    {
      class: name === 'loader' ? 'icon spinner' : 'icon',
      width: String(size),
      height: String(size),
      viewBox: '0 0 24 24',
      fill: 'none',
      stroke: 'currentColor',
      'stroke-width': '1.7',
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      'aria-hidden': 'true',
      focusable: 'false',
    },
    svgEl('path', { d: ICON_PATHS[name] }),
  )
}

export function windMark(): SVGElement {
  return svgEl(
    'svg',
    {
      class: 'wind-mark',
      width: '30',
      height: '30',
      viewBox: '50 35 422 422',
      fill: 'none',
      'aria-hidden': 'true',
    },
    svgEl('path', {
      d: 'M187 382C128 387 83 360 62 317c32 12 62 5 81-17-31-2-53-20-61-47 23 14 46 10 63-6-25-39 1-87 45-91-3-40 27-71 66-65 21-31 65-33 92-6 20 20 25 46 16 68 49 6 85 43 85 90 21 31 11 71-17 93-1 42-38 74-81 65-24 30-66 33-93 10-30 13-60-1-71-29Z',
      fill: 'var(--spirit)',
      stroke: 'var(--spirit-ink)',
      'stroke-width': '16',
      'stroke-linejoin': 'round',
    }),
    svgEl('path', {
      d: 'M178 271c-2-46 35-81 79-79 33-21 80-12 103 19 35 8 59 42 53 78-6 47-51 80-105 79-64 6-121-25-128-67-2-10-3-20-2-30Z',
      fill: '#fff9e9',
    }),
    svgEl(
      'g',
      { stroke: 'var(--spirit-ink)', 'stroke-width': '12', 'stroke-linecap': 'round' },
      svgEl('path', { d: 'M224 158c-14-21-4-45 17-51 23-7 45 8 45 27 0 14-13 22-24 15' }),
      svgEl(
        'g',
        { transform: 'rotate(-7 300 276)' },
        svgEl('path', { d: 'M258 258v12m84-12v12M285 284q15 18 30 0' }),
      ),
    ),
  )
}

const HILL_PATH = 'M-99-13c7-28 42-27 55-47s32-47 61-34 29 39 49 51 46 16 45 45-27 33-45 54-34 35-63 28-49-22-71-38-37-35-31-59Z'
const CONTOUR_SCALES = [2.38, 2.13, 1.88, 1.64, 1.41, 1.2, 1, 0.81, 0.62, 0.44]

function contours(transform: string): SVGElement {
  return svgEl(
    'g',
    { transform },
    ...CONTOUR_SCALES.map(scale => svgEl('path', {
      d: HILL_PATH,
      transform: `scale(${scale})`,
      'vector-effect': 'non-scaling-stroke',
    })),
  )
}

function paperStack(x: number, y: number, small = false): SVGElement {
  const layers = [0, -10, -20].map(offset => svgEl(
    'g',
    { transform: `translate(0 ${offset})` },
    svgEl('path', { d: 'm-30-2 29-14 31 14L1 13Z', fill: 'var(--map-top)' }),
    svgEl('path', { d: 'M-30-2 1 13v8L-30 6Z', fill: 'var(--map-front)' }),
    svgEl('path', { d: 'M1 13 30-2v8L1 21Z', fill: 'var(--map-side)' }),
    svgEl('path', { d: 'm-22 4 9 4m31-3 3-1.5', fill: 'none', 'stroke-linecap': 'round' }),
  ))
  return svgEl(
    'g',
    { transform: `translate(${x} ${y}) scale(${small ? 0.56 : 1})`, 'stroke-linejoin': 'round' },
    svgEl('ellipse', { cy: '12', rx: '40', ry: '15', class: 'art-shadow' }),
    svgEl(
      'g',
      { stroke: 'var(--map-ink)', 'stroke-width': '1.4' },
      ...layers,
      svgEl('path', { d: 'm-3-25 9 4m-13-2 9 4', fill: 'none', 'stroke-linecap': 'round' }),
    ),
  )
}

export function entryArtwork(): SVGElement {
  return svgEl(
    'svg',
    {
      class: 'entry-artwork',
      viewBox: '0 0 440 420',
      fill: 'none',
      preserveAspectRatio: 'xMidYMid slice',
      'aria-hidden': 'true',
      focusable: 'false',
    },
    svgEl(
      'g',
      { class: 'art-contours' },
      contours('translate(12 -14) rotate(-19) scale(1.02 .94)'),
      contours('translate(425 384) rotate(-34) scale(1.08 .89)'),
    ),
    svgEl(
      'g',
      { class: 'art-marks' },
      svgEl('path', { d: 'M54 139h6m-3-3v6m156-75h6m-3-3v6m127 64h6m-3-3v6M91 331h6m-3-3v6m171 33h6m-3-3v6' }),
    ),
    svgEl(
      'g',
      { class: 'art-routes' },
      svgEl('path', { d: 'M-8 312c56 10 56-30 97-31 57-2 76-50 135-70 61-20 69-66 119-73 49-6 65-50 106-53' }),
      svgEl('path', { d: 'M224 211c49 5 55 46 98 52' }),
    ),
    paperStack(89, 277, true),
    paperStack(343, 134, true),
    svgEl(
      'g',
      { fill: 'var(--map-paper)', stroke: 'var(--map-ink)', 'stroke-width': '1.5' },
      svgEl('circle', { cx: '322', cy: '263', r: '5' }),
      svgEl('circle', { cx: '322', cy: '263', r: '1.2', fill: 'var(--map-ink)' }),
    ),
    paperStack(224, 203),
  )
}
