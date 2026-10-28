// Schema-driven admin settings forms (GET/PATCH /admin/settings). Each tab edits
// one or more namespaces; only changed fields are sent; secrets are write-only
// ("set"/"not set", replace or clear); server validation errors land on fields.

import { h, cls, mount } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { field, input, select, switchInput, passwordInput, deepEqual, setFieldError, clearErrors, formBanner } from '../../core/forms.js';
import { formatDurationLong } from '../../core/format.js';
import { ensureStyle } from '../../core/style.js';

const MiB = 1024 ** 2;
const GiB = 1024 ** 3;
const TiB = 1024 ** 4;
const BYTE_UNITS = [{ value: String(MiB), label: 'MiB' }, { value: String(GiB), label: 'GiB' }, { value: String(TiB), label: 'TiB' }];

const EXT_ORIGIN = /^(chrome-extension:\/\/[a-p]{32}|moz-extension:\/\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|safari-web-extension:\/\/[0-9A-Fa-f-]{8,64})$/;

/* ------------------------------------------------------------------ schema */
// mail_in.trusted_authserv_ids entry (mirrors authservIDPattern in internal/mail/senderauth.go;
// "*.com" is refused: a wildcard needs two labels).
const MAIL_AUTHSERV_ID = /^(?!\*\.[a-z0-9-]+$)(\*\.)?[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$/;

const TABS = {
  general: {
    title: 'General', desc: 'Identity of this server and how people get accounts.',
    cards: [
      {
        ns: 'general', title: 'Server', icon: 'server', fields: [
          { key: 'server_name', label: 'Server name', type: 'text', required: true, maxlength: 64, hint: 'Shown in the sidebar, page titles and emails.' },
          { key: 'public_url', label: 'Public URL', type: 'url', placeholder: 'https://nas.example.com', hint: 'The address people use to reach this server. Used in emails and TV feed links. Leave empty to use the request address.' },
        ],
      },
      {
        ns: 'general', title: 'Accounts', icon: 'users', fields: [
          { key: 'registration_enabled', label: 'Open registration', type: 'switch', hint: 'Anyone who can reach the server can create an account.' },
          { key: 'require_email_verification', label: 'Require email verification', type: 'switch', hint: 'New accounts must confirm their email before signing in (needs SMTP).' },
          { key: 'default_role', label: 'Default role', type: 'select', options: [{ value: 'user', label: 'User' }, { value: 'admin', label: 'Administrator' }], hint: 'Role given to self-registered accounts.' },
          { key: 'default_quota_bytes', label: 'Default storage quota', type: 'bytes', hint: '0 means unlimited.' },
        ],
      },
    ],
  },
  security: {
    title: 'Security', desc: 'Sessions, passwords and sign-in protection.',
    cards: [
      {
        ns: 'security', title: 'Sessions', icon: 'clock', fields: [
          { key: 'session_idle_minutes', label: 'Idle timeout', type: 'number', min: 5, max: 525600, suffix: 'minutes', describe: (v) => (v ? `Signed out after ${formatDurationLong(v * 60)} without activity.` : '') },
          { key: 'session_absolute_days', label: 'Maximum session age', type: 'number', min: 1, max: 365, suffix: 'days', hint: 'Sessions end after this long even when active.' },
        ],
      },
      {
        ns: 'security', title: 'Sign-in protection', icon: 'shield', fields: [
          { key: 'password_min_length', label: 'Minimum password length', type: 'number', min: 8, max: 128, suffix: 'characters' },
          { key: 'require_totp_for_admins', label: 'Require two-factor for administrators', type: 'switch', hint: 'Admins must enable an authenticator app.' },
          { key: 'login_lockout_enabled', label: 'Lock out repeated failures', type: 'switch', hint: 'After 5 failed attempts, sign-in for that account and IP is delayed exponentially (up to 15 minutes).' },
        ],
      },
      {
        ns: 'security', title: 'Browser extensions', icon: 'globe', fields: [
          {
            key: 'allowed_extension_origins', label: 'Allowed extension origins', type: 'list', placeholder: 'chrome-extension://abcdefghijklmnopabcdefghijklmnop',
            hint: 'Extensions may call the API with Bearer tokens only (never cookies). Leave empty to allow any extension origin.',
            emptyText: 'None — any extension origin is allowed with Bearer tokens.',
            validateItem: (v) => (EXT_ORIGIN.test(v) ? null : 'Use chrome-extension://<32 letters a–p> or moz-extension://<uuid>.'),
          },
        ],
      },
    ],
  },
  email: {
    title: 'Email', desc: 'Outgoing mail for resets and verification, and the mail-in inbox for notes.',
    cards: [
      {
        ns: 'smtp', title: 'Outgoing mail (SMTP)', icon: 'mail', test: 'smtp', fields: [
          { key: 'enabled', label: 'Send email', type: 'switch', hint: 'Password resets, verification and notifications.' },
          { key: 'host', label: 'Host', type: 'text', placeholder: 'smtp.example.com', half: true },
          { key: 'port', label: 'Port', type: 'number', min: 1, max: 65535, half: true },
          { key: 'security', label: 'Encryption', type: 'select', options: [{ value: 'starttls', label: 'STARTTLS' }, { value: 'tls', label: 'TLS (implicit)' }, { value: 'none', label: 'None (insecure)' }], half: true },
          { key: 'username', label: 'Username', type: 'text', autocomplete: 'off', half: true },
          { key: 'password', label: 'Password', type: 'secret' },
          { key: 'from_address', label: 'From address', type: 'email', placeholder: 'ludworks@example.com', half: true },
          { key: 'from_name', label: 'From name', type: 'text', half: true },
        ],
      },
      {
        ns: 'imap', title: 'Mail-in inbox (IMAP)', icon: 'archive', test: 'imap',
        intro: 'Unread messages from a user’s verified address (with mail-in enabled in their profile) become notes. Subject → title, body → Markdown, attachments → Home › Mail Attachments.',
        fields: [
          { key: 'enabled', label: 'Poll mailbox', type: 'switch' },
          { key: 'host', label: 'Host', type: 'text', placeholder: 'imap.example.com', half: true },
          { key: 'port', label: 'Port', type: 'number', min: 1, max: 65535, half: true },
          { key: 'security', label: 'Encryption', type: 'select', options: [{ value: 'tls', label: 'TLS (implicit)' }, { value: 'starttls', label: 'STARTTLS' }, { value: 'none', label: 'None (insecure)' }], half: true },
          { key: 'username', label: 'Username', type: 'text', autocomplete: 'off', half: true },
          { key: 'password', label: 'Password', type: 'secret' },
          { key: 'mailbox', label: 'Mailbox', type: 'text', placeholder: 'INBOX', half: true },
          { key: 'poll_minutes', label: 'Check every', type: 'number', min: 1, max: 1440, suffix: 'minutes', half: true },
          { key: 'mark_seen', label: 'Mark imported messages as read', type: 'switch' },
          { key: 'save_attachments', label: 'Save attachments to Files', type: 'switch' },
        ],
      },
      {
        ns: 'mail_in', title: 'Mail-in sender check', icon: 'shield',
        intro: 'A From address is easy to forge, so a message is only imported when a mail server you trust reports that it passed DMARC for the sender’s domain (its Authentication-Results header). Until a server is listed, nothing is imported.',
        fields: [
          {
            key: 'trusted_authserv_ids', label: 'Trusted receiving mail servers', type: 'list', placeholder: 'mx.google.com',
            hint: 'The authserv-id your mailbox provider writes first in Authentication-Results, e.g. mx.google.com. *.example.net matches its subdomains. At most 20.',
            emptyText: 'None — no mail is imported.',
            validateItem: (v) => (MAIL_AUTHSERV_ID.test(v.trim().toLowerCase().replace(/\.$/, '')) ? null : 'Use a host name such as mx.example.net, or *.example.net.'),
          },
          { key: 'allow_unauthenticated_senders', label: 'Import mail from unauthenticated senders (unsafe)', type: 'switch', hint: 'Skips the DMARC check: anyone who can send mail to the inbox can then create notes and files in the account whose address they forge.' },
        ],
      },
    ],
  },
  media: {
    title: 'Media & TV', desc: 'Probing, thumbnails, transcoding, metadata, TV feeds, music and casting.',
    cards: [
      {
        ns: 'media', title: 'Media processing', icon: 'film', ffmpegStatus: true, fields: [
          { key: 'ffmpeg_path', label: 'ffmpeg path', type: 'path', placeholder: 'Auto-detect from PATH', half: true },
          { key: 'ffprobe_path', label: 'ffprobe path', type: 'path', placeholder: 'Auto-detect from PATH', half: true },
          { key: 'auto_probe', label: 'Probe new videos automatically', type: 'switch', hint: 'Reads duration and codecs when files are uploaded.' },
          { key: 'auto_thumbnails', label: 'Generate thumbnails', type: 'switch', hint: 'Video thumbnails need ffmpeg; images work without it.' },
          { key: 'transcode_enabled', label: 'Transcode for browsers', type: 'switch', hint: 'Converts formats browsers can’t play (needs ffmpeg).' },
        ],
      },
      {
        ns: 'media', title: 'Metadata', icon: 'library', fields: [
          { key: 'tmdb_api_key', label: 'TMDB API key', type: 'secret', hint: 'Enables matching movies and shows with posters and descriptions from themoviedb.org.' },
        ],
      },
      {
        ns: 'tv', title: 'TV feeds', icon: 'radio', fields: [
          { key: 'guide_days', label: 'Guide length', type: 'number', min: 1, max: 14, suffix: 'days', hint: 'How far ahead the XMLTV guide is generated.' },
          { key: 'hdhr_enabled', label: 'HDHomeRun emulation', type: 'switch', hint: 'Lets Plex and Jellyfin add your channels as a live TV tuner.' },
          { key: 'hdhr_device_name', label: 'Device name', type: 'text', half: true },
          { key: 'hdhr_tuner_count', label: 'Tuners', type: 'number', min: 1, max: 16, half: true },
        ],
      },
      {
        ns: 'music', title: 'Music', icon: 'music',
        intro: 'The music library reads tags and cover art from audio files. Mobile apps can keep tracks for offline listening, as the original file or as a smaller AAC copy that the server converts once and caches.',
        fields: [
          { key: 'download_cache_bytes', label: 'Offline copy cache', type: 'bytes', hint: '0 disables transcoded downloads (offline copies in high/normal/low quality); originals always work. Least recently used copies are removed first. Converting needs ffmpeg.' },
          { key: 'folder_art', label: 'Use cover images from track folders', type: 'switch', hint: 'Use a cover.jpg-style image next to the tracks as album art when no custom or embedded art exists.' },
        ],
      },
      {
        ns: 'cast', title: 'Casting', icon: 'cast', castStatus: true,
        intro: 'Lets people play videos, TV channels and music on Chromecast and AirPlay devices (Chromecast, Google TV, Apple TV, HomePod, AirPlay speakers) from any browser or app. The server controls the devices itself and gives them short-lived links to just the media of one session.',
        fields: [
          { key: 'enabled', label: 'Enable casting', type: 'switch', hint: 'Off by default. While off, the server opens no casting sockets and people see no devices.' },
          { key: 'discovery', label: 'Find devices automatically', type: 'switch', hint: 'Looks for devices with mDNS (Bonjour) on the server’s local network. In Docker, discovery needs host networking.' },
          {
            key: 'media_port', label: 'Cast media port', type: 'number', min: 0, max: 65535, half: true,
            check: (v) => (v === 0 || (v >= 1024 && v <= 65535) ? null : 'Use 0 (off) or a port from 1024 to 65535.'),
            hint: 'A plain-HTTP port for devices that cannot trust the server’s self-signed certificate. It serves only the session-bound media links handed to cast devices (everything else is 404). 0 = off; it must differ from the server’s own port.',
          },
          {
            key: 'media_base_url', label: 'Media base URL', type: 'url', placeholder: 'https://nas.example.com', half: true,
            check: (v) => checkBaseUrl(v),
            hint: 'Optional: an https address with a certificate the devices trust (e.g. through a reverse proxy) that they use instead. Just scheme, host and port — no path.',
          },
          {
            key: 'manual_devices', label: 'Manual devices', type: 'devices',
            hint: 'Devices discovery cannot see (another subnet or VLAN, Docker without host networking). The port defaults to 8009 for Chromecast and 7000 for AirPlay.',
          },
        ],
      },
    ],
  },
  files: {
    title: 'Files', desc: 'Upload limits, trash and integrity checks.',
    cards: [
      {
        ns: 'files', title: 'Uploads', icon: 'upload', fields: [
          { key: 'max_upload_bytes', label: 'Maximum file size', type: 'bytes', hint: '0 means unlimited (still limited by quota and free disk space).' },
          { key: 'upload_expiry_hours', label: 'Abandoned uploads expire after', type: 'number', min: 1, max: 720, suffix: 'hours', hint: 'Incomplete resumable uploads are purged after this time.' },
          { key: 'compute_sha256', label: 'Compute SHA-256 checksums', type: 'switch', hint: 'Enables integrity checks and duplicate detection; costs some CPU on large files.' },
        ],
      },
      {
        ns: 'files', title: 'Trash', icon: 'trash', fields: [
          { key: 'trash_retention_days', label: 'Keep deleted files for', type: 'number', min: 0, max: 3650, suffix: 'days', hint: '0 keeps trash until it is emptied manually.' },
        ],
      },
    ],
  },
};

/* ---------------------------------------------------------------- controls */

function pickUnit(bytes) {
  if (!bytes) return GiB;
  if (bytes % TiB === 0) return TiB;
  if (bytes % GiB === 0 || bytes >= GiB) return GiB;
  return MiB;
}

function bytesControl(name) {
  const num = input({ type: 'number', min: '0', step: 'any', name, className: 'nums' });
  const unit = select({ options: BYTE_UNITS, value: String(GiB), 'aria-label': 'Unit' });
  const el = h('div', { class: 'input-addon' }, num, unit);
  return {
    el, focusEl: num,
    get() {
      if (num.value === '') return 0;
      const v = Number(num.value);
      return Number.isFinite(v) && v >= 0 ? Math.round(v * Number(unit.value)) : NaN;
    },
    set(v) {
      const b = Number(v) || 0;
      const u = pickUnit(b);
      unit.value = String(u);
      num.value = b === 0 ? '0' : String(Math.round((b / u) * 1000) / 1000);
    },
    validate() {
      const v = this.get();
      return Number.isNaN(v) ? 'Enter a size of 0 or more.' : null;
    },
  };
}

function secretControl(label) {
  let isSet = false;
  let mode = 'keep';
  const wrap = h('div', { class: 'secret-field' });
  const pw = passwordInput({ autocomplete: 'new-password', placeholder: `New ${label.toLowerCase()}` });
  const notify = () => wrap.dispatchEvent(new Event('input', { bubbles: true }));
  const render = () => {
    const btn = (lbl, variant, fn, ic) => {
      const b = h('button', { type: 'button', class: cls('btn btn-sm', `btn-${variant}`) }, ic ? icon(ic, 14) : null, h('span', { class: 'btn-label' }, lbl));
      b.addEventListener('click', fn);
      return b;
    };
    if (mode === 'replace') {
      wrap.replaceChildren(pw, btn('Cancel', 'ghost', () => { mode = 'keep'; pw.inputEl.value = ''; render(); notify(); }));
      requestAnimationFrame(() => pw.inputEl.focus());
    } else if (mode === 'clear') {
      wrap.replaceChildren(h('span', { class: 'badge badge-danger' }, icon('x', 12), 'Will be removed on save'),
        btn('Undo', 'ghost', () => { mode = 'keep'; render(); notify(); }));
    } else {
      mount(wrap,
        isSet ? h('span', { class: 'badge badge-success' }, icon('lock', 12), 'Set') : h('span', { class: 'badge' }, 'Not set'),
        btn(isSet ? 'Replace' : 'Set', 'secondary', () => { mode = 'replace'; render(); notify(); }, 'edit'),
        isSet ? btn('Remove', 'danger-ghost', () => { mode = 'clear'; render(); notify(); }, 'trash') : null);
    }
  };
  pw.inputEl.addEventListener('input', notify);
  render();
  return {
    el: wrap, focusEl: pw.inputEl,
    get() {
      if (mode === 'clear') return null;
      if (mode === 'replace' && pw.inputEl.value !== '') return pw.inputEl.value;
      return undefined;
    },
    set(v) {
      isSet = typeof v === 'object' && v !== null ? !!v.set : !!v;
      mode = 'keep';
      pw.inputEl.value = '';
      render();
    },
  };
}

function listControl(def) {
  let values = [];
  const chips = h('div', { class: 'chips' });
  const entry = input({ placeholder: def.placeholder || '', className: 'mono', autocomplete: 'off', spellcheck: 'false' });
  const err = h('div', { class: 'field-error', role: 'alert', hidden: true });
  const addBtn = h('button', { type: 'button', class: 'btn btn-secondary' }, icon('plus', 14), h('span', { class: 'btn-label' }, 'Add'));
  const el = h('div', { class: 'list-editor' }, chips, h('div', { class: 'list-editor-row' }, entry, addBtn), err);
  const notify = () => el.dispatchEvent(new Event('input', { bubbles: true }));
  const render = () => {
    chips.replaceChildren(...(values.length ? values.map((v) => {
      const c = h('span', { class: 'chip' }, h('span', { class: 'chip-label mono' }, v));
      const x = h('button', { type: 'button', class: 'chip-x', aria: { label: `Remove ${v}` } }, icon('x', 12));
      x.addEventListener('click', () => { values = values.filter((y) => y !== v); render(); notify(); });
      c.append(x);
      return c;
    }) : [h('span', { class: 'small faint' }, def.emptyText || 'None.')]));
  };
  const add = () => {
    const v = entry.value.trim();
    if (!v) return;
    const problem = def.validateItem?.(v);
    if (problem) {
      err.textContent = problem;
      err.hidden = false;
      return;
    }
    err.hidden = true;
    if (!values.includes(v)) values = [...values, v];
    entry.value = '';
    render();
    notify();
  };
  addBtn.addEventListener('click', add);
  entry.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      add();
    }
  });
  entry.addEventListener('input', (e) => { e.stopPropagation(); err.hidden = true; });
  return {
    el, focusEl: entry,
    get: () => values.slice(),
    set(v) {
      values = Array.isArray(v) ? v.slice() : [];
      render();
    },
  };
}

/* ------------------------------------------------- cast.manual_devices editor */

const DEVICE_KINDS = [{ value: 'chromecast', label: 'Chromecast' }, { value: 'airplay', label: 'AirPlay' }];
const DEFAULT_DEVICE_PORT = { chromecast: 8009, airplay: 7000 };
const MAX_MANUAL_DEVICES = 50;
const HOST_NAME = /^(?=.{1,253}$)[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*\.?$/;

/** A host name or an IPv4/IPv6 literal (mirrors settings.ValidHost on the server). */
export function validDeviceHost(v) {
  const t = String(v || '').trim();
  if (!t) return false;
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(t)) return t.split('.').every((n) => Number(n) <= 255);
  const inner = t.replace(/^\[(.*)\]$/, '$1');
  if (inner.includes(':')) return /^[0-9A-Fa-f:.]+$/.test(inner) && (inner.match(/:/g) || []).length >= 2;
  return HOST_NAME.test(t);
}

/** A manual device in the canonical form the server stores (default port filled in). */
function canonicalDevice(d) {
  const kind = d?.kind === 'airplay' ? 'airplay' : 'chromecast';
  const port = Number(d?.port) > 0 ? Number(d.port) : DEFAULT_DEVICE_PORT[kind];
  return { kind, host: String(d?.host || '').trim().toLowerCase(), port, name: String(d?.name || '').trim() };
}

/** Whether two manual_devices lists are the same once defaults are filled in. */
export function sameDevices(a, b) {
  const x = Array.isArray(a) ? a : [];
  const y = Array.isArray(b) ? b : [];
  return x.length === y.length && x.every((d, i) => deepEqual(canonicalDevice(d), canonicalDevice(y[i])));
}

/**
 * Problem with a manual_devices list (client-side check; the server validates too), or null.
 * @param {Array<{kind: string, host: string, port?: number|string, name?: string}>} list
 */
export function manualDevicesProblem(list) {
  if (list.length > MAX_MANUAL_DEVICES) return `At most ${MAX_MANUAL_DEVICES} devices.`;
  const seen = new Set();
  for (const [i, d] of list.entries()) {
    const n = `Device ${i + 1}: `;
    if (!validDeviceHost(d.host)) return `${n}enter a host name or IP address.`;
    if (d.port !== undefined && d.port !== '' && d.port !== null) {
      const p = Number(d.port);
      if (!Number.isInteger(p) || p < 1 || p > 65535) return `${n}the port must be between 1 and 65535 (or empty for the default).`;
    }
    const name = String(d.name || '');
    if (name.length > 100) return `${n}the name can have at most 100 characters.`;
    if (/[\u0000-\u001f\u007f]/.test(name)) return `${n}the name must not contain control characters.`;
    const c = canonicalDevice(d);
    const key = `${c.host}:${c.port}`;
    if (seen.has(key)) return `${n}${d.host.trim()} is listed twice.`;
    seen.add(key);
  }
  return null;
}

function devicesControl(def) {
  let rows = []; // [{kind, host, port: string, name}]
  const list = h('div', { class: 'cast-manual-list' });
  const addBtn = h('button', { type: 'button', class: 'btn btn-secondary btn-sm' }, icon('plus', 14), h('span', { class: 'btn-label' }, 'Add device'));
  const el = h('div', { class: 'cast-manual', role: 'group', aria: { label: def.label } }, list, h('div', { class: 'cast-manual-actions' }, addBtn));
  const notify = () => el.dispatchEvent(new Event('input', { bubbles: true }));

  const rowEl = (row, i) => {
    const kind = select({ options: DEVICE_KINDS, value: row.kind, 'aria-label': `Device ${i + 1} type` });
    const host = input({ value: row.host, placeholder: '192.168.1.20 or tv.local', className: 'mono', autocomplete: 'off', spellcheck: 'false', 'aria-label': `Device ${i + 1} host name or IP address` });
    const port = input({ type: 'number', min: '1', max: '65535', step: '1', value: row.port, placeholder: `Default ${DEFAULT_DEVICE_PORT[row.kind]}`, className: 'nums', 'aria-label': `Device ${i + 1} port` });
    const name = input({ value: row.name, placeholder: 'Name (optional)', maxlength: '100', autocomplete: 'off', 'aria-label': `Device ${i + 1} name` });
    const remove = h('button', { type: 'button', class: 'btn btn-ghost btn-icon btn-sm', aria: { label: `Remove device ${i + 1}` }, dataset: { tip: 'Remove' } }, icon('x', 14));
    kind.addEventListener('change', () => { row.kind = kind.value; port.placeholder = `Default ${DEFAULT_DEVICE_PORT[row.kind]}`; });
    host.addEventListener('input', () => { row.host = host.value; host.removeAttribute('aria-invalid'); });
    port.addEventListener('input', () => { row.port = port.value; port.removeAttribute('aria-invalid'); });
    name.addEventListener('input', () => { row.name = name.value; });
    remove.addEventListener('click', () => {
      rows = rows.filter((r) => r !== row);
      render();
      notify();
      (list.querySelector('.cast-manual-row input') || addBtn).focus();
    });
    return h('div', { class: 'cast-manual-row', dataset: { index: String(i) } }, kind, host, port, name, remove);
  };

  const render = () => {
    list.replaceChildren(...(rows.length ? rows.map(rowEl) : [h('span', { class: 'small faint' }, 'None — only discovered devices are listed.')]));
    addBtn.disabled = rows.length >= MAX_MANUAL_DEVICES;
  };
  addBtn.addEventListener('click', () => {
    rows = [...rows, { kind: 'chromecast', host: '', port: '', name: '' }];
    render();
    notify();
    [...list.querySelectorAll('.cast-manual-row')].at(-1)?.querySelector('input')?.focus();
  });
  return {
    el, focusEl: addBtn,
    get: () => rows.map((r) => {
      const out = { kind: r.kind, host: r.host.trim() };
      if (String(r.port).trim() !== '') out.port = Number(r.port);
      if (r.name.trim()) out.name = r.name.trim();
      return out;
    }),
    set(v) {
      rows = (Array.isArray(v) ? v : []).map((d) => ({
        kind: d?.kind === 'airplay' ? 'airplay' : 'chromecast', host: String(d?.host || ''),
        port: Number(d?.port) > 0 ? String(d.port) : '', name: String(d?.name || ''),
      }));
      render();
    },
    equals: (a, b) => sameDevices(a, b),
    validate() {
      const problem = manualDevicesProblem(this.get());
      if (problem) {
        const i = Number(/^Device (\d+):/.exec(problem)?.[1]) - 1;
        const row = list.querySelectorAll('.cast-manual-row')[i];
        const which = problem.includes('the port') ? 1 : problem.includes('the name') ? 2 : 0; // host, port, name inputs
        row?.querySelectorAll('input')[which]?.setAttribute('aria-invalid', 'true');
      }
      return problem;
    },
  };
}

/** A cast media base URL: empty, or an absolute http(s) URL without path, query or credentials. */
export function checkBaseUrl(v) {
  if (!v) return null;
  let u;
  try {
    u = new URL(v);
  } catch {
    return 'Enter a full http(s):// URL.';
  }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') return 'Use an http:// or https:// address.';
  if ((u.pathname && u.pathname !== '/') || u.search || u.hash || u.username || u.password) {
    return 'Use just the scheme, host and port (e.g. https://nas.example.com) — no path or query.';
  }
  return null;
}

function makeControl(ns, def) {
  const name = `${ns}.${def.key}`;
  let ctl;
  switch (def.type) {
    case 'switch': {
      const sw = switchInput({ label: def.label });
      ctl = { el: sw, focusEl: sw.inputEl, get: () => sw.inputEl.checked, set: (v) => { sw.inputEl.checked = !!v; } };
      break;
    }
    case 'select': {
      const sel = select({ options: def.options, name });
      ctl = { el: sel, focusEl: sel, get: () => sel.value, set: (v) => { sel.value = v ?? def.options[0].value; } };
      break;
    }
    case 'number': {
      const num = input({ type: 'number', name, min: def.min, max: def.max, step: '1', className: 'nums' });
      const el = def.suffix ? h('div', { class: 'input-addon' }, num, h('span', { class: 'btn btn-secondary', aria: { hidden: 'true' } }, def.suffix)) : num;
      ctl = {
        el, focusEl: num,
        get: () => (num.value === '' ? null : Number(num.value)),
        set: (v) => { num.value = v === null || v === undefined ? '' : String(v); },
        validate() {
          const v = this.get();
          if (v === null || !Number.isInteger(v)) return 'Enter a whole number.';
          if (def.min !== undefined && v < def.min) return `Must be at least ${def.min}.`;
          if (def.max !== undefined && v > def.max) return `Must be at most ${def.max}.`;
          return def.check ? def.check(v) : null;
        },
      };
      break;
    }
    case 'bytes':
      ctl = bytesControl(name);
      break;
    case 'secret':
      ctl = secretControl(def.label);
      break;
    case 'list':
      ctl = listControl(def);
      break;
    case 'devices':
      ctl = devicesControl(def);
      break;
    default: {
      const t = def.type === 'url' ? 'url' : def.type === 'email' ? 'email' : 'text';
      const inp = input({
        type: t, name, placeholder: def.placeholder, maxlength: def.maxlength, autocomplete: def.autocomplete || 'off',
        className: def.type === 'path' ? 'mono' : undefined, spellcheck: 'false',
      });
      ctl = {
        el: inp, focusEl: inp,
        get: () => inp.value.trim(),
        set: (v) => { inp.value = v ?? ''; },
        validate() {
          const v = this.get();
          if (def.required && !v) return 'This field is required.';
          if (v && def.type === 'url' && !/^https?:\/\/[^\s/]+/i.test(v)) return 'Enter a full http(s):// URL.';
          if (v && def.type === 'email' && !inp.checkValidity()) return 'Enter a valid email address.';
          return def.check ? def.check(v) : null;
        },
      };
    }
  }
  const describe = def.describe ? h('span', { class: 'field-describe' }) : null;
  const hint = def.hint || describe ? h('span', {}, def.hint || '', def.hint && describe ? ' ' : '', describe) : undefined;
  const wrap = field({ label: def.label, hint, control: ctl.el, name, inline: def.type === 'switch' });
  if (def.half) wrap.classList.add('half');
  else wrap.classList.add('span-2');
  if (describe) {
    const upd = () => { describe.textContent = def.describe(ctl.get()) || ''; };
    ctl.el.addEventListener('input', upd);
    ctl.afterSet = upd;
  }
  return { ...ctl, ns, def, name, wrap };
}

/* ------------------------------------------------------------------ render */

/**
 * Render a settings tab.
 * @param {HTMLElement} host
 * @param {object} ctx app ctx (with signal)
 * @param {{settings: object, loadSettings: Function, setSettings: Function}} state shared admin state
 * @param {'general'|'security'|'email'|'media'|'files'} tabId
 */
export function render(host, ctx, state, tabId) {
  const { ui } = ctx;
  const tab = TABS[tabId];
  host.append(ui.loadingBlock('Loading settings…'));
  let controls = [];
  let saveBar = null;
  let form = null;
  let castStatusSlot = null;
  if (tab.cards.some((c) => c.ns === 'cast')) ensureStyle('/assets/css/lib/cast.css'); // manual devices editor
  const testButtons = [];


  function patch() {
    const out = {};
    for (const c of controls) {
      const v = c.get();
      const orig = state.settings?.[c.ns]?.[c.def.key];
      if (c.def.type === 'secret') {
        if (v !== undefined) (out[c.ns] ??= {})[c.def.key] = v;
      } else if (!(c.equals ? c.equals(orig, v) : deepEqual(orig, v))) {
        (out[c.ns] ??= {})[c.def.key] = v;
      }
    }
    return out;
  }

  function syncDirty() {
    const p = patch();
    const dirty = Object.keys(p).length > 0;
    saveBar.hidden = !dirty;
    for (const t of testButtons) {
      const blocked = !!p[t.ns];
      t.btn.disabled = blocked;
      t.btn.dataset.tip = blocked ? 'Save your changes before testing' : '';
    }
    ctx.router.setLeaveGuard(dirty ? () => ui.confirm({ title: 'Discard unsaved changes?', message: 'Your edits to these settings will be lost.', confirmLabel: 'Discard', danger: true }) : null);
    return p;
  }

  function fill(settings) {
    for (const c of controls) {
      c.set(settings?.[c.ns]?.[c.def.key]);
      c.afterSet?.();
    }
    clearErrors(form);
    syncDirty();
  }

  function attachErrors(fields, sentNs) {
    const unmatched = [];
    let first = null;
    for (const [key, msg] of Object.entries(fields || {})) {
      let ctl = null;
      if (key.includes('.')) {
        ctl = controls.find((c) => c.name === key)
          || controls.find((c) => key.startsWith(`${c.name}.`) || key.startsWith(`${c.name}[`));
      } else ctl = controls.find((c) => c.def.key === key && sentNs.includes(c.ns));
      if (ctl) {
        setFieldError(ctl.wrap, msg);
        first ??= ctl;
      } else unmatched.push(`${key}: ${msg}`);
    }
    first?.focusEl?.focus();
    return unmatched;
  }

  async function save(btn) {
    clearErrors(form);
    const p = patch();
    // Client-side checks on changed fields only.
    let invalid = null;
    for (const c of controls) {
      if (!p[c.ns] || !(c.def.key in p[c.ns]) || !c.validate) continue;
      const problem = c.validate();
      if (problem) {
        setFieldError(c.wrap, problem);
        invalid ??= c;
      }
    }
    if (invalid) {
      invalid.focusEl?.focus();
      return;
    }
    if (!Object.keys(p).length) return;
    await ui.withLoading(btn, async () => {
      try {
        const res = await ctx.api.patch('/admin/settings', p);
        state.setSettings(res);
        fill(res);
        if (p.general?.server_name) ctx.store.set({ status: { ...(ctx.store.get().status || {}), server_name: res.general?.server_name ?? p.general.server_name } });
        if (p.general && 'registration_enabled' in p.general) ctx.store.set({ status: { ...(ctx.store.get().status || {}), registration_enabled: !!res.general?.registration_enabled } });
        ui.toast.success('Settings saved');
        if (p.cast) {
          refreshCastStatus();
          setTimeout(() => { if (!ctx.signal.aborted) refreshCastStatus(); }, 4000); // discovery takes a moment
        }
      } catch (err) {
        const unmatched = err.fields ? attachErrors(err.fields, Object.keys(p)) : [];
        const msg = err.fields ? unmatched.join(' · ') : ui.errorMessage(err);
        if (msg) form.prepend(formBanner(msg));
      }
    });
  }

  async function runTest(kind, btn) {
    if (kind === 'smtp') {
      const to = await ui.prompt({
        title: 'Send a test email', label: 'Recipient', type: 'email', value: ctx.user?.email || '', confirmLabel: 'Send',
        validate: (v) => (/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(v.trim()) ? null : 'Enter a valid email address.'),
      });
      if (!to) return;
      await ui.withLoading(btn, async () => {
        try {
          await ctx.api.post('/admin/settings/smtp/test', { to: to.trim() });
          ui.toast.success('Test email sent', { description: `Check the inbox of ${to.trim()}.` });
        } catch (err) {
          ui.modal({ title: 'SMTP test failed', size: 'sm', body: ui.alert({ variant: 'danger', title: err.code === 'smtp_failed' ? 'The mail server rejected the connection' : 'Request failed', message: ui.errorMessage(err) }), footer: [{ label: 'Close', variant: 'primary', value: true }] });
        }
      });
      return;
    }
    const resultHost = btn.closest('.card').querySelector('.test-result');
    await ui.withLoading(btn, async () => {
      try {
        const res = await ctx.api.post('/admin/settings/imap/test', {});
        resultHost.replaceChildren(ui.alert({
          variant: 'success', title: 'Connected',
          message: h('div', { class: 'stack-sm' },
            h('span', {}, `${res?.unseen ?? 0} unread ${res?.unseen === 1 ? 'message' : 'messages'} waiting.`),
            res?.mailboxes?.length ? h('div', { class: 'chips' }, res.mailboxes.slice(0, 30).map((m) => h('span', { class: 'chip' }, h('span', { class: 'chip-label' }, m)))) : null),
        }));
      } catch (err) {
        resultHost.replaceChildren(ui.alert({ variant: 'danger', title: 'Connection failed', message: ui.errorMessage(err) }));
      }
    });
  }

  /** Casting card: whether casting is on and how many devices the server sees. */
  function refreshCastStatus() {
    const slot = castStatusSlot;
    if (!slot) return;
    ctx.api.get('/cast/devices', { signal: ctx.signal }).then((r) => {
      if (!r?.enabled) {
        slot.replaceChildren(ui.alert({ variant: 'info', message: 'Casting is off. Turn it on below to let people cast from the TV player and the music player.' }));
        return;
      }
      const n = Array.isArray(r.devices) ? r.devices.length : 0;
      const names = (r.devices || []).slice(0, 6).map((d) => d.name || d.host).filter(Boolean).join(', ');
      slot.replaceChildren(n
        ? ui.alert({ variant: 'success', title: `Casting is on — ${n} ${n === 1 ? 'device' : 'devices'} found`, message: names })
        : ui.alert({ variant: 'warning', title: 'Casting is on, but no devices were found yet', message: 'Devices must be on the server’s network. Discovery needs host networking in Docker; you can also add devices manually below.' }));
    }).catch(() => slot.replaceChildren());
  }

  function build(settings) {
    controls = [];
    const cards = tab.cards.filter((card) => settings?.[card.ns] !== undefined).map((card) => {
      const cardControls = card.fields.map((def) => makeControl(card.ns, def));
      controls.push(...cardControls);
      const grid = h('div', { class: 'form-grid cols-2' }, cardControls.map((c) => c.wrap));
      const extras = [];
      if (card.intro) extras.push(h('p', { class: 'small muted' }, card.intro));
      if (card.ffmpegStatus) {
        const slot = h('div');
        extras.push(slot);
        ctx.api.get('/system/info', { signal: ctx.signal }).then((info) => {
          const ff = info?.ffmpeg || {};
          slot.replaceChildren(ff.available
            ? ui.alert({ variant: 'success', title: 'ffmpeg detected', message: ff.version || 'Video thumbnails and transcoding are available.' })
            : ui.alert({ variant: 'warning', title: 'ffmpeg not found', message: 'Install ffmpeg (or set its path below) to enable video thumbnails, transcoding and live TV streams.' }));
        }).catch(() => {});
      }
      if (card.castStatus) {
        castStatusSlot = h('div', { class: 'cast-status' });
        extras.push(castStatusSlot);
        refreshCastStatus();
      }
      let actions = null;
      if (card.test) {
        const btn = ui.button({ label: card.test === 'smtp' ? 'Send test email' : 'Test connection', icon: card.test === 'smtp' ? 'mail' : 'refresh', size: 'sm' });
        btn.addEventListener('click', () => runTest(card.test, btn));
        testButtons.push({ ns: card.ns, btn });
        actions = btn;
      }
      return ui.card({
        title: card.title, icon: card.icon, actions,
        body: h('div', { class: 'stack' }, ...extras, grid, card.test === 'imap' ? h('div', { class: 'test-result' }) : null),
      });
    });

    const saveBtn = ui.button({ label: 'Save changes', variant: 'primary', type: 'submit' });
    const discard = ui.button({ label: 'Discard', variant: 'ghost', onClick: () => fill(state.settings) });
    saveBar = h('div', { class: 'save-bar', hidden: true },
      h('div', { class: 'save-bar-msg' }, h('span', { class: 'save-bar-dot', aria: { hidden: 'true' } }), 'You have unsaved changes'),
      discard, saveBtn);
    form = h('form', { class: 'stack', novalidate: true },
      h('div', { class: 'settings-section-header' },
        h('h2', { class: 'settings-section-title' }, tab.title),
        h('p', { class: 'settings-section-desc' }, tab.desc)),
      ...cards, saveBar);
    form.addEventListener('input', () => syncDirty());
    form.addEventListener('change', () => syncDirty());
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      save(saveBtn);
    });
    host.replaceChildren(form);
    fill(settings);
  }

  state.loadSettings().then((s) => {
    if (ctx.signal.aborted) return;
    build(s);
  }).catch((err) => {
    if (ctx.signal.aborted) return;
    host.replaceChildren(ui.errorState(err, () => {
      host.replaceChildren(ui.loadingBlock('Loading settings…'));
      state.loadSettings(true).then(build).catch((e) => host.replaceChildren(ui.errorState(e)));
    }));
  });

  return () => ctx.router.setLeaveGuard(null);
}

