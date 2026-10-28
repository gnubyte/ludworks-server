// Card detail, shown over the board (modal on desktop, full screen on phones) at
// /kanban/<board>/c/<card>: title, Markdown description, labels, assignees, priority, dates,
// completion, subtasks (drag or Alt+arrows to reorder, promote to card), attachments (drop or
// pick, from Files, previews and lightbox, covers, save to Files), comments and activity.
//
// Quick field changes (labels, dates, priority…) are sent without base_version (they touch one
// field each); title and description edits carry it, and a 409 opens a merge dialog. Polled
// board changes refresh the detail without clobbering an edit in progress.

import { h, cls, pickFiles, debounce, mount as fill } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatBytes, formatDateTime } from '../../core/format.js';
import { renderToDOM, toggleTask } from '../../lib/markdown.js';
import { createMarkdownEditor } from '../../lib/mdeditor.js';
import { kicon } from './icons.js';
import { MAX_ATTACHMENT } from './api.js';
import {
  paths, PRIORITIES, priorityLabel, pickLabelColor, localDate, localDateTime, datesPatch, dueLabel, dueState,
  memberName, activityText, reorder,
} from './model.js';
import { createDnd } from './dnd.js';
import {
  labelChip, memberAvatar, colorSwatches, popover, closePopover, filesPicker, saveToFilesDialog, iconButton,
} from './widgets.js';

/**
 * Open the detail of a card.
 * @param {object} app {ctx, kapi, me}
 * @param {object} host callbacks and getters of the board view
 * @returns {{cardId: string, close: (o?: {silent?: boolean}) => void, cardPatched: (c: object) => void, boardChanged: () => void}}
 */
export function openCardDetail(app, host) {
  const { ctx, kapi } = app;
  const { ui } = ctx;
  const cardId = host.cardId;
  const drafts = app.drafts || new Map();
  const D = {
    card: null,
    closed: false,
    silent: false,
    refreshing: false,
    descEditor: null,
    descBase: 0,
    descConflictNotice: false,
    commentsAll: false,
    uploads: new Map(),
  };
  const offs = [];
  const editor = () => host.editor();
  const admin = () => host.admin();
  const members = () => host.members();
  const labelsMap = () => host.labels();
  const memberLabel = (id) => memberName(members().get(id));

  /* ----------------------------------------------------------------- shell */
  const coverEl = h('div', { class: 'kb-d-cover', hidden: true });
  const headEl = h('div', { class: 'kb-d-head' });
  const propsEl = h('div', { class: 'kb-d-props' });
  const descEl = h('section', { class: 'kb-d-section kb-d-desc', aria: { label: 'Description' } });
  const subsEl = h('section', { class: 'kb-d-section', aria: { label: 'Subtasks' } });
  const attEl = h('section', { class: 'kb-d-section kb-d-atts', aria: { label: 'Attachments' } });
  const commentsList = h('div', { class: 'kb-comments' });
  const commentsHead = h('div', { class: 'kb-d-section-head' });
  const composer = buildComposer();
  const commentsEl = h('section', { class: 'kb-d-section', aria: { label: 'Comments' } }, commentsHead, commentsList, composer.el);
  const actEl = h('section', { class: 'kb-d-section kb-d-activity', aria: { label: 'Activity' } });
  const sideEl = h('aside', { class: 'kb-d-side', aria: { label: 'Card actions' } });
  const dropEl = h('div', { class: 'kb-d-drop', hidden: true }, kicon('paperclip', 28), h('div', {}, 'Drop files to attach'));
  const main = h('div', { class: 'kb-d-main' }, propsEl, descEl, subsEl, attEl, commentsEl, actEl);
  const body = h('div', { class: 'kb-detail' }, coverEl, headEl, h('div', { class: 'kb-d-grid' }, main, sideEl), dropEl);
  const loading = h('div', { class: 'kb-d-loading' }, ui.skeletonText(2), ui.skeleton({ height: '120px' }), ui.skeletonText(4));

  const m = ui.modal({
    title: '', size: 'xl', className: 'kb-card-modal', body: h('div', {}, loading),
    onClose: () => {
      D.closed = true;
      closePopover();
      for (const off of offs.splice(0)) off?.();
      for (const u of D.uploads.values()) u.ctrl.abort();
      if (D.descEditor) {
        // Keep unsaved text for when the card is opened again (this session only).
        const md = D.descEditor.getMarkdown();
        if (D.card && md !== D.card.description) drafts.set(`d:${cardId}`, { md, base: D.descBase });
        try { D.descEditor.destroy(); } catch { /* already gone */ }
      }
      host.onClosed?.(!D.silent);
    },
  });
  m.backdrop.classList.add('kb-card-backdrop');
  const modalBody = m.body;

  /* ------------------------------------------------------------------ load */
  async function load() {
    try {
      const c = await kapi.card(cardId, ctx.signal);
      if (D.closed) return;
      D.card = c;
      modalBody.replaceChildren(body);
      renderAll();
      const draft = drafts.get(`d:${cardId}`);
      if (draft && editor()) {
        editDescription(draft.md, draft.base);
        ui.toast.info('Restored your unsaved description', { action: { label: 'Discard', onClick: () => { drafts.delete(`d:${cardId}`); closeEditor(); renderDescription(); } } });
      }
    } catch (err) {
      if (D.closed || err?.name === 'AbortError') return;
      if (err?.status === 404) {
        ui.toast.warning('That card does not exist any more.');
        m.close();
        return;
      }
      modalBody.replaceChildren(ui.errorState(err, () => { modalBody.replaceChildren(loading); load(); }));
    }
  }

  async function refresh() {
    if (D.refreshing || D.closed) return;
    D.refreshing = true;
    try {
      const c = await kapi.card(cardId, ctx.signal);
      if (D.closed) return;
      if (D.descEditor && c.description !== D.card.description) D.descConflictNotice = true;
      D.card = c;
      renderAll();
    } catch (err) {
      if (err?.status === 404 && !D.closed) {
        ui.toast.warning('This card was deleted.');
        m.close();
      }
    } finally {
      D.refreshing = false;
    }
  }
  const refreshSoon = debounce(refresh, 250);

  /** Merge a Card (from a write) into the detail and tell the board. */
  function applyCard(c, { notify = true } = {}) {
    D.card = { ...D.card, ...c };
    renderAll();
    if (notify) host.cardChanged(c);
  }

  /** Tell the board about changed counters (subtasks, attachments, comments). Only the
   * counters travel: the detail's copy of the position may be older than the board's. */
  function countersChanged() {
    const c = D.card;
    c.subtasks_total = c.subtasks.length;
    c.subtasks_done = c.subtasks.filter((s) => s.done).length;
    c.attachment_count = c.attachments.length;
    host.cardChanged({
      id: c.id, subtasks_total: c.subtasks_total, subtasks_done: c.subtasks_done, comment_count: c.comment_count,
      attachment_count: c.attachment_count, cover_attachment_id: c.cover_attachment_id,
    });
  }

  async function patch(fields, { versioned = false } = {}) {
    const body2 = versioned ? { ...fields, base_version: D.card.version } : fields;
    const c = await kapi.updateCard(cardId, body2);
    applyCard(c);
    return c;
  }

  async function quick(fields) {
    try {
      await patch(fields);
    } catch (err) {
      ui.toast.error(err);
    }
  }

  // True while the user types in a field inside el (refreshes must not throw that away).
  function editingIn(el) {
    const a = document.activeElement;
    return !!a && el.contains(a) && ((a.tagName === 'INPUT' && a.type !== 'checkbox') || a.tagName === 'TEXTAREA');
  }

  function renderAll() {
    if (!D.card) return;
    renderCover();
    renderHead();
    renderProps();
    renderDescription();
    if (!editingIn(subsEl)) renderSubtasks();
    renderAttachments();
    if (!editingIn(commentsList)) renderComments();
    renderActivity();
    renderSide();
  }

  /* ----------------------------------------------------------------- cover */
  function renderCover() {
    const id = D.card.cover_attachment_id;
    coverEl.hidden = !id;
    if (!id) { coverEl.replaceChildren(); return; }
    if (coverEl.dataset.id === id) return;
    coverEl.dataset.id = id;
    const img = h('img', { src: kapi.attachmentUrl(id), alt: '' });
    img.addEventListener('error', () => { coverEl.hidden = true; });
    coverEl.replaceChildren(img);
  }

  /* ------------------------------------------------------------------ head */
  let titleInput = null;
  function renderHead() {
    const c = D.card;
    const board = host.board();
    const col = board.columns.find((x) => x.id === c.column_id);
    if (titleInput && document.activeElement === titleInput) return; // editing
    const done = h('input', {
      type: 'checkbox', class: 'checkbox kb-d-done', checked: c.completed, disabled: !editor(),
      aria: { label: c.completed ? 'Completed — mark as not done' : 'Mark as done' }, dataset: { tip: c.completed ? 'Completed' : 'Mark as done' },
    });
    done.addEventListener('change', () => quick({ completed: done.checked }));
    let title;
    if (editor()) {
      titleInput = h('textarea', { class: 'kb-d-title', rows: 1, maxlength: 500, spellcheck: 'true', aria: { label: 'Card title' } });
      titleInput.value = c.title;
      const autosize = () => { titleInput.style.height = 'auto'; titleInput.style.height = `${titleInput.scrollHeight}px`; };
      titleInput.addEventListener('input', autosize);
      titleInput.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') { e.preventDefault(); titleInput.blur(); }
        if (e.key === 'Escape') { e.preventDefault(); titleInput.value = D.card.title; titleInput.blur(); }
      });
      titleInput.addEventListener('blur', () => saveTitle());
      requestAnimationFrame(autosize);
      title = titleInput;
    } else {
      titleInput = null;
      title = h('h2', { class: 'kb-d-title is-static' }, c.title);
    }
    const colBtn = h('button', { type: 'button', class: 'kb-d-colbtn', disabled: !editor() }, col ? col.name : 'Unknown column', editor() ? icon('chevron-down', 12) : null);
    colBtn.addEventListener('click', () => moveMenu(colBtn));
    headEl.replaceChildren(
      h('div', { class: 'kb-d-titlerow' }, done, title),
      h('div', { class: 'kb-d-meta' },
        h('span', { class: 'kb-d-num' }, `#${c.number}`), h('span', { class: 'faint' }, 'in'), colBtn,
        c.archived ? h('span', { class: 'badge badge-warning' }, icon('archive', 12), 'Archived') : null,
        c.completed && c.completed_at ? h('span', { class: 'badge badge-success', dataset: { tip: formatDateTime(c.completed_at) } }, icon('check', 12), 'Done') : null));
  }

  async function saveTitle() {
    if (!titleInput) return;
    const v = titleInput.value.replace(/\s+/g, ' ').trim();
    if (!v) { titleInput.value = D.card.title; return; }
    if (v === D.card.title) return;
    try {
      await patch({ title: v }, { versioned: true });
    } catch (err) {
      if (err?.status === 409 && err.details?.current) {
        const cur = err.details.current;
        const ok = await ui.confirm({
          title: 'Someone else changed this card',
          message: `The card is now titled “${cur.title}”. Replace it with “${v}”?`,
          confirmLabel: 'Use my title', cancelLabel: 'Keep theirs',
        });
        D.card = { ...D.card, ...cur };
        if (ok) {
          try { await patch({ title: v }, { versioned: true }); } catch (e2) { ui.toast.error(e2); }
        } else {
          applyCard(cur);
        }
        return;
      }
      ui.toast.error(err);
      titleInput.value = D.card.title;
    }
  }

  function moveMenu(anchor) {
    const board = host.board();
    const cols = [...board.columns].sort((a, b) => a.position - b.position);
    ui.openMenu({
      anchor, items: [{ heading: 'Move to column' }, ...cols.map((c) => ({
        label: c.name, checked: c.id === D.card.column_id,
        onSelect: async () => {
          if (c.id === D.card.column_id) return;
          await host.moveCard(cardId, c.id, null);
          const moved = host.board().cards.find((x) => x.id === cardId);
          if (moved) { D.card = { ...D.card, column_id: moved.column_id, position: moved.position }; renderHead(); }
        },
      }))],
    });
  }

  /* ----------------------------------------------------------------- props */
  function renderProps() {
    const c = D.card;
    const ed = editor();
    const groups = [];
    // Labels
    const labelEls = c.labels.map((id) => labelsMap().get(id)).filter(Boolean).map((l) => labelChip(l));
    const addLabel = ed ? iconButton('plus', 'Edit labels', (e) => labelPicker(e.currentTarget)) : null;
    groups.push(prop('Labels', labelEls.length || ed ? h('div', { class: 'kb-d-chips' }, labelEls, addLabel) : null, !labelEls.length && !ed));
    // Assignees
    const people = c.assignees.map((id) => memberAvatar(members().get(id) || { username: 'Unknown user' }, 28));
    const addPerson = ed ? iconButton('user-plus', 'Assign people', (e) => assigneePicker(e.currentTarget)) : null;
    groups.push(prop('Assignees', people.length || ed ? h('div', { class: 'kb-d-chips' }, people, addPerson) : null, !people.length && !ed));
    // Priority
    const prioBtn = h('button', { type: 'button', class: cls('kb-d-chipbtn', `kb-prio`, `is-${c.priority}`), disabled: !ed },
      kicon('flag', 14), priorityLabel(c.priority));
    prioBtn.addEventListener('click', () => ui.openMenu({
      anchor: prioBtn, items: PRIORITIES.map((p) => ({ label: p.label, checked: c.priority === p.value, onSelect: () => quick({ priority: p.value }) })),
    }));
    groups.push(prop('Priority', prioBtn));
    // Dates
    const dateText = c.start_at || c.due_at
      ? [c.start_at ? dueLabel({ due_at: c.start_at, all_day: c.all_day }) : null, c.due_at ? dueLabel(c) : null].filter(Boolean).join(' → ')
      : 'Add dates';
    const state = dueState(c);
    const dateBtn = h('button', { type: 'button', class: cls('kb-d-chipbtn', 'kb-due', state && `is-${state}`), disabled: !ed && !c.due_at },
      icon('calendar', 14), dateText);
    dateBtn.addEventListener('click', () => { if (ed) datesEditor(dateBtn); });
    groups.push(prop(c.start_at ? 'Dates' : 'Due', dateBtn, !c.start_at && !c.due_at && !ed));
    propsEl.replaceChildren(...groups.filter(Boolean));
  }

  function prop(name, content, hidden = false) {
    if (hidden || !content) return null;
    return h('div', { class: 'kb-d-prop' }, h('div', { class: 'kb-d-prop-name' }, name), h('div', { class: 'kb-d-prop-value' }, content));
  }

  /* ---------------------------------------------------------- label picker */
  function labelPicker(anchor) {
    const board = host.board();
    const search = h('input', { class: 'input input-sm', placeholder: 'Search or create a label…', maxlength: 200, aria: { label: 'Search labels' } });
    const list = h('div', { class: 'kb-check-list', role: 'group', aria: { label: 'Labels' } });
    const draw = () => {
      const q = search.value.trim().toLowerCase();
      const items = board.labels.filter((l) => !q || l.name.toLowerCase().includes(q));
      list.replaceChildren(...items.map((l) => {
        const cb = h('input', { type: 'checkbox', class: 'checkbox', checked: D.card.labels.includes(l.id) });
        cb.addEventListener('change', async () => {
          const set = new Set(D.card.labels);
          if (cb.checked) set.add(l.id);
          else set.delete(l.id);
          await quick({ labels: [...set] });
        });
        const edit = iconButton('edit', `Edit “${l.name}”`, () => labelEditor(anchor, l, () => labelPicker(anchor)));
        return h('div', { class: 'kb-check-row' }, h('label', { class: 'kb-check-main' }, cb, labelChip(l)), edit);
      }));
      const exact = board.labels.some((l) => l.name.toLowerCase() === q);
      if (q && !exact) {
        const create = h('button', { type: 'button', class: 'kb-create-row' }, icon('plus', 14), `Create “${search.value.trim()}”`);
        create.addEventListener('click', () => createLabel(search.value.trim()));
        list.append(create);
      }
      if (!list.childElementCount) list.append(h('div', { class: 'faint small kb-hint' }, 'No labels yet — type a name to create one.'));
    };
    const createLabel = async (name) => {
      try {
        const l = await kapi.createLabel(board.id, { name, color: pickLabelColor(board.labels) });
        board.labels = [...board.labels, l].sort((a, b) => a.name.localeCompare(b.name));
        labelsMap().set(l.id, l);
        search.value = '';
        await quick({ labels: [...new Set([...D.card.labels, l.id])] });
        await host.labelsChanged();
        draw();
      } catch (err) {
        ui.toast.error(err);
      }
    };
    search.addEventListener('input', draw);
    search.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        const q = search.value.trim();
        const match = board.labels.find((l) => l.name.toLowerCase() === q.toLowerCase());
        if (match) list.querySelector('input')?.click();
        else if (q) createLabel(q);
      }
    });
    draw();
    popover({ anchor, content: h('div', { class: 'kb-pop-body kb-pop-labels' }, h('div', { class: 'kb-pop-title' }, 'Labels'), search, list), label: 'Labels' });
  }

  function labelEditor(anchor, l, back) {
    const name = h('input', { class: 'input input-sm', value: l.name, maxlength: 200, aria: { label: 'Label name' } });
    let color = l.color;
    const sw = colorSwatches({ value: l.color, label: 'Label color', onChange: (c) => { color = c; } });
    const save = async () => {
      const n = name.value.trim();
      if (!n) return;
      try {
        await kapi.updateLabel(l.id, { name: n, color });
        await host.labelsChanged();
        renderAll();
        back();
      } catch (err) {
        ui.toast.error(err);
      }
    };
    const del = async () => {
      if (!(await ui.confirm({ title: `Delete the label “${l.name}”?`, message: 'It is removed from every card of the board.', confirmLabel: 'Delete label', danger: true }))) return;
      try {
        await kapi.deleteLabel(l.id);
        D.card.labels = D.card.labels.filter((x) => x !== l.id);
        await host.labelsChanged();
        renderAll();
        refreshSoon();
        back();
      } catch (err) {
        ui.toast.error(err);
      }
    };
    name.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); save(); } });
    popover({
      anchor, label: 'Edit label',
      content: h('div', { class: 'kb-pop-body' }, h('div', { class: 'kb-pop-title' }, 'Edit label'), name, sw.el,
        h('div', { class: 'row between' }, ui.button({ label: 'Delete', variant: 'danger-ghost', size: 'sm', onClick: del }),
          h('div', { class: 'row' }, ui.button({ label: 'Back', size: 'sm', variant: 'ghost', onClick: () => back() }), ui.button({ label: 'Save', size: 'sm', variant: 'primary', onClick: save })))),
    });
  }

  function assigneePicker(anchor) {
    const board = host.board();
    const list = h('div', { class: 'kb-check-list', role: 'group', aria: { label: 'Members' } });
    for (const mb of board.members) {
      const cb = h('input', { type: 'checkbox', class: 'checkbox', checked: D.card.assignees.includes(mb.user_id) });
      cb.addEventListener('change', () => {
        const next = D.card.assignees.filter((x) => x !== mb.user_id);
        if (cb.checked) next.push(mb.user_id);
        quick({ assignees: next });
      });
      list.append(h('label', { class: 'kb-check-row kb-check-main' }, cb, memberAvatar(mb, 24), h('span', { class: 'truncate' }, memberName(mb)),
        mb.user_id === app.me ? h('span', { class: 'faint small' }, '(you)') : null));
    }
    popover({ anchor, content: h('div', { class: 'kb-pop-body' }, h('div', { class: 'kb-pop-title' }, 'Assignees'), list,
      h('div', { class: 'faint small' }, 'Only board members can be assigned.')), label: 'Assignees' });
  }

  function datesEditor(anchor) {
    const c = D.card;
    let allDay = c.all_day || (!c.due_at && !c.start_at);
    const allDayCb = h('input', { type: 'checkbox', class: 'switch-input', checked: allDay, aria: { label: 'All day' } });
    const startIn = h('input', { class: 'input input-sm', aria: { label: 'Start' } });
    const dueIn = h('input', { class: 'input input-sm', aria: { label: 'Due' } });
    const setInputs = () => {
      startIn.type = allDay ? 'date' : 'datetime-local';
      dueIn.type = allDay ? 'date' : 'datetime-local';
      startIn.value = allDay ? localDate(c.start_at) : localDateTime(c.start_at);
      dueIn.value = allDay ? localDate(c.due_at) : localDateTime(c.due_at);
    };
    setInputs();
    allDayCb.addEventListener('change', () => {
      const s = startIn.value;
      const d = dueIn.value;
      allDay = allDayCb.checked;
      startIn.type = allDay ? 'date' : 'datetime-local';
      dueIn.type = allDay ? 'date' : 'datetime-local';
      startIn.value = allDay ? s.slice(0, 10) : s ? `${s.slice(0, 10)}T09:00` : '';
      dueIn.value = allDay ? d.slice(0, 10) : d ? `${d.slice(0, 10)}T17:00` : '';
    });
    const err = h('div', { class: 'field-error', hidden: true });
    const save = async () => {
      const p = datesPatch({ start: startIn.value, due: dueIn.value, allDay });
      if (p.start_at && p.due_at && p.due_at < p.start_at) {
        err.textContent = 'The due date is before the start.';
        err.hidden = false;
        return;
      }
      closePopover();
      await quick(p);
    };
    const clear = async () => {
      closePopover();
      await quick({ start_at: null, due_at: null, all_day: false });
    };
    const shortcuts = h('div', { class: 'kb-date-shortcuts' }, [['Today', 0], ['Tomorrow', 1], ['In a week', 7]].map(([label, days]) => {
      const b = h('button', { type: 'button', class: 'kb-chip-btn' }, label);
      b.addEventListener('click', () => {
        const d = new Date();
        d.setDate(d.getDate() + days);
        const ds = localDate(d.toISOString());
        dueIn.value = allDay ? ds : `${ds}T17:00`;
      });
      return b;
    }));
    popover({
      anchor, label: 'Dates',
      content: h('div', { class: 'kb-pop-body kb-pop-dates' }, h('div', { class: 'kb-pop-title' }, 'Dates'),
        h('label', { class: 'kb-switch-row' }, h('span', { class: 'switch' }, allDayCb, h('span', { class: 'switch-track' })), h('span', {}, 'All day')),
        h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Start'), startIn),
        h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Due'), dueIn, shortcuts), err,
        h('div', { class: 'row between' }, ui.button({ label: 'Remove dates', size: 'sm', variant: 'danger-ghost', onClick: clear }),
          ui.button({ label: 'Save', size: 'sm', variant: 'primary', onClick: save }))),
    });
  }

  /* ----------------------------------------------------------- description */
  function renderDescription() {
    if (D.descEditor) {
      const notice = descEl.querySelector('.kb-d-notice');
      if (D.descConflictNotice && !notice) {
        const alertEl = ui.alert({ variant: 'warning', title: 'Changed by someone else', message: 'Saving asks how to merge with their version.' });
        alertEl.classList.add('kb-d-notice');
        descEl.querySelector('.kb-d-section-head')?.after(alertEl);
      }
      return;
    }
    const c = D.card;
    const head = h('div', { class: 'kb-d-section-head' }, kicon('align-left', 16), h('h3', {}, 'Description'),
      editor() && c.description ? ui.button({ label: 'Edit', size: 'sm', variant: 'ghost', onClick: () => editDescription() }) : null);
    let content;
    if (c.description) {
      content = h('div', { class: 'md kb-d-md' });
      content.append(renderToDOM(c.description, document, {
        onTaskToggle: editor() ? (line, checked) => toggleDescriptionTask(line, checked) : null,
      }));
      if (editor()) content.addEventListener('dblclick', (e) => { if (!e.target.closest('a,input')) editDescription(); });
    } else if (editor()) {
      content = h('button', { type: 'button', class: 'kb-d-placeholder', onClick: () => editDescription() }, 'Add a description… (Markdown)');
    } else {
      content = h('p', { class: 'faint' }, 'No description.');
    }
    descEl.replaceChildren(head, content);
  }

  async function toggleDescriptionTask(line, checked) {
    const next = toggleTask(D.card.description, line, checked);
    if (next === D.card.description) return;
    try {
      await patch({ description: next }, { versioned: true });
    } catch (err) {
      ui.toast.error(err?.status === 409 ? 'The description changed meanwhile; it was reloaded.' : err);
      refresh();
    }
  }

  function editDescription(initial = D.card.description, base = D.card.version) {
    if (D.descEditor || !editor()) return;
    D.descBase = base; // a restored draft keeps the version it was based on: saving it may conflict
    D.descConflictNotice = false;
    const host2 = h('div', { class: 'kb-d-editor' });
    const save = async () => {
      const md = D.descEditor.getMarkdown();
      if (md === D.card.description) { closeEditor(); return; }
      try {
        const c = await kapi.updateCard(cardId, { description: md, base_version: D.descBase });
        closeEditor();
        applyCard(c);
      } catch (err) {
        if (err?.status === 409 && err.details?.current) { descriptionConflict(md, err.details.current); return; }
        ui.toast.error(err);
      }
    };
    const cancel = async () => {
      const md = D.descEditor.getMarkdown();
      if (md !== D.card.description && !(await ui.confirm({ title: 'Discard your changes?', message: 'The description keeps its saved text.', confirmLabel: 'Discard', danger: true }))) return;
      closeEditor();
      renderDescription();
    };
    const actions = h('div', { class: 'kb-d-editor-actions' },
      ui.button({ label: 'Save', variant: 'primary', size: 'sm', onClick: save }),
      ui.button({ label: 'Cancel', variant: 'ghost', size: 'sm', onClick: cancel }),
      h('span', { class: 'faint small' }, `${app.isApple ? '⌘' : 'Ctrl'}+Enter to save`));
    descEl.replaceChildren(h('div', { class: 'kb-d-section-head' }, kicon('align-left', 16), h('h3', {}, 'Description')), host2, actions);
    D.descEditor = createMarkdownEditor({
      container: host2, markdown: initial, mode: 'source', placeholder: 'Describe the card… Markdown works: **bold**, - [ ] tasks, `code`, tables.',
      ariaLabel: 'Card description', autofocus: true, debounceMs: 400,
    });
    D.descEditor.on('error', (err) => ui.toast.error(err));
    host2.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && (app.isApple ? e.metaKey : e.ctrlKey)) { e.preventDefault(); save(); }
      else if (e.key === 'Escape' && !e.defaultPrevented) { e.preventDefault(); e.stopPropagation(); cancel(); }
    });
  }

  function closeEditor() {
    drafts.delete(`d:${cardId}`);
    if (!D.descEditor) return;
    try { D.descEditor.destroy(); } catch { /* ignore */ }
    D.descEditor = null;
    D.descConflictNotice = false;
  }

  function descriptionConflict(mine, current) {
    const theirs = h('div', { class: 'md kb-d-md kb-conflict-theirs' });
    theirs.append(current.description ? renderToDOM(current.description, document) : h('p', { class: 'faint' }, 'Their version has no description.'));
    const cm = ui.modal({
      title: 'Someone else changed this card', size: 'lg',
      description: 'The description was saved by someone else while you were editing. Their version:',
      body: theirs,
      footer: [
        { label: 'Keep editing', value: 'edit' },
        { label: 'Discard mine', value: 'theirs', variant: 'danger-ghost' },
        { label: 'Save mine', value: 'mine', variant: 'primary' },
      ],
    });
    cm.closed.then(async (choice) => {
      D.card = { ...D.card, ...current };
      if (choice === 'mine') {
        try {
          const c = await kapi.updateCard(cardId, { description: mine, base_version: current.version });
          closeEditor();
          applyCard(c);
        } catch (err) {
          ui.toast.error(err);
        }
      } else if (choice === 'theirs') {
        closeEditor();
        applyCard(current);
      } else {
        D.descBase = current.version; // the user has seen their version and merges by hand
        D.descEditor?.focus();
      }
    });
  }

  /* --------------------------------------------------------------- subtasks */
  let subDnd = null;
  function renderSubtasks() {
    const c = D.card;
    const subs = [...c.subtasks].sort((a, b) => a.position - b.position);
    const done = subs.filter((s) => s.done).length;
    const ed = editor();
    const bar = ui.progress({ value: done, max: Math.max(subs.length, 1), size: 'sm', variant: subs.length && done === subs.length ? 'success' : 'accent', label: 'Subtasks done' });
    const head = h('div', { class: 'kb-d-section-head' }, kicon('checklist', 16), h('h3', {}, 'Subtasks'),
      subs.length ? h('span', { class: 'faint small nums' }, `${done}/${subs.length}`) : null);
    const list = h('ul', { class: 'kb-subs', aria: { label: 'Subtasks' } });
    for (const s of subs) list.append(subtaskRow(s, subs));
    const add = ed ? subtaskAdder() : null;
    fill(subsEl, head, subs.length ? bar.el : null, list, add, !subs.length && !ed ? h('p', { class: 'faint small' }, 'No subtasks.') : null);
    subDnd?.destroy();
    subDnd = ed ? createDnd({
      root: list, itemSelector: '.kb-sub', handleSelector: '.kb-sub-handle', axis: 'y',
      containers: () => [{ id: 'subs', el: list, list }],
      onDrop: ({ id, beforeId }) => {
        const cur = [...D.card.subtasks].sort((a, b) => a.position - b.position);
        const from = cur.findIndex((x) => x.id === id);
        let to = beforeId ? cur.findIndex((x) => x.id === beforeId) : cur.length;
        if (from < to) to -= 1;
        moveSubtask(id, to);
      },
    }) : null;
  }

  function subtaskRow(s, subs) {
    const ed = editor();
    const cb = h('input', { type: 'checkbox', class: 'checkbox', checked: s.done, disabled: !ed, aria: { label: `Done: ${s.title}` } });
    cb.addEventListener('change', () => updateSubtask(s.id, { done: cb.checked }));
    const title = h('span', { class: cls('kb-sub-title', { 'is-done': s.done }) }, s.title);
    if (ed) {
      title.tabIndex = 0;
      title.setAttribute('role', 'button');
      title.setAttribute('aria-label', `Rename “${s.title}”`);
      title.addEventListener('click', () => renameSubtaskInline(s, title));
      title.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); renameSubtaskInline(s, title); } });
    }
    const due = s.due_at ? h('span', { class: cls('kb-due', `is-${s.done ? 'done' : dueState({ due_at: s.due_at }) || 'later'}`) }, icon('clock', 12), dueLabel({ due_at: s.due_at, all_day: true })) : null;
    const who = s.assignee_id ? memberAvatar(members().get(s.assignee_id) || { username: 'Unknown user' }, 22) : null;
    const i = subs.findIndex((x) => x.id === s.id);
    const menuBtn = ed ? iconButton('more', 'Subtask actions', () => ui.openMenu({
      anchor: menuBtn, align: 'end', items: [
        { label: 'Rename', icon: 'edit', onSelect: () => renameSubtaskInline(s, title) },
        { label: s.due_at ? 'Change due date…' : 'Set due date…', icon: 'calendar', onSelect: () => subtaskDue(s, menuBtn) },
        { label: 'Assign…', icon: 'user', onSelect: () => subtaskAssign(s, menuBtn) },
        { label: 'Turn into card', icon: 'arrow-up', onSelect: () => promote(s) },
        { label: 'Move up', icon: 'arrow-up', disabled: i <= 0, onSelect: () => moveSubtask(s.id, i - 1) },
        { label: 'Move down', icon: 'arrow-down', disabled: i >= subs.length - 1, onSelect: () => moveSubtask(s.id, i + 1) },
        { separator: true },
        { label: 'Delete', icon: 'trash', danger: true, onSelect: () => deleteSubtask(s) },
      ],
    })) : null;
    const handle = ed ? h('button', { type: 'button', class: 'kb-sub-handle kb-drag-handle', aria: { label: `Reorder “${s.title}” (Alt+↑/↓)`, keyshortcuts: 'Alt+ArrowUp Alt+ArrowDown' }, dataset: { tip: 'Drag or Alt+↑/↓ to reorder' } }, kicon('grip', 14)) : null;
    handle?.addEventListener('keydown', (e) => {
      if (e.altKey && (e.key === 'ArrowUp' || e.key === 'ArrowDown')) {
        e.preventDefault();
        const to = e.key === 'ArrowUp' ? i - 1 : i + 1;
        if (to >= 0 && to < subs.length) moveSubtask(s.id, to, { refocus: true });
      }
    });
    return h('li', { class: 'kb-sub', dataset: { id: s.id } }, handle, cb, title, h('span', { class: 'kb-sub-meta' }, due, who), menuBtn);
  }

  function subtaskAdder() {
    const input = h('input', { class: 'input input-sm kb-sub-add', placeholder: 'Add a subtask…', maxlength: 500, aria: { label: 'New subtask' } });
    input.addEventListener('keydown', async (e) => {
      if (e.key !== 'Enter' || e.isComposing) return;
      e.preventDefault();
      const title = input.value.trim();
      if (!title) return;
      input.disabled = true;
      try {
        const s = await kapi.createSubtask(cardId, { title });
        D.card.subtasks = [...D.card.subtasks, s];
        renderSubtasks();
        countersChanged();
        requestAnimationFrame(() => subsEl.querySelector('.kb-sub-add')?.focus());
      } catch (err) {
        ui.toast.error(err);
        input.disabled = false;
      }
    });
    return input;
  }

  function renameSubtaskInline(s, titleEl) {
    const input = h('input', { class: 'input input-sm', value: s.title, maxlength: 500, aria: { label: 'Subtask title' } });
    let done = false;
    const finish = async (commit) => {
      if (done) return;
      done = true;
      const v = input.value.trim();
      if (commit && v && v !== s.title) await updateSubtask(s.id, { title: v });
      else renderSubtasks();
    };
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') { e.preventDefault(); finish(true); }
      if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); finish(false); }
    });
    input.addEventListener('blur', () => finish(true));
    titleEl.replaceWith(input);
    input.focus();
    input.select();
  }

  async function updateSubtask(id, p) {
    try {
      const s = await kapi.updateSubtask(id, p);
      D.card.subtasks = D.card.subtasks.map((x) => (x.id === id ? s : x));
      renderSubtasks();
      countersChanged();
    } catch (err) {
      ui.toast.error(err);
      renderSubtasks();
    }
  }

  async function moveSubtask(id, to, { refocus = false } = {}) {
    const before = D.card.subtasks;
    D.card.subtasks = reorder(D.card.subtasks, id, to);
    renderSubtasks();
    if (refocus) subsEl.querySelector(`.kb-sub[data-id="${CSS.escape(id)}"] .kb-sub-handle`)?.focus();
    try {
      D.card.subtasks = await kapi.moveSubtask(id, to);
      renderSubtasks();
      if (refocus) subsEl.querySelector(`.kb-sub[data-id="${CSS.escape(id)}"] .kb-sub-handle`)?.focus();
      host.boardChanged();
    } catch (err) {
      D.card.subtasks = before;
      renderSubtasks();
      ui.toast.error(err);
    }
  }

  function subtaskDue(s, anchor) {
    const input = h('input', { type: 'date', class: 'input input-sm', value: localDate(s.due_at), aria: { label: 'Due date' } });
    const save = async (v) => {
      closePopover();
      await updateSubtask(s.id, { due_at: v ? datesPatch({ start: '', due: v, allDay: true }).due_at : null });
    };
    popover({
      anchor, label: 'Subtask due date',
      content: h('div', { class: 'kb-pop-body' }, h('div', { class: 'kb-pop-title' }, 'Due date'), input,
        h('div', { class: 'row between' }, ui.button({ label: 'Remove', size: 'sm', variant: 'danger-ghost', onClick: () => save('') }),
          ui.button({ label: 'Save', size: 'sm', variant: 'primary', onClick: () => save(input.value) }))),
    });
  }

  function subtaskAssign(s, anchor) {
    const board = host.board();
    ui.openMenu({
      anchor, align: 'end', items: [
        { heading: 'Assign to' },
        { label: 'Nobody', checked: !s.assignee_id, onSelect: () => updateSubtask(s.id, { assignee_id: null }) },
        ...board.members.map((mb) => ({ label: memberName(mb), checked: s.assignee_id === mb.user_id, onSelect: () => updateSubtask(s.id, { assignee_id: mb.user_id }) })),
      ],
    });
  }

  async function promote(s) {
    try {
      const card = await kapi.promoteSubtask(s.id);
      D.card.subtasks = D.card.subtasks.filter((x) => x.id !== s.id);
      renderSubtasks();
      countersChanged();
      host.cardChanged(card);
      ui.toast.success(`“${card.title}” is now a card`, { action: { label: 'Open', onClick: () => ctx.router.navigate(paths.card(host.board().id, card.id)) } });
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function deleteSubtask(s) {
    try {
      await kapi.deleteSubtask(s.id);
      D.card.subtasks = D.card.subtasks.filter((x) => x.id !== s.id)
        .map((x) => (x.position > s.position ? { ...x, position: x.position - 1 } : x));
      renderSubtasks();
      countersChanged();
    } catch (err) {
      ui.toast.error(err);
    }
  }

  /* ------------------------------------------------------------ attachments */
  function renderAttachments() {
    const c = D.card;
    const ed = editor();
    const head = h('div', { class: 'kb-d-section-head' }, kicon('paperclip', 16), h('h3', {}, 'Attachments'),
      c.attachments.length ? h('span', { class: 'faint small nums' }, String(c.attachments.length)) : null,
      ed ? h('span', { class: 'kb-d-head-actions' },
        ui.button({ label: 'Upload', icon: 'upload', size: 'sm', variant: 'ghost', onClick: async () => upload(await pickFiles({ multiple: true })) }),
        ui.button({ label: 'From Files', icon: 'folder', size: 'sm', variant: 'ghost', onClick: () => fromFiles() })) : null);
    const list = h('ul', { class: 'kb-atts' });
    for (const a of c.attachments) list.append(attachmentRow(a));
    for (const u of D.uploads.values()) list.append(u.row);
    const empty = !c.attachments.length && !D.uploads.size
      ? (ed ? h('button', { type: 'button', class: 'kb-d-placeholder kb-d-dropzone', onClick: async () => upload(await pickFiles({ multiple: true })) },
        kicon('paperclip', 16), 'Drop files here or click to attach (up to 50 MiB each)') : h('p', { class: 'faint small' }, 'No attachments.'))
      : null;
    fill(attEl, head, list, empty);
  }

  function attachmentRow(a) {
    const c = D.card;
    const isCover = c.cover_attachment_id === a.id;
    const thumb = a.thumbnail_url
      ? h('img', { src: `${a.thumbnail_url}?size=sm`, alt: '', loading: 'lazy' })
      : a.is_image ? h('img', { src: a.url, alt: '', loading: 'lazy' })
        : h('span', { class: 'kb-att-ext' }, (a.filename.split('.').pop() || 'file').slice(0, 4).toUpperCase());
    const open = () => (a.is_image ? lightbox(a) : kapi.downloadAttachment(a.id));
    const thumbBtn = h('button', { type: 'button', class: 'kb-att-thumb', aria: { label: a.is_image ? `Preview ${a.filename}` : `Download ${a.filename}` } }, thumb);
    thumbBtn.addEventListener('click', open);
    const nameBtn = h('button', { type: 'button', class: 'kb-att-name truncate' }, a.filename);
    nameBtn.addEventListener('click', open);
    const uploader = members().get(a.uploaded_by);
    const canDelete = editor() || a.uploaded_by === app.me;
    const menuBtn = iconButton('more', 'Attachment actions', () => ui.openMenu({
      anchor: menuBtn, align: 'end', items: [
        a.is_image ? { label: 'Preview', icon: 'eye', onSelect: () => lightbox(a) } : null,
        { label: 'Download', icon: 'download', onSelect: () => kapi.downloadAttachment(a.id) },
        editor() && a.is_image ? { label: isCover ? 'Remove cover' : 'Make cover', icon: 'image', onSelect: () => quick({ cover_attachment_id: isCover ? null : a.id }) } : null,
        editor() ? { label: 'Rename…', icon: 'edit', onSelect: () => renameAttachment(a) } : null,
        { label: 'Save to Files…', icon: 'folder', onSelect: () => saveToFiles(a) },
        canDelete ? { separator: true } : null,
        canDelete ? { label: 'Delete', icon: 'trash', danger: true, onSelect: () => deleteAttachment(a) } : null,
      ].filter(Boolean),
    }));
    return h('li', { class: 'kb-att', dataset: { id: a.id } }, thumbBtn,
      h('div', { class: 'kb-att-info' }, nameBtn, h('div', { class: 'kb-att-meta faint small' },
        `${formatBytes(a.size)} · ${uploader ? memberName(uploader) : 'former member'} · `, ui.timeAgo(a.created_at),
        isCover ? h('span', { class: 'badge badge-accent kb-cover-badge' }, 'Cover') : null)),
      menuBtn);
  }

  function upload(files) {
    if (!files?.length) return;
    if (!editor()) { ui.toast.error('Viewers cannot attach files.'); return; }
    for (const file of files) {
      if (file.size > MAX_ATTACHMENT) { ui.toast.error(`“${file.name}” is larger than 50 MiB.`); continue; }
      if (file.size === 0) { ui.toast.error(`“${file.name}” is empty.`); continue; }
      const key = `${Date.now()}-${Math.random()}`;
      const ctrl = new AbortController();
      const bar = ui.progress({ value: 0, max: 1, size: 'sm', label: `Uploading ${file.name}` });
      const row = h('li', { class: 'kb-att is-uploading' }, h('span', { class: 'kb-att-thumb' }, ui.spinner({ size: 'sm' })),
        h('div', { class: 'kb-att-info' }, h('span', { class: 'kb-att-name truncate' }, file.name), bar.el),
        iconButton('x', 'Cancel upload', () => ctrl.abort()));
      D.uploads.set(key, { row, ctrl });
      renderAttachments();
      kapi.upload(cardId, file, { onProgress: (p) => bar.set(p.fraction, 1), signal: ctrl.signal })
        .then((a) => {
          D.card.attachments = [...D.card.attachments, a];
          countersChanged();
        })
        .catch((err) => { if (err?.name !== 'AbortError') ui.toast.error(err, { description: file.name }); })
        .finally(() => {
          D.uploads.delete(key);
          if (!D.closed) renderAttachments();
        });
    }
  }

  async function fromFiles() {
    const entry = await filesPicker(ctx);
    if (!entry) return;
    try {
      const a = await kapi.attachFromFile(cardId, entry.id);
      D.card.attachments = [...D.card.attachments, a];
      renderAttachments();
      countersChanged();
      ui.toast.success(`Attached “${a.filename}”`);
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function renameAttachment(a) {
    const name = await ui.prompt({ title: 'Rename attachment', label: 'File name', value: a.filename, confirmLabel: 'Rename', validate: (v) => (!v.trim() ? 'Enter a name.' : null) });
    if (name === null || name.trim() === a.filename) return;
    try {
      const n = await kapi.renameAttachment(a.id, name.trim());
      D.card.attachments = D.card.attachments.map((x) => (x.id === a.id ? n : x));
      renderAttachments();
      host.boardChanged();
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function deleteAttachment(a) {
    if (!(await ui.confirm({ title: `Delete “${a.filename}”?`, message: 'The file is removed from the card.', confirmLabel: 'Delete', danger: true }))) return;
    try {
      await kapi.deleteAttachment(a.id);
      D.card.attachments = D.card.attachments.filter((x) => x.id !== a.id);
      if (D.card.cover_attachment_id === a.id) D.card.cover_attachment_id = null;
      renderAll();
      countersChanged();
      refreshSoon(); // the cover change moved the card version on
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function saveToFiles(a) {
    const target = await saveToFilesDialog(ctx, a);
    if (!target) return;
    try {
      const e = await kapi.saveToFiles(a.id, target);
      const dir = e.path.includes('/') ? e.path.slice(0, e.path.lastIndexOf('/')) : '';
      ui.toast.success(`Saved “${e.name}” to Files`, { action: { label: 'Show', onClick: () => ctx.router.navigate(`/files?space=${encodeURIComponent(e.space_id)}${dir ? `&path=${encodeURIComponent(dir)}` : ''}`) } });
    } catch (err) {
      ui.toast.error(err);
    }
  }

  function lightbox(start) {
    const images = D.card.attachments.filter((a) => a.is_image);
    let i = Math.max(0, images.findIndex((a) => a.id === start.id));
    const img = h('img', { class: 'kb-lightbox-img', alt: '' });
    const caption = h('div', { class: 'kb-lightbox-caption' });
    const prev = iconButton('chevron-left', 'Previous image', () => show(i - 1), { size: 'md', className: 'kb-lightbox-prev' });
    const next = iconButton('chevron-right', 'Next image', () => show(i + 1), { size: 'md', className: 'kb-lightbox-next' });
    function show(n) {
      if (!images.length) return;
      i = (n + images.length) % images.length;
      const a = images[i];
      img.src = a.url;
      img.alt = a.filename;
      caption.replaceChildren(h('span', { class: 'truncate' }, a.filename), h('span', { class: 'faint' }, `${i + 1} / ${images.length} · ${formatBytes(a.size)}`),
        ui.button({ label: 'Download', icon: 'download', size: 'sm', variant: 'ghost', onClick: () => kapi.downloadAttachment(a.id) }));
      prev.hidden = next.hidden = images.length < 2;
    }
    const lm = ui.modal({ title: '', size: 'xl', className: 'kb-lightbox', body: h('div', { class: 'kb-lightbox-stage' }, prev, img, next, caption) });
    lm.el.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowLeft') { e.preventDefault(); show(i - 1); }
      if (e.key === 'ArrowRight') { e.preventDefault(); show(i + 1); }
    });
    show(i);
  }

  // Files dropped anywhere on the card attach to it.
  let dragDepth = 0;
  const hasFiles = (e) => [...(e.dataTransfer?.types || [])].includes('Files');
  m.el.addEventListener('dragenter', (e) => { if (!hasFiles(e) || !editor()) return; e.preventDefault(); dragDepth++; dropEl.hidden = false; });
  m.el.addEventListener('dragover', (e) => { if (!hasFiles(e) || !editor()) return; e.preventDefault(); e.dataTransfer.dropEffect = 'copy'; });
  m.el.addEventListener('dragleave', () => { dragDepth = Math.max(0, dragDepth - 1); if (!dragDepth) dropEl.hidden = true; });
  m.el.addEventListener('drop', (e) => {
    if (!hasFiles(e) || !editor() || e.target.closest('.mde')) return;
    e.preventDefault();
    dragDepth = 0;
    dropEl.hidden = true;
    upload([...e.dataTransfer.files]);
  });
  // Images pasted outside the description editor attach too.
  m.el.addEventListener('paste', (e) => {
    if (!editor() || e.target.closest('textarea, input')) return;
    const files = [...(e.clipboardData?.files || [])];
    if (files.length) { e.preventDefault(); upload(files); }
  });

  /* --------------------------------------------------------------- comments */
  function buildComposer() {
    const ta = h('textarea', { class: 'textarea kb-comment-input', rows: 3, placeholder: 'Write a comment… (Markdown)', maxlength: 20000, aria: { label: 'New comment' } });
    ta.value = drafts.get(`c:${cardId}`) || '';
    ta.addEventListener('input', () => { if (ta.value) drafts.set(`c:${cardId}`, ta.value); else drafts.delete(`c:${cardId}`); });
    const preview = h('div', { class: 'md kb-comment-preview', hidden: true });
    const tabs = ui.segmented({
      options: [{ value: 'write', label: 'Write' }, { value: 'preview', label: 'Preview' }], value: 'write', size: 'sm', ariaLabel: 'Comment view',
      onChange: (v) => {
        preview.hidden = v !== 'preview';
        ta.hidden = v === 'preview';
        if (v === 'preview') preview.replaceChildren(ta.value.trim() ? renderToDOM(ta.value, document) : h('p', { class: 'faint' }, 'Nothing to preview.'));
      },
    });
    const send = async () => {
      const body2 = ta.value;
      if (!body2.trim()) { ta.focus(); return; }
      ta.disabled = true;
      try {
        const cmt = await kapi.createComment(cardId, body2);
        ta.value = '';
        drafts.delete(`c:${cardId}`);
        tabs.setValue('write');
        preview.hidden = true;
        ta.hidden = false;
        D.card.comments = [...D.card.comments, cmt];
        D.card.comment_count = (D.card.comment_count || 0) + 1;
        renderComments();
        countersChanged();
        commentsList.lastElementChild?.scrollIntoView({ block: 'nearest' });
      } catch (err) {
        ui.toast.error(err);
      } finally {
        ta.disabled = false;
        ta.focus();
      }
    };
    ta.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && (app.isApple ? e.metaKey : e.ctrlKey)) { e.preventDefault(); send(); }
      else if (e.key === 'Escape' && ta.value.trim()) { e.preventDefault(); ta.blur(); }
    });
    const el = h('div', { class: 'kb-composer-comment' }, avatarFor(app.me, 28), h('div', { class: 'kb-composer-main' }, ta, preview,
      h('div', { class: 'row between' }, tabs.el, h('div', { class: 'row' }, h('span', { class: 'faint small hide-sm' }, `${app.isApple ? '⌘' : 'Ctrl'}+Enter`),
        ui.button({ label: 'Comment', variant: 'primary', size: 'sm', onClick: send })))));
    return { el, ta };
  }

  function avatarFor(userId, size) {
    const mb = members().get(userId) || { username: ctx.user?.username || 'You', display_name: ctx.user?.display_name };
    return memberAvatar(mb, size);
  }

  function renderComments() {
    const c = D.card;
    fill(commentsHead, kicon('comment', 16), h('h3', {}, 'Comments'),
      c.comment_count ? h('span', { class: 'faint small nums' }, String(c.comment_count)) : null);
    const items = [];
    if (!D.commentsAll && c.comment_count > c.comments.length) {
      items.push(ui.button({ label: `Show all ${c.comment_count} comments`, size: 'sm', variant: 'ghost', onClick: () => loadAllComments() }));
    }
    for (const cm of c.comments) items.push(commentItem(cm));
    commentsList.replaceChildren(...items);
  }

  async function loadAllComments() {
    try {
      const all = [];
      let cursor;
      do {
        const page = await kapi.comments(cardId, { cursor, limit: 200, signal: ctx.signal });
        all.push(...page.items);
        cursor = page.next_cursor || undefined;
      } while (cursor);
      D.card.comments = all;
      D.commentsAll = true;
      renderComments();
    } catch (err) {
      ui.toast.error(err);
    }
  }

  function commentItem(cm) {
    const mine = cm.author_id === app.me;
    const bodyEl = h('div', { class: 'md kb-comment-body' });
    bodyEl.append(renderToDOM(cm.body, document));
    const menu = mine || admin() ? iconButton('more', 'Comment actions', () => ui.openMenu({
      anchor: menu, align: 'end', items: [
        mine ? { label: 'Edit', icon: 'edit', onSelect: () => editComment(cm, bodyEl) } : null,
        { label: 'Delete', icon: 'trash', danger: true, onSelect: () => deleteComment(cm) },
      ].filter(Boolean),
    })) : null;
    const author = members().get(cm.author_id);
    return h('article', { class: 'kb-comment', dataset: { id: cm.id } },
      author ? memberAvatar(author, 28) : h('span', { class: 'kb-avatar' }, ui.avatar({ name: cm.author_name, size: 28 })),
      h('div', { class: 'kb-comment-main' },
        h('div', { class: 'kb-comment-head' }, h('span', { class: 'kb-comment-author' }, cm.author_name), ui.timeAgo(cm.created_at),
          cm.edited ? h('span', { class: 'faint small', dataset: { tip: `Edited ${formatDateTime(cm.updated_at)}` } }, '(edited)') : null, h('span', { class: 'spacer' }), menu),
        bodyEl));
  }

  function editComment(cm, bodyEl) {
    const ta = h('textarea', { class: 'textarea', rows: 3, maxlength: 20000, aria: { label: 'Edit comment' } });
    ta.value = cm.body;
    const save = async () => {
      if (!ta.value.trim()) return;
      try {
        const n = await kapi.updateComment(cm.id, ta.value);
        D.card.comments = D.card.comments.map((x) => (x.id === cm.id ? n : x));
        renderComments();
      } catch (err) {
        ui.toast.error(err);
      }
    };
    ta.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && (app.isApple ? e.metaKey : e.ctrlKey)) { e.preventDefault(); save(); }
      if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); renderComments(); }
    });
    bodyEl.replaceWith(h('div', { class: 'stack-xs' }, ta, h('div', { class: 'row' },
      ui.button({ label: 'Save', variant: 'primary', size: 'sm', onClick: save }), ui.button({ label: 'Cancel', variant: 'ghost', size: 'sm', onClick: () => renderComments() }))));
    ta.focus();
  }

  async function deleteComment(cm) {
    if (!(await ui.confirm({ title: 'Delete this comment?', confirmLabel: 'Delete', danger: true }))) return;
    try {
      await kapi.deleteComment(cm.id);
      D.card.comments = D.card.comments.filter((x) => x.id !== cm.id);
      D.card.comment_count = Math.max(0, (D.card.comment_count || 1) - 1);
      renderComments();
      countersChanged();
    } catch (err) {
      ui.toast.error(err);
    }
  }

  /* --------------------------------------------------------------- activity */
  let activityExtra = null; // older entries loaded on demand
  let activityCursor = null;
  function renderActivity() {
    const entries = [...D.card.activity, ...(activityExtra || [])];
    const users = (id) => memberLabel(id);
    const list = h('ol', { class: 'kb-activity' }, entries.map((a) => h('li', { class: 'kb-activity-item' },
      h('span', { class: 'kb-activity-dot', aria: { hidden: 'true' } }),
      h('span', { class: 'kb-activity-text' }, activityText(a, { users, inCard: true })), ui.timeAgo(a.created_at))));
    const more = (activityExtra === null && D.card.activity.length >= 50) || activityCursor
      ? ui.button({ label: 'Show older activity', size: 'sm', variant: 'ghost', onClick: () => loadOlderActivity() }) : null;
    fill(actEl, h('div', { class: 'kb-d-section-head' }, icon('activity', 16), h('h3', {}, 'Activity')),
      entries.length ? list : h('p', { class: 'faint small' }, 'No activity yet.'), more);
  }

  async function loadOlderActivity() {
    try {
      if (activityExtra === null) {
        // Page through from the newest and skip what the detail already shows.
        const page = await kapi.activity(host.board().id, { cardId, limit: 100, signal: ctx.signal });
        const seen = new Set(D.card.activity.map((a) => a.id));
        activityExtra = page.items.filter((a) => !seen.has(a.id));
        activityCursor = page.next_cursor;
      } else if (activityCursor) {
        const page = await kapi.activity(host.board().id, { cardId, cursor: activityCursor, limit: 100, signal: ctx.signal });
        activityExtra.push(...page.items);
        activityCursor = page.next_cursor;
      }
      renderActivity();
    } catch (err) {
      ui.toast.error(err);
    }
  }

  /* ------------------------------------------------------------------- side */
  function renderSide() {
    const c = D.card;
    const ed = editor();
    const btn = (label, ic, onClick, opts = {}) => {
      const b = h('button', { type: 'button', class: cls('kb-side-btn', opts.danger && 'is-danger') }, kicon(ic, 15), h('span', {}, label));
      b.addEventListener('click', (e) => onClick(e.currentTarget));
      return b;
    };
    const groups = [];
    if (ed) {
      groups.push(h('div', { class: 'kb-side-group' }, h('div', { class: 'kb-side-title' }, 'Add to card'),
        btn('Labels', 'tag', (b) => labelPicker(b)),
        btn('Assignees', 'user-plus', (b) => assigneePicker(b)),
        btn('Dates', 'calendar', (b) => datesEditor(b)),
        btn('Subtask', 'checklist', () => subsEl.querySelector('.kb-sub-add')?.focus()),
        btn('Attachment', 'paperclip', async () => upload(await pickFiles({ multiple: true }))),
        btn('Cover', 'cover', (b) => coverMenu(b))));
      groups.push(h('div', { class: 'kb-side-group' }, h('div', { class: 'kb-side-title' }, 'Actions'),
        btn('Move', 'arrow-right', (b) => moveMenu(b)),
        btn(c.completed ? 'Mark not done' : 'Mark done', 'check-circle', () => quick({ completed: !c.completed })),
        btn(c.archived ? 'Restore' : 'Archive', c.archived ? 'restore' : 'archive', () => quick({ archived: !c.archived })),
        btn('Copy link', 'link', () => ui.copyText(new URL(paths.card(host.board().id, cardId), location.origin).href, { message: 'Link copied' })),
        btn('Delete', 'trash', async () => { if (await host.deleteCard(cardId)) m.close(); }, { danger: true })));
    } else {
      groups.push(h('div', { class: 'kb-side-group' }, h('div', { class: 'kb-side-title' }, 'Actions'),
        btn('Copy link', 'link', () => ui.copyText(new URL(paths.card(host.board().id, cardId), location.origin).href, { message: 'Link copied' })),
        h('p', { class: 'faint small' }, 'You can read and comment on this board.')));
    }
    const creator = members().get(c.created_by);
    groups.push(h('div', { class: 'kb-side-group kb-side-facts faint small' },
      h('div', {}, 'Created ', ui.timeAgo(c.created_at), ` by ${creator ? memberName(creator) : 'a former member'}`),
      h('div', {}, 'Updated ', ui.timeAgo(c.updated_at))));
    sideEl.replaceChildren(...groups);
  }

  function coverMenu(anchor) {
    const images = D.card.attachments.filter((a) => a.is_image);
    ui.openMenu({
      anchor, items: [
        { heading: images.length ? 'Use an image as cover' : 'Attach an image to use it as cover' },
        ...images.map((a) => ({ label: a.filename, checked: D.card.cover_attachment_id === a.id, onSelect: () => quick({ cover_attachment_id: a.id }) })),
        D.card.cover_attachment_id ? { label: 'Remove cover', icon: 'x', onSelect: () => quick({ cover_attachment_id: null }) } : null,
      ].filter(Boolean),
    });
  }

  /* --------------------------------------------------------------- start */
  load();

  return {
    cardId,
    close({ silent = false } = {}) {
      D.silent = silent;
      if (!D.closed) m.close();
    },
    cardPatched(c) {
      if (!D.card || c.id !== cardId) return;
      D.card = { ...D.card, ...c, description: D.card.description };
      if (c.version !== undefined) refreshSoon();
      else renderAll();
    },
    boardChanged() {
      if (!D.card) return;
      const b = host.board();
      const bc = b.cards.find((x) => x.id === cardId);
      if (!bc) {
        ui.toast.warning('This card was deleted.');
        m.close();
        return;
      }
      const changed = bc.version !== D.card.version || bc.updated_at !== D.card.updated_at || bc.comment_count !== D.card.comment_count
        || bc.attachment_count !== D.card.attachments.length || bc.subtasks_total !== D.card.subtasks.length
        || bc.subtasks_done !== D.card.subtasks.filter((s) => s.done).length || bc.column_id !== D.card.column_id;
      if (changed) refreshSoon();
      else { renderHead(); renderProps(); }
    },
  };
}
