// TV library — bulk edit: apply category, show, season, genres, rating or year to many
// items at once (POST /media/bulk). Only ticked fields are sent.

import { h } from '../../../core/dom.js';
import { plural } from '../../../core/format.js';
import { field, showFormError } from '../../../core/forms.js';
import { mediaApi } from './api.js';
import { genreInput, categorySelects } from './widgets.js';
import { intField } from './model.js';

/**
 * @param {object} ctx
 * @param {string[]} ids file ids
 * @param {{show?: string}} [opts] prefill for "edit all episodes"
 * @returns {Promise<{updated: number}|null>}
 */
export async function openBulkEditor(ctx, ids, opts = {}) {
  const { ui } = ctx;
  const api = mediaApi(ctx.api);
  const [categories, facets] = await Promise.all([api.categories().catch(() => []), api.facets({}).catch(() => null)]);
  const genres = genreInput({ suggestions: (facets?.genres || []).map((g) => g.value) });
  const cats = categorySelects(categories, { category: opts.show ? 'tv' : '', subcategory: opts.show ? 'episode' : '' });
  const showTitle = h('input', { class: 'input', maxlength: '500', autocomplete: 'off' });
  showTitle.value = opts.show || '';
  const season = h('input', { class: 'input', inputmode: 'numeric', maxlength: '5' });
  const year = h('input', { class: 'input', inputmode: 'numeric', maxlength: '4' });
  const rating = h('input', { class: 'input', maxlength: '32' });

  // Each row: a checkbox enabling the field + the control.
  const rows = [];
  const row = (name, label, control, hint = '') => {
    const cb = h('input', { type: 'checkbox', class: 'checkbox', aria: { label: `Change ${label}` } });
    const wrap = field({ label, control, name, hint });
    const sync = () => {
      wrap.classList.toggle('is-disabled', !cb.checked);
      for (const el of control.querySelectorAll ? [control, ...control.querySelectorAll('input,select,textarea')] : [control]) {
        if (el.matches?.('input,select,textarea')) el.disabled = !cb.checked;
      }
    };
    cb.addEventListener('change', () => {
      sync();
      if (cb.checked) (control.matches?.('input,select') ? control : control.querySelector('input,select'))?.focus();
    });
    sync();
    rows.push({ name, cb });
    return h('div', { class: 'tvl-bulk-row' }, cb, wrap);
  };
  const catControl = h('div', { class: 'form-grid cols-2' }, cats.category, cats.subcategory);
  const body = h('div', { class: 'stack' },
    h('p', { class: 'muted' }, `Tick the fields to change on ${plural(ids.length, 'item')}. Other fields are left as they are.`),
    row('category', 'Category & type', catControl),
    row('show_title', 'Show', showTitle, 'Groups the items as episodes of one show.'),
    row('season', 'Season', season),
    row('genres', 'Genres', genres.el, 'Replaces the genres of every item.'),
    row('content_rating', 'Rating', rating),
    row('year', 'Year', year));
  if (opts.show) rows.find((r) => r.name === 'show_title').cb.click();

  const m = ui.modal({
    title: 'Edit selected items', size: 'md', body,
    footer: [{ label: 'Cancel', value: null }, { label: 'Apply', variant: 'primary', type: 'submit' }],
    onSubmit: async () => {
      const set = {};
      const on = (n) => rows.find((r) => r.name === n).cb.checked;
      const fields = {};
      if (on('category')) Object.assign(set, cats.value);
      if (on('show_title')) set.show_title = showTitle.value.trim();
      if (on('season')) {
        set.season = intField(season.value);
        if (Number.isNaN(set.season)) fields.season = 'Enter a whole number.';
      }
      if (on('genres')) set.genres = genres.value;
      if (on('content_rating')) set.content_rating = rating.value.trim();
      if (on('year')) {
        set.year = intField(year.value);
        if (Number.isNaN(set.year) || (set.year !== null && (set.year < 1800 || set.year > 3000))) fields.year = 'Enter a year between 1800 and 3000.';
      }
      if (Object.keys(fields).length) {
        showFormError(body, { fields });
        return false;
      }
      if (!Object.keys(set).length) {
        ui.toast.info('Tick at least one field to change.');
        return false;
      }
      try {
        const r = await api.bulk(ids, set);
        if (r.errors?.length) ui.toast.warning(`Updated ${plural(r.updated, 'item')}; ${plural(r.errors.length, 'item')} could not be changed.`, { description: r.errors[0].error });
        else ui.toast.success(`Updated ${plural(r.updated, 'item')}`);
        return r;
      } catch (err) {
        if (err.fields) {
          const mapped = {};
          for (const [k, v] of Object.entries(err.fields)) mapped[k.replace(/^set\./, '')] = v;
          showFormError(body, { fields: mapped });
          return false;
        }
        throw err;
      }
    },
  });
  const res = await m.closed;
  return res && typeof res === 'object' ? res : null;
}
