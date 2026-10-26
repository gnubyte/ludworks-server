// Files app — preview overlay: images (prev/next, zoom), video/audio players, PDF in a
// frame, text and code (lib/highlight.js), Markdown (lib/markdown.js).

import { h, cls } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatBytes, formatDateTime } from '../../core/format.js';
import { ensureStyle } from '../../core/style.js';
import { previewKind, fileKind, KIND_ICONS, KIND_LABELS, languageFor, isMedia } from './model.js';

/**
 * Open the preview for entries[index]; ←/→ step through the other previewable entries.
 * @param {object} ctx app context
 * @param {object} fapi files api
 * @param {{entries: object[], index: number, onDetails?: (e: object) => void}} o
 */
export function openPreview(ctx, fapi, { entries, index, onDetails }) {
  const { ui } = ctx;
  let i = Math.max(0, Math.min(index, entries.length - 1));
  let cleanup = null;
  let token = 0;
  const stage = h('div', { class: 'fx-pv-stage' });
  const counter = h('span', { class: 'fx-pv-count nums' });
  const nameEl = h('span', { class: 'fx-pv-name truncate' });
  const metaEl = h('span', { class: 'fx-pv-meta' });
  const actions = h('div', { class: 'fx-pv-actions' });
  const prevBtn = ui.button({ icon: 'chevron-left', title: 'Previous (←)', variant: 'ghost', className: 'fx-pv-nav fx-pv-prev', onClick: () => go(-1) });
  const nextBtn = ui.button({ icon: 'chevron-right', title: 'Next (→)', variant: 'ghost', className: 'fx-pv-nav fx-pv-next', onClick: () => go(1) });
  const closeBtn = ui.button({ icon: 'x', title: 'Close (Esc)', variant: 'ghost', onClick: () => m.close() });
  const header = h('div', { class: 'fx-pv-header' },
    h('div', { class: 'fx-pv-title' }, nameEl, metaEl), counter, actions, closeBtn);
  const body = h('div', { class: 'fx-pv' }, header, h('div', { class: 'fx-pv-main' }, prevBtn, stage, nextBtn));
  const m = ui.modal({ body, size: 'xl', className: 'fx-preview', dismissible: true, onClose: () => cleanup?.() });
  m.el.setAttribute('aria-label', 'File preview');

  m.el.addEventListener('keydown', (e) => {
    const t = e.target;
    if (t instanceof HTMLMediaElement || t?.closest?.('.fx-pv-text')) return; // let players/text handle keys
    if (e.key === 'ArrowLeft') { e.preventDefault(); go(-1); }
    if (e.key === 'ArrowRight') { e.preventDefault(); go(1); }
  });

  function go(delta) {
    if (entries.length < 2) return;
    i = (i + delta + entries.length) % entries.length;
    render();
  }

  function actionButtons(e) {
    const out = [ui.button({ icon: 'download', title: 'Download', variant: 'ghost', onClick: () => fapi.download(e.id) })];
    if (['pdf', 'image', 'video', 'audio', 'text'].includes(previewKind(e))) {
      out.push(ui.button({ icon: 'external', title: 'Open in new tab', variant: 'ghost', href: fapi.contentUrl(e.id), target: '_blank' }));
    }
    if (isMedia(e)) out.push(ui.button({ icon: 'tv', title: 'Open in TV library', variant: 'ghost', href: `/tv/library/item/${encodeURIComponent(e.id)}` }));
    if (onDetails) out.push(ui.button({ icon: 'info', title: 'Details', variant: 'ghost', onClick: () => { m.close(); onDetails(e); } }));
    return out;
  }

  async function render() {
    const my = ++token;
    cleanup?.();
    cleanup = null;
    const e = entries[i];
    nameEl.textContent = e.name;
    metaEl.textContent = `${formatBytes(e.size)} · ${formatDateTime(e.mod_time)}`;
    counter.textContent = entries.length > 1 ? `${i + 1} / ${entries.length}` : '';
    prevBtn.hidden = nextBtn.hidden = entries.length < 2;
    actions.replaceChildren(...actionButtons(e));
    stage.replaceChildren();
    stage.className = 'fx-pv-stage';
    const kind = previewKind(e);
    try {
      if (kind === 'image') cleanup = renderImage(stage, e);
      else if (kind === 'video' || kind === 'audio') cleanup = renderMedia(stage, e, kind);
      else if (kind === 'pdf') renderPdf(stage, e);
      else if (kind === 'markdown' || kind === 'code' || kind === 'text') await renderText(stage, e, kind, () => my === token);
      else renderNone(stage, e);
    } catch (err) {
      if (my !== token || err?.name === 'AbortError') return;
      stage.replaceChildren(ui.errorState(err, () => render()));
    }
    // Warm the cache for the neighbours (images only).
    for (const d of [1, -1]) {
      const n = entries[(i + d + entries.length) % entries.length];
      if (n && n !== e && previewKind(n) === 'image') new Image().src = fapi.contentUrl(n.id);
    }
  }

  function renderImage(host, e) {
    host.classList.add('is-image');
    const img = h('img', { class: 'fx-pv-img', alt: e.name, src: fapi.contentUrl(e.id), draggable: 'false' });
    const wrap = h('div', { class: 'fx-pv-imgwrap' }, img);
    let zoom = 1;
    const zoomLabel = h('span', { class: 'nums' }, 'Fit');
    const apply = () => {
      wrap.classList.toggle('is-zoomed', zoom !== 1);
      img.style.setProperty('--zoom', String(zoom));
      zoomLabel.textContent = zoom === 1 ? 'Fit' : `${Math.round(zoom * 100)}%`;
    };
    const setZoom = (z) => { zoom = Math.max(1, Math.min(8, z)); apply(); };
    const tools = h('div', { class: 'fx-pv-zoom' },
      ui.button({ icon: 'minus', title: 'Zoom out (-)', size: 'sm', variant: 'ghost', onClick: () => setZoom(zoom / 1.5) }),
      zoomLabel,
      ui.button({ icon: 'plus', title: 'Zoom in (+)', size: 'sm', variant: 'ghost', onClick: () => setZoom(zoom * 1.5) }));
    img.addEventListener('click', () => setZoom(zoom === 1 ? 2 : 1));
    wrap.addEventListener('wheel', (ev) => {
      if (!ev.ctrlKey && !ev.metaKey) return;
      ev.preventDefault();
      setZoom(zoom * (ev.deltaY < 0 ? 1.15 : 1 / 1.15));
    }, { passive: false });
    const onKey = (ev) => {
      if (ev.key === '+' || ev.key === '=') setZoom(zoom * 1.5);
      else if (ev.key === '-') setZoom(zoom / 1.5);
      else if (ev.key === '0') setZoom(1);
    };
    m.el.addEventListener('keydown', onKey);
    img.addEventListener('error', () => host.replaceChildren(ui.emptyState({ icon: 'image', title: 'This image can’t be displayed', message: 'The format may not be supported by your browser.' })));
    host.append(wrap, tools);
    return () => m.el.removeEventListener('keydown', onKey);
  }

  function renderMedia(host, e, kind) {
    host.classList.add(kind === 'video' ? 'is-video' : 'is-audio');
    const el = h(kind, { class: `fx-pv-${kind}`, controls: true, preload: 'metadata', src: fapi.contentUrl(e.id) });
    el.autoplay = true;
    el.addEventListener('error', () => {
      host.replaceChildren(ui.emptyState({
        icon: KIND_ICONS[fileKind(e)], title: 'Your browser can’t play this file',
        message: isMedia(e) ? 'Open it in the TV library for a transcoded stream, or download it.' : 'Download it to play it with another app.',
        action: isMedia(e) ? ui.button({ label: 'Open in TV library', icon: 'tv', href: `/tv/library/item/${encodeURIComponent(e.id)}` }) : ui.button({ label: 'Download', icon: 'download', onClick: () => fapi.download(e.id) }),
      }));
    });
    if (kind === 'audio') host.append(h('div', { class: 'fx-pv-audioart' }, icon('music', 48)));
    host.append(el);
    return () => {
      el.pause();
      el.removeAttribute('src');
      el.load();
    };
  }

  function renderPdf(host, e) {
    host.classList.add('is-pdf');
    const frame = h('iframe', { class: 'fx-pv-pdf', title: e.name, src: fapi.contentUrl(e.id), referrerpolicy: 'no-referrer' });
    host.append(frame,
      h('div', { class: 'fx-pv-pdf-fallback' },
        h('span', { class: 'small muted' }, 'PDF not showing?'),
        ui.button({ label: 'Open in new tab', icon: 'external', size: 'sm', variant: 'ghost', href: fapi.contentUrl(e.id), target: '_blank' })));
  }

  async function renderText(host, e, kind, current) {
    host.classList.add('is-text');
    host.append(ui.loadingBlock('Loading…'));
    const res = await ctx.api.request('GET', `/files/${encodeURIComponent(e.id)}/content`, { raw: true, signal: ctx.signal });
    const text = await res.text();
    if (!current()) return;
    host.replaceChildren();
    if (kind === 'markdown') {
      await Promise.all([ensureStyle('/assets/css/lib/markdown.css'), ensureStyle('/assets/css/lib/highlight.css')]);
      const md = await import('../../lib/markdown.js');
      if (!current()) return;
      const rendered = h('div', { class: 'fx-pv-text fx-pv-md md', tabindex: '0' });
      md.renderInto(rendered, text, { highlight: true });
      const source = h('pre', { class: 'fx-pv-text fx-pv-code', tabindex: '0', hidden: true }, h('code', {}, text));
      const toggle = ui.segmented({
        options: [{ value: 'rendered', label: 'Preview' }, { value: 'source', label: 'Source' }], value: 'rendered', size: 'sm', ariaLabel: 'Markdown view',
        onChange: (v) => { rendered.hidden = v !== 'rendered'; source.hidden = v === 'rendered'; },
      });
      host.append(h('div', { class: 'fx-pv-texttools' }, toggle.el), rendered, source);
      return;
    }
    const pre = h('pre', { class: cls('fx-pv-text', 'fx-pv-code'), tabindex: '0' });
    const lang = kind === 'code' ? languageFor(e.name) : '';
    if (lang) {
      await ensureStyle('/assets/css/lib/highlight.css');
      const hl = await import('../../lib/highlight.js');
      if (!current()) return;
      const code = h('code', { class: 'hl' });
      code.append(hl.highlight(text, lang));
      pre.append(code);
    } else {
      pre.append(h('code', {}, text));
    }
    host.append(pre);
  }

  function renderNone(host, e) {
    const k = fileKind(e);
    host.classList.add('is-none');
    host.append(h('div', { class: 'fx-pv-none' },
      h('div', { class: `fx-pv-bigicon fx-kind-${k}` }, icon(KIND_ICONS[k], 40)),
      h('div', { class: 'fx-pv-none-title' }, e.name),
      h('div', { class: 'muted small' }, `${KIND_LABELS[k]} · ${formatBytes(e.size)}`),
      h('p', { class: 'muted' }, 'There is no preview for this kind of file.'),
      h('div', { class: 'row' },
        ui.button({ label: 'Download', icon: 'download', variant: 'primary', onClick: () => fapi.download(e.id) }),
        isMedia(e) ? ui.button({ label: 'Open in TV library', icon: 'tv', href: `/tv/library/item/${encodeURIComponent(e.id)}` }) : null)));
  }

  render();
  return m;
}
