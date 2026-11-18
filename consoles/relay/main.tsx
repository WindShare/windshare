import { createRoot } from 'react-dom/client'
import { LocaleProvider } from '../ui/LocaleProvider'
import { takeFragment } from '../ui/location'
import { App } from './App'
import { deviceCode } from './pairing-code'

const initialToken = takeFragment('initialize')
const code = deviceCode(new URLSearchParams(location.search).get('code') ?? '')

createRoot(document.getElementById('root')!).render(<LocaleProvider><App initialToken={initialToken} code={code} /></LocaleProvider>)
