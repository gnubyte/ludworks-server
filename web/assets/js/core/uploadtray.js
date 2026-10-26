// Global upload tray: shows the shared UploadQueue (progress, speed, ETA) with
// pause/resume/cancel, independent of which app is open.

import { h, cls } from './dom.js';
import { icon } from './icons.js';
import { formatBytes, formatSpeed, formatDurationLong } from './format.js';
import { button, progress, confirm } from './ui.js';

const STATE_LABEL = {
  queued: 'Waiting', uploading: 'Uploading', retrying: 'Reconnecting…', paused: 'Paused',
  completed: 'Done', error: 'Failed', cancelled: 'Cancelled',
};

/**
 * Mount the tray for a queue. Returns {el, destroy}.
 * @param {import('./upload.js').UploadQueue} queue
 */
export function createUploadTray(queue) {
  let collapsed = false;
  const rows = new Map(); // upload → {el, bar, meta, status, actions}
  const title = h('span', { class: 'upload-tray-title' });
  const summary = h('span', { class: 'upload-tray-summary' });
  const overall = progress({ size: 'sm', label: 'Overall upload progress' });
  const toggleBtn = button({ icon: 'chevron-down', title: 'Collapse', variant: 'ghost', size: 'sm' });
  const clearBtn = button({ icon: 'x', title: 'Close', variant: 'ghost', size: 'sm' });
  const list = h('div', { class: 'upload-list', role: 'list' });
  const el = h('section', { class: 'upload-tray', aria: { label: 'Uploads' }, hidden: true },
    h('header', { class: 'upload-tray-header' },
      icon('upload', { className: 'upload-tray-icon' }),
      h('div', { class: 'upload-tray-heading' }, title, summary),
      toggleBtn, clearBtn),
    overall.el,
    list);
  document.body.appendChild(el);

  toggleBtn.addEventListener('click', () => {
    collapsed = !collapsed;
    el.classList.toggle('is-collapsed', collapsed);
    toggleBtn.replaceChildren(icon(collapsed ? 'chevron-up' : 'chevron-down', 14));
    toggleBtn.dataset.tip = collapsed ? 'Expand' : 'Collapse';
    toggleBtn.setAttribute('aria-label', collapsed ? 'Expand' : 'Collapse');
    syncOffset();
  });
  clearBtn.addEventListener('click', async () => {
    if (queue.busy) {
      const ok = await confirm({
        title: 'Cancel active uploads?',
        message: 'Uploads still in progress will be cancelled. Paused uploads can also be resumed later by adding the same files again.',
        confirmLabel: 'Cancel uploads', danger: true,
      });
      if (!ok) return;
      for (const u of [...queue.uploads]) if (!['completed', 'cancelled', 'error'].includes(u.state)) await queue.cancel(u);
    }
    for (const u of [...queue.uploads]) queue.remove(u);
    queue.clearFinished();
  });

  function rowFor(u) {
    let r = rows.get(u);
    if (r) return r;
    const bar = progress({ size: 'sm', label: `Upload progress for ${u.name}` });
    const status = h('span', { class: 'upload-status' });
    const meta = h('span', { class: 'upload-meta-right' });
    const actions = h('div', { class: 'upload-actions' });
    const rowEl = h('div', { class: 'upload-item', role: 'listitem' },
      h('div', { class: 'upload-item-top' },
        icon('file', { className: 'upload-file-icon' }),
        h('span', { class: 'upload-name', title: u.name }, u.name),
        actions),
      bar.el,
      h('div', { class: 'upload-meta' }, status, meta));
    r = { el: rowEl, bar, status, meta, actions, lastState: null };
    rows.set(u, r);
    list.prepend(rowEl);
    return r;
  }

  function renderActions(u, r) {
    const a = [];
    if (['uploading', 'retrying', 'queued'].includes(u.state)) a.push(button({ icon: 'pause', title: 'Pause', variant: 'ghost', size: 'sm', onClick: () => queue.pause(u) }));
    if (u.state === 'paused' || u.state === 'error') a.push(button({ icon: u.state === 'error' ? 'refresh' : 'play', title: u.state === 'error' ? 'Retry' : 'Resume', variant: 'ghost', size: 'sm', onClick: () => queue.resume(u) }));
    if (!['completed', 'cancelled'].includes(u.state)) a.push(button({ icon: 'x', title: 'Cancel upload', variant: 'ghost', size: 'sm', onClick: () => queue.cancel(u) }));
    else a.push(button({ icon: 'x', title: 'Remove from list', variant: 'ghost', size: 'sm', onClick: () => queue.remove(u) }));
    r.actions.replaceChildren(...a);
  }

  function renderRow(u) {
    const r = rowFor(u);
    const p = u.progress;
    r.bar.set(p.bytesUploaded, p.bytesTotal || 1);
    r.bar.setVariant(u.state === 'error' ? 'danger' : u.state === 'completed' ? 'success' : u.state === 'paused' ? 'neutral' : 'accent');
    r.el.className = cls('upload-item', `is-${u.state}`);
    let statusText = STATE_LABEL[u.state] || u.state;
    if (u.state === 'error' && u.error) statusText = u.error.message;
    r.status.textContent = statusText;
    r.status.title = statusText;
    if (u.state === 'uploading' && p.speed > 0) {
      r.meta.textContent = `${formatBytes(p.bytesUploaded)} of ${formatBytes(p.bytesTotal)} · ${formatSpeed(p.speed)}${p.eta != null ? ` · ${formatDurationLong(p.eta)} left` : ''}`;
    } else {
      r.meta.textContent = u.state === 'completed' ? formatBytes(p.bytesTotal) : `${formatBytes(p.bytesUploaded)} of ${formatBytes(p.bytesTotal)}`;
    }
    if (r.lastState !== u.state) {
      r.lastState = u.state;
      renderActions(u, r);
    }
  }

  function renderSummary() {
    const t = queue.totals;
    const active = t.counts.uploading + t.counts.retrying + t.counts.queued;
    title.textContent = active ? `Uploading ${active} ${active === 1 ? 'file' : 'files'}` : t.counts.error ? 'Some uploads failed' : t.counts.paused ? 'Uploads paused' : 'Uploads complete';
    summary.textContent = t.bytesTotal ? `${formatBytes(t.bytesUploaded)} of ${formatBytes(t.bytesTotal)}${t.speed > 0 ? ` · ${formatSpeed(t.speed)}` : ''}` : '';
    overall.set(t.bytesUploaded, t.bytesTotal || 1);
    clearBtn.dataset.tip = queue.busy ? 'Cancel all' : 'Close';
    clearBtn.setAttribute('aria-label', queue.busy ? 'Cancel all uploads' : 'Close uploads');
  }

  let lastOffset = -1;
  function syncOffset() {
    // Lift toasts above the tray so they don't overlap.
    const hgt = el.hidden ? 0 : el.offsetHeight + 12;
    if (hgt === lastOffset) return;
    lastOffset = hgt;
    document.documentElement.style.setProperty('--tray-offset', `${hgt}px`);
  }
  // The tray's height also changes outside renderAll: its slide-in animation, rows laid out in a
  // later frame, collapse/expand, the list reaching its max height. Measuring only on render
  // left toasts drawn over the tray header, so follow its real size.
  const resizeObs = typeof ResizeObserver === 'function' ? new ResizeObserver(() => syncOffset()) : null;
  resizeObs?.observe(el);

  function renderAll() {
    for (const [u, r] of rows) {
      if (!queue.uploads.includes(u)) {
        r.el.remove();
        rows.delete(u);
      }
    }
    for (const u of queue.uploads) renderRow(u);
    el.hidden = queue.uploads.length === 0;
    renderSummary();
    syncOffset();
  }

  let raf = 0;
  const schedule = () => {
    if (raf) return;
    raf = requestAnimationFrame(() => {
      raf = 0;
      renderAll();
    });
  };
  const offs = [queue.on('change', schedule), queue.on('progress', schedule)];
  const beforeUnload = (e) => {
    if (queue.busy) {
      e.preventDefault();
      e.returnValue = '';
    }
  };
  window.addEventListener('beforeunload', beforeUnload);
  renderAll();
  return {
    el,
    destroy() {
      offs.forEach((off) => off());
      resizeObs?.disconnect();
      window.removeEventListener('beforeunload', beforeUnload);
      el.remove();
      document.documentElement.style.setProperty('--tray-offset', '0px');
    },
  };
}
