import { createRoot } from 'react-dom/client'
import { PreferencesProvider } from '../ui/PreferencesProvider'
import { takeFragment } from '../ui/location'
import { ReportForm } from './ReportForm'
import { decode, encode } from './identity'

function publicShareFragment() {
  const share = takeFragment('share')
  const bytes = share && decode(share, 'share')
  return bytes ? encode(bytes) : ''
}
const initialShare = publicShareFragment()

createRoot(document.getElementById('root')!).render(<PreferencesProvider><ReportForm initialShare={initialShare} /></PreferencesProvider>)
