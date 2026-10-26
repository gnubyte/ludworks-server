// Inline SVG icon set (24x24 grid, 1.75px round strokes, currentColor).
// Built with DOM APIs so icons work under the strict CSP and inherit text color.

import { s } from './dom.js';

const c = (cx, cy, r, fill = false) => ({ t: 'circle', a: { cx, cy, r, ...(fill ? { fill: 'currentColor', stroke: 'none' } : {}) } });
const r = (x, y, width, height, rx = 0) => ({ t: 'rect', a: { x, y, width, height, rx } });

// Gear outline generated from geometry (8 teeth) so it stays crisp and original.
function gearPath() {
  const n = 8, ro = 10, ri = 7.4, cx = 12, cy = 12;
  const pt = (rad, ang) => `${(cx + rad * Math.cos(ang)).toFixed(2)} ${(cy + rad * Math.sin(ang)).toFixed(2)}`;
  const step = (2 * Math.PI) / n;
  const tw = step * 0.2; // half-width of a tooth tip
  const bw = step * 0.3; // half-width of a tooth base
  let d = '';
  for (let i = 0; i < n; i++) {
    const a = i * step - Math.PI / 2;
    d += `${i === 0 ? 'M' : 'L'}${pt(ri, a - bw)} L${pt(ro, a - tw)} L${pt(ro, a + tw)} L${pt(ri, a + bw)} `;
    d += `A${ri} ${ri} 0 0 1 ${pt(ri, a + step - bw)} `;
  }
  return d.trim() + ' Z';
}

const FILE = 'M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z';
const TRAY = 'M4 15v4a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-4';
const SHIELD = 'M12 3l8 3v6c0 4.5-3.4 8.3-8 9-4.6-.7-8-4.5-8-9V6z';

/** Icon definitions: strings are path data; objects are other shapes. */
const ICONS = {
  home: ['M3 10.5 12 3l9 7.5', 'M5 9.5V20a1 1 0 0 0 1 1h4v-6h4v6h4a1 1 0 0 0 1-1V9.5'],
  file: [FILE, 'M14 3v5h5'],
  'file-text': [FILE, 'M14 3v5h5', 'M9 13h6', 'M9 17h6', 'M9 9h2'],
  folder: ['M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z'],
  'folder-open': ['M3 17V7a2 2 0 0 1 2-2h4l2 2h6a2 2 0 0 1 2 2v1', 'M3 17l2.4-6.2A1.5 1.5 0 0 1 6.8 10H20a1 1 0 0 1 .95 1.3l-2 6.3A2 2 0 0 1 17 19H5a2 2 0 0 1-2-2z'],
  image: [r(3, 3, 18, 18, 2), c(9, 9, 2), 'M21 15l-5-5L5 21'],
  film: [r(3, 3, 18, 18, 2), 'M7 3v18', 'M17 3v18', 'M3 7.5h4', 'M3 12h18', 'M3 16.5h4', 'M17 7.5h4', 'M17 16.5h4'],
  tv: [r(2, 7, 20, 14, 2), 'M7 3l5 4 5-4'],
  music: ['M9 18V5l12-2v13', c(6, 18, 3), c(18, 16, 3)],
  gamepad: ['M6 11h4', 'M8 9v4', 'M15 12h.01', 'M18 10h.01', 'M17.3 5H6.7a4 4 0 0 0-3.96 3.43l-.9 6.27A2.6 2.6 0 0 0 4.4 17.7c.8 0 1.5-.4 2-1L8 15h8l1.6 1.7c.5.6 1.2 1 2 1a2.6 2.6 0 0 0 2.56-3l-.9-6.27A4 4 0 0 0 17.3 5z'],
  note: [r(4, 3, 16, 18, 2), 'M8 8h8', 'M8 12h8', 'M8 16h5'],
  lock: [r(4, 11, 16, 10, 2), 'M8 11V7a4 4 0 0 1 8 0v4'],
  unlock: [r(4, 11, 16, 10, 2), 'M8 11V7a4 4 0 0 1 7.75-1.4'],
  key: [c(7.5, 15.5, 4.5), 'M10.7 12.3 20 3', 'M15.5 7.5l3 3', 'M18 5l2.5 2.5'],
  card: [r(2.5, 5, 19, 14, 2.2), 'M2.5 10h19', 'M6 15h4'],
  id: [r(3, 4.5, 18, 15, 2.2), c(9, 11, 2.4), 'M5.6 16.6c.8-1.5 2-2.3 3.4-2.3s2.6.8 3.4 2.3', 'M15 10h3', 'M15 13.5h3'],
  sheet: [r(3, 3, 18, 18, 2), 'M3 9h18', 'M3 15h18', 'M9 3v18'],
  settings: [gearPath(), c(12, 12, 3)],
  sliders: ['M4 21v-7', 'M4 10V3', 'M12 21v-9', 'M12 8V3', 'M20 21v-5', 'M20 12V3', 'M1 14h6', 'M9 8h6', 'M17 16h6'],
  shield: [SHIELD],
  'shield-check': [SHIELD, 'M9 12l2 2 4-4'],
  upload: ['M12 15V3', 'M7 8l5-5 5 5', TRAY],
  download: ['M12 3v12', 'M7 10l5 5 5-5', TRAY],
  search: [c(11, 11, 7), 'M20 20l-3.5-3.5'],
  filter: ['M3 5h18l-7 8.5V19l-4 2v-7.5z'],
  grid: [r(3, 3, 7, 7, 1.5), r(14, 3, 7, 7, 1.5), r(3, 14, 7, 7, 1.5), r(14, 14, 7, 7, 1.5)],
  list: ['M8 6h13', 'M8 12h13', 'M8 18h13', 'M3.5 6h.01', 'M3.5 12h.01', 'M3.5 18h.01'],
  play: ['M7 4.5v15a1 1 0 0 0 1.5.86l12-7.5a1 1 0 0 0 0-1.72l-12-7.5A1 1 0 0 0 7 4.5z'],
  pause: [r(6, 4, 4, 16, 1), r(14, 4, 4, 16, 1)],
  next: ['M5 5l10 7-10 7z', 'M19 5v14'],
  prev: ['M19 19 9 12l10-7z', 'M5 5v14'],
  shuffle: ['M16 3h5v5', 'M4 20 21 3', 'M21 16v5h-5', 'M15 15l6 6', 'M4 4l5 5'],
  edit: ['M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z', 'M15 5l4 4'],
  trash: ['M3 6h18', 'M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2', 'M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6', 'M10 11v6', 'M14 11v6'],
  restore: ['M3 12a9 9 0 1 0 2.64-6.36L3 8', 'M3 3v5h5'],
  star: ['M12 3l2.8 5.7 6.2.9-4.5 4.4 1.06 6.2L12 17.3l-5.56 2.9L7.5 14 3 9.6l6.2-.9z'],
  pin: ['M12 17v5', 'M9 3h6l-1 6 3 3v2H7v-2l3-3z'],
  tag: ['M3 3h8l10 10-8 8L3 11z', c(7.5, 7.5, 1.5)],
  link: ['M10 14a4 4 0 0 0 5.66 0l3-3a4 4 0 0 0-5.66-5.66l-1 1', 'M14 10a4 4 0 0 0-5.66 0l-3 3a4 4 0 0 0 5.66 5.66l1-1'],
  copy: [r(9, 9, 12, 12, 2), 'M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1'],
  eye: ['M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z', c(12, 12, 3)],
  'eye-off': ['M3 3l18 18', 'M10.6 5.1A10.4 10.4 0 0 1 12 5c6.5 0 10 7 10 7a17.6 17.6 0 0 1-3.17 4.19', 'M6.6 6.6C3.9 8.4 2 12 2 12s3.5 7 10 7a9.7 9.7 0 0 0 5.4-1.6', 'M9.9 9.9a3 3 0 0 0 4.2 4.2'],
  user: [c(12, 8, 4), 'M4 21a8 8 0 0 1 16 0'],
  users: [c(9, 8, 4), 'M2 21a7 7 0 0 1 14 0', 'M16 3.13a4 4 0 0 1 0 7.75', 'M22 21a7 7 0 0 0-4-6.3'],
  'user-plus': [c(9, 8, 4), 'M2 21a7 7 0 0 1 14 0', 'M19 8v6', 'M16 11h6'],
  mail: [r(3, 5, 18, 14, 2), 'M3 7l9 6 9-6'],
  server: [r(3, 4, 18, 7, 2), r(3, 13, 18, 7, 2), 'M7 7.5h.01', 'M7 16.5h.01'],
  storage: ['M22 12H2', 'M5.45 5.1 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.9A2 2 0 0 0 16.8 4H7.2a2 2 0 0 0-1.75 1.1z', 'M6 16h.01', 'M10 16h.01'],
  database: ['M4 6c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3z', 'M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6', 'M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3'],
  'chevron-down': ['M6 9l6 6 6-6'],
  'chevron-up': ['M6 15l6-6 6 6'],
  'chevron-left': ['M15 6l-6 6 6 6'],
  'chevron-right': ['M9 6l6 6-6 6'],
  'chevrons-left': ['M11 17l-5-5 5-5', 'M18 17l-5-5 5-5'],
  'chevrons-right': ['M13 17l5-5-5-5', 'M6 17l5-5-5-5'],
  'chevrons-up-down': ['M7 15l5 5 5-5', 'M7 9l5-5 5 5'],
  'arrow-up': ['M12 19V5', 'M5 12l7-7 7 7'],
  'arrow-down': ['M12 5v14', 'M19 12l-7 7-7-7'],
  'arrow-left': ['M19 12H5', 'M12 19l-7-7 7-7'],
  'arrow-right': ['M5 12h14', 'M12 5l7 7-7 7'],
  sort: ['M7 4v16', 'M3 8l4-4 4 4', 'M17 20V4', 'M13 16l4 4 4-4'],
  plus: ['M12 5v14', 'M5 12h14'],
  minus: ['M5 12h14'],
  x: ['M18 6 6 18', 'M6 6l12 12'],
  check: ['M20 6 9 17l-5-5'],
  more: [c(5, 12, 1.25, true), c(12, 12, 1.25, true), c(19, 12, 1.25, true)],
  'more-vertical': [c(12, 5, 1.25, true), c(12, 12, 1.25, true), c(12, 19, 1.25, true)],
  menu: ['M4 6h16', 'M4 12h16', 'M4 18h16'],
  sun: [c(12, 12, 4), 'M12 2v2', 'M12 20v2', 'M4.93 4.93l1.41 1.41', 'M17.66 17.66l1.41 1.41', 'M2 12h2', 'M20 12h2', 'M6.34 17.66l-1.41 1.41', 'M19.07 4.93l-1.41 1.41'],
  moon: ['M20.5 14.1A8.5 8.5 0 1 1 9.9 3.5a6.6 6.6 0 0 0 10.6 10.6z'],
  monitor: [r(2, 3, 20, 14, 2), 'M8 21h8', 'M12 17v4'],
  refresh: ['M21 12a9 9 0 0 1-15.36 6.36L3 16', 'M3 21v-5h5', 'M3 12a9 9 0 0 1 15.36-6.36L21 8', 'M21 3v5h-5'],
  external: ['M14 3h7v7', 'M10 14 21 3', 'M19 14v5a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2h5'],
  'log-out': ['M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4', 'M16 17l5-5-5-5', 'M21 12H9'],
  'log-in': ['M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4', 'M10 17l5-5-5-5', 'M15 12H3'],
  info: [c(12, 12, 9), 'M12 16v-4', 'M12 8h.01'],
  alert: ['M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z', 'M12 9v4', 'M12 17h.01'],
  'alert-circle': [c(12, 12, 9), 'M12 8v4', 'M12 16h.01'],
  'check-circle': [c(12, 12, 9), 'M8 12l3 3 5-6'],
  'x-circle': [c(12, 12, 9), 'M15 9l-6 6', 'M9 9l6 6'],
  clock: [c(12, 12, 9), 'M12 7v5l3 2'],
  calendar: [r(3, 5, 18, 16, 2), 'M3 10h18', 'M8 3v4', 'M16 3v4'],
  globe: [c(12, 12, 9), 'M3 12h18', 'M12 3a14 14 0 0 1 0 18', 'M12 3a14 14 0 0 0 0 18'],
  smartphone: [r(6, 2, 12, 20, 2), 'M11 18h2'],
  laptop: ['M4 6a1 1 0 0 1 1-1h14a1 1 0 0 1 1 1v10H4z', 'M2 19h20'],
  code: ['M16 18l6-6-6-6', 'M8 6l-6 6 6 6'],
  terminal: ['M4 17l6-6-6-6', 'M12 19h8'],
  activity: ['M22 12h-4l-3 9L9 3l-3 9H2'],
  hash: ['M4 9h16', 'M4 15h16', 'M10 3 8 21', 'M16 3l-2 18'],
  layers: ['M12 2 2 7l10 5 10-5z', 'M2 17l10 5 10-5', 'M2 12l10 5 10-5'],
  library: ['M4 19.5A2.5 2.5 0 0 1 6.5 17H20', 'M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z'],
  zap: ['M13 2 3 14h9l-1 8 10-12h-9z'],
  command: ['M18 3a3 3 0 0 0-3 3v12a3 3 0 1 0 3-3H6a3 3 0 1 0 3 3V6a3 3 0 1 0-3 3h12a3 3 0 0 0 0-6z'],
  keyboard: [r(2, 6, 20, 12, 2), 'M6 10h.01', 'M10 10h.01', 'M14 10h.01', 'M18 10h.01', 'M7 14h10'],
  archive: [r(3, 4, 18, 4, 1), 'M5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8', 'M10 12h4'],
  box: ['M21 8 12 3 3 8v8l9 5 9-5z', 'M3 8l9 5 9-5', 'M12 13v8'],
  radio: [c(12, 12, 2), 'M16.24 7.76a6 6 0 0 1 0 8.48', 'M7.76 16.24a6 6 0 0 1 0-8.48', 'M19.07 4.93a10 10 0 0 1 0 14.14', 'M4.93 19.07a10 10 0 0 1 0-14.14'],
  qr: [r(3, 3, 7, 7, 1), r(14, 3, 7, 7, 1), r(3, 14, 7, 7, 1), 'M14 14h3v3h-3z', 'M20 14v.01', 'M14 20h.01', 'M17 20h4v-3'],
  pie: ['M21 12A9 9 0 1 1 12 3v9z', 'M15 3.5A9 9 0 0 1 20.5 9H15z'],
  'bar-chart': ['M4 20V10', 'M10 20V4', 'M16 20v-7', 'M21 20H3'],
  cpu: [r(6, 6, 12, 12, 2), 'M9 2v4', 'M15 2v4', 'M9 18v4', 'M15 18v4', 'M2 9h4', 'M2 15h4', 'M18 9h4', 'M18 15h4'],
  help: [c(12, 12, 9), 'M9.1 9a3 3 0 0 1 5.8 1c0 2-3 3-3 3', 'M12 17h.01'],
  book: ['M2 4h6a4 4 0 0 1 4 4v13a3 3 0 0 0-3-3H2z', 'M22 4h-6a4 4 0 0 0-4 4v13a3 3 0 0 1 3-3h7z'],
  history: ['M3 12a9 9 0 1 0 2.64-6.36L3 8', 'M3 3v5h5', 'M12 7v5l4 2'],
  sparkle: ['M12 3l1.9 5.6L19.5 10l-5.6 1.9L12 17.5l-1.9-5.6L4.5 10l5.6-1.4z', 'M19 17v4', 'M17 19h4'],
  ban: [c(12, 12, 9), 'M5.6 5.6l12.8 12.8'],
  'panel-left': [r(3, 3, 18, 18, 2), 'M9 3v18'],
  'circle-dot': [c(12, 12, 9), c(12, 12, 2, true)],
  fingerprint: ['M12 11v3a6 6 0 0 1-1 3.3', 'M8.5 7.6A5 5 0 0 1 17 11v2.5', 'M6.6 10.5A5.5 5.5 0 0 0 7 13v1a9 9 0 0 1-.6 3.2', 'M17 17.5c-.3 1-.7 2-1.2 2.9', 'M3.5 9a9 9 0 0 1 16.4-1.5', 'M20.5 12v1'],
};

/** All icon names (for pickers and tests). */
export const iconNames = Object.keys(ICONS);

/**
 * Build an SVG icon element.
 * @param {string} name icon name (unknown names render an empty frame)
 * @param {{size?: number, className?: string, label?: string, strokeWidth?: number}} [opts]
 *   label → role="img" + <title>; otherwise the icon is aria-hidden (decorative).
 * @returns {SVGSVGElement}
 */
export function icon(name, opts = {}) {
  const { size = 16, className = '', label = '', strokeWidth = 1.75 } = typeof opts === 'number' ? { size: opts } : opts;
  const svg = s('svg', {
    viewBox: '0 0 24 24', width: size, height: size, fill: 'none', stroke: 'currentColor',
    'stroke-width': strokeWidth, 'stroke-linecap': 'round', 'stroke-linejoin': 'round',
    class: ['icon', className], focusable: 'false',
  });
  if (label) {
    svg.setAttribute('role', 'img');
    svg.setAttribute('aria-label', label);
    svg.appendChild(s('title', { text: label }));
  } else {
    svg.setAttribute('aria-hidden', 'true');
  }
  for (const part of ICONS[name] || []) {
    if (typeof part === 'string') svg.appendChild(s('path', { d: part }));
    else svg.appendChild(s(part.t, part.a));
  }
  return svg;
}

/**
 * The Ludworks brand mark: an accent tile with three stacked "drive bays".
 * @param {number} [size]
 */
export function logoMark(size = 28) {
  return s('svg', { viewBox: '0 0 32 32', width: size, height: size, class: 'logo-mark', 'aria-hidden': 'true', focusable: 'false' },
    s('rect', { x: 1, y: 1, width: 30, height: 30, rx: 8, class: 'logo-bg' }),
    s('rect', { x: 8, y: 8.5, width: 16, height: 3.5, rx: 1.75, class: 'logo-fg' }),
    s('rect', { x: 8, y: 14.25, width: 16, height: 3.5, rx: 1.75, class: 'logo-fg' }),
    s('rect', { x: 8, y: 20, width: 9, height: 3.5, rx: 1.75, class: 'logo-fg' }),
    s('circle', { cx: 22, cy: 21.75, r: 1.9, class: 'logo-dot' }),
  );
}
