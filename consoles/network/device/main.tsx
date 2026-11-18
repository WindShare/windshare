import { createRoot } from 'react-dom/client'
import { PreferencesProvider } from '../../ui/PreferencesProvider'
import { DevicePage } from './Consent'

createRoot(document.getElementById('root')!).render(<PreferencesProvider><DevicePage /></PreferencesProvider>)
