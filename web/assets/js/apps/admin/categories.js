// Admin › Categories: edit the file category taxonomy (GET /categories,
// PUT /admin/categories) used to tag movies, TV, music, images, games, etc.

import { h } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { input, select, deepEqual } from '../../core/forms.js';

const SLUG = /^[a-z0-9][a-z0-9_-]{0,31}$/;

const ICON_CHOICES = ['film', 'tv', 'music', 'image', 'gamepad', 'file-text', 'box', 'layers', 'archive', 'book', 'code', 'folder', 'star', 'tag', 'radio', 'library'];

const sub = (pairs) => pairs.map(([slug, name]) => ({ slug, name }));

/** Default taxonomy (API.md §5). */
export const DEFAULT_CATEGORIES = [
  { slug: 'movie', name: 'Movies', icon: 'film', subcategories: sub([['feature', 'Feature'], ['short', 'Short'], ['documentary', 'Documentary'], ['animation', 'Animation'], ['concert', 'Concert'], ['trailer', 'Trailer'], ['other', 'Other']]) },
  { slug: 'tv', name: 'TV', icon: 'tv', subcategories: sub([['episode', 'Episode'], ['special', 'Special'], ['miniseries', 'Miniseries'], ['documentary', 'Documentary'], ['animation', 'Animation'], ['kids', 'Kids'], ['news', 'News'], ['other', 'Other']]) },
  { slug: 'music', name: 'Music', icon: 'music', subcategories: sub([['album', 'Album'], ['single', 'Single'], ['soundtrack', 'Soundtrack'], ['podcast', 'Podcast'], ['audiobook', 'Audiobook'], ['other', 'Other']]) },
  { slug: 'image', name: 'Images', icon: 'image', subcategories: sub([['photo', 'Photo'], ['screenshot', 'Screenshot'], ['artwork', 'Artwork'], ['wallpaper', 'Wallpaper'], ['scan', 'Scan'], ['other', 'Other']]) },
  { slug: 'game', name: 'Games', icon: 'gamepad', subcategories: sub([['rom', 'ROM'], ['iso', 'ISO'], ['pc', 'PC'], ['mod', 'Mod'], ['save', 'Save'], ['soundtrack', 'Soundtrack'], ['other', 'Other']]) },
  { slug: 'document', name: 'Documents', icon: 'file-text', subcategories: sub([['pdf', 'PDF'], ['ebook', 'E-book'], ['spreadsheet', 'Spreadsheet'], ['text', 'Text'], ['presentation', 'Presentation'], ['other', 'Other']]) },
  { slug: 'software', name: 'Software', icon: 'box', subcategories: sub([['installer', 'Installer'], ['archive', 'Archive'], ['source', 'Source'], ['other', 'Other']]) },
  { slug: 'other', name: 'Other', icon: 'layers', subcategories: sub([['other', 'Other']]) },
];

/** "Blu-ray Rips!" → "blu-ray-rips" */
export function slugify(name) {
  return String(name || '').toLowerCase().normalize('NFKD').replace(/[̀-ͯ]/g, '')
    .replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 32);
}

/**
 * Validate a taxonomy; returns a list of problems (empty when valid).
 * @param {Array<{slug: string, name: string, subcategories: Array<{slug: string, name: string}>}>} cats
 */
export function validateCategories(cats) {
  const problems = [];
  const seen = new Set();
  cats.forEach((c, i) => {
    const label = c.name || `Category ${i + 1}`;
    if (!SLUG.test(c.slug || '')) problems.push(`${label}: slug must be 1–32 lowercase letters, digits, - or _.`);
    if (seen.has(c.slug)) problems.push(`${label}: slug “${c.slug}” is used twice.`);
    seen.add(c.slug);
    if (!String(c.name || '').trim()) problems.push(`Category “${c.slug}” needs a name.`);
    const subSeen = new Set();
    for (const s of c.subcategories || []) {
      if (!SLUG.test(s.slug || '')) problems.push(`${label} › ${s.name || s.slug}: invalid slug.`);
      if (subSeen.has(s.slug)) problems.push(`${label}: subcategory “${s.slug}” is used twice.`);
      subSeen.add(s.slug);
      if (!String(s.name || '').trim()) problems.push(`${label} › ${s.slug}: needs a name.`);
    }
  });
  return problems;
}

const clone = (x) => JSON.parse(JSON.stringify(x));

export function render(host, ctx) {
  const { ui } = ctx;
  let original = [];
  let cats = [];
  const list = h('div', { class: 'cat-list' });
  const problemsHost = h('div');
  const saveBtn = ui.button({ label: 'Save taxonomy', variant: 'primary' });
  const discardBtn = ui.button({ label: 'Discard', variant: 'ghost' });
  const saveBar = h('div', { class: 'save-bar', hidden: true },
    h('div', { class: 'save-bar-msg' }, h('span', { class: 'save-bar-dot', aria: { hidden: 'true' } }), 'Unsaved changes to categories'),
    discardBtn, saveBtn);

  const dirty = () => !deepEqual(original, cats);
  const sync = () => {
    const d = dirty();
    saveBar.hidden = !d;
    ctx.router.setLeaveGuard(d ? () => ui.confirm({ title: 'Discard category changes?', confirmLabel: 'Discard', danger: true }) : null);
  };

  function renderList() {
    list.replaceChildren(...cats.map((c, i) => {
      const iconPreview = h('span', { class: 'cat-icon' }, icon(c.icon || 'tag', 18));
      const name = input({ value: c.name, 'aria-label': 'Category name', placeholder: 'Name', className: 'cat-name' });
      const slugEl = input({ value: c.slug, 'aria-label': 'Category slug', placeholder: 'slug', className: 'mono', spellcheck: 'false' });
      const iconSel = select({ options: ICON_CHOICES.map((x) => ({ value: x, label: x })), value: ICON_CHOICES.includes(c.icon) ? c.icon : 'tag', 'aria-label': 'Icon', className: 'cat-iconsel' });
      name.addEventListener('input', () => {
        const auto = !c.slug || c.slug === slugify(c.name);
        c.name = name.value;
        if (auto && c._new) {
          c.slug = slugify(name.value);
          slugEl.value = c.slug;
        }
        sync();
      });
      slugEl.addEventListener('input', () => { c.slug = slugEl.value.trim(); sync(); });
      iconSel.addEventListener('change', () => {
        c.icon = iconSel.value;
        iconPreview.replaceChildren(icon(c.icon, 18));
        sync();
      });

      const subs = h('div', { class: 'sub-list' }, (c.subcategories || []).map((s, j) => {
        const chip = h('span', { class: 'chip', dataset: { tip: `slug: ${s.slug}` } },
          h('button', { type: 'button', class: 'chip-label link-button', aria: { label: `Rename ${s.name}` } }, s.name));
        chip.firstChild.addEventListener('click', async () => {
          const v = await ui.prompt({ title: 'Rename subcategory', label: 'Name', value: s.name, hint: `Slug stays “${s.slug}” so existing tags keep working.` });
          if (v && v.trim()) {
            s.name = v.trim();
            renderList();
            sync();
          }
        });
        const x = h('button', { type: 'button', class: 'chip-x', aria: { label: `Remove ${s.name}` } }, icon('x', 12));
        x.addEventListener('click', () => {
          c.subcategories.splice(j, 1);
          renderList();
          sync();
        });
        chip.append(x);
        return chip;
      }));
      const subInput = input({ placeholder: 'Add subcategory', className: 'input-sm', 'aria-label': `Add subcategory to ${c.name}` });
      const addSub = () => {
        const nm = subInput.value.trim();
        if (!nm) return;
        const slug = slugify(nm);
        if (!slug) {
          ui.toast.warning('Use letters or numbers in the name.');
          return;
        }
        c.subcategories ??= [];
        if (c.subcategories.some((s) => s.slug === slug)) {
          ui.toast.warning(`“${nm}” already exists.`);
          return;
        }
        c.subcategories.push({ slug, name: nm });
        renderList();
        sync();
        list.querySelectorAll('.cat-card')[i]?.querySelector('.sub-add input')?.focus();
      };
      subInput.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') {
          e.preventDefault();
          addSub();
        }
      });

      const move = (d) => {
        const j = i + d;
        if (j < 0 || j >= cats.length) return;
        [cats[i], cats[j]] = [cats[j], cats[i]];
        renderList();
        sync();
      };
      return h('div', { class: 'card cat-card' },
        h('div', { class: 'cat-head' },
          iconPreview, name, slugEl, iconSel,
          h('div', { class: 'row' },
            ui.button({ icon: 'arrow-up', title: 'Move up', size: 'sm', variant: 'ghost', disabled: i === 0, onClick: () => move(-1) }),
            ui.button({ icon: 'arrow-down', title: 'Move down', size: 'sm', variant: 'ghost', disabled: i === cats.length - 1, onClick: () => move(1) }),
            ui.button({
              icon: 'trash', title: 'Remove category', size: 'sm', variant: 'ghost',
              onClick: async () => {
                const ok = await ui.confirm({ title: `Remove “${c.name || c.slug}”?`, message: 'Files already tagged with it keep the tag, but it disappears from pickers and filters.', confirmLabel: 'Remove', danger: true });
                if (!ok) return;
                cats.splice(i, 1);
                renderList();
                sync();
              },
            }))),
        h('div', { class: 'stack-sm' }, subs,
          h('div', { class: 'sub-add' }, subInput, ui.button({ label: 'Add', icon: 'plus', size: 'sm', onClick: addSub }))));
    }));
  }

  function setData(data) {
    original = clone(data).map((c) => ({ slug: c.slug, name: c.name, icon: c.icon || 'tag', subcategories: (c.subcategories || []).map((s) => ({ slug: s.slug, name: s.name })) }));
    cats = clone(original);
    problemsHost.replaceChildren();
    renderList();
    sync();
  }

  saveBtn.addEventListener('click', async () => {
    const problems = validateCategories(cats);
    problemsHost.replaceChildren();
    if (problems.length) {
      problemsHost.append(ui.alert({ variant: 'danger', title: 'Fix these before saving', message: h('ul', {}, problems.map((p) => h('li', {}, p))) }));
      problemsHost.scrollIntoView({ block: 'nearest' });
      return;
    }
    const payload = cats.map((c) => ({ slug: c.slug, name: c.name.trim(), icon: c.icon, subcategories: c.subcategories.map((s) => ({ slug: s.slug, name: s.name.trim() })) }));
    await ui.withLoading(saveBtn, async () => {
      try {
        const res = await ctx.api.put('/admin/categories', payload);
        setData(Array.isArray(res) ? res : payload);
        ui.toast.success('Categories saved');
      } catch (err) {
        problemsHost.append(ui.alert({ variant: 'danger', title: 'Couldn’t save', message: ui.errorMessage(err) }));
      }
    });
  });
  discardBtn.addEventListener('click', () => setData(original));

  const addCat = () => {
    cats.push({ slug: '', name: '', icon: 'tag', subcategories: [{ slug: 'other', name: 'Other' }], _new: true });
    renderList();
    sync();
    list.lastElementChild?.querySelector('.cat-name')?.focus();
  };
  const resetDefaults = async () => {
    const ok = await ui.confirm({ title: 'Restore the default categories?', message: 'Your custom categories are replaced in the editor. Nothing is saved until you click Save.', confirmLabel: 'Restore defaults' });
    if (!ok) return;
    cats = clone(DEFAULT_CATEGORIES);
    renderList();
    sync();
  };

  host.append(h('div', { class: 'stack' },
    h('div', { class: 'toolbar' },
      h('div', { class: 'stack-xs' }, h('h2', { class: 'section-title' }, 'Categories'), h('p', { class: 'section-desc' }, 'The kinds of files people can tag, each with subcategories. Slugs are stored on files — keep them stable.')),
      h('span', { class: 'spacer' }),
      ui.button({ label: 'Restore defaults', icon: 'restore', variant: 'ghost', onClick: resetDefaults }),
      ui.button({ label: 'New category', icon: 'plus', onClick: addCat })),
    problemsHost, list, saveBar));
  list.append(...ui.skeletonCards(3, '64px'));

  ctx.api.get('/categories', { signal: ctx.signal }).then((res) => setData(Array.isArray(res) ? res : DEFAULT_CATEGORIES)).catch((err) => {
    if (err?.name === 'AbortError') return;
    list.replaceChildren(ui.errorState(err, () => ctx.router.reload()));
  });
  return () => ctx.router.setLeaveGuard(null);
}
