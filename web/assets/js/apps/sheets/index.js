// Sheets app entry: /sheets lists workbooks, /sheets/<id>[?sheet=<sheet_id>] opens the editor.

import { h } from '../../core/dom.js';
import { ensureStyle } from '../../core/style.js';
import { mountList } from './list.js';
import { Editor } from './editor.js';

const ID = /^[A-Za-z0-9_-]{8,64}$/;

export async function mount(container, ctx) {
  await ensureStyle('/assets/css/apps/sheets.css');
  let current = null; // {kind: 'list'|'editor', id?, view}
  let token = 0;

  async function show(c) {
    const my = ++token;
    const rest = (c.params.rest || '').replace(/\/+$/, '');
    if (!rest) {
      if (current?.kind === 'list') { current.view.update(c); return; }
      teardown();
      const root = h('div', { class: 'sh-root' });
      container.replaceChildren(root);
      current = { kind: 'list', view: mountList(root, c) };
      return;
    }
    if (!ID.test(rest)) {
      teardown();
      container.replaceChildren(h('div', { class: 'page' }, c.ui.emptyState({
        icon: 'sheet', title: 'Workbook not found', message: 'This link does not point to a workbook.',
        action: c.ui.button({ label: 'All workbooks', icon: 'arrow-left', href: '/sheets' }),
      })));
      return;
    }
    if (current?.kind === 'editor' && current.id === rest) {
      const sid = c.query.sheet;
      if (sid && current.view.sheet?.id !== sid && current.view.wb.sheet(sid)) current.view.setSheet(sid);
      return;
    }
    teardown();
    c.setLayout({ full: true });
    c.setSearch(null);
    const root = h('div', { class: 'sh-root sh-root-editor' });
    container.replaceChildren(root);
    root.replaceChildren(h('div', { class: 'sh-loading' }, c.ui.loadingBlock('Opening workbook…')));
    let json;
    try {
      json = await c.api.get(`/sheets/${rest}`, { signal: c.signal });
    } catch (err) {
      if (my !== token || err?.name === 'AbortError') return;
      root.replaceChildren(h('div', { class: 'page' }, err?.status === 404
        ? c.ui.emptyState({ icon: 'sheet', title: 'Workbook not found', message: 'It may have been deleted.', action: c.ui.button({ label: 'All workbooks', icon: 'arrow-left', href: '/sheets' }) })
        : c.ui.errorState(err, () => show(c))));
      return;
    }
    if (my !== token) return;
    c.setTitle(json.name);
    const editor = new Editor(c, root, json);
    current = { kind: 'editor', id: rest, view: editor };
    c.router.setLeaveGuard(async () => {
      await editor.saver.flush();
      for (let i = 0; i < 50 && !editor.saver.idle; i++) await new Promise((r) => setTimeout(r, 100));
      if (editor.saver.idle) return true;
      return c.ui.confirm({ title: 'Leave with unsaved changes?', message: 'Some edits have not reached the server yet.', confirmLabel: 'Leave', danger: true });
    });
  }

  function teardown() {
    if (current?.kind === 'editor') {
      current.view.destroy();
      ctx.router.setLeaveGuard(null);
      ctx.setLayout({});
    }
    current?.view?.unmount?.();
    current = null;
  }

  const onUnload = (e) => {
    if (current?.kind === 'editor' && !current.view.saver.idle) {
      current.view.saver.flush();
      e.preventDefault();
    }
  };
  window.addEventListener('beforeunload', onUnload);

  await show(ctx);
  return {
    update(next) {
      ctx = next;
      return show(next);
    },
    unmount() {
      window.removeEventListener('beforeunload', onUnload);
      teardown();
    },
  };
}
