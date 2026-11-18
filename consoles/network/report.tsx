import { createRoot } from 'react-dom/client'
import { takeFragment } from '../ui/location'
import { ReportForm } from './ReportForm'
import { decode, encode, SHARE_BYTES } from './identity'

function publicShareFragment() {
  const share = takeFragment('share')
  if (!share) return ''
  try {
    return encode(decode(share, SHARE_BYTES, 'share ID'))
  } catch {
    return ''
  }
}
const initialShare = publicShareFragment()

createRoot(document.getElementById('root')!).render(<ReportForm initialShare={initialShare} />)
