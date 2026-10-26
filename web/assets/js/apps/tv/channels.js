// /tv/channels — channel manager: own channels (create/edit dialog with number, name, group,
// description, logo, lineup, shuffle + mode, playback mode, visibility; reshuffle, restart,
// delete), channels shared by others, and the auto-generate dialog.

import { h, cls, uid } from '../../core/dom.js';
import { showFormError } from '../../core/forms.js';
import { allPages } from './tvapi.js';
import { channelLogo } from './shared.js';
import { sortChannels } from './model.js';

const STRATEGIES = [
  { value: 'per_show', icon: 'tv', title: 'One channel per show', desc: 'Every show with enough episodes gets its own channel.' },
  { value: 'per_genre', icon: 'film', title: 'One channel per movie genre', desc: 'Horror, Comedy, Drama… one channel each.' },
  { value: 'movies', icon: 'film', title: 'A movie channel', desc: 'All your movies on one channel.' },
  { value: 'all_tv', icon: 'layers', title: 'An everything-TV channel', desc: 'Every episode of every show on one channel.' },
];

function field(label, control, { hint = '', name = '' } = {}) {
  const target = control.matches?.('input,select,textarea') ? control : control.querySelector?.('input,select,textarea');
  if (target) target.id ||= uid('ch');
  return h('div', { class: 'field', dataset: { field: name } },
    h('label', { class: 'field-label', for: target?.id }, label), control,
    hint ? h('div', { class: 'field-hint' }, hint) : null,
    h('div', { class: 'field-error', role: 'alert', hidden: true }));
}

/**
 * Create/edit channel dialog. Resolves with the saved channel or null.
 * @param {{ctx: any, api: any, ui: any, signal: AbortSignal}} vctx
 * @param {{channel?: object, lineupId?: string, name?: string, groups?: string[]}} [o]
 */
export async function openChannelDialog(vctx, o = {}) {
  const { ctx, api, ui, signal } = vctx;
  const ch = o.channel || null;
  const me = ctx.user?.id;
  let lineups = [];
  try {
    lineups = (await allPages((c) => api.lineups(c, signal))).filter((l) => l.owner_id === me);
  } catch (err) {
    ui.toast.error(err);
    return null;
  }
  if (!lineups.length) {
    const go = await ui.confirm({ title: 'Create a lineup first', message: 'A channel plays a lineup. You have no lineups yet.', confirmLabel: 'Create a lineup' });
    if (go) ctx.router.navigate('/tv/lineups');
    return null;
  }
  const number = h('input', { class: 'input nums', name: 'number', type: 'number', min: '1', max: '9999', inputmode: 'numeric', placeholder: 'Auto' });
  const name = h('input', { class: 'input', name: 'name', required: true, maxlength: '200', autocomplete: 'off' });
  const groupList = uid('grp');
  const group = h('input', { class: 'input', name: 'group', maxlength: '100', autocomplete: 'off', list: groupList, placeholder: 'e.g. Movies, Kids' });
  const groupDl = h('datalist', { id: groupList }, (o.groups || []).map((g) => h('option', { value: g })));
  const desc = h('textarea', { class: 'textarea', name: 'description', rows: 2, maxlength: '2000' });
  const lineup = h('select', { class: 'select', name: 'lineup_id', required: true },
    lineups.map((l) => h('option', { value: l.id }, `${l.name} (${l.item_count} programs)`)));
  const shuffle = h('input', { type: 'checkbox', class: 'switch-input', role: 'switch' });
  const shuffleMode = ui.segmented({ options: [{ value: 'per_cycle', label: 'New order every loop' }, { value: 'fixed', label: 'Fixed order' }], value: 'per_cycle', size: 'sm', ariaLabel: 'Shuffle mode' });
  const playback = ui.segmented({ options: [{ value: 'live', label: 'Live', icon: 'radio' }, { value: 'on_demand', label: 'On demand', icon: 'play' }], value: 'live', size: 'sm', ariaLabel: 'Playback mode' });
  const visibility = ui.segmented({ options: [{ value: 'private', label: 'Private', icon: 'lock' }, { value: 'shared', label: 'Shared', icon: 'users' }], value: 'private', size: 'sm', ariaLabel: 'Visibility' });
  const logoInput = h('input', { type: 'file', accept: 'image/png,image/jpeg,image/gif', class: 'hidden' });
  const logoPreview = h('div', { class: 'tv-logo-edit' });
  let logoFile = null;
  let removeLogo = false;
  let previewUrl = '';

  if (ch) {
    number.value = String(ch.number);
    name.value = ch.name;
    group.value = ch.group || '';
    desc.value = ch.description || '';
    lineup.value = ch.lineup_id;
    shuffle.checked = ch.shuffle;
    shuffleMode.setValue(ch.shuffle_mode);
    playback.setValue(ch.playback_mode);
    visibility.setValue(ch.visibility);
  } else {
    if (o.lineupId) lineup.value = o.lineupId;
    name.value = o.name || '';
  }
  const modeRow = h('div', { class: 'tv-shuffle-mode' }, shuffleMode.el);
  const syncShuffle = () => { modeRow.hidden = !shuffle.checked; };
  shuffle.addEventListener('change', syncShuffle);
  syncShuffle();

  function renderLogo() {
    const shown = logoFile ? { ...ch, number: number.value || '?', name: name.value, logo_url: previewUrl } : removeLogo ? { ...ch, logo_url: null } : ch || { number: number.value || '#', name: name.value };
    logoPreview.replaceChildren(
      channelLogo({ number: shown.number ?? '#', name: shown.name || '', logo_url: shown.logo_url || null }, { size: 'xl' }),
      h('div', { class: 'stack-xs' },
        h('div', { class: 'row wrap' },
          ui.button({ label: logoFile || (ch?.logo_url && !removeLogo) ? 'Replace…' : 'Upload…', icon: 'upload', size: 'sm', onClick: () => logoInput.click() }),
          (logoFile || (ch?.logo_url && !removeLogo)) ? ui.button({ label: 'Remove', icon: 'trash', size: 'sm', variant: 'ghost', onClick: () => {
            if (logoFile) { URL.revokeObjectURL(previewUrl); logoFile = null; previewUrl = ''; } else removeLogo = true;
            renderLogo();
          } }) : null),
        h('div', { class: 'field-hint' }, 'PNG, JPEG or GIF up to 20 MiB; resized to 512 px.')));
  }
  logoInput.addEventListener('change', () => {
    const f = logoInput.files?.[0];
    logoInput.value = '';
    if (!f) return;
    if (f.size > 20 * 1024 * 1024) {
      ui.toast.error('That image is larger than 20 MiB.');
      return;
    }
    if (previewUrl) URL.revokeObjectURL(previewUrl);
    logoFile = f;
    removeLogo = false;
    previewUrl = URL.createObjectURL(f);
    renderLogo();
  });
  renderLogo();

  const shuffleRow = h('label', { class: 'switch tv-switch-row' }, shuffle, h('span', { class: 'switch-track', aria: { hidden: 'true' } }),
    h('span', {}, 'Shuffle the lineup'));
  const body = h('div', { class: 'stack-md tv-channel-form' },
    h('div', { class: 'tv-form-2' }, field('Number', number, { name: 'number', hint: 'Leave empty to pick the next free number.' }), field('Name', name, { name: 'name' })),
    field('Lineup', lineup, { name: 'lineup_id' }),
    h('div', { class: 'tv-form-2' }, field('Group', h('div', {}, group, groupDl), { name: 'group' }), h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Logo'), logoPreview, logoInput)),
    field('Description', desc, { name: 'description' }),
    h('div', { class: 'field' }, shuffleRow, modeRow),
    h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Playback'), playback.el,
      h('div', { class: 'field-hint' }, 'Live: everyone sees the same thing at the same time. On demand: each viewer resumes where they left off.')),
    h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Visibility'), visibility.el,
      h('div', { class: 'field-hint' }, 'Shared channels can be watched by every user on this server — even without access to your files.')));

  const m = ui.modal({
    title: ch ? `Edit channel ${ch.number}` : 'New channel', size: 'md', body,
    footer: [{ label: 'Cancel', value: null }, { label: ch ? 'Save' : 'Create channel', variant: 'primary', type: 'submit' }],
    onSubmit: async () => {
      const payload = {
        name: name.value, group: group.value, description: desc.value, lineup_id: lineup.value,
        shuffle: shuffle.checked, shuffle_mode: shuffleMode.value, playback_mode: playback.value, visibility: visibility.value,
      };
      if (number.value !== '') payload.number = Number(number.value);
      try {
        let saved = ch ? await api.updateChannel(ch.id, payload) : await api.createChannel(payload);
        if (logoFile) saved = await api.uploadLogo(saved.id, logoFile);
        else if (removeLogo && ch?.logo_url) {
          await api.deleteLogo(saved.id);
          saved = { ...saved, logo_url: null };
        }
        return saved;
      } catch (err) {
        showFormError(body, err);
        return false;
      }
    },
  });
  const result = await m.closed;
  if (previewUrl) URL.revokeObjectURL(previewUrl);
  return result || null;
}

/** Auto-generate dialog; resolves with the created channels (or null). */
export function openAutoGenerate({ api, ui }) {
  let strategy = 'per_show';
  const cards = h('div', { class: 'tv-strategies', role: 'radiogroup', aria: { label: 'What to create' } });
  const renderCards = () => cards.replaceChildren(...STRATEGIES.map((s) => {
    const b = h('button', { type: 'button', role: 'radio', class: cls('tv-strategy', { 'is-active': s.value === strategy }), aria: { checked: String(s.value === strategy) }, tabindex: s.value === strategy ? '0' : '-1' },
      h('span', { class: 'tv-strategy-title bold' }, s.title), h('span', { class: 'small muted' }, s.desc));
    b.addEventListener('click', () => { strategy = s.value; renderCards(); cards.querySelector('[aria-checked="true"]')?.focus(); });
    return b;
  }));
  cards.addEventListener('keydown', (e) => {
    const i = STRATEGIES.findIndex((s) => s.value === strategy);
    const d = e.key === 'ArrowDown' || e.key === 'ArrowRight' ? 1 : e.key === 'ArrowUp' || e.key === 'ArrowLeft' ? -1 : 0;
    if (!d) return;
    e.preventDefault();
    strategy = STRATEGIES[(i + d + STRATEGIES.length) % STRATEGIES.length].value;
    renderCards();
    cards.querySelector('[aria-checked="true"]')?.focus();
  });
  renderCards();
  const shuffle = h('input', { type: 'checkbox', class: 'switch-input', role: 'switch', checked: true });
  const min = h('input', { class: 'input nums', type: 'number', min: '1', max: '50000', value: '3', inputmode: 'numeric' });
  min.id = uid('min');
  const banner = h('div');
  const m = ui.modal({
    title: 'Create channels automatically', size: 'md',
    description: 'Builds a lineup and a channel for each match in your library. Existing channels with the same name are kept.',
    body: h('div', { class: 'stack-md' }, banner, cards,
      h('div', { class: 'tv-form-2' },
        h('div', { class: 'field' }, h('label', { class: 'switch tv-switch-row' }, shuffle, h('span', { class: 'switch-track', aria: { hidden: 'true' } }), h('span', {}, 'Shuffle the new channels'))),
        h('div', { class: 'field' }, h('label', { class: 'field-label', for: min.id }, 'Minimum programs'), min, h('div', { class: 'field-hint' }, 'Skip shows or genres with fewer playable files.')))),
    footer: [{ label: 'Cancel', value: null }, { label: 'Create channels', icon: 'sparkle', variant: 'primary', type: 'submit' }],
    onSubmit: async () => {
      try {
        const r = await api.autoGenerate({ strategy, shuffle: shuffle.checked, min_items: Math.max(1, Number(min.value) || 1) });
        return r.created;
      } catch (err) {
        banner.replaceChildren(ui.alert({ variant: 'danger', message: ui.errorMessage(err) }));
        return false;
      }
    },
  });
  return m.closed.then((v) => (Array.isArray(v) ? v : null));
}

export async function render(host, vctx) {
  const { ctx, api, ui, signal, navigate } = vctx;
  const me = ctx.user?.id;
  let channels = [];
  const mineHost = h('div');
  const sharedHost = h('section', { class: 'stack-sm', hidden: true });
  const page = h('div', { class: 'page wide tv-channels' },
    ui.pageHeader({
      title: 'Channels',
      subtitle: 'Each channel plays a lineup on a loop, like broadcast TV.',
      actions: [
        ui.button({ label: 'Auto-generate', icon: 'sparkle', onClick: () => autoGen() }),
        ui.button({ label: 'New channel', icon: 'plus', variant: 'primary', onClick: () => edit(null) }),
      ],
    }),
    mineHost, sharedHost);
  host.append(page);

  const modeBadges = (c) => h('span', { class: 'row wrap tv-badges' },
    c.playback_mode === 'on_demand' ? ui.badge('On demand', 'info') : ui.badge('Live', 'success', { dot: true }),
    c.shuffle ? ui.badge(c.shuffle_mode === 'fixed' ? 'Shuffled (fixed)' : 'Shuffled', 'accent', { icon: 'shuffle' }) : null,
    c.visibility === 'shared' ? ui.badge('Shared', 'neutral', { icon: 'users' }) : null);
  const chanCell = (c) => h('div', { class: 'row tv-chan-cell' }, channelLogo(c, { size: 'sm' }),
    h('span', { class: 'tv-chan-num nums' }, String(c.number)),
    h('div', { class: 'cell-stack' }, h('a', { class: 'cell-title', href: `/tv/watch/${c.id}` }, c.name),
      c.description ? h('span', { class: 'cell-sub truncate' }, c.description) : null));

  const mine = ui.dataTable({
    columns: [
      { key: 'number', label: 'Channel', sortable: true, render: chanCell },
      { key: 'group', label: 'Group', sortable: true, hideBelow: 'sm' },
      { key: 'lineup_name', label: 'Lineup', sortable: true, hideBelow: 'md', render: (c) => h('a', { href: `/tv/lineups/${c.lineup_id}` }, c.lineup_name) },
      { key: 'mode', label: 'Mode', hideBelow: 'sm', render: modeBadges },
      { key: 'actions', label: '', align: 'right', render: (c) => h('div', { class: 'cell-actions' },
        ui.button({ icon: 'play', title: 'Watch', size: 'sm', variant: 'ghost', href: `/tv/watch/${c.id}` }),
        ui.menuButton({
          icon: 'more', title: `Actions for ${c.name}`, size: 'sm',
          items: () => [
            { label: 'Edit…', icon: 'edit', onSelect: () => edit(c) },
            { label: 'Reshuffle', icon: 'shuffle', disabled: !c.shuffle, onSelect: () => act(() => api.reshuffle(c.id), 'Reshuffled — a new order starts now') },
            { label: 'Restart from the top', icon: 'refresh', onSelect: () => act(() => api.restart(c.id), 'Restarted — the lineup starts from the beginning') },
            { label: 'Edit lineup', icon: 'list', onSelect: () => navigate(`/tv/lineups/${c.lineup_id}`) },
            { separator: true },
            { label: 'Delete…', icon: 'trash', danger: true, onSelect: () => del(c) },
          ],
        })) },
    ],
    rowKey: 'id', sort: { key: 'number', dir: 'asc' }, caption: 'Your channels',
    onRowClick: (c) => edit(c),
    empty: () => ui.emptyState({
      icon: 'radio', title: 'No channels yet',
      message: 'Create one from a lineup, or let Ludworks build channels from your shows and genres.',
      action: h('div', { class: 'row wrap' },
        ui.button({ label: 'Auto-generate', icon: 'sparkle', variant: 'primary', onClick: () => autoGen() }),
        ui.button({ label: 'New channel', icon: 'plus', onClick: () => edit(null) })),
    }),
  });
  const shared = ui.dataTable({
    columns: [
      { key: 'number', label: 'Channel', sortable: true, render: chanCell },
      { key: 'group', label: 'Group', sortable: true, hideBelow: 'sm' },
      { key: 'mode', label: 'Mode', hideBelow: 'sm', render: modeBadges },
      { key: 'actions', label: '', align: 'right', render: (c) => ui.button({ label: 'Watch', icon: 'play', size: 'sm', href: `/tv/watch/${c.id}` }) },
    ],
    rowKey: 'id', sort: { key: 'number', dir: 'asc' }, caption: 'Channels shared with you',
    onRowClick: (c) => navigate(`/tv/watch/${c.id}`),
  });
  mineHost.append(mine.el);
  sharedHost.append(h('h2', { class: 'section-title' }, 'Shared with you'), h('p', { class: 'small muted' }, 'Channels other users share with everyone on this server.'), shared.el);
  mine.setLoading(true);

  async function load() {
    try {
      channels = sortChannels(await api.channels(signal));
      mine.setRows(channels.filter((c) => c.owner_id === me));
      const others = channels.filter((c) => c.owner_id !== me);
      shared.setRows(others);
      sharedHost.hidden = !others.length;
    } catch (err) {
      if (!signal.aborted) mineHost.replaceChildren(ui.errorState(err, () => { mineHost.replaceChildren(mine.el); load(); }));
    }
  }

  async function edit(c) {
    const groups = [...new Set(channels.map((x) => x.group).filter(Boolean))].sort();
    const saved = await openChannelDialog(vctx, { channel: c, groups });
    if (saved) {
      ui.toast.success(c ? 'Channel saved' : `Channel ${saved.number} created`, c ? {} : { action: { label: 'Watch', onClick: () => navigate(`/tv/watch/${saved.id}`) } });
      load();
    }
  }

  async function act(fn, msg) {
    try {
      await fn();
      ui.toast.success(msg);
      load();
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function del(c) {
    const ok = await ui.confirm({ title: `Delete channel ${c.number} “${c.name}”?`, message: 'The channel and its logo are deleted. Its lineup and your media are kept.', confirmLabel: 'Delete channel', danger: true });
    if (ok) act(() => api.deleteChannel(c.id), 'Channel deleted');
  }

  async function autoGen() {
    const created = await openAutoGenerate(vctx);
    if (!created) return;
    if (created.length) ui.toast.success(`Created ${created.length} channel${created.length === 1 ? '' : 's'}`);
    else ui.toast.info('Nothing new to create', { description: 'No matching shows or genres without a channel (check the minimum and that media durations are known).' });
    load();
  }

  await load();
  if (ctx.query?.auto === '1') {
    ctx.router.setQuery?.({ auto: null });
    autoGen();
  }
  return null;
}
