// Files app — spaces + lazily loaded folder tree (sidebar and the move/copy picker).
// Keyboard: ↑/↓ move, → expand / first child, ← collapse / parent, Enter/Space select.

import { h, cls } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatBytes } from '../../core/format.js';
import { listFolders } from './fsapi.js';
import { joinPath, isWithin } from './model.js';

/**
 * @param {{fapi: object, onSelect: (spaceId: string, path: string) => void, compact?: boolean,
 *          showUsage?: boolean, label?: string, signal?: AbortSignal, onError?: (err: any) => void}} o
 */
export function createFolderTree(o) {
  const { fapi, onSelect, showUsage = false, label = 'Folders', signal, onError = () => {} } = o;
  const root = h('div', { class: 'fx-tree', role: 'tree', aria: { label } });
  /** key "space\u0000path" → node */
  const nodes = new Map();
  let active = { spaceId: '', path: '' };
  let focusKey = null;
  const key = (spaceId, path) => `${spaceId}\u0000${path}`;

  function makeNode(spaceId, path, labelText, level, { iconName = 'folder', space = null } = {}) {
    const k = key(spaceId, path);
    const toggle = h('span', { class: 'fx-tree-toggle', aria: { hidden: 'true' } }, icon('chevron-right', 14));
    const usage = space && showUsage ? h('span', { class: 'fx-tree-usage', title: 'Used' }, formatBytes(space.usage_bytes || 0)) : null;
    const row = h('div', {
      class: cls('fx-tree-row', { 'is-space': !!space }), role: 'treeitem', tabindex: '-1',
      aria: { level: String(level), expanded: 'false', selected: 'false', label: labelText },
      dataset: { space: spaceId, path, drop: '1' },
    },
    toggle, icon(iconName, { size: 16, className: 'fx-tree-icon' }),
    h('span', { class: 'fx-tree-label' }, labelText),
    space && space.access === 'read' ? h('span', { class: 'fx-tree-badge', title: 'Read only' }, icon('lock', 12)) : null,
    usage);
    row.style.setProperty('--level', String(level - 1));
    const children = h('div', { class: 'fx-tree-children', role: 'group', hidden: true });
    const el = h('div', { class: 'fx-tree-node' }, row, children);
    const node = { k, spaceId, path, level, el, row, children, toggle, loaded: false, loading: null, expanded: false, empty: false };
    nodes.set(k, node);
    toggle.addEventListener('click', (e) => {
      e.stopPropagation();
      setExpanded(node, !node.expanded);
    });
    row.addEventListener('click', () => {
      focusNode(node);
      onSelect(spaceId, path);
    });
    row.addEventListener('dblclick', () => setExpanded(node, !node.expanded));
    return node;
  }

  async function load(node, force = false) {
    if (node.loaded && !force) return;
    if (node.loading) return node.loading;
    node.row.classList.add('is-loading');
    node.loading = (async () => {
      try {
        const dirs = await listFolders(fapi, node.spaceId, node.path, signal);
        // Drop stale descendants before re-rendering.
        for (const k of [...nodes.keys()]) {
          const [s, p] = k.split('\u0000');
          if (s === node.spaceId && p !== node.path && isWithin(p, node.path) && (node.path !== '' || p !== '')) nodes.delete(k);
        }
        node.children.replaceChildren(...dirs.map((d) => makeNode(node.spaceId, joinPath(node.path, d.name), d.name, node.level + 1).el));
        node.loaded = true;
        node.empty = dirs.length === 0;
        node.row.classList.toggle('is-leaf', node.empty);
      } catch (err) {
        if (err?.name !== 'AbortError') onError(err);
      } finally {
        node.row.classList.remove('is-loading');
        node.loading = null;
      }
    })();
    return node.loading;
  }

  async function setExpanded(node, open) {
    node.expanded = open;
    node.row.setAttribute('aria-expanded', String(open));
    node.row.classList.toggle('is-open', open);
    node.children.hidden = !open;
    if (open) await load(node);
  }

  function focusNode(node, { focus = true } = {}) {
    for (const n of nodes.values()) n.row.tabIndex = -1;
    node.row.tabIndex = 0;
    focusKey = node.k;
    if (focus) node.row.focus({ preventScroll: false });
  }

  function visibleRows() {
    return [...root.querySelectorAll('.fx-tree-row')].filter((r) => !r.closest('[hidden]'));
  }

  root.addEventListener('keydown', (e) => {
    const row = e.target.closest?.('.fx-tree-row');
    if (!row) return;
    const node = nodes.get(key(row.dataset.space, row.dataset.path));
    if (!node) return;
    const rows = visibleRows();
    const i = rows.indexOf(row);
    const go = (r) => {
      if (!r) return;
      const n = nodes.get(key(r.dataset.space, r.dataset.path));
      if (n) focusNode(n);
    };
    switch (e.key) {
      case 'ArrowDown': e.preventDefault(); go(rows[i + 1]); break;
      case 'ArrowUp': e.preventDefault(); go(rows[i - 1]); break;
      case 'Home': e.preventDefault(); go(rows[0]); break;
      case 'End': e.preventDefault(); go(rows[rows.length - 1]); break;
      case 'ArrowRight':
        e.preventDefault();
        if (!node.expanded) setExpanded(node, true);
        else go(node.children.querySelector('.fx-tree-row'));
        break;
      case 'ArrowLeft': {
        e.preventDefault();
        if (node.expanded) setExpanded(node, false);
        else go(row.parentElement.parentElement.closest('.fx-tree-node')?.querySelector('.fx-tree-row'));
        break;
      }
      case 'Enter':
      case ' ':
        e.preventDefault();
        onSelect(node.spaceId, node.path);
        break;
      default:
    }
  });

  const api = {
    el: root,
    /** Render the spaces as top-level nodes. */
    setSpaces(spaces) {
      nodes.clear();
      root.replaceChildren(...spaces.map((sp) => {
        const n = makeNode(sp.id, '', sp.kind === 'home' ? 'Home' : sp.name, 1, { iconName: sp.kind === 'home' ? 'home' : 'library', space: sp });
        return n.el;
      }));
      api.setActive(active.spaceId, active.path, { reveal: true });
    },
    /** Highlight the current folder and expand its ancestors. */
    async setActive(spaceId, path, { reveal = true } = {}) {
      active = { spaceId, path };
      for (const n of nodes.values()) {
        const on = n.spaceId === spaceId && n.path === path;
        n.row.setAttribute('aria-selected', String(on));
        n.row.classList.toggle('is-active', on);
      }
      if (!reveal || !spaceId) return;
      const parts = path ? path.split('/') : [];
      let cur = nodes.get(key(spaceId, ''));
      if (!cur) return;
      if (!cur.expanded && !parts.length) await setExpanded(cur, true);
      for (let i = 0; i <= parts.length; i++) {
        if (i < parts.length) {
          if (!cur.expanded) await setExpanded(cur, true);
          const next = nodes.get(key(spaceId, parts.slice(0, i + 1).join('/')));
          if (!next) break;
          cur = next;
        }
      }
      const target = nodes.get(key(spaceId, path));
      if (target) {
        target.row.setAttribute('aria-selected', 'true');
        target.row.classList.add('is-active');
        if (!focusKey || !nodes.has(focusKey)) focusNode(target, { focus: false });
        target.row.scrollIntoView?.({ block: 'nearest' });
      }
    },
    /** Reload the children of a folder (after mkdir/move/delete). */
    async refresh(spaceId, path) {
      const n = nodes.get(key(spaceId, path));
      if (n && n.loaded) await load(n, true);
    },
    /** Update usage numbers of spaces. */
    updateSpaces(spaces) {
      for (const sp of spaces) {
        const n = nodes.get(key(sp.id, ''));
        const u = n?.row.querySelector('.fx-tree-usage');
        if (u) u.textContent = formatBytes(sp.usage_bytes || 0);
      }
    },
    /** Expand a space root (used by the picker). */
    async expand(spaceId, path = '') {
      const n = nodes.get(key(spaceId, path));
      if (n) await setExpanded(n, true);
    },
    get active() {
      return { ...active };
    },
  };
  return api;
}
