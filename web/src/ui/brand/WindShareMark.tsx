import './windshare-mark.css'

interface WindShareMarkProps {
  readonly size: number
}

export function WindShareMark({ size }: WindShareMarkProps) {
  return <img className="windshare-mark" src={`${import.meta.env.BASE_URL}brand/windshare.svg`}
    width={size} height={size} alt="" aria-hidden="true" draggable={false} />
}
