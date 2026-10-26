// Appearance: theme preference (system / light / dark) stored per browser.

import { h, cls } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { getThemePref, setThemePref, onThemeChange } from '../../core/theme.js';

const OPTIONS = [
  { value: 'system', label: 'System', icon: 'monitor', desc: 'Follow your device setting' },
  { value: 'light', label: 'Light', icon: 'sun', desc: 'Bright and crisp' },
  { value: 'dark', label: 'Dark', icon: 'moon', desc: 'Easy on the eyes' },
];

export function render(host, ctx) {
  const { ui } = ctx;
  const cards = OPTIONS.map((o) => h('button', {
    type: 'button', role: 'radio', class: 'theme-card', dataset: { value: o.value },
    aria: { label: `${o.label} theme` },
  },
  h('div', { class: cls('theme-preview', o.value), aria: { hidden: 'true' } },
    h('div', { class: 'tp-side' }),
    h('div', { class: 'tp-main' }, h('div', { class: 'tp-line' }), h('div', { class: 'tp-line short' }), h('div', { class: 'tp-accent' }))),
  h('div', { class: 'theme-card-label' }, icon(o.icon, 14), o.label, h('span', { class: 'faint small' }, `· ${o.desc}`))));

  const group = h('div', { class: 'theme-cards', role: 'radiogroup', aria: { label: 'Theme' } }, cards);
  const sync = () => {
    const pref = getThemePref();
    for (const c of cards) {
      const on = c.dataset.value === pref;
      c.setAttribute('aria-checked', String(on));
      c.tabIndex = on ? 0 : -1;
    }
  };
  cards.forEach((c, i) => {
    c.addEventListener('click', () => {
      setThemePref(c.dataset.value);
      sync();
    });
    c.addEventListener('keydown', (e) => {
      const d = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
      if (!d) return;
      e.preventDefault();
      const n = cards[(i + d + cards.length) % cards.length];
      n.focus();
      n.click();
    });
  });
  sync();
  const off = onThemeChange(sync);

  host.append(
    h('div', { class: 'settings-section-header' },
      h('h2', { class: 'settings-section-title' }, 'Appearance'),
      h('p', { class: 'settings-section-desc' }, 'Saved in this browser only.')),
    ui.card({ title: 'Theme', icon: 'sun', body: group }),
    ui.card({
      title: 'Keyboard', icon: 'keyboard',
      body: h('div', { class: 'setting-rows' },
        h('div', { class: 'setting-row' },
          h('div', { class: 'setting-row-text' },
            h('span', { class: 'setting-label' }, 'Shortcuts'),
            h('span', { class: 'setting-desc' }, 'Press ? anywhere to see them, or Ctrl/Cmd+K to open the command palette.')),
          ui.button({ label: 'View shortcuts', icon: 'keyboard', size: 'sm', onClick: () => ctx.actions.showShortcuts() }))),
    }),
    ui.alert({ variant: 'info', message: 'Animations are reduced automatically when your operating system asks for reduced motion.' }));
  return off;
}
