const paths = {
  sun: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8Zm0-6v2m0 16v2M2 12h2m16 0h2M5 5l1 1m12 12 1 1M5 19l1-1M18 6l1-1',
  moon: 'M20.5 14A9 9 0 0 1 10 3.5 9 9 0 1 0 20.5 14Z',
  arrow: 'M5 12h14m-5-5 5 5-5 5',
  chevronDown: 'm6 9 6 6 6-6',
  download: 'M12 3v12m-5-5 5 5 5-5M5 16v4h14v-4',
  terminal: 'm4 5 6 6-6 6m9 2h7',
  monitor: 'M4 3h16a1 1 0 0 1 1 1v12H3V4a1 1 0 0 1 1-1ZM8 21h8m-4-5v5',
  smartphone: 'M7 2h10a2 2 0 0 1 2 2v16a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2Zm3 3h4m-3 14h2',
  shield: 'm12 3 8 3v6c0 5-8 9-8 9s-8-4-8-9V6l8-3Zm-4 9 3 3 5-6',
  lock: 'M7 10V7a5 5 0 0 1 10 0v3M5 10h14v11H5V10Zm7 5v2',
  link: 'm10 14 4-4m-6 7-1 1a4.24 4.24 0 0 1-6-6l4-4a4.24 4.24 0 0 1 6 0m2 8a4.24 4.24 0 0 0 6 0l4-4a4.24 4.24 0 0 0-6-6l-1 1',
  clipboard: 'M9 4H6a2 2 0 0 0-2 2v14h16V6a2 2 0 0 0-2-2h-3M9 2h6v5H9V2Z',
  folder: 'M3 5h6l2 3h10v12H3V5Z',
  globe: 'M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0ZM3 12h18M12 3c5 5 5 13 0 18-5-5-5-13 0-18Z',
  bolt: 'm13 2-9 12h7l-1 8 10-13h-7l1-7Z',
  check: 'm5 12 4 4L19 6',
  menu: 'M4 6h16M4 12h16M4 18h16',
  github: 'M9 19c-4 1-4-2-6-2m12 5v-4a3.5 3.5 0 0 0-1-3c4 0 7-2 7-6a5 5 0 0 0-1-3 4 4 0 0 0 0-4s-2 0-4 2a14 14 0 0 0-8 0C6 2 4 2 4 2a4 4 0 0 0 0 4 5 5 0 0 0-1 3c0 4 3 6 7 6a3.5 3.5 0 0 0-1 3v4',
} as const

export type PortalIconName = keyof typeof paths

export function PortalIcon({ name, size = 20 }: {
  readonly name: PortalIconName
  readonly size?: number
}) {
  return <svg width={size} height={size} viewBox="0 0 24 24" fill="none"
    stroke="currentColor" strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round"
    aria-hidden="true"><path d={paths[name]} /></svg>
}
