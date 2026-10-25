import type { ReactNode, SVGProps } from 'react';

export type IconName =
  | 'hand'
  | 'release'
  | 'camera'
  | 'fullscreen'
  | 'exitFullscreen'
  | 'fit'
  | 'native'
  | 'keyboard'
  | 'clipboard'
  | 'upload'
  | 'download'
  | 'warning'
  | 'check'
  | 'x'
  | 'undo'
  | 'redo'
  | 'select'
  | 'rect'
  | 'arrow'
  | 'pen'
  | 'text'
  | 'pin'
  | 'redact'
  | 'crop'
  | 'zoomIn'
  | 'zoomOut'
  | 'pan'
  | 'trash'
  | 'settings'
  | 'refresh'
  | 'send'
  | 'lock'
  | 'eye'
  | 'chevronDown'
  | 'chevronUp'
  | 'activity'
  | 'file'
  | 'plug'
  | 'back'
  | 'power'
  | 'link'
  | 'ellipse'
  | 'diamond'
  | 'line'
  | 'copy'
  | 'toFront'
  | 'toBack'
  | 'forward'
  | 'backward'
  | 'alignLeft'
  | 'alignCenter'
  | 'alignRight'
  | 'alignTop'
  | 'alignMiddle'
  | 'alignBottom'
  | 'drawing'
  | 'highlight'
  | 'spotlight'
  | 'blur'
  | 'pixelate'
  | 'filledRect'
  | 'step'
  | 'arrowFilled'
  | 'arrowOpen'
  | 'arrowDouble'
  | 'arrowCurved'
  | 'smooth'
  | 'plus'
  | 'eyedropper'
  | 'pause'
  | 'play';

const PATHS: Record<IconName, ReactNode> = {
  hand: <path d="M8 13V5.5a1.5 1.5 0 0 1 3 0V11m0-1V4.5a1.5 1.5 0 0 1 3 0V11m0-4.5a1.5 1.5 0 0 1 3 0V13m0-3.5a1.5 1.5 0 0 1 3 0V15a6 6 0 0 1-6 6h-1a6 6 0 0 1-5-2.7L4.5 14a1.5 1.5 0 0 1 2.4-1.8L8 13.5" />,
  release: <path d="M4 12h11m0 0-4-4m4 4-4 4M20 4v16" />,
  camera: <path d="M4 8h3l1.5-2h7L17 8h3v11H4zM12 17a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7z" />,
  fullscreen: <path d="M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5" />,
  exitFullscreen: <path d="M9 4v5H4M15 4v5h5M9 20v-5H4M15 20v-5h5" />,
  fit: <path d="M4 4h16v16H4zM8 8h8v8H8z" />,
  native: <path d="M5 5h14v14H5zM9 12h6M12 9v6" />,
  keyboard: <path d="M3 7h18v10H3zM6 10h1m3 0h1m3 0h1m3 0h1M7 14h10" />,
  clipboard: <path d="M9 4h6v3H9zM7 5.5H5V20h14V5.5h-2M9 12h6M9 16h4" />,
  upload: <path d="M12 16V4m0 0-4 4m4-4 4 4M4 16v4h16v-4" />,
  download: <path d="M12 4v12m0 0-4-4m4 4 4-4M4 20h16" />,
  warning: <path d="M12 4 2.5 20h19zM12 10v5m0 2.5v.5" />,
  check: <path d="m4 12.5 5 5L20 6.5" />,
  x: <path d="M5 5l14 14M19 5 5 19" />,
  undo: <path d="M9 14 4 9l5-5M4 9h10a6 6 0 0 1 0 12h-3" />,
  redo: <path d="m15 14 5-5-5-5M20 9H10a6 6 0 0 0 0 12h3" />,
  select: <path d="M5 3l14 8-6 1.5L10.5 19z" />,
  rect: <path d="M4 6h16v12H4z" />,
  arrow: <path d="M5 19 19 5m0 0H9m10 0v10" />,
  pen: <path d="M4 20c4-1 4-7 8-7s2 5 8-9" />,
  text: <path d="M5 6V4h14v2M12 4v16M9 20h6" />,
  pin: <path d="M12 21s6-5.5 6-11a6 6 0 0 0-12 0c0 5.5 6 11 6 11zM12 12.5a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5z" />,
  redact: <path d="M4 7h16v10H4z" fill="currentColor" />,
  crop: <path d="M7 3v14h14M3 7h14v14" />,
  zoomIn: <path d="M10.5 18a7.5 7.5 0 1 0 0-15 7.5 7.5 0 0 0 0 15zM16 16l5 5M10.5 7.5v6M7.5 10.5h6" />,
  zoomOut: <path d="M10.5 18a7.5 7.5 0 1 0 0-15 7.5 7.5 0 0 0 0 15zM16 16l5 5M7.5 10.5h6" />,
  pan: <path d="M12 3v18M3 12h18M12 3 9.5 5.5M12 3l2.5 2.5M12 21l-2.5-2.5M12 21l2.5-2.5M3 12l2.5-2.5M3 12l2.5 2.5M21 12l-2.5-2.5M21 12l-2.5 2.5" />,
  trash: <path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13M10 11v6M14 11v6" />,
  settings: <path d="M4 7h10M18 7h2M4 17h2M10 17h10M16 4v6M8 14v6" />,
  refresh: <path d="M20 12a8 8 0 1 1-2.5-5.8M20 4v5h-5" />,
  send: <path d="M3 11 21 3l-8 18-2-7z" />,
  lock: <path d="M6 11h12v9H6zM8.5 11V8a3.5 3.5 0 0 1 7 0v3" />,
  eye: <path d="M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12zM12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z" />,
  chevronDown: <path d="m6 9 6 6 6-6" />,
  chevronUp: <path d="m6 15 6-6 6 6" />,
  activity: <path d="M3 12h4l3-8 4 16 3-8h4" />,
  file: <path d="M6 3h8l4 4v14H6zM14 3v4h4" />,
  plug: <path d="M9 3v5M15 3v5M6 8h12v4a6 6 0 0 1-12 0zM12 18v3" />,
  back: <path d="M10 6l-6 6 6 6M4 12h16" />,
  power: <path d="M12 3v9M6.5 6.5a8 8 0 1 0 11 0" />,
  link: <path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1" />,
  ellipse: <path d="M12 5c5 0 9 3 9 7s-4 7-9 7-9-3-9-7 4-7 9-7z" />,
  diamond: <path d="M12 3 21 12 12 21 3 12z" />,
  line: <path d="M5 19 19 5" />,
  copy: <path d="M9 9h11v11H9zM15 9V4H4v11h5" />,
  toFront: <path d="M9 9h11v11H9zM4 4h8v5M4 4v8h5" />,
  toBack: <path d="M4 4h11v11H4zM15 9h5v11H9v-5" />,
  forward: <path d="M12 20V6m0 0-5 5m5-5 5 5M5 3h14" />,
  backward: <path d="M12 4v14m0 0-5-5m5 5 5-5M5 21h14" />,
  alignLeft: <path d="M4 3v18M8 7h12M8 14h7" />,
  alignCenter: <path d="M12 3v18M5 7h14M8 14h8" />,
  alignRight: <path d="M20 3v18M4 7h12M9 14h7" />,
  alignTop: <path d="M3 4h18M7 8v12M14 8v7" />,
  alignMiddle: <path d="M3 12h18M7 5v14M14 8v8" />,
  alignBottom: <path d="M3 20h18M7 4v12M14 9v7" />,
  drawing: <path d="M4 20l1-4L16.5 4.5a2 2 0 0 1 3 3L8 19zM14 7l3 3" />,
  highlight: <path d="M14 4l6 6-8.5 8.5H6V13zM4 21h16" />,
  spotlight: (
    <>
      <path d="M3 3h18v18H3zM8 8v8h8V8z" fill="currentColor" fillOpacity={0.3} fillRule="evenodd" />
      <path d="M8 8h8v8H8z" />
    </>
  ),
  blur: <path d="M12 3s6 6.5 6 11a6 6 0 0 1-12 0c0-4.5 6-11 6-11zM9.5 15a2.5 2.5 0 0 0 2.5 2.5" />,
  pixelate: (
    <>
      <path d="M4 4h5v5H4zM14 4h5v5h-5zM9 9h5v5H9zM4 14h5v5H4zM14 14h5v5h-5z" />
      <path d="M9 9h5v5H9zM4 4h5v5H4zM14 14h5v5h-5z" fill="currentColor" fillOpacity={0.35} />
    </>
  ),
  filledRect: <path d="M4 6h16v12H4z" fill="currentColor" fillOpacity={0.55} />,
  step: <path d="M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM10.5 9.5 12.5 8v8" />,
  arrowFilled: (
    <>
      <path d="M5 19 14 10" />
      <path d="M20 4l-9.5 3 6.5 6.5z" fill="currentColor" />
    </>
  ),
  arrowOpen: <path d="M5 19 19 5M10 5h9v9" />,
  arrowDouble: <path d="M7 17 17 7M11 7h6v6M13 17H7v-6" />,
  arrowCurved: <path d="M5 19C6 10 12 6 19 6M14.5 2.5 19 6l-4 4" />,
  smooth: <path d="M3 14c3-8 6-8 9 0s6 8 9 0" />,
  plus: <path d="M12 5v14M5 12h14" />,
  eyedropper: <path d="M20 4a2.5 2.5 0 0 0-3.5 0L13 7.5 16.5 11 20 7.5a2.5 2.5 0 0 0 0-3.5zM13 7.5l-8 8V19h3.5l8-8" />,
  pause: <path d="M7 4h3.5v16H7zM13.5 4H17v16h-3.5z" fill="currentColor" />,
  play: <path d="M7 4.5 19 12 7 19.5z" fill="currentColor" />,
};

interface IconProps extends Omit<SVGProps<SVGSVGElement>, 'name'> {
  name: IconName;
  size?: number;
}

export function Icon({ name, size = 18, ...rest }: IconProps): ReactNode {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.75}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
      {...rest}
    >
      {PATHS[name]}
    </svg>
  );
}
