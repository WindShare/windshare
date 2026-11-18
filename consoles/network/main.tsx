import { createRoot } from 'react-dom/client'
import { LocaleProvider } from '../ui/LocaleProvider'
import { takeFragment } from '../ui/location'
import { App } from './App'
import './network.css'

const initialToken = takeFragment('initialize')

createRoot(document.getElementById('root')!).render(<LocaleProvider><App initialToken={initialToken} /></LocaleProvider>)
