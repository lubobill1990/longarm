import type { ReactNode, SVGProps } from 'react'

/** Minimal 16 px stroke icons (currentColor), the desktop apps' icon style. */
type P = SVGProps<SVGSVGElement> & { size?: number }

function Svg({ size = 16, children, ...rest }: P & { children: ReactNode }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.4}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
      {...rest}
    >
      {children}
    </svg>
  )
}

/** Workers: stacked machines. */
export const IconServers = (p: P) => (
  <Svg {...p}>
    <rect x="2.5" y="2.75" width="11" height="4.5" rx="1.25" />
    <rect x="2.5" y="8.75" width="11" height="4.5" rx="1.25" />
    <path d="M5 5h.01M5 11h.01" strokeWidth={2} />
  </Svg>
)

/** My jobs: a terminal prompt. */
export const IconTerminal = (p: P) => (
  <Svg {...p}>
    <rect x="2" y="2.75" width="12" height="10.5" rx="1.75" />
    <path d="m4.75 6.25 2 1.75-2 1.75M8.5 10h2.75" />
  </Svg>
)

/** Forwards: arrows both ways. */
export const IconForward = (p: P) => (
  <Svg {...p}>
    <path d="M2.75 5.5h9.5m0 0L10 3.25M12.25 5.5 10 7.75M13.25 10.5h-9.5m0 0L6 8.25M3.75 10.5 6 12.75" />
  </Svg>
)

/** This worker: a chip. */
export const IconChip = (p: P) => (
  <Svg {...p}>
    <rect x="4" y="4" width="8" height="8" rx="1.5" />
    <path d="M6.5 1.75V4M9.5 1.75V4M6.5 12v2.25M9.5 12v2.25M1.75 6.5H4M1.75 9.5H4M12 6.5h2.25M12 9.5h2.25" />
  </Svg>
)

export const IconSettings = (p: P) => (
  <Svg {...p}>
    <circle cx="8" cy="8" r="2" />
    <path d="M8 1.75v1.5M8 12.75v1.5M14.25 8h-1.5M3.25 8h-1.5M12.42 3.58l-1.06 1.06M4.64 11.36l-1.06 1.06M12.42 12.42l-1.06-1.06M4.64 4.64 3.58 3.58" />
  </Svg>
)

export const IconUser = (p: P) => (
  <Svg {...p}>
    <circle cx="8" cy="5.5" r="2.75" />
    <path d="M2.75 13.75c.6-2.5 2.7-4 5.25-4s4.65 1.5 5.25 4" />
  </Svg>
)

export const IconPin = (p: P) => (
  <Svg {...p}>
    <path d="M6 2.25h4M6.75 2.25v4L4.5 8.75v1h7v-1L9.25 6.25v-4M8 9.75v4" />
  </Svg>
)

export const IconClose = (p: P) => (
  <Svg {...p}>
    <path d="m4 4 8 8M12 4l-8 8" />
  </Svg>
)

export const IconMore = (p: P) => (
  <Svg {...p}>
    <path d="M3.5 8h.01M8 8h.01M12.5 8h.01" strokeWidth={2.4} />
  </Svg>
)

export const IconExternal = (p: P) => (
  <Svg {...p} size={p.size ?? 12}>
    <path d="M6.5 3.25h-3.25v9.5h9.5V9.5M9.25 3.25h3.5v3.5M12.5 3.5 7.5 8.5" />
  </Svg>
)

export const IconAlert = (p: P) => (
  <Svg {...p}>
    <path d="M8 2.5 14 13H2L8 2.5z" />
    <path d="M8 6.5v3M8 11.25h.01" />
  </Svg>
)

export const IconInfo = (p: P) => (
  <Svg {...p}>
    <circle cx="8" cy="8" r="5.75" />
    <path d="M8 7.25v3.5M8 5.25h.01" />
  </Svg>
)

export const IconCheck = (p: P) => (
  <Svg {...p}>
    <circle cx="8" cy="8" r="5.75" />
    <path d="m5.5 8.25 1.75 1.75 3.25-3.5" />
  </Svg>
)

export const IconDownload = (p: P) => (
  <Svg {...p}>
    <path d="M8 2.5v7.5m0 0L5 7m3 3 3-3M3 12.5h10" />
  </Svg>
)
