// Small SVG/DOM charts for storage views: donut, stacked bar and legend.
// Colours come from CSS custom properties so both themes stay legible.

import { h, s } from './dom.js';
import { formatBytes } from './format.js';

/** Known categories (API.md §5) with labels and CSS colour variables. */
export const CATEGORY_META = {
  movie: { label: 'Movies', color: 'var(--cat-movie)' },
  tv: { label: 'TV', color: 'var(--cat-tv)' },
  music: { label: 'Music', color: 'var(--cat-music)' },
  image: { label: 'Images', color: 'var(--cat-image)' },
  game: { label: 'Games', color: 'var(--cat-game)' },
  document: { label: 'Documents', color: 'var(--cat-document)' },
  software: { label: 'Software', color: 'var(--cat-software)' },
  other: { label: 'Other', color: 'var(--cat-other)' },
  '': { label: 'Uncategorized', color: 'var(--cat-none)' },
  trash: { label: 'Trash', color: 'var(--cat-trash)' },
};

const FALLBACK = ['var(--cat-movie)', 'var(--cat-tv)', 'var(--cat-music)', 'var(--cat-image)', 'var(--cat-game)', 'var(--cat-document)', 'var(--cat-software)', 'var(--cat-other)'];

/**
 * Turn {category: bytes} into sorted segments [{key, label, value, color}] (largest first).
 * @param {Record<string, number>} byCategory
 * @param {{trashBytes?: number}} [opts]
 */
export function categorySegments(byCategory, { trashBytes = 0 } = {}) {
  const segs = Object.entries(byCategory || {})
    .filter(([, v]) => Number(v) > 0)
    .map(([k, v], i) => {
      const meta = CATEGORY_META[k] || { label: k.charAt(0).toUpperCase() + k.slice(1), color: FALLBACK[i % FALLBACK.length] };
      return { key: k, label: meta.label, value: Number(v), color: meta.color };
    })
    .sort((a, b) => b.value - a.value);
  if (trashBytes > 0) segs.push({ key: 'trash', label: 'Trash', value: trashBytes, color: CATEGORY_META.trash.color });
  return segs;
}

/**
 * Donut chart.
 * @param {Array<{label: string, value: number, color: string}>} segments
 * @param {{size?: number, thickness?: number, centerLabel?: string, centerValue?: string, ariaLabel?: string}} [opts]
 */
export function donutChart(segments, { size = 168, thickness = 18, centerLabel = '', centerValue = '', ariaLabel = 'Chart' } = {}) {
  const total = segments.reduce((sum, x) => sum + x.value, 0);
  const r = (size - thickness) / 2;
  const c = 2 * Math.PI * r;
  const svg = s('svg', { viewBox: `0 0 ${size} ${size}`, width: size, height: size, class: 'donut', role: 'img', 'aria-label': ariaLabel });
  svg.append(s('circle', { cx: size / 2, cy: size / 2, r, class: 'donut-track', 'stroke-width': thickness, fill: 'none' }));
  if (total > 0) {
    let offset = 0;
    const gap = segments.length > 1 ? Math.min(2, c * 0.004) : 0; // hairline gap between segments
    for (const seg of segments) {
      const len = (seg.value / total) * c;
      const arc = s('circle', {
        cx: size / 2, cy: size / 2, r, fill: 'none', 'stroke-width': thickness, class: 'donut-seg',
        'stroke-dasharray': `${Math.max(0, len - gap)} ${c}`,
        'stroke-dashoffset': String(-offset),
        transform: `rotate(-90 ${size / 2} ${size / 2})`,
      });
      arc.style.stroke = seg.color;
      arc.append(s('title', { text: `${seg.label}: ${formatBytes(seg.value)}` }));
      svg.append(arc);
      offset += len;
    }
  }
  return h('div', { class: 'donut-wrap-inner' }, svg,
    h('div', { class: 'donut-center' },
      h('div', { class: 'donut-value' }, centerValue),
      h('div', { class: 'donut-label' }, centerLabel)));
}

/**
 * Horizontal stacked bar (e.g. usage by category within a quota).
 * @param {Array<{label: string, value: number, color: string}>} segments
 * @param {number} total denominator (quota or volume size); falls back to the segment sum
 */
export function stackedBar(segments, total) {
  const sum = segments.reduce((a, x) => a + x.value, 0);
  const denom = total > 0 ? Math.max(total, sum) : sum;
  const bar = h('div', { class: 'stacked-bar', role: 'img', aria: { label: segments.map((x) => `${x.label} ${formatBytes(x.value)}`).join(', ') || 'Empty' } });
  if (denom > 0) {
    for (const seg of segments) {
      const part = h('span', { class: 'stacked-seg', dataset: { tip: `${seg.label}: ${formatBytes(seg.value)}` } });
      part.style.width = `${(seg.value / denom) * 100}%`;
      part.style.background = seg.color;
      bar.append(part);
    }
  }
  return bar;
}

/**
 * Legend list with swatches and formatted values.
 * @param {Array<{label: string, value: number, color: string}>} segments
 * @param {(v: number) => string} [fmt]
 */
export function legend(segments, fmt = formatBytes) {
  return h('ul', { class: 'legend' }, segments.map((seg) => {
    const sw = h('span', { class: 'legend-swatch', aria: { hidden: 'true' } });
    sw.style.background = seg.color;
    return h('li', { class: 'legend-item' }, sw, h('span', { class: 'legend-label' }, seg.label), h('span', { class: 'legend-value' }, fmt(seg.value)));
  }));
}

/**
 * Usage meter variant from a fraction: <85% accent, <95% warning, else danger.
 * @param {number} fraction
 */
export function usageVariant(fraction) {
  if (fraction >= 0.95) return 'danger';
  if (fraction >= 0.85) return 'warning';
  return 'accent';
}
