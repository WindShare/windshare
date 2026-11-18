const paths = {
  access: 'M4 4h16v12H4zM8 20h8m-4-4v4',
  network: 'M9 3h6v5H9zM3 16h6v5H3zm12 0h6v5h-6zM12 8v4m-6 4v-4h12v4',
  server: 'M4 4h16v6H4zM4 14h16v6H4zM7 7h.1M7 17h.1',
  report: 'M5 21V4c5-3 9 3 15 0v10c-6 3-10-3-15 0',
  policy: 'M4 6h8m4 0h4M4 12h2m4 0h10M4 18h10m4 0h2M12 3v6m-6 0v6m8 0v6',
  key: 'M14 3a6 6 0 0 0-5.7 8L2.5 17v4h4v-3h3v-2l2.4-2.4A6 6 0 1 0 14 3Zm2 4h.01',
  shield: 'm12 3 8 3v6c0 5-8 9-8 9s-8-4-8-9V6l8-3Zm-4 9 3 3 5-6',
  pair: 'M3 4h14v10H3zM6 18h6m-2-4v4M16 11h5v10h-5z',
  globe: 'M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0ZM3 12h18M12 3c5 5 5 13 0 18-5-5-5-13 0-18Z',
  system: 'M3 4h18v13H3zM8 21h8m-4-4v4',
  light: 'M16 12a4 4 0 1 1-8 0 4 4 0 0 1 8 0ZM12 2v2m0 16v2M2 12h2m16 0h2M5 5l1.5 1.5m11 11L19 19M5 19l1.5-1.5m11-11L19 5',
  dark: 'M20.5 14A9 9 0 0 1 10 3.5 9 9 0 1 0 20.5 14Z',
  down: 'm7 10 5 5 5-5',
  right: 'm9 6 6 6-6 6',
  arrow: 'M4 12h15m-6-6 6 6-6 6',
  back: 'M20 12H5m6-6-6 6 6 6',
  logout: 'M10 4H4v16h6m-1-8h12m-5-5 5 5-5 5',
  plus: 'M12 5v14M5 12h14',
  refresh: 'M20 9a8 8 0 0 0-14-3L3 9m0-5v5h5m-4 6a8 8 0 0 0 14 3l3-3m0 5v-5h-5',
  clock: 'M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0ZM12 7v5l3 2',
  book: 'M5 3h14v18H6a2 2 0 0 1-2-2V5a2 2 0 0 1 1-2Zm-1 15c0-1 1-2 3-2h12M8 7h7',
  check: 'm4 12 5 5L20 6',
  alert: 'M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0ZM12 7v6m0 3v.2',
  eye: 'M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12Zm13 0a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z',
  eyeOff: 'm3 3 18 18M9 5.5a11 11 0 0 1 3-.5c6 0 10 7 10 7a21 21 0 0 1-3 3.5M6 6.5A22 22 0 0 0 2 12s4 7 10 7c1.6 0 3-.4 4.5-1.2',
  copy: 'M8 8h12v13H8zM16 8V3H3v13h5',
  search: 'M17 10a7 7 0 1 1-14 0 7 7 0 0 1 14 0Zm-2 5 6 6',
  user: 'M16 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0ZM4 21v-2a8 7 0 0 1 16 0v2',
  loader: 'M20.5 12A8.5 8.5 0 1 1 12 3.5',
} as const

export type IconName = keyof typeof paths

export function Icon({ name, size = 18 }: { name: IconName; size?: number }) {
  return <svg className={name === 'loader' ? 'icon spinner' : 'icon'} width={size} height={size}
    viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.65"
    strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
    <path d={paths[name]} />
  </svg>
}
