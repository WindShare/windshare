import { createRoot } from 'react-dom/client'
import { PreferencesProvider } from '../../ui/PreferencesProvider'
import { CompletionPage } from './Completion'

createRoot(document.getElementById('root')!).render(<PreferencesProvider><CompletionPage /></PreferencesProvider>)
