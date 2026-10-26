// Password / passphrase generator (VAULT-CRYPTO.md §5): CSPRNG with rejection sampling from the
// vault library, strength estimate, copy, and a history kept in memory only (cleared on lock).
// The chosen options (not the passwords) are remembered in the non-sensitive prefs.

import { h, uid } from '../../../core/dom.js';
import * as ui from '../../../core/ui.js';
import { generatePassword, generatePassphrase } from '../../../lib/vault/generator.js';
import { strengthBar, copySecret, vicon } from '../widgets.js';

const HISTORY_MAX = 20;

/** Generate a value for the given options (throws on impossible option sets). */
export function generateFrom(g) {
  return g.mode === 'passphrase'
    ? generatePassphrase({ words: g.words, separator: g.separator, capitalize: g.capitalize, includeNumber: g.includeNumber })
    : generatePassword({ length: g.length, upper: g.upper, lower: g.lower, digits: g.digits, symbols: g.symbols, excludeAmbiguous: g.excludeAmbiguous });
}

/**
 * The generator panel. Returns {el, value()}.
 * @param {{vault: object, compact?: boolean, onUse?: (v: string) => void}} o
 */
export function generatorPanel({ vault, compact = false, onUse = null }) {
  let g = { ...vault.prefs.gen };
  let value = '';
  const out = h('output', { class: 'vault-gen-output mono', aria: { live: 'polite', label: 'Generated password' } });
  const meter = strengthBar();
  const historyList = h('ol', { class: 'vault-gen-history' });

  const regen = ui.button({ icon: 'refresh', title: 'Generate another', variant: 'ghost' });
  const copy = ui.button({ icon: 'copy', title: 'Copy', variant: 'ghost' });
  regen.addEventListener('click', () => generate(true));
  copy.addEventListener('click', () => copySecret(vault, value, 'Password'));

  const mode = ui.segmented({
    ariaLabel: 'Generator type', value: g.mode, size: 'sm',
    options: [{ value: 'password', label: 'Password' }, { value: 'passphrase', label: 'Passphrase' }],
    onChange: (v) => set({ mode: v }),
  });

  // Password options
  const lenId = uid('len');
  const len = h('input', { type: 'range', min: '8', max: '128', value: String(g.length), id: lenId, class: 'vault-range' });
  const lenNum = h('input', { type: 'number', min: '8', max: '128', value: String(g.length), class: 'input input-sm vault-num', aria: { label: 'Length' } });
  len.addEventListener('input', () => { lenNum.value = len.value; set({ length: Number(len.value) }); });
  lenNum.addEventListener('change', () => {
    const n = Math.max(8, Math.min(128, Math.round(Number(lenNum.value) || 20)));
    lenNum.value = String(n);
    len.value = String(n);
    set({ length: n });
  });
  const toggle = (key, label, hint) => {
    const input = h('input', { type: 'checkbox', class: 'checkbox', checked: !!g[key] });
    input.addEventListener('change', () => {
      const next = { [key]: input.checked };
      const sets = { ...g, ...next };
      if (!sets.upper && !sets.lower && !sets.digits && !sets.symbols) {
        input.checked = true;
        ui.toast.warning('Keep at least one character set.');
        return;
      }
      set(next);
    });
    return h('label', { class: 'check-row' }, input, h('span', { class: 'check-text' }, h('span', { class: 'check-label' }, label), hint ? h('span', { class: 'check-hint mono' }, hint) : null));
  };
  const pwOpts = h('div', { class: 'stack-md' },
    h('div', { class: 'field' }, h('label', { class: 'field-label', for: lenId }, 'Length'), h('div', { class: 'row gap-3' }, len, lenNum)),
    h('div', { class: 'vault-gen-checks' },
      toggle('upper', 'Uppercase', 'A-Z'), toggle('lower', 'Lowercase', 'a-z'), toggle('digits', 'Digits', '0-9'),
      toggle('symbols', 'Symbols', '!@#$%^&*'), toggle('excludeAmbiguous', 'Avoid ambiguous characters', 'I l 1 O 0')));

  // Passphrase options
  const wordsId = uid('words');
  const words = h('input', { type: 'range', min: '3', max: '12', value: String(g.words), id: wordsId, class: 'vault-range' });
  const wordsOut = h('span', { class: 'vault-num-label' }, String(g.words));
  words.addEventListener('input', () => { wordsOut.textContent = words.value; set({ words: Number(words.value) }); });
  const sepSel = h('select', { class: 'select', aria: { label: 'Separator' } },
    [['-', 'Hyphen (-)'], [' ', 'Space'], ['.', 'Period (.)'], ['_', 'Underscore (_)'], [',', 'Comma (,)'], ['', 'None']].map(([v, l]) => h('option', { value: v }, l)));
  sepSel.value = g.separator;
  sepSel.addEventListener('change', () => set({ separator: sepSel.value }));
  const ppOpts = h('div', { class: 'stack-md' },
    h('div', { class: 'field' }, h('label', { class: 'field-label', for: wordsId }, 'Words'), h('div', { class: 'row gap-3' }, words, wordsOut)),
    h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Separator'), sepSel),
    h('div', { class: 'vault-gen-checks' }, toggle('capitalize', 'Capitalize words'), toggle('includeNumber', 'Include a number')));

  const useBtn = onUse ? ui.button({ label: 'Use this password', icon: 'check', variant: 'primary', onClick: () => onUse(value) }) : null;

  const el = h('div', { class: compact ? 'vault-gen compact' : 'vault-gen' },
    h('div', { class: 'vault-gen-result' }, out, h('div', { class: 'vault-gen-result-actions' }, regen, copy)),
    meter.el,
    h('div', { class: 'vault-gen-mode' }, mode.el),
    pwOpts, ppOpts,
    useBtn ? h('div', { class: 'row end' }, useBtn) : null,
    compact ? null : h('section', { class: 'vault-gen-history-wrap' },
      h('div', { class: 'vault-section-title' }, h('span', {}, 'Recently generated'), h('span', { class: 'muted xs' }, 'Kept in memory only · cleared when the vault locks')),
      historyList));

  function renderHistory() {
    const items = vault.ui.genHistory;
    historyList.replaceChildren(...(items.length ? items.map((p) => h('li', { class: 'vault-gen-history-item' },
      h('span', { class: 'mono truncate' }, p.value), h('time', { class: 'muted xs' }, new Date(p.at).toLocaleTimeString()),
      (() => { const b = ui.button({ icon: 'copy', title: 'Copy', size: 'sm', variant: 'ghost' }); b.addEventListener('click', () => copySecret(vault, p.value, 'Password')); return b; })()))
      : [h('li', { class: 'muted small vault-gen-history-empty' }, 'Nothing yet.')]));
  }

  function set(patch) {
    g = { ...g, ...patch };
    vault.setPrefs({ gen: patch });
    generate(false);
  }

  function generate(record) {
    pwOpts.hidden = g.mode !== 'password';
    ppOpts.hidden = g.mode !== 'passphrase';
    try {
      value = generateFrom(g);
    } catch (err) {
      value = '';
      ui.toast.error(err.message);
    }
    out.replaceChildren(...[...value].map((ch) => h('span', { class: /\d/.test(ch) ? 'gc-digit' : /[A-Za-z]/.test(ch) ? 'gc-letter' : 'gc-symbol' }, ch)));
    meter.update(value);
    if (record !== false && !compact) remember();
  }

  function remember() {
    if (!value) return;
    const hist = vault.ui.genHistory;
    hist.unshift({ value, at: Date.now() });
    hist.length = Math.min(hist.length, HISTORY_MAX);
    renderHistory();
  }

  // Copying or using a value records it in the history too.
  copy.addEventListener('click', () => { if (compact) remember(); });

  generate(true);
  renderHistory();
  return { el, value: () => value };
}

/** Modal generator (from the item editor). Resolves with the chosen password or null. */
export function openGeneratorDialog({ vault }) {
  let chosen = null;
  const m = ui.modal({ title: 'Generate password', size: 'md', className: 'vault-gen-modal', body: h('div') });
  const panel = generatorPanel({ vault, compact: true, onUse: (v) => { chosen = v; m.close(v); } });
  m.body.replaceChildren(panel.el);
  return m.closed.then(() => {
    if (chosen) vault.ui.genHistory.unshift({ value: chosen, at: Date.now() });
    vault.ui.genHistory.length = Math.min(vault.ui.genHistory.length, HISTORY_MAX);
    return chosen;
  });
}

/** Tool page. */
export function render(host, { vault, ctx }) {
  ctx.setTitle('Generator · Passwords');
  const panel = generatorPanel({ vault });
  host.replaceChildren(h('div', { class: 'page narrow vault-page' },
    ui.pageHeader({ title: 'Password generator', subtitle: 'Random passwords and passphrases from your browser’s cryptographic random number generator.' }),
    h('div', { class: 'card' }, h('div', { class: 'card-body' }, panel.el)),
    h('p', { class: 'muted small vault-page-foot' }, vicon('shield-check', 14), ' Generated values never leave this device unless you save them in an item.')));
  return () => {};
}
