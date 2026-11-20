const ICON_PATHS = {
  folder: 'M3 7a2 2 0 0 1 2-2h5l2 2h7a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2Z',
  file: 'M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9Zm0 0v6h6M8 13h8M8 17h6',
  image: 'M5 3h14a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2Zm-2 13 5-5 4 4 3-3 6 6M15 7h.01',
  video: 'M4 4h16v16H4ZM9 4v16M4 9h5M4 15h5m4-7 5 4-5 4Z',
  archive: 'M4 5a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v3H4Zm2 3v11a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V8M10 12h4',
  audio: 'M9 18V6l11-2v12M9 18a2.5 2.5 0 1 1-5 0 2.5 2.5 0 0 1 5 0Zm11-2a2.5 2.5 0 1 1-5 0 2.5 2.5 0 0 1 5 0Z',
  document: 'M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9Zm0 0v6h6M8 13h8M8 17h5M8 9h2',
  code: 'm8 9-4 3 4 3m8-6 4 3-4 3m-5 2 2-10',
  eye: 'M2.5 12s3.5-6 9.5-6 9.5 6 9.5 6-3.5 6-9.5 6-9.5-6-9.5-6Zm9.5 2.5a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5Z',
  download: 'M12 3v12m-5-5 5 5 5-5M4 16v4h16v-4',
  select: 'M9 3H5a2 2 0 0 0-2 2v4m12-6h4a2 2 0 0 1 2 2v4M3 15v4a2 2 0 0 0 2 2h4m6 0h4a2 2 0 0 0 2-2v-4M7 12l3 3 7-7',
  close: 'm6 6 12 12M6 18 18 6',
  'chevron-right': 'm9 5 7 7-7 7',
  'chevron-left': 'm15 5-7 7 7 7',
  'chevron-down': 'm6 9 6 6 6-6',
  check: 'm5 12 4 4L19 6',
  pause: 'M8 5v14M16 5v14',
  play: 'm7 4 14 8-14 8Z',
  clock: 'M12 8v4l3 2m6-2a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z',
  alert: 'm12 3 10 18H2Zm0 6v5m0 3h.01',
  lock: 'M7 10V7a5 5 0 0 1 10 0v3M5 10h14v11H5Zm7 4v3',
  connection: 'M5 9a10 10 0 0 1 14 0M8 12a6 6 0 0 1 8 0m-5 4a1 1 0 1 0 2 0 1 1 0 0 0-2 0',
  more: 'M5 12h.01M12 12h.01M19 12h.01',
  info: 'M12 11v6m0-10h.01m9 5a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z',
  sun: 'M12 4v2m0 12v2M4 12h2m12 0h2M6.34 6.34l1.42 1.42m8.48 8.48 1.42 1.42M17.66 6.34l-1.42 1.42m-8.48 8.48-1.42 1.42M16 12a4 4 0 1 1-8 0 4 4 0 0 1 8 0Z',
  moon: 'M20.5 14.5A8.5 8.5 0 1 1 9.5 3.5a6.5 6.5 0 0 0 11 11Z',
  monitor: 'M4 5h16a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1Zm5 15h6m-3-3v3',
  'view-list': 'M8 6h12M8 12h12M8 18h12M4 6h.01M4 12h.01M4 18h.01',
  'view-grid': 'M4 5a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v4a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1Zm10 0a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v4a1 1 0 0 1-1 1h-4a1 1 0 0 1-1-1ZM4 15a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v4a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1Zm10 0a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v4a1 1 0 0 1-1 1h-4a1 1 0 0 1-1-1Z',
} as const

export type ReceiverIconName = keyof typeof ICON_PATHS

/** Labels and status text remain with the control that owns their meaning. */
export function ReceiverIcon({ name, className = '' }: {
  readonly name: ReceiverIconName
  readonly className?: string
}) {
  return <svg className={`receiver-icon ${className}`} viewBox="0 0 24 24" fill="none"
    stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"
    aria-hidden="true" focusable="false"><path d={ICON_PATHS[name]} /></svg>
}
