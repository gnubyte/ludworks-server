// Spreadsheet-specific toolbar icons (24×24, 1.75px round strokes, currentColor), drawn with
// DOM APIs like core/icons.js; falls back to the core set for every other name.

import { s } from '../../core/dom.js';
import { icon as coreIcon } from '../../core/icons.js';

const P = (d) => ({ t: 'path', a: { d } });
const R = (x, y, w, h, extra = {}) => ({ t: 'rect', a: { x, y, width: w, height: h, rx: 1, ...extra } });

const ICONS = {
  undo: [P('M9 14L4 9l5-5'), P('M4 9h10.5a5.5 5.5 0 0 1 0 11H11')],
  redo: [P('M15 14l5-5-5-5'), P('M20 9H9.5a5.5 5.5 0 0 0 0 11H13')],
  bold: [P('M7 5h6a3.5 3.5 0 0 1 0 7H7z'), P('M7 12h7a3.5 3.5 0 0 1 0 7H7z')],
  italic: [P('M14 5h-4'), P('M14 19h-4'), P('M14 5l-4 14'), P('M18 5h-4'), P('M10 19H6')],
  underline: [P('M7 4v7a5 5 0 0 0 10 0V4'), P('M5 20h14')],
  strike: [P('M16 6.5c-.6-1.4-2.2-2.5-4.3-2.5C9.3 4 7.5 5.3 7.5 7.2c0 1.3.8 2.2 2.4 2.8'), P('M4 12h16'), P('M15.8 14.5c.5.5.7 1.1.7 1.8 0 2.1-2 3.7-4.6 3.7-2.3 0-4.1-1.1-4.8-2.8')],
  'text-color': [P('M6 16l6-12 6 12'), P('M8.5 11h7')],
  'fill-color': [P('M5 12.5L11.5 6l6.5 6.5-6.5 6.5z'), P('M8.5 3.5L11.5 6'), P('M19.5 15.5s1.5 1.8 1.5 2.8a1.5 1.5 0 0 1-3 0c0-1 1.5-2.8 1.5-2.8z')],
  'align-left': [P('M4 6h16'), P('M4 10h10'), P('M4 14h16'), P('M4 18h10')],
  'align-center': [P('M4 6h16'), P('M7 10h10'), P('M4 14h16'), P('M7 18h10')],
  'align-right': [P('M4 6h16'), P('M10 10h10'), P('M4 14h16'), P('M10 18h10')],
  'valign-top': [P('M4 4h16'), P('M12 20V8'), P('M8 12l4-4 4 4')],
  'valign-middle': [P('M4 12h16'), P('M12 3v5'), P('M9.5 5.5L12 8l2.5-2.5'), P('M12 21v-5'), P('M9.5 18.5L12 16l2.5 2.5')],
  'valign-bottom': [P('M4 20h16'), P('M12 4v12'), P('M8 12l4 4 4-4')],
  wrap: [P('M4 6h16'), P('M4 12h13a3 3 0 0 1 0 6h-4'), P('M15 16l-2 2 2 2'), P('M4 18h5')],
  'border-all': [R(4, 4, 16, 16), P('M12 4v16'), P('M4 12h16')],
  'border-outer': [R(4, 4, 16, 16), { t: 'path', a: { d: 'M12 8v8M8 12h8', 'stroke-dasharray': '1.5 2.5' } }],
  'border-none': [{ t: 'path', a: { d: 'M4 4h16v16H4zM12 4v16M4 12h16', 'stroke-dasharray': '1.5 2.5' } }],
  'border-top': [P('M4 4h16'), { t: 'path', a: { d: 'M4 20h16M4 4v16M20 4v16', 'stroke-dasharray': '1.5 2.5' } }],
  'border-bottom': [P('M4 20h16'), { t: 'path', a: { d: 'M4 4h16M4 4v16M20 4v16', 'stroke-dasharray': '1.5 2.5' } }],
  'border-left': [P('M4 4v16'), { t: 'path', a: { d: 'M4 4h16M4 20h16M20 4v16', 'stroke-dasharray': '1.5 2.5' } }],
  'border-right': [P('M20 4v16'), { t: 'path', a: { d: 'M4 4h16M4 20h16M4 4v16', 'stroke-dasharray': '1.5 2.5' } }],
  'clear-format': [P('M5 5h11'), P('M10.5 5L8 13'), P('M14 14l6 6'), P('M20 14l-6 6')],
  'decimal-inc': [P('M4 17h.01'), P('M9 9.5a2.5 2.5 0 0 1 5 0v5a2.5 2.5 0 0 1-5 0z'), P('M17 15h5'), P('M19.5 12.5v5')],
  'decimal-dec': [P('M4 17h.01'), P('M9 9.5a2.5 2.5 0 0 1 5 0v5a2.5 2.5 0 0 1-5 0z'), P('M17 15h5')],
  percent: [P('M19 5L5 19'), { t: 'circle', a: { cx: 7, cy: 7, r: 2.5 } }, { t: 'circle', a: { cx: 17, cy: 17, r: 2.5 } }],
  currency: [P('M12 3v18'), P('M16.5 7.5c-.6-1.5-2.3-2.5-4.5-2.5-2.5 0-4.5 1.3-4.5 3.3 0 4.7 9 2.4 9 7 0 2-2 3.2-4.5 3.2-2.3 0-4-1-4.7-2.6')],
  sigma: [P('M17 5H7l6 7-6 7h10')],
  freeze: [R(3, 3, 18, 18), P('M3 9h18'), P('M9 3v18')],
  'insert-row': [R(3, 4, 18, 6), P('M3 16h18'), P('M12 13v6'), P('M9 16h6')],
  'insert-col': [R(4, 3, 6, 18), P('M16 3v18'), P('M13 12h6'), P('M16 9v6')],
  'delete-row': [R(3, 9, 18, 6), P('M9 3l6 3'), P('M15 3L9 6'), P('M9 18l6 3'), P('M15 18l-6 3')],
  'sort-asc': [P('M7 4v16'), P('M4 17l3 3 3-3'), P('M14 5h4l-4 6h4'), P('M14 19l2-6 2 6'), P('M14.7 17h2.6')],
  'sort-desc': [P('M7 4v16'), P('M4 17l3 3 3-3'), P('M14 13h4l-4 6h4'), P('M14 11l2-6 2 6'), P('M14.7 9h2.6')],
  'paste': [R(6, 4, 12, 17, { rx: 2 }), P('M9 4V3h6v1'), P('M9 10h6'), P('M9 14h6')],
  'function': [P('M14 4h-1.5A2.5 2.5 0 0 0 10 6.5V20'), P('M7 10h6'), P('M15 13l5 6'), P('M20 13l-5 6')],
  robot: [R(5, 8, 14, 11, { rx: 3 }), P('M12 4v4'), { t: 'circle', a: { cx: 12, cy: 3.5, r: 1 } }, P('M9.5 13h.01'), P('M14.5 13h.01'), P('M10 16h4'), P('M3 12v3'), P('M21 12v3')],
};

/**
 * Icon element: sheet-specific names first, then the core icon set.
 * @param {string} name
 * @param {number|{size?: number, className?: string}} [opts]
 */
export function sicon(name, opts = {}) {
  const parts = ICONS[name];
  if (!parts) return coreIcon(name, opts);
  const { size = 16, className = '' } = typeof opts === 'number' ? { size: opts } : opts;
  const svg = s('svg', {
    viewBox: '0 0 24 24', width: size, height: size, fill: 'none', stroke: 'currentColor',
    'stroke-width': 1.75, 'stroke-linecap': 'round', 'stroke-linejoin': 'round', class: ['icon', className],
    focusable: 'false', 'aria-hidden': 'true',
  });
  for (const p of parts) svg.appendChild(s(p.t, p.a));
  return svg;
}

export const sheetIconNames = Object.keys(ICONS);
