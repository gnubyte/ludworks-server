// Player icons missing from the core set (same 24px grid, 1.75px round strokes, currentColor).
// Falls back to core icons for everything else.

import { s } from '../../core/dom.js';
import { icon as coreIcon } from '../../core/icons.js';

const P = {
  volume: ['M4 9.5h3.5L12 5.5v13l-4.5-4H4z', 'M15.5 9a4 4 0 0 1 0 6', 'M18.2 6.5a7.5 7.5 0 0 1 0 11'],
  'volume-x': ['M4 9.5h3.5L12 5.5v13l-4.5-4H4z', 'M16 9.5l5 5', 'M21 9.5l-5 5'],
  maximize: ['M4 9V4h5', 'M20 9V4h-5', 'M4 15v5h5', 'M20 15v5h-5'],
  minimize: ['M9 4v5H4', 'M15 4v5h5', 'M9 20v-5H4', 'M15 20v-5h5'],
  pip: ['M3 6.5A1.5 1.5 0 0 1 4.5 5h15A1.5 1.5 0 0 1 21 6.5v11a1.5 1.5 0 0 1-1.5 1.5h-15A1.5 1.5 0 0 1 3 17.5z', 'M12.5 12.5h6v4h-6z'],
  captions: ['M3 6.5A1.5 1.5 0 0 1 4.5 5h15A1.5 1.5 0 0 1 21 6.5v11a1.5 1.5 0 0 1-1.5 1.5h-15A1.5 1.5 0 0 1 3 17.5z', 'M10.5 10.2a2.5 2.5 0 1 0 0 3.6', 'M17 10.2a2.5 2.5 0 1 0 0 3.6'],
  'channel-up': ['M7 14l5-5 5 5'],
  'channel-down': ['M7 10l5 5 5-5'],
  rewind: ['M11 7l-6 5 6 5z', 'M19 7l-6 5 6 5z'],
  forward: ['M13 7l6 5-6 5z', 'M5 7l6 5-6 5z'],
  'skip-next': ['M6 6l9 6-9 6z', 'M18 6v12'],
  antenna: ['M12 12v9', 'M8 21h8', 'M4.5 4.5a10.5 10.5 0 0 0 0 15', 'M19.5 4.5a10.5 10.5 0 0 1 0 15', 'M8 8a5.5 5.5 0 0 0 0 8', 'M16 8a5.5 5.5 0 0 1 0 8'],
  grip: ['M9 6h.01', 'M15 6h.01', 'M9 12h.01', 'M15 12h.01', 'M9 18h.01', 'M15 18h.01'],
};

/**
 * Icon by name: TV-specific paths first, then the core icon set.
 * @param {string} name
 * @param {number|{size?: number, className?: string, label?: string}} [opts]
 */
export function tvIcon(name, opts = {}) {
  const parts = P[name];
  if (!parts) return coreIcon(name, opts);
  const { size = 16, className = '', label = '' } = typeof opts === 'number' ? { size: opts } : opts;
  const svg = s('svg', {
    viewBox: '0 0 24 24', width: size, height: size, fill: 'none', stroke: 'currentColor',
    'stroke-width': name === 'grip' ? 3 : 1.75, 'stroke-linecap': 'round', 'stroke-linejoin': 'round',
    class: ['icon', className], focusable: 'false',
  });
  if (label) {
    svg.setAttribute('role', 'img');
    svg.setAttribute('aria-label', label);
    svg.appendChild(s('title', { text: label }));
  } else {
    svg.setAttribute('aria-hidden', 'true');
  }
  for (const d of parts) svg.appendChild(s('path', { d }));
  return svg;
}

/** Names this module adds (for tests). */
export const tvIconNames = Object.keys(P);
