import { useId, type CSSProperties } from 'react'

// The mark: three chevrons receding into an indigo tile.
//
// It has to survive two things that most logos never face here. The header sits
// on white in the light theme and on near-black in the dark one, so the mark
// carries its own background rather than relying on the page for contrast — a
// bare glyph tuned for one theme is invisible in the other. And the same
// artwork is the browser favicon at sixteen pixels, where anything with fine
// detail turns to porridge; the chevrons fade towards the tail, so as the icon
// shrinks the faint ones dissolve into the tile and the leading one is still a
// clean arrow.
//
// Why chevrons: a fleet is many of something moving the same way, and the
// depth ordering — larger, brighter, further ahead — says that in one glance
// without spelling out a runner, a machine or a container.
const chevrons = [
  { d: 'M6.2 10.2 11.2 16 6.2 21.8', width: 2.6, opacity: 0.3 },
  { d: 'M12.2 9 18.3 16 12.2 23', width: 3, opacity: 0.58 },
  { d: 'M18.3 7.9 25.3 16 18.3 24.1', width: 3.6, opacity: 1 },
]

export interface LogoProps {
  /** Rendered edge length in pixels; the artwork is square. */
  size?: number
  /** Accessible name, or the empty string for a purely decorative mark. */
  title?: string
  /** Placement is the caller's business; the mark itself has no margin. */
  style?: CSSProperties
}

export function Logo({ size = 28, title = 'runner-fleet', style }: LogoProps) {
  // Two logos on one page would otherwise share gradient ids, and the second
  // definition silently repaints the first. React has spelled these ids with
  // colons in it; a colon is legal in a fragment reference and confuses enough
  // tooling that it is worth dropping.
  const uid = useId().replace(/:/g, '')

  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 32 32"
      style={style}
      role="img"
      aria-hidden={title === '' ? true : undefined}
      xmlns="http://www.w3.org/2000/svg"
    >
      {title !== '' && <title>{title}</title>}
      <defs>
        <linearGradient id={`${uid}-tile`} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="#7C86FF" />
          <stop offset="0.5" stopColor="#5A54F0" />
          <stop offset="1" stopColor="#3A2FC2" />
        </linearGradient>
        <linearGradient id={`${uid}-sheen`} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopColor="#FFFFFF" stopOpacity="0.16" />
          <stop offset="0.6" stopColor="#FFFFFF" stopOpacity="0" />
        </linearGradient>
      </defs>
      <rect width="32" height="32" rx="8" fill={`url(#${uid}-tile)`} />
      <rect width="32" height="32" rx="8" fill={`url(#${uid}-sheen)`} />
      {chevrons.map((chevron) => (
        <path
          key={chevron.d}
          d={chevron.d}
          fill="none"
          stroke="#FFFFFF"
          strokeOpacity={chevron.opacity}
          strokeWidth={chevron.width}
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      ))}
      {/* A hairline of the tile's own light, so the dark corner of the gradient
          still has an edge against a dark page. */}
      <rect
        x="0.5"
        y="0.5"
        width="31"
        height="31"
        rx="7.5"
        fill="none"
        stroke="#FFFFFF"
        strokeOpacity="0.18"
      />
    </svg>
  )
}
