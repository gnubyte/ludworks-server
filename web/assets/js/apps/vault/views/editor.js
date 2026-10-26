// Item editor for all four types: multiple URIs with match modes, custom fields (hidden or
// visible), password generator, TOTP secret / otpauth URI with a live preview, card and identity
// data. Unknown keys of the decrypted item are preserved (VAULT-CRYPTO.md §3).

import { h, debounce } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import { field, input, textarea, select, passwordInput, checkbox, clearErrors, showFieldErrors } from '../../../core/forms.js';
import * as ui from '../../../core/ui.js';
import { TYPES, TYPE_INFO, MATCH_MODES, CARD_BRANDS, IDENTITY_FIELDS, blankItem, finalizeItem, totpError, detectCardBrand, str } from '../model.js';
import { ConflictError } from '../session.js';
import { vicon, strengthBar, totpWidget } from '../widgets.js';
import { openGeneratorDialog } from './generator.js';

const isObj = (v) => v !== null && typeof v === 'object' && !Array.isArray(v);
const NAME_PLACEHOLDER = { login: 'e.g. GitHub', note: 'e.g. Wi-Fi details', card: 'e.g. Personal Visa', identity: 'e.g. Passport details' };

/**
 * Open the editor. Resolves with the saved record, or null when cancelled.
 * @param {{vault: object, ctx: object, rec?: object, type?: string, folderId?: string|null, template?: object}} o
 */
export function openEditor({ vault, ctx, rec = null, type = 'login', folderId = null, template = null }) {
  const { session } = vault;
  const original = rec?.plain || null;
  let base = structuredClone(original || template || blankItem(type));
  if (!TYPES.includes(base.type)) base.type = 'note';
  let curType = base.type;
  const creating = !rec;
  const stops = [];

  /* ------------------------------------------------------------- common */
  const name = input({ name: 'name', value: str(base.name), maxlength: '500', autocomplete: 'off', placeholder: NAME_PLACEHOLDER[curType] || '' });
  const folders = [...session.folders.values()].filter((f) => !f.error).sort((a, b) => a.path.localeCompare(b.path));
  const folderSel = select({ name: 'folder', options: [{ value: '', label: 'No folder' }, ...folders.map((f) => ({ value: f.id, label: f.path }))] });
  folderSel.value = (rec ? rec.folder_id : folderId) || '';
  const fav = checkbox({ label: 'Favorite', checked: base.favorite === true });
  const notes = textarea({ name: 'notes', rows: 4, value: str(base.notes), spellcheck: 'true' });

  /* -------------------------------------------------------------- login */
  const login = isObj(base.login) ? base.login : {};
  const username = input({ name: 'username', value: str(login.username), autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false' });
  const pw = passwordInput({ name: 'password', value: str(login.password), autocomplete: 'new-password' });
  const meter = strengthBar();
  pw.inputEl.addEventListener('input', () => meter.update(pw.inputEl.value));
  meter.update(pw.inputEl.value);
  const genBtn = ui.button({ icon: 'dice', title: 'Generate a password', variant: 'secondary' });
  genBtn.replaceChildren(vicon('dice', 16));
  genBtn.addEventListener('click', async () => {
    const v = await openGeneratorDialog({ vault });
    if (v) {
      pw.inputEl.value = v;
      pw.inputEl.type = 'text';
      pw.inputEl.dispatchEvent(new Event('input'));
    }
  });
  const totp = input({ name: 'totp', className: 'mono', value: str(login.totp), autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', placeholder: 'otpauth://totp/… or base32 secret' });
  const totpPreview = h('div', { class: 'vault-totp-preview' });
  let totpStop = null;
  const updateTotpPreview = () => {
    totpStop?.();
    totpStop = null;
    const val = totp.value.trim();
    if (!val || totpError(val)) {
      totpPreview.replaceChildren();
      return;
    }
    const w = totpWidget(vault, val);
    totpStop = w.stop;
    totpPreview.replaceChildren(w.el);
  };
  totp.addEventListener('change', updateTotpPreview);
  totp.addEventListener('input', debounce(updateTotpPreview, 300));
  stops.push(() => totpStop?.());

  const uriRows = h('div', { class: 'vault-rows' });
  const addUri = (u = { uri: '', match: '' }) => {
    const uriIn = input({ value: str(u.uri), placeholder: 'https://example.com/login', autocomplete: 'off', spellcheck: 'false', aria: { label: 'URI' } });
    const matchSel = select({ options: MATCH_MODES, value: MATCH_MODES.some((m) => m.value === str(u.match)) ? str(u.match) : '', aria: { label: 'Match detection' } });
    const rm = ui.button({ icon: 'x', title: 'Remove URI', variant: 'ghost', size: 'sm' });
    const row = h('div', { class: 'vault-uri-row' }, uriIn, matchSel, rm);
    row.collect = () => ({ uri: uriIn.value.trim(), match: matchSel.value || null });
    row.extra = isObj(u) ? u : {};
    rm.addEventListener('click', () => {
      row.remove();
      (uriRows.querySelector('input') || addUriBtn).focus();
    });
    uriRows.append(row);
    return uriIn;
  };
  const addUriBtn = ui.button({ label: 'Add URI', icon: 'plus', variant: 'ghost', size: 'sm', onClick: () => addUri().focus() });
  const uris = Array.isArray(login.uris) ? login.uris.filter(isObj) : [];
  if (uris.length) uris.forEach((u) => addUri(u));
  else addUri();

  const loginSection = h('div', { class: 'stack' },
    field({ label: 'Username', control: username, name: 'username' }),
    h('div', { class: 'stack-sm' },
      field({ label: 'Password', control: h('div', { class: 'vault-pw-row' }, pw, genBtn), name: 'password' }), meter.el),
    h('div', { class: 'field', dataset: { field: 'uris' } }, h('span', { class: 'field-label' }, 'Websites'), uriRows, h('div', {}, addUriBtn),
      h('div', { class: 'field-error', role: 'alert', hidden: true })),
    h('div', { class: 'stack-sm' }, field({ label: 'Authenticator key (TOTP)', control: totp, name: 'totp', hint: 'Paste the setup key or the otpauth:// link shown when enabling two-factor authentication.' }), totpPreview));

  /* --------------------------------------------------------------- card */
  const cardData = isObj(base.card) ? base.card : {};
  const cardholder = input({ value: str(cardData.cardholder), autocomplete: 'off' });
  const number = input({ className: 'mono', value: str(cardData.number), inputmode: 'numeric', autocomplete: 'off' });
  const brandSel = select({ options: [{ value: '', label: '—' }, ...CARD_BRANDS.map((b) => ({ value: b, label: b }))] });
  brandSel.value = CARD_BRANDS.includes(str(cardData.brand)) ? str(cardData.brand) : (str(cardData.brand) ? 'Other' : '');
  const customBrand = str(cardData.brand) && !CARD_BRANDS.includes(str(cardData.brand)) ? str(cardData.brand) : '';
  number.addEventListener('input', () => {
    const b = detectCardBrand(number.value);
    if (b && (!brandSel.value || brandSel.dataset.auto === '1')) {
      brandSel.value = b;
      brandSel.dataset.auto = '1';
    }
  });
  brandSel.addEventListener('change', () => { brandSel.dataset.auto = ''; });
  const expMonth = select({ options: [{ value: '', label: 'Month' }, ...Array.from({ length: 12 }, (_, i) => ({ value: String(i + 1).padStart(2, '0'), label: String(i + 1).padStart(2, '0') }))] });
  const em = str(cardData.exp_month).trim();
  expMonth.value = /^\d{1,2}$/.test(em) && Number(em) >= 1 && Number(em) <= 12 ? em.padStart(2, '0') : '';
  const expYear = input({ value: str(cardData.exp_year), inputmode: 'numeric', maxlength: '4', placeholder: 'YYYY', autocomplete: 'off' });
  const code = passwordInput({ name: 'code', value: str(cardData.code), autocomplete: 'off' });
  const cardSection = h('div', { class: 'form-grid cols-2' },
    field({ label: 'Cardholder name', control: cardholder, name: 'cardholder' }),
    field({ label: 'Brand', control: brandSel, name: 'brand' }),
    field({ label: 'Number', control: number, name: 'number' }),
    field({ label: 'Security code', control: code, name: 'code' }),
    field({ label: 'Expiry month', control: expMonth, name: 'exp_month' }),
    field({ label: 'Expiry year', control: expYear, name: 'exp_year' }));

  /* ----------------------------------------------------------- identity */
  const idData = isObj(base.identity) ? base.identity : {};
  const idInputs = {};
  const idLabels = {};
  for (const [k, label, secret] of IDENTITY_FIELDS) {
    idInputs[k] = secret ? passwordInput({ name: k, value: str(idData[k]), autocomplete: 'off' }) : input({ name: k, value: str(idData[k]), autocomplete: 'off' });
    idLabels[k] = label;
  }
  const idField = (k, extraClass = '') => field({ label: idLabels[k], control: idInputs[k], name: k, className: extraClass });
  const identitySection = h('div', { class: 'form-grid cols-2' },
    idField('title'), idField('first_name'), idField('middle_name'), idField('last_name'),
    idField('company'), idField('username'), idField('email'), idField('phone'),
    idField('address1', 'span-2'), idField('address2', 'span-2'), idField('city'), idField('state'), idField('postal_code'), idField('country'),
    idField('ssn'), idField('passport'), idField('license'));

  /* -------------------------------------------------------- custom fields */
  const fieldRows = h('div', { class: 'vault-rows' });
  const addField = (f = { name: '', value: '', hidden: false }) => {
    const nameIn = input({ value: str(f.name), placeholder: 'Name', autocomplete: 'off', aria: { label: 'Field name' } });
    const valueIn = input({ value: str(f.value), placeholder: 'Value', autocomplete: 'off', type: f.hidden ? 'password' : 'text', aria: { label: 'Field value' } });
    const hide = ui.button({ icon: f.hidden ? 'eye-off' : 'eye', title: f.hidden ? 'Hidden field (click to make visible)' : 'Visible field (click to hide)', variant: 'ghost', size: 'sm' });
    let hidden = f.hidden === true;
    hide.setAttribute('aria-pressed', String(hidden));
    hide.addEventListener('click', () => {
      hidden = !hidden;
      valueIn.type = hidden ? 'password' : 'text';
      hide.replaceChildren(icon(hidden ? 'eye-off' : 'eye', 14));
      hide.setAttribute('aria-pressed', String(hidden));
      const t = hidden ? 'Hidden field (click to make visible)' : 'Visible field (click to hide)';
      hide.dataset.tip = t;
      hide.setAttribute('aria-label', t);
    });
    const rm = ui.button({ icon: 'x', title: 'Remove field', variant: 'ghost', size: 'sm' });
    const row = h('div', { class: 'vault-field-row' }, nameIn, valueIn, hide, rm);
    row.collect = () => ({ ...(isObj(f) ? f : {}), name: nameIn.value.trim(), value: valueIn.value, hidden });
    rm.addEventListener('click', () => {
      row.remove();
      addFieldBtn.focus();
    });
    fieldRows.append(row);
    return nameIn;
  };
  const addFieldBtn = ui.button({ label: 'Add field', icon: 'plus', variant: 'ghost', size: 'sm', onClick: () => addField().focus() });
  (Array.isArray(base.fields) ? base.fields : []).filter(isObj).forEach((f) => addField(f));

  /* -------------------------------------------------------------- layout */
  const typeHost = h('div');
  const sections = { login: loginSection, card: cardSection, identity: identitySection, note: null };
  const typeSeg = creating && !template ? ui.segmented({
    ariaLabel: 'Item type', value: curType,
    options: TYPES.map((t) => ({ value: t, label: TYPE_INFO[t].label })),
    onChange: (t) => {
      curType = t;
      showType();
      const title = m.el.querySelector('.modal-title');
      if (title) title.textContent = `New ${TYPE_INFO[t].label.toLowerCase()}`;
    },
  }) : null;
  function showType() {
    typeHost.replaceChildren(sections[curType] ? h('section', { class: 'vault-form-section' }, h('h3', { class: 'vault-form-title' }, TYPE_INFO[curType].label), sections[curType]) : h('span'));
    notesField.querySelector('.field-label').firstChild.textContent = curType === 'note' ? 'Note' : 'Notes';
    notes.rows = curType === 'note' ? 10 : 4;
    name.placeholder = NAME_PLACEHOLDER[curType] || '';
  }
  const notesField = field({ label: 'Notes', control: notes, name: 'notes' });

  const body = h('div', { class: 'stack-lg vault-editor' },
    typeSeg ? h('div', { class: 'vault-type-seg' }, typeSeg.el) : null,
    h('div', { class: 'form-grid cols-2' },
      field({ label: 'Name', control: name, name: 'name', required: true, className: 'span-2' }),
      field({ label: 'Folder', control: folderSel, name: 'folder' }),
      h('div', { class: 'vault-fav-field' }, fav)),
    typeHost,
    notesField,
    h('section', { class: 'vault-form-section' }, h('h3', { class: 'vault-form-title' }, 'Custom fields'), fieldRows, h('div', {}, addFieldBtn)));
  showType();

  /* -------------------------------------------------------------- collect */
  function collect() {
    const out = structuredClone(base);
    out.type = curType;
    out.name = name.value.trim();
    out.favorite = fav.inputEl.checked;
    out.notes = notes.value;
    out.fields = [...fieldRows.children].map((r) => r.collect()).filter((f) => f.name || f.value);
    if (!Array.isArray(out.password_history)) out.password_history = [];
    if (curType === 'login') {
      const prev = isObj(out.login) ? out.login : {};
      out.login = { ...prev, username: username.value.trim(), password: pw.inputEl.value, totp: totp.value.trim(),
        uris: [...uriRows.children].map((r) => ({ ...r.extra, ...r.collect() })).filter((u) => u.uri).map((u) => {
          const x = { ...u };
          if (!x.match) delete x.match;
          return x;
        }) };
    }
    if (curType === 'card') {
      const prev = isObj(out.card) ? out.card : {};
      out.card = { ...prev, cardholder: cardholder.value.trim(), brand: brandSel.value === 'Other' ? (customBrand || 'Other') : brandSel.value,
        number: number.value.replace(/\s+/g, ''), exp_month: expMonth.value, exp_year: expYear.value.trim(), code: code.inputEl.value };
    }
    if (curType === 'identity') {
      const prev = isObj(out.identity) ? out.identity : {};
      out.identity = { ...prev };
      for (const [k] of IDENTITY_FIELDS) out.identity[k] = (idInputs[k].inputEl || idInputs[k]).value.trim();
    }
    // A new item carries only the sub-object of its type (§3).
    if (creating && !template) for (const t of ['login', 'card', 'identity']) if (t !== curType) delete out[t];
    return out;
  }
  const snapshot = () => JSON.stringify({ p: collect(), f: folderSel.value });
  let initial = '';

  function validate(p) {
    clearErrors(body);
    const errs = {};
    if (!p.name) errs.name = 'Give the item a name.';
    if (p.type === 'login') {
      const t = totpError(p.login.totp);
      if (t) errs.totp = t;
    }
    if (p.type === 'card' && p.card.exp_year && !/^\d{2}(\d{2})?$/.test(p.card.exp_year)) errs.exp_year = 'Use YY or YYYY.';
    if (Object.keys(errs).length) {
      showFieldErrors(body, errs);
      return false;
    }
    return true;
  }

  let result = null;
  const m = ui.modal({
    title: creating ? `New ${TYPE_INFO[curType].label.toLowerCase()}` : `Edit ${TYPE_INFO[curType].label.toLowerCase()}`,
    size: 'lg', dismissible: false, className: 'vault-editor-modal', body,
    footer: [
      { label: 'Cancel', onClick: () => tryClose() },
      { label: creating ? 'Save item' : 'Save changes', variant: 'primary', type: 'submit', icon: 'check' },
    ],
    onSubmit: async () => {
      const p = collect();
      if (!validate(p)) return false;
      const final = finalizeItem(original, p);
      const fid = folderSel.value || null;
      try {
        result = await session.saveItem({ plain: final, folderId: fid, original: rec });
      } catch (err) {
        if (err instanceof ConflictError) return resolveConflict(err, final, fid);
        if (err?.status === 404 && rec) {
          ui.toast.error('This item was deleted on another device.');
          return null;
        }
        ui.toast.error(err);
        return false;
      }
      ui.toast.success(creating ? 'Item saved' : 'Changes saved');
      return result;
    },
  });
  initial = snapshot();
  requestAnimationFrame(() => name.focus());

  async function tryClose() {
    if (snapshot() !== initial) {
      const ok = await ui.confirm({ title: 'Discard changes?', message: 'Your changes to this item have not been saved.', confirmLabel: 'Discard', danger: true });
      if (!ok) return;
    }
    m.close(null);
  }
  m.dialog.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !e.defaultPrevented && !document.querySelector('.menu')) {
      e.preventDefault();
      tryClose();
    }
  });

  async function resolveConflict(err, mine, fid) {
    const theirs = err.current;
    const choice = await new Promise((resolve) => {
      const cm = ui.modal({
        title: 'Changed on another device', size: 'sm',
        body: h('div', { class: 'stack-sm' },
          h('p', {}, 'This item was saved somewhere else while you were editing it.'),
          theirs?.view ? h('p', { class: 'muted small' }, `The other version is “${theirs.view.name}”, updated ${new Date(theirs.updated_at).toLocaleString()}.`) : null),
        footer: [
          { label: 'Keep theirs', value: 'theirs' },
          { label: 'Overwrite with mine', variant: 'primary', value: 'mine' },
        ],
        onClose: (v) => resolve(v || 'cancel'),
      });
      void cm;
    });
    if (choice === 'mine' && theirs?.plain) {
      try {
        result = await session.saveItem({ plain: finalizeItem(theirs.plain, mine), folderId: fid, original: theirs });
        ui.toast.success('Changes saved');
        return result;
      } catch (e2) {
        ui.toast.error(e2 instanceof ConflictError ? 'It changed again — please reopen the item.' : e2);
        return null;
      }
    }
    if (choice === 'theirs') return theirs?.plain ? theirs : null;
    return false;
  }

  return m.closed.then((v) => {
    stops.forEach((fn) => fn());
    return v && typeof v === 'object' ? v : null;
  });
}

