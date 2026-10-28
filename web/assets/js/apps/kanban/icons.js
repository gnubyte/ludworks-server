// Kanban-specific icons (24×24, 1.75px round strokes, currentColor) drawn with DOM APIs like
// core/icons.js; every other name falls back to the core set.

import { s } from '../../core/dom.js';
import { icon as coreIcon } from '../../core/icons.js';

const P = (d) => ({ t: 'path', a: { d } });
const C = (cx, cy, r, fill = false) => ({ t: 'circle', a: { cx, cy, r, ...(fill ? { fill: 'currentColor', stroke: 'none' } : {}) } });
const R = (x, y, width, height, rx = 2) => ({ t: 'rect', a: { x, y, width, height, rx } });

const ICONS = {
  paperclip: [P('M21 11.5l-8.6 8.6a5.5 5.5 0 0 1-7.8-7.8l8.6-8.6a3.7 3.7 0 0 1 5.2 5.2l-8.6 8.6a1.8 1.8 0 0 1-2.6-2.6l8-8')],
  comment: [P('M21 12a8 8 0 0 1-11.7 7.1L4 20.5l1.4-5A8 8 0 1 1 21 12z')],
  flag: [P('M5 21V4'), P('M5 4h11l-2 4 2 4H5')],
  grip: [C(9, 6, 1.2, true), C(15, 6, 1.2, true), C(9, 12, 1.2, true), C(15, 12, 1.2, true), C(9, 18, 1.2, true), C(15, 18, 1.2, true)],
  checklist: [R(3.5, 3.5, 17, 17, 3), P('M8 12.2l2.6 2.6L16.2 9')],
  'align-left': [P('M4 6h16'), P('M4 10.5h11'), P('M4 15h16'), P('M4 19.5h9')],
  columns: [R(3, 4, 5, 16, 1.5), R(9.5, 4, 5, 11, 1.5), R(16, 4, 5, 7, 1.5)],
  'board-plus': [R(3, 4, 5, 16, 1.5), R(9.5, 4, 5, 9, 1.5), P('M18.5 12v8'), P('M14.5 16h8')],
  cover: [R(3, 3, 18, 18, 2), P('M3 10h18'), C(8, 6.5, 1)],
  move: [P('M5 9l-3 3 3 3'), P('M9 5l3-3 3 3'), P('M15 19l-3 3-3-3'), P('M19 9l3 3-3 3'), P('M2 12h20'), P('M12 2v20')],
  'arrow-up-down': [P('M7 4v16'), P('M3.5 7.5L7 4l3.5 3.5'), P('M17 20V4'), P('M13.5 16.5L17 20l3.5-3.5')],
  promote: [R(4, 13, 16, 7, 2), P('M12 10V3'), P('M8.5 6.5L12 3l3.5 3.5')],
  collapse: [P('M4 4v16'), P('M20 4v16'), P('M14 8l-4 4 4 4')],
  expand: [P('M4 4v16'), P('M20 4v16'), P('M10 8l4 4-4 4')],
  palette: [P('M12 3a9 9 0 0 0 0 18c1.4 0 2-1 2-2 0-1.4-1.2-1.7-1.2-3 0-1.1.9-2 2-2H17a4 4 0 0 0 4-4c0-3.9-4-7-9-7z'), C(7.5, 11, 1, true), C(10, 7, 1, true), C(14.5, 7, 1, true), C(17, 10.5, 1, true)],
  limit: [P('M4 18h16'), P('M6 14V8'), P('M10 14V6'), P('M14 14V9'), P('M18 4l-2 2')],
  person: [C(12, 8, 3.5), P('M5 20a7 7 0 0 1 14 0')],
  inbox: [P('M3 13l3-8h12l3 8'), P('M3 13v6a1 1 0 0 0 1 1h16a1 1 0 0 0 1-1v-6h-5l-1.5 2.5h-5L8 13z')],
};

/**
 * Icon element: kanban names first, then the core icon set.
 * @param {string} name
 * @param {number|{size?: number, className?: string}} [opts]
 */
export function kicon(name, opts = {}) {
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

export const kanbanIconNames = Object.keys(ICONS);
