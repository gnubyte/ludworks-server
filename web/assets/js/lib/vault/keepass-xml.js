/**
 * KeePass 2 XML document model, shared by KDBX (kdbx.js) and the unencrypted "KeePass XML (2.x)" export.
 *
 * Tree shape (JSON-friendly; times are ISO-8601 UTC strings with second precision, UUIDs are 32-char
 * lowercase hex):
 *
 *   KdbxTree  { meta: Meta, root: Group, deletedObjects: [{uuid, deletionTime}],
 *               binaries: [{data: Uint8Array, protected: boolean}] }
 *   Meta      { generator, databaseName, databaseNameChanged, databaseDescription, ..., memoryProtection:
 *               {title, userName, password, url, notes}, recycleBinEnabled, recycleBinUuid, ...,
 *               customIcons: [{uuid, data, name?, lastModified?}], customData: [{key, value, lastModified?}],
 *               extra: XmlElement[] }
 *   Group     { uuid, name, notes, iconId, customIconUuid?, times, isExpanded, defaultAutoTypeSequence,
 *               enableAutoType, enableSearching, lastTopVisibleEntry, previousParentGroup?, tags: [],
 *               customData: [], entries: Entry[], groups: Group[], extra: XmlElement[] }
 *   Entry     { uuid, iconId, customIconUuid?, foregroundColor, backgroundColor, overrideUrl,
 *               qualityCheck?, tags: [], previousParentGroup?, times, fields: [{key, value, protected}],
 *               binaries: [{key, ref}], autoType: XmlElement|null, customData: [], history: Entry[],
 *               extra: XmlElement[] }
 *   Times     { creationTime, lastModificationTime, lastAccessTime, expiryTime, expires, usageCount,
 *               locationChanged }
 *
 * Unknown elements are kept verbatim in `extra` so a read → write round trip does not drop data.
 */
import { VaultError } from './errors.js';
import { b64decode, b64encode, fromHex, gunzip, randomBytes, toHex, utf8Decode, utf8Encode } from './bytes.js';
import { childElements, el, firstChild, parseXml, serializeXml, textOf } from './xml.js';

const ZERO_UUID = '00000000000000000000000000000000';
const EPOCH_OFFSET = 62135596800; // seconds between 0001-01-01T00:00:00Z and the Unix epoch
const MIN_MS = -EPOCH_OFFSET * 1000;
const MAX_MS = 253402300799000; // 9999-12-31T23:59:59Z

export const TIME_TAGS = new Set([
  'CreationTime', 'LastModificationTime', 'LastAccessTime', 'ExpiryTime', 'LocationChanged', 'DeletionTime',
  'SettingsChanged', 'DatabaseNameChanged', 'DatabaseDescriptionChanged', 'DefaultUserNameChanged',
  'MasterKeyChanged', 'RecycleBinChanged', 'EntryTemplatesGroupChanged',
]);

// --- scalar codecs ------------------------------------------------------------------------------

/** 16 random bytes as lowercase hex. */
export function newKdbxUuid() {
  return toHex(randomBytes(16));
}

function isoFromMs(ms) {
  const clamped = Math.min(MAX_MS, Math.max(MIN_MS, ms));
  return new Date(Math.floor(clamped / 1000) * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z');
}

/** Current time as a KeePass-precision ISO string. */
export function nowIso() {
  return isoFromMs(Date.now());
}

/** Parse a KeePass time (ISO text or KDBX4 base64 int64 seconds) → ISO string, or null. */
export function parseKdbxTime(text) {
  const t = (text || '').trim();
  if (!t) return null;
  if (/^\d{4}-\d{2}-\d{2}T/.test(t)) {
    const ms = Date.parse(t);
    return Number.isNaN(ms) ? null : isoFromMs(ms);
  }
  try {
    const b = b64decode(t);
    if (b.length !== 8) return null;
    const secs = Number(new DataView(b.buffer, b.byteOffset, 8).getBigInt64(0, true));
    return isoFromMs((secs - EPOCH_OFFSET) * 1000);
  } catch {
    return null;
  }
}

/** Format an ISO time for the given mode: KDBX 4 uses base64 int64 seconds, everything else ISO. */
export function formatKdbxTime(iso, mode) {
  const ms = iso ? Date.parse(iso) : NaN;
  const value = isoFromMs(Number.isNaN(ms) ? Date.now() : ms);
  if (mode !== 'kdbx4') return value;
  const secs = BigInt(Math.floor(Date.parse(value) / 1000) + EPOCH_OFFSET);
  const b = new Uint8Array(8);
  new DataView(b.buffer).setBigInt64(0, secs, true);
  return b64encode(b);
}

function parseUuid(text) {
  const t = (text || '').trim();
  if (!t) return null;
  try {
    const b = b64decode(t);
    return b.length === 16 ? toHex(b) : null;
  } catch {
    return null;
  }
}

function formatUuid(hex) {
  return b64encode(fromHex(hex && /^[0-9a-f]{32}$/.test(hex) ? hex : ZERO_UUID));
}

const parseBool = (t, dflt = false) => {
  const v = (t || '').trim().toLowerCase();
  if (v === 'true' || v === '1') return true;
  if (v === 'false' || v === '0') return false;
  return dflt;
};
const fmtBool = (b) => (b ? 'True' : 'False');
const parseInt10 = (t, dflt = 0) => {
  const n = Number.parseInt((t || '').trim(), 10);
  return Number.isFinite(n) ? n : dflt;
};
const parseTags = (t) =>
  (t || '')
    .split(/[;,]/)
    .map((s) => s.trim())
    .filter(Boolean);

const childText = (node, name) => textOf(firstChild(node, name));

/** Apply fn to every time-valued element below `node` (used for elements kept verbatim). */
function mapRawTimes(node, fn) {
  for (const c of node.children) {
    if (typeof c !== 'object') continue;
    if (TIME_TAGS.has(c.name) && c.children.every((k) => typeof k === 'string')) {
      const v = fn(textOf(c));
      if (v !== null) c.children = [v];
    } else {
      mapRawTimes(c, fn);
    }
  }
}

function cloneXml(node) {
  const c = { name: node.name, attrs: Object.assign(Object.create(null), node.attrs), children: node.children.map((k) => (typeof k === 'object' ? cloneXml(k) : k)) };
  if (node.protectedBytes) c.protectedBytes = node.protectedBytes;
  return c;
}

function rawIn(node) {
  const c = cloneXml(node);
  mapRawTimes(c, (t) => parseKdbxTime(t));
  return c;
}

function rawOut(node, mode) {
  const c = cloneXml(node);
  mapRawTimes(c, (t) => (/^\d{4}-\d{2}-\d{2}T/.test(t.trim()) ? formatKdbxTime(t.trim(), mode) : null));
  return c;
}

// --- reading -------------------------------------------------------------------------------------

/**
 * Read a protected-or-not value element.
 * kdbx.js decrypts Protected="True" values before calling us and stores the plaintext in
 * node.protectedBytes; in a plain XML export such values cannot be decrypted.
 */
function readValue(node) {
  if (!node) return { value: '', protected: false };
  if (node.protectedBytes) return { value: utf8Decode(node.protectedBytes), protected: true };
  if (parseBool(node.attrs.Protected)) throw new VaultError('unsupported', 'XML contains encrypted (Protected) values without a KDBX stream');
  return { value: textOf(node), protected: parseBool(node.attrs.ProtectInMemory) };
}

function readTimes(node) {
  const t = node || { children: [] };
  return {
    creationTime: parseKdbxTime(childText(t, 'CreationTime')),
    lastModificationTime: parseKdbxTime(childText(t, 'LastModificationTime')),
    lastAccessTime: parseKdbxTime(childText(t, 'LastAccessTime')),
    expiryTime: parseKdbxTime(childText(t, 'ExpiryTime')),
    expires: parseBool(childText(t, 'Expires')),
    usageCount: parseInt10(childText(t, 'UsageCount')),
    locationChanged: parseKdbxTime(childText(t, 'LocationChanged')),
  };
}

function readCustomData(node) {
  if (!node) return [];
  return childElements(node, 'Item').map((it) => {
    const item = { key: childText(it, 'Key'), value: childText(it, 'Value') };
    const lm = firstChild(it, 'LastModificationTime');
    if (lm) item.lastModified = parseKdbxTime(textOf(lm));
    return item;
  });
}

const ENTRY_KNOWN = new Set(['UUID', 'IconID', 'CustomIconUUID', 'ForegroundColor', 'BackgroundColor', 'OverrideURL', 'QualityCheck', 'Tags', 'PreviousParentGroup', 'Times', 'String', 'Binary', 'AutoType', 'CustomData', 'History']);
const GROUP_KNOWN = new Set(['UUID', 'Name', 'Notes', 'IconID', 'CustomIconUUID', 'Times', 'IsExpanded', 'DefaultAutoTypeSequence', 'EnableAutoType', 'EnableSearching', 'LastTopVisibleEntry', 'PreviousParentGroup', 'Tags', 'CustomData', 'Entry', 'Group']);

/** Default cap on all attachment data decoded from one document (VAULT-CRYPTO.md §8.4: 512 MiB in total). */
export const DEFAULT_MAX_DECOMPRESSED_BYTES = 512 * 1024 * 1024;

/** Placeholder for an attachment that was not decoded (decodeBinaries: false). */
function skippedBinary(prot) {
  return { data: new Uint8Array(0), protected: prot, skipped: true };
}

/**
 * Charge decoded attachment bytes to the document's cumulative budget. The cap applies to all binaries
 * together (each one alone is also capped by maxBinaryBytes), so many small gzip bombs cannot add up to
 * gigabytes.
 */
function spendBudget(ctx, n) {
  ctx.budget.remaining -= n;
  if (ctx.budget.remaining < 0) throw new VaultError('limit_exceeded', 'attachments exceed the decompressed size limit');
}

function readEntry(node, ctx, inHistory) {
  const e = {
    uuid: parseUuid(childText(node, 'UUID')) || newKdbxUuid(),
    iconId: parseInt10(childText(node, 'IconID')),
    foregroundColor: childText(node, 'ForegroundColor'),
    backgroundColor: childText(node, 'BackgroundColor'),
    overrideUrl: childText(node, 'OverrideURL'),
    tags: parseTags(childText(node, 'Tags')),
    times: readTimes(firstChild(node, 'Times')),
    fields: [],
    binaries: [],
    autoType: null,
    customData: readCustomData(firstChild(node, 'CustomData')),
    history: [],
    extra: [],
  };
  const ci = parseUuid(childText(node, 'CustomIconUUID'));
  if (ci) e.customIconUuid = ci;
  const qc = firstChild(node, 'QualityCheck');
  if (qc) e.qualityCheck = parseBool(textOf(qc), true);
  const pp = parseUuid(childText(node, 'PreviousParentGroup'));
  if (pp) e.previousParentGroup = pp;
  const seen = new Set();
  for (const c of childElements(node)) {
    if (c.name === 'String') {
      const key = childText(c, 'Key');
      if (seen.has(key)) continue; // KeePass keys are unique; keep the first
      seen.add(key);
      const v = readValue(firstChild(c, 'Value'));
      e.fields.push({ key, value: v.value, protected: v.protected });
    } else if (c.name === 'Binary') {
      const key = childText(c, 'Key');
      const vn = firstChild(c, 'Value');
      if (!vn) continue;
      if (vn.attrs.Ref !== undefined) {
        const ref = ctx.binaryIndex.get(vn.attrs.Ref);
        if (ref !== undefined) e.binaries.push({ key, ref });
      } else {
        // Very old files stored attachment data inline.
        if (ctx.decodeBinaries) {
          const data = vn.protectedBytes || b64decode(textOf(vn).replace(/\s+/g, ''));
          spendBudget(ctx, data.length);
          ctx.binaries.push({ data, protected: !!vn.protectedBytes });
        } else {
          ctx.binaries.push(skippedBinary(!!vn.protectedBytes));
        }
        e.binaries.push({ key, ref: ctx.binaries.length - 1 });
      }
    } else if (c.name === 'AutoType') {
      e.autoType = rawIn(c);
    } else if (c.name === 'History' && !inHistory) {
      e.history = childElements(c, 'Entry').map((h) => readEntry(h, ctx, true));
    } else if (!ENTRY_KNOWN.has(c.name)) {
      e.extra.push(rawIn(c));
    }
  }
  return e;
}

function readGroup(node, ctx, depth) {
  if (depth > ctx.maxGroupDepth) throw new VaultError('limit_exceeded', 'group nesting too deep');
  const g = {
    uuid: parseUuid(childText(node, 'UUID')) || newKdbxUuid(),
    name: childText(node, 'Name'),
    notes: childText(node, 'Notes'),
    iconId: parseInt10(childText(node, 'IconID'), 48),
    times: readTimes(firstChild(node, 'Times')),
    isExpanded: parseBool(childText(node, 'IsExpanded'), true),
    defaultAutoTypeSequence: childText(node, 'DefaultAutoTypeSequence'),
    enableAutoType: childText(node, 'EnableAutoType') || 'null',
    enableSearching: childText(node, 'EnableSearching') || 'null',
    lastTopVisibleEntry: parseUuid(childText(node, 'LastTopVisibleEntry')) || ZERO_UUID,
    tags: parseTags(childText(node, 'Tags')),
    customData: readCustomData(firstChild(node, 'CustomData')),
    entries: [],
    groups: [],
    extra: [],
  };
  const ci = parseUuid(childText(node, 'CustomIconUUID'));
  if (ci) g.customIconUuid = ci;
  const pp = parseUuid(childText(node, 'PreviousParentGroup'));
  if (pp) g.previousParentGroup = pp;
  for (const c of childElements(node)) {
    if (c.name === 'Entry') g.entries.push(readEntry(c, ctx, false));
    else if (c.name === 'Group') g.groups.push(readGroup(c, ctx, depth + 1));
    else if (!GROUP_KNOWN.has(c.name)) g.extra.push(rawIn(c));
  }
  return g;
}

const META_SCALARS = [
  ['Generator', 'generator', 'text'],
  ['SettingsChanged', 'settingsChanged', 'time?'],
  ['DatabaseName', 'databaseName', 'text'],
  ['DatabaseNameChanged', 'databaseNameChanged', 'time'],
  ['DatabaseDescription', 'databaseDescription', 'text'],
  ['DatabaseDescriptionChanged', 'databaseDescriptionChanged', 'time'],
  ['DefaultUserName', 'defaultUserName', 'text'],
  ['DefaultUserNameChanged', 'defaultUserNameChanged', 'time'],
  ['MaintenanceHistoryDays', 'maintenanceHistoryDays', 'int'],
  ['Color', 'color', 'text'],
  ['MasterKeyChanged', 'masterKeyChanged', 'time'],
  ['MasterKeyChangeRec', 'masterKeyChangeRec', 'int'],
  ['MasterKeyChangeForce', 'masterKeyChangeForce', 'int'],
  ['MemoryProtection', null, 'memprot'],
  ['CustomIcons', null, 'icons'],
  ['RecycleBinEnabled', 'recycleBinEnabled', 'bool'],
  ['RecycleBinUUID', 'recycleBinUuid', 'uuid'],
  ['RecycleBinChanged', 'recycleBinChanged', 'time'],
  ['EntryTemplatesGroup', 'entryTemplatesGroup', 'uuid'],
  ['EntryTemplatesGroupChanged', 'entryTemplatesGroupChanged', 'time'],
  ['HistoryMaxItems', 'historyMaxItems', 'int'],
  ['HistoryMaxSize', 'historyMaxSize', 'int'],
  ['LastSelectedGroup', 'lastSelectedGroup', 'uuid'],
  ['LastTopVisibleGroup', 'lastTopVisibleGroup', 'uuid'],
  ['Binaries', null, 'binaries'],
  ['CustomData', null, 'customData'],
];
const META_KNOWN = new Set([...META_SCALARS.map((m) => m[0]), 'HeaderHash']);
const MEM_PROT = [
  ['ProtectTitle', 'title', false],
  ['ProtectUserName', 'userName', false],
  ['ProtectPassword', 'password', true],
  ['ProtectURL', 'url', false],
  ['ProtectNotes', 'notes', false],
];

/** Default Meta for new databases. */
export function defaultMeta(name = '') {
  const now = nowIso();
  return {
    generator: 'Ludworks',
    databaseName: name,
    databaseNameChanged: now,
    databaseDescription: '',
    databaseDescriptionChanged: now,
    defaultUserName: '',
    defaultUserNameChanged: now,
    maintenanceHistoryDays: 365,
    color: '',
    masterKeyChanged: now,
    masterKeyChangeRec: -1,
    masterKeyChangeForce: -1,
    memoryProtection: { title: false, userName: false, password: true, url: false, notes: false },
    customIcons: [],
    recycleBinEnabled: true,
    recycleBinUuid: ZERO_UUID,
    recycleBinChanged: now,
    entryTemplatesGroup: ZERO_UUID,
    entryTemplatesGroupChanged: now,
    historyMaxItems: 10,
    historyMaxSize: 6291456,
    lastSelectedGroup: ZERO_UUID,
    lastTopVisibleGroup: ZERO_UUID,
    customData: [],
    extra: [],
  };
}

async function readMeta(node, ctx) {
  const meta = defaultMeta();
  if (!node) return meta;
  for (const [tag, prop, kind] of META_SCALARS) {
    const c = firstChild(node, tag);
    if (!c) {
      if (kind === 'time?') delete meta[prop];
      continue;
    }
    const t = textOf(c);
    switch (kind) {
      case 'text':
        meta[prop] = t;
        break;
      case 'time':
      case 'time?':
        meta[prop] = parseKdbxTime(t);
        break;
      case 'int':
        meta[prop] = parseInt10(t, meta[prop]);
        break;
      case 'bool':
        meta[prop] = parseBool(t, meta[prop]);
        break;
      case 'uuid':
        meta[prop] = parseUuid(t) || ZERO_UUID;
        break;
      case 'memprot':
        for (const [mt, mp, d] of MEM_PROT) meta.memoryProtection[mp] = parseBool(childText(c, mt), d);
        break;
      case 'icons':
        meta.customIcons = childElements(c, 'Icon').map((ic) => {
          const icon = { uuid: parseUuid(childText(ic, 'UUID')) || newKdbxUuid(), data: b64decode(childText(ic, 'Data').replace(/\s+/g, '')) };
          const nm = firstChild(ic, 'Name');
          if (nm) icon.name = textOf(nm);
          const lm = firstChild(ic, 'LastModificationTime');
          if (lm) icon.lastModified = parseKdbxTime(textOf(lm));
          return icon;
        });
        break;
      case 'binaries':
        for (const b of childElements(c, 'Binary')) {
          const prot = !!b.protectedBytes || parseBool(b.attrs.ProtectInMemory);
          ctx.binaryIndex.set(b.attrs.ID ?? String(ctx.binaries.length), ctx.binaries.length);
          if (!ctx.decodeBinaries) {
            // Callers that drop attachments (vault import) only need to know they exist: nothing is
            // decoded or decompressed, so a file full of gzip bombs costs nothing.
            ctx.binaries.push(skippedBinary(prot));
            continue;
          }
          let data = b.protectedBytes || b64decode(textOf(b).replace(/\s+/g, ''));
          if (parseBool(b.attrs.Compressed)) data = await gunzip(data, Math.min(ctx.maxBinaryBytes, ctx.budget.remaining));
          spendBudget(ctx, data.length);
          ctx.binaries.push({ data, protected: prot });
        }
        break;
      case 'customData':
        meta.customData = readCustomData(c);
        break;
      default:
        break;
    }
  }
  for (const c of childElements(node)) if (!META_KNOWN.has(c.name)) meta.extra.push(rawIn(c));
  return meta;
}

/**
 * Convert a parsed KeePassFile element into a tree.
 * @param {import('./xml.js').XmlElement} doc
 * @param {{binaries?: Array<{data:Uint8Array, protected:boolean}>, maxBinaryBytes?: number, maxGroupDepth?: number,
 *          maxDecompressedBytes?: number, decodeBinaries?: boolean}} [opts]
 *   `binaries` = KDBX 4 inner-header pool (Ref="n" indexes it directly).
 *   `maxDecompressedBytes` = cumulative cap on all attachment data decoded from the XML (default 512 MiB);
 *   `maxBinaryBytes` = cap for one attachment. `decodeBinaries: false` records attachments without decoding
 *   or decompressing them (their `data` is empty and `skipped` is true) — for callers that drop them.
 * @returns {Promise<{tree: object, headerHash: string|null}>}
 */
export async function xmlToTree(doc, opts = {}) {
  if (doc.name !== 'KeePassFile') throw new VaultError('corrupt', 'XML root is not <KeePassFile>');
  const ctx = {
    binaries: opts.binaries ? [...opts.binaries] : [],
    binaryIndex: new Map(),
    maxBinaryBytes: opts.maxBinaryBytes ?? 256 * 1024 * 1024,
    maxGroupDepth: opts.maxGroupDepth ?? 200,
    decodeBinaries: opts.decodeBinaries !== false,
    budget: { remaining: Math.max(0, opts.maxDecompressedBytes ?? DEFAULT_MAX_DECOMPRESSED_BYTES) },
  };
  if (opts.binaries) opts.binaries.forEach((_, i) => ctx.binaryIndex.set(String(i), i));
  const metaNode = firstChild(doc, 'Meta');
  const meta = await readMeta(metaNode, ctx);
  const rootNode = firstChild(doc, 'Root');
  const groupNode = rootNode && firstChild(rootNode, 'Group');
  if (!groupNode) throw new VaultError('corrupt', 'document has no root group');
  const root = readGroup(groupNode, ctx, 0);
  const deletedObjects = [];
  const del = firstChild(rootNode, 'DeletedObjects');
  if (del) {
    for (const d of childElements(del, 'DeletedObject')) {
      const uuid = parseUuid(childText(d, 'UUID'));
      if (uuid) deletedObjects.push({ uuid, deletionTime: parseKdbxTime(childText(d, 'DeletionTime')) });
    }
  }
  const hh = metaNode && firstChild(metaNode, 'HeaderHash');
  return { tree: { meta, root, deletedObjects, binaries: ctx.binaries }, headerHash: hh ? textOf(hh).trim() : null };
}

// --- writing -------------------------------------------------------------------------------------

function writeTimes(t = {}, mode) {
  return el(
    'Times',
    null,
    el('CreationTime', null, formatKdbxTime(t.creationTime, mode)),
    el('LastModificationTime', null, formatKdbxTime(t.lastModificationTime, mode)),
    el('LastAccessTime', null, formatKdbxTime(t.lastAccessTime, mode)),
    el('ExpiryTime', null, formatKdbxTime(t.expiryTime, mode)),
    el('Expires', null, fmtBool(t.expires)),
    el('UsageCount', null, String(t.usageCount || 0)),
    el('LocationChanged', null, formatKdbxTime(t.locationChanged, mode)),
  );
}

function writeCustomData(items, mode) {
  if (!items || items.length === 0) return null;
  return el(
    'CustomData',
    null,
    items.map((it) =>
      el('Item', null, el('Key', null, it.key), el('Value', null, it.value ?? ''), it.lastModified ? el('LastModificationTime', null, formatKdbxTime(it.lastModified, mode)) : null),
    ),
  );
}

/** A value element; protected values are marked for kdbx.js to encrypt (or flagged in plain XML). */
function writeValue(value, isProtected, mode) {
  const text = value ?? '';
  if (!isProtected) return el('Value', null, text);
  if (mode === 'plain') return el('Value', { ProtectInMemory: 'True' }, text);
  const v = el('Value', { Protected: 'True' });
  v.protectedBytes = utf8Encode(text);
  return v;
}

function writeEntry(e, ctx, inHistory) {
  const { mode } = ctx;
  const kids = [
    el('UUID', null, formatUuid(e.uuid)),
    el('IconID', null, String(e.iconId ?? 0)),
    e.customIconUuid ? el('CustomIconUUID', null, formatUuid(e.customIconUuid)) : null,
    el('ForegroundColor', null, e.foregroundColor || ''),
    el('BackgroundColor', null, e.backgroundColor || ''),
    el('OverrideURL', null, e.overrideUrl || ''),
    e.qualityCheck === false ? el('QualityCheck', null, 'False') : null,
    el('Tags', null, (e.tags || []).join(';')),
    e.previousParentGroup ? el('PreviousParentGroup', null, formatUuid(e.previousParentGroup)) : null,
    writeTimes(e.times, mode),
  ];
  for (const f of e.fields || []) kids.push(el('String', null, el('Key', null, f.key), writeValue(f.value, f.protected, mode)));
  for (const b of e.binaries || []) {
    if (Number.isInteger(b.ref) && b.ref >= 0 && b.ref < ctx.binaryCount) kids.push(el('Binary', null, el('Key', null, b.key), el('Value', { Ref: String(b.ref) })));
  }
  kids.push(e.autoType ? rawOut(e.autoType, mode) : el('AutoType', null, el('Enabled', null, 'True'), el('DataTransferObfuscation', null, '0')));
  if (mode !== 'kdbx3') kids.push(writeCustomData(e.customData, mode));
  for (const x of e.extra || []) kids.push(rawOut(x, mode));
  if (!inHistory) kids.push(el('History', null, (e.history || []).map((h) => writeEntry(h, ctx, true))));
  return el('Entry', null, kids);
}

function writeGroup(g, ctx, depth) {
  const { mode } = ctx;
  if (depth > 200) throw new VaultError('limit_exceeded', 'group nesting too deep');
  return el(
    'Group',
    null,
    el('UUID', null, formatUuid(g.uuid)),
    el('Name', null, g.name ?? ''),
    el('Notes', null, g.notes ?? ''),
    el('IconID', null, String(g.iconId ?? 48)),
    g.customIconUuid ? el('CustomIconUUID', null, formatUuid(g.customIconUuid)) : null,
    writeTimes(g.times, mode),
    el('IsExpanded', null, fmtBool(g.isExpanded ?? true)),
    el('DefaultAutoTypeSequence', null, g.defaultAutoTypeSequence ?? ''),
    el('EnableAutoType', null, g.enableAutoType || 'null'),
    el('EnableSearching', null, g.enableSearching || 'null'),
    el('LastTopVisibleEntry', null, formatUuid(g.lastTopVisibleEntry)),
    g.previousParentGroup ? el('PreviousParentGroup', null, formatUuid(g.previousParentGroup)) : null,
    g.tags && g.tags.length ? el('Tags', null, g.tags.join(';')) : null,
    mode !== 'kdbx3' ? writeCustomData(g.customData, mode) : null,
    (g.extra || []).map((x) => rawOut(x, mode)),
    (g.entries || []).map((e) => writeEntry(e, ctx, false)),
    (g.groups || []).map((c) => writeGroup(c, ctx, depth + 1)),
  );
}

function writeMeta(metaIn, ctx, headerHash) {
  const { mode } = ctx;
  const meta = { ...defaultMeta(), ...metaIn };
  const kids = [];
  for (const [tag, prop, kind] of META_SCALARS) {
    if (tag === 'SettingsChanged' && headerHash && mode === 'kdbx3') kids.push(el('HeaderHash', null, headerHash));
    switch (kind) {
      case 'text':
        kids.push(el(tag, null, meta[prop] ?? ''));
        break;
      case 'time':
        kids.push(el(tag, null, formatKdbxTime(meta[prop], mode)));
        break;
      case 'time?':
        if (meta[prop]) kids.push(el(tag, null, formatKdbxTime(meta[prop], mode)));
        break;
      case 'int':
        kids.push(el(tag, null, String(meta[prop] ?? 0)));
        break;
      case 'bool':
        kids.push(el(tag, null, fmtBool(meta[prop])));
        break;
      case 'uuid':
        kids.push(el(tag, null, formatUuid(meta[prop])));
        break;
      case 'memprot': {
        const mp = { ...defaultMeta().memoryProtection, ...meta.memoryProtection };
        kids.push(el(tag, null, MEM_PROT.map(([mt, p]) => el(mt, null, fmtBool(mp[p])))));
        break;
      }
      case 'icons':
        if (meta.customIcons && meta.customIcons.length) {
          kids.push(
            el(
              tag,
              null,
              meta.customIcons.map((ic) =>
                el(
                  'Icon',
                  null,
                  el('UUID', null, formatUuid(ic.uuid)),
                  el('Data', null, b64encode(ic.data)),
                  ic.name !== undefined ? el('Name', null, ic.name) : null,
                  ic.lastModified ? el('LastModificationTime', null, formatKdbxTime(ic.lastModified, mode)) : null,
                ),
              ),
            ),
          );
        }
        break;
      case 'binaries':
        // KDBX 4 keeps attachments in the inner header; KDBX 3 and plain XML embed them here.
        if (mode !== 'kdbx4' && ctx.binaries.length) {
          kids.push(
            el(
              tag,
              null,
              ctx.binaries.map((b, i) => {
                if (b.protected && mode === 'kdbx3') {
                  const n = el('Binary', { ID: String(i), Protected: 'True' });
                  n.protectedBytes = b.data;
                  return n;
                }
                return el('Binary', { ID: String(i), ProtectInMemory: b.protected ? 'True' : undefined }, b64encode(b.data));
              }),
            ),
          );
        }
        break;
      case 'customData': {
        const cd = writeCustomData(meta.customData, mode);
        if (cd) kids.push(cd);
        break;
      }
      default:
        break;
    }
  }
  for (const x of meta.extra || []) kids.push(rawOut(x, mode));
  return el('Meta', null, kids);
}

/**
 * Build the KeePassFile element for a tree.
 * @param {object} tree KdbxTree
 * @param {{mode: 'kdbx4'|'kdbx3'|'plain', headerHash?: string}} opts
 * @returns {import('./xml.js').XmlElement}
 */
export function treeToXml(tree, { mode, headerHash } = {}) {
  if (!['kdbx4', 'kdbx3', 'plain'].includes(mode)) throw new VaultError('invalid_argument', 'treeToXml: unknown mode');
  if (!tree || !tree.root) throw new VaultError('invalid_argument', 'tree has no root group');
  const binaries = tree.binaries || [];
  const ctx = { mode, binaries, binaryCount: binaries.length };
  const root = el(
    'Root',
    null,
    writeGroup(tree.root, ctx, 0),
    el(
      'DeletedObjects',
      null,
      (tree.deletedObjects || []).map((d) => el('DeletedObject', null, el('UUID', null, formatUuid(d.uuid)), el('DeletionTime', null, formatKdbxTime(d.deletionTime, mode)))),
    ),
  );
  return el('KeePassFile', null, writeMeta(tree.meta || {}, ctx, headerHash), root);
}

// --- public plain-XML API ------------------------------------------------------------------------

/**
 * Parse an unencrypted KeePass 2 XML export.
 * @param {string|Uint8Array} input
 * @param {{xmlLimits?: object, maxDecompressedBytes?: number, decodeBinaries?: boolean}} [opts]
 *   `maxDecompressedBytes`: cumulative cap on decoded attachment data (default 512 MiB);
 *   `decodeBinaries: false`: do not decode attachments at all (see xmlToTree).
 * @returns {Promise<object>} KdbxTree
 */
export async function parseKeePassXml(input, opts = {}) {
  const text = typeof input === 'string' ? input : utf8Decode(input);
  const doc = parseXml(text, opts.xmlLimits);
  return (await xmlToTree(doc, { maxDecompressedBytes: opts.maxDecompressedBytes, decodeBinaries: opts.decodeBinaries })).tree;
}

/**
 * Serialize a tree as an unencrypted KeePass 2 XML export (contains plaintext secrets!).
 * @param {object} tree KdbxTree
 * @returns {string}
 */
export function serializeKeePassXml(tree) {
  return serializeXml(treeToXml(tree, { mode: 'plain' }));
}

// --- tree construction helpers (used by convert.js) ----------------------------------------------

/** New Times object; all timestamps default to now. */
export function newTimes(created, modified) {
  const now = nowIso();
  const c = created || now;
  const m = modified || c;
  return { creationTime: c, lastModificationTime: m, lastAccessTime: m, expiryTime: c, expires: false, usageCount: 0, locationChanged: m };
}

/** New empty group. */
export function newGroup(name, iconId = 48) {
  return {
    uuid: newKdbxUuid(), name, notes: '', iconId, times: newTimes(), isExpanded: true, defaultAutoTypeSequence: '',
    enableAutoType: 'null', enableSearching: 'null', lastTopVisibleEntry: ZERO_UUID, tags: [], customData: [],
    entries: [], groups: [], extra: [],
  };
}

/** New empty entry. */
export function newEntry() {
  return {
    uuid: newKdbxUuid(), iconId: 0, foregroundColor: '', backgroundColor: '', overrideUrl: '', tags: [],
    times: newTimes(), fields: [], binaries: [], autoType: null, customData: [], history: [], extra: [],
  };
}

/** New empty database tree with a root group named `name`. */
export function newTree(name = 'Database') {
  return { meta: defaultMeta(name), root: newGroup(name, 49), deletedObjects: [], binaries: [] };
}
