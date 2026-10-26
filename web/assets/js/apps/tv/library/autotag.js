// TV library — auto-tag dialog: pick a space and folder, then let the server recognise
// movies and episodes from file/folder names and Kodi .nfo files.

import { h } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import { plural } from '../../../core/format.js';
import { mediaApi } from './api.js';
import { folderPicker, checkRow } from './widgets.js';

/**
 * @param {object} ctx
 * @param {{spaceId?: string, path?: string}} [opts]
 * @returns {Promise<{updated: number, skipped: number}|null>}
 */
export async function openAutoTag(ctx, opts = {}) {
  const { ui } = ctx;
  const api = mediaApi(ctx.api);
  const where = h('div', { class: 'tvl-autotag-where small' });
  const picker = folderPicker({
    api, ui, writableOnly: true, spaceId: opts.spaceId || '', path: opts.path || '',
    onChange: ({ path }) => { where.replaceChildren(icon('folder', 14), ` ${path || 'Whole space'}`); },
  });
  const recursive = checkRow('Include subfolders', true);
  const overwrite = checkRow('Overwrite existing details', false, 'Replace titles and other details that were edited by hand, by TMDB or by an earlier run.');
  const result = h('div', { aria: { live: 'polite' } });
  let last = null;
  const m = ui.modal({
    title: 'Auto-tag a folder',
    description: 'Recognises movies and episodes from names like “Show.Name.S01E02.mkv”, “Show/Season 1/…” or “Movie (2010).mp4”, and reads Kodi .nfo files.',
    size: 'md',
    body: h('div', { class: 'stack' }, picker.el, h('div', { class: 'tvl-autotag-summary' }, h('span', { class: 'muted small' }, 'Folder:'), where),
      h('div', { class: 'stack-xs' }, recursive, overwrite), result),
    footer: [{ label: 'Close', value: null }, { label: 'Run auto-tag', icon: 'sparkle', variant: 'primary', type: 'submit' }],
    onSubmit: async () => {
      const { spaceId, path } = picker.value;
      if (!spaceId) {
        ui.toast.error('Choose a space you can write to.');
        return false;
      }
      result.replaceChildren(ui.loadingBlock('Reading names and .nfo files…'));
      try {
        last = await api.autoTag({ space_id: spaceId, path, recursive: recursive.inputEl.checked, overwrite: overwrite.inputEl.checked });
        result.replaceChildren(ui.alert({
          variant: last.updated ? 'success' : 'info',
          title: last.updated ? `Updated ${plural(last.updated, 'item')}` : 'Nothing to change',
          message: last.skipped ? `${plural(last.skipped, 'item')} unchanged (already tagged or not recognised).` : '',
        }));
        m.buttons[1].querySelector('.btn-label').textContent = 'Run again';
      } catch (err) {
        result.replaceChildren(ui.alert({ variant: 'danger', title: 'Auto-tag failed', message: ui.errorMessage(err) }));
      }
      return false; // keep the dialog open to show the result
    },
  });
  await m.closed;
  return last;
}
