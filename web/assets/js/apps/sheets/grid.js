// Virtualized spreadsheet grid drawn on a <canvas>: only the visible cells are painted, so
// 100k+ rows scroll smoothly. Native scrolling comes from a transparent scroller with a
// spacer sized to the sheet; the canvas repaints on scroll. Frozen panes, headers,
// selection, fill handle, formula-reference highlights, copy marquee and find matches are
// all drawn here. Pointer input is reported to the editor as hit-tests.

import { h } from '../../core/dom.js';
import { colName, cellKey } from './a1.js';

export const COL_HEADER_H = 24;
const MAX_SCROLL = 8_000_000; // keep scrollable size well below browser limits
const RESIZE_SLOP = 4;
const REF_COLORS = ['#1a73e8', '#d93025', '#188038', '#a142f4', '#e8710a', '#0097a7', '#c2185b'];

/** Colour of a formula reference by index. */
export const refColor = (i) => REF_COLORS[i % REF_COLORS.length];

function luminance(hex) {
  const m = /^#([0-9a-f]{6})$/i.exec(hex || '');
  if (!m) return 1;
  const n = parseInt(m[1], 16);
  const ch = [(n >> 16) & 255, (n >> 8) & 255, n & 255].map((v) => {
    const c = v / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * ch[0] + 0.7152 * ch[1] + 0.0722 * ch[2];
}

export class Grid {
  /**
   * @param {{onPointer: Function, onDblClick: Function, onContextMenu: Function, onResizeEnd: Function,
   *          onFillEnd: Function, onScroll?: Function}} handlers
   */
  constructor(handlers) {
    this.handlers = handlers;
    this.sheet = null;
    this.sel = null;
    this.highlights = [];
    this.copyRange = null;
    this.find = null; // {keys:Set, current:key}
    this.fillPreview = null;
    this.resizePreview = null;
    this.textCache = new Map();
    this.dpr = 1;
    this.theme = null;
    this.rowHeaderW = 46;

    this.canvas = h('canvas', { class: 'sh-canvas', aria: { hidden: 'true' } });
    this.spacer = h('div', { class: 'sh-spacer' });
    this.scroller = h('div', { class: 'sh-scroller' }, this.spacer);
    this.el = h('div', { class: 'sh-grid' }, this.canvas, this.scroller);
    this.ctx = this.canvas.getContext('2d');

    this.scroller.addEventListener('scroll', () => {
      this.invalidate();
      this.handlers.onScroll?.();
    }, { passive: true });
    this.scroller.addEventListener('pointerdown', (e) => this.pointerDown(e));
    this.scroller.addEventListener('pointermove', (e) => this.hover(e));
    this.scroller.addEventListener('dblclick', (e) => this.dblclick(e));
    this.scroller.addEventListener('contextmenu', (e) => {
      const hit = this.hitTest(e);
      if (hit) this.handlers.onContextMenu?.(hit, e);
    });
    this.ro = new ResizeObserver(() => this.layout());
    this.ro.observe(this.el);
    this.themeObs = new MutationObserver(() => { this.theme = null; this.invalidate(); });
    this.themeObs.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme', 'class'] });
    this.mql = window.matchMedia('(prefers-color-scheme: dark)');
    this.onMql = () => { this.theme = null; this.invalidate(); };
    this.mql.addEventListener('change', this.onMql);
  }

  destroy() {
    this.ro.disconnect();
    this.themeObs.disconnect();
    this.mql.removeEventListener('change', this.onMql);
    cancelAnimationFrame(this.raf);
    cancelAnimationFrame(this.autoRaf);
  }

  /* ----------------------------------------------------------------- state */

  setSheet(sheet) {
    this.sheet = sheet;
    this.textCache.clear();
    this.updateSpacer();
    this.invalidate();
  }

  setSelection(sel) {
    this.sel = sel;
    this.invalidate();
  }

  setHighlights(list) {
    this.highlights = list || [];
    this.invalidate();
  }

  setCopyRange(r) {
    this.copyRange = r;
    this.invalidate();
  }

  setFind(find) {
    this.find = find;
    this.invalidate();
  }

  /** Geometry changed (sizes, frozen panes, row/col counts). */
  refresh() {
    this.updateSpacer();
    this.invalidate();
  }

  invalidate() {
    if (this.raf) return;
    this.raf = requestAnimationFrame(() => {
      this.raf = 0;
      this.draw();
    });
  }

  layout() {
    const w = this.el.clientWidth;
    const hgt = this.el.clientHeight;
    this.dpr = window.devicePixelRatio || 1;
    this.viewW = this.scroller.clientWidth || w;
    this.viewH = this.scroller.clientHeight || hgt;
    this.canvas.width = Math.max(1, Math.round(this.viewW * this.dpr));
    this.canvas.height = Math.max(1, Math.round(this.viewH * this.dpr));
    this.canvas.style.width = `${this.viewW}px`;
    this.canvas.style.height = `${this.viewH}px`;
    this.updateSpacer();
    this.draw();
  }

  updateSpacer() {
    const sh = this.sheet;
    if (!sh) return;
    const digits = String(sh.rows).length;
    this.rowHeaderW = Math.max(40, 14 + digits * 8);
    const contentW = this.rowHeaderW + sh.colAxis.total();
    const contentH = COL_HEADER_H + sh.rowAxis.total();
    this.contentW = contentW;
    this.contentH = contentH;
    this.spacer.style.width = `${Math.min(contentW, MAX_SCROLL)}px`;
    this.spacer.style.height = `${Math.min(contentH, MAX_SCROLL)}px`;
  }

  // Map scroll positions to content offsets (scaled when the sheet exceeds MAX_SCROLL).
  get sx() {
    const s = this.scroller.scrollLeft;
    if (this.contentW <= MAX_SCROLL) return s;
    const maxS = MAX_SCROLL - this.viewW;
    return maxS > 0 ? (s / maxS) * (this.contentW - this.viewW) : 0;
  }

  get sy() {
    const s = this.scroller.scrollTop;
    if (this.contentH <= MAX_SCROLL) return s;
    const maxS = MAX_SCROLL - this.viewH;
    return maxS > 0 ? (s / maxS) * (this.contentH - this.viewH) : 0;
  }

  setScroll(x, y) {
    const toS = (v, content, view) => (content <= MAX_SCROLL ? v : (v / Math.max(1, content - view)) * (MAX_SCROLL - view));
    if (x !== null) this.scroller.scrollLeft = toS(x, this.contentW, this.viewW);
    if (y !== null) this.scroller.scrollTop = toS(y, this.contentH, this.viewH);
  }

  frozenW() { return this.sheet.colAxis.pos(Math.min(this.sheet.frozenCols, this.sheet.cols)); }

  frozenH() { return this.sheet.rowAxis.pos(Math.min(this.sheet.frozenRows, this.sheet.rows)); }

  /** Screen x of column c's left edge (relative to the grid). */
  colX(c) {
    const p = this.sheet.colAxis.pos(c);
    return this.rowHeaderW + p - (c >= this.sheet.frozenCols ? this.sx : 0);
  }

  rowY(r) {
    const p = this.sheet.rowAxis.pos(r);
    return COL_HEADER_H + p - (r >= this.sheet.frozenRows ? this.sy : 0);
  }

  /** Screen rectangle of a cell (may be partly hidden). */
  cellRect(row, col) {
    return { x: this.colX(col), y: this.rowY(row), w: this.sheet.colAxis.size(col), h: this.sheet.rowAxis.size(row) };
  }

  /** Scroll so that (row, col) is fully visible (outside frozen panes). */
  scrollIntoView(row, col) {
    const sh = this.sheet;
    if (!sh) return;
    const fw = this.frozenW();
    const fh = this.frozenH();
    let x = null;
    let y = null;
    if (col >= sh.frozenCols) {
      const left = sh.colAxis.pos(col) - fw;
      const right = sh.colAxis.pos(col + 1) - fw;
      const viewMain = this.viewW - this.rowHeaderW - fw;
      if (left < this.sx) x = left;
      else if (right > this.sx + viewMain) x = right - viewMain;
    }
    if (row >= sh.frozenRows) {
      const top = sh.rowAxis.pos(row) - fh;
      const bottom = sh.rowAxis.pos(row + 1) - fh;
      const viewMain = this.viewH - COL_HEADER_H - fh;
      if (top < this.sy) y = top;
      else if (bottom > this.sy + viewMain) y = bottom - viewMain;
    }
    if (x !== null || y !== null) this.setScroll(x === null ? null : Math.max(0, x), y === null ? null : Math.max(0, y));
  }

  /** Number of fully visible scrollable rows (for PageUp/PageDown). */
  pageRows() {
    return Math.max(1, Math.floor((this.viewH - COL_HEADER_H - this.frozenH()) / 21) - 1);
  }

  /* ----------------------------------------------------------- hit testing */

  hitTest(e) {
    if (!this.sheet) return null;
    const rect = this.scroller.getBoundingClientRect();
    return this.hitAt(e.clientX - rect.left, e.clientY - rect.top);
  }

  hitAt(x, y) {
    const sh = this.sheet;
    const fw = this.frozenW();
    const fh = this.frozenH();
    const col = x - this.rowHeaderW < fw ? sh.colAxis.indexAt(x - this.rowHeaderW) : sh.colAxis.indexAt(x - this.rowHeaderW + this.sx);
    const row = y - COL_HEADER_H < fh ? sh.rowAxis.indexAt(y - COL_HEADER_H) : sh.rowAxis.indexAt(y - COL_HEADER_H + this.sy);
    let area = 'cell';
    if (x < this.rowHeaderW && y < COL_HEADER_H) area = 'corner';
    else if (y < COL_HEADER_H) area = 'colhead';
    else if (x < this.rowHeaderW) area = 'rowhead';
    const hit = { area, row, col, x, y };
    if (area === 'colhead') {
      const right = this.colX(col) + sh.colAxis.size(col);
      const left = this.colX(col);
      if (Math.abs(x - right) <= RESIZE_SLOP) hit.resize = { axis: 'col', index: col };
      else if (Math.abs(x - left) <= RESIZE_SLOP && col > 0) hit.resize = { axis: 'col', index: col - 1 };
    } else if (area === 'rowhead') {
      const bottom = this.rowY(row) + sh.rowAxis.size(row);
      const top = this.rowY(row);
      if (Math.abs(y - bottom) <= RESIZE_SLOP) hit.resize = { axis: 'row', index: row };
      else if (Math.abs(y - top) <= RESIZE_SLOP && row > 0) hit.resize = { axis: 'row', index: row - 1 };
    } else if (area === 'cell' && this.sel) {
      const s = this.sel;
      const hx = this.colX(s.c1) + sh.colAxis.size(s.c1);
      const hy = this.rowY(s.r1) + sh.rowAxis.size(s.r1);
      if (Math.abs(x - hx) <= 5 && Math.abs(y - hy) <= 5) hit.fill = true;
    }
    return hit;
  }

  hover(e) {
    if (this.drag || e.pointerType === 'touch') return;
    const hit = this.hitTest(e);
    let cursor = 'cell';
    if (!hit) cursor = 'default';
    else if (hit.resize) cursor = hit.resize.axis === 'col' ? 'col-resize' : 'row-resize';
    else if (hit.fill) cursor = 'crosshair';
    else if (hit.area === 'colhead' || hit.area === 'rowhead' || hit.area === 'corner') cursor = 'default';
    if (this.scroller.style.cursor !== cursor) this.scroller.style.cursor = cursor;
  }

  dblclick(e) {
    const hit = this.hitTest(e);
    if (!hit) return;
    if (hit.resize) {
      this.handlers.onAutoFit?.(hit.resize);
      return;
    }
    if (hit.area === 'cell') this.handlers.onDblClick?.(hit, e);
  }

  /* ------------------------------------------------------- pointer drags */

  pointerDown(e) {
    if (!this.sheet || (e.button !== 0 && e.pointerType === 'mouse')) return;
    const hit = this.hitTest(e);
    if (!hit) return;
    // Clicks on the native scrollbars land on the scroller outside the client area.
    if (hit.x > this.viewW || hit.y > this.viewH) return;
    if (e.pointerType === 'touch') {
      // Let the browser scroll; treat a tap as a click.
      const sx = e.clientX;
      const sy = e.clientY;
      const up = (ev) => {
        this.scroller.removeEventListener('pointerup', up);
        if (Math.hypot(ev.clientX - sx, ev.clientY - sy) < 8) {
          this.handlers.onPointer?.('down', hit, e);
          this.handlers.onPointer?.('up', hit, ev);
        }
      };
      this.scroller.addEventListener('pointerup', up);
      return;
    }
    e.preventDefault();
    this.handlers.onFocus?.();
    let kind = 'select';
    if (hit.resize) kind = 'resize';
    else if (hit.fill) kind = 'fill';
    this.drag = { kind, start: hit, last: hit, pointerId: e.pointerId };
    this.scroller.setPointerCapture(e.pointerId);
    if (kind === 'resize') {
      const ax = hit.resize.axis === 'col' ? this.sheet.colAxis : this.sheet.rowAxis;
      this.drag.orig = ax.size(hit.resize.index);
      this.drag.origPos = hit.resize.axis === 'col' ? e.clientX : e.clientY;
    } else if (kind === 'select') {
      this.handlers.onPointer?.('down', hit, e);
    }
    const move = (ev) => this.dragMove(ev);
    const up = (ev) => {
      this.scroller.removeEventListener('pointermove', move);
      this.scroller.removeEventListener('pointerup', up);
      this.scroller.removeEventListener('pointercancel', up);
      cancelAnimationFrame(this.autoRaf);
      this.autoRaf = 0;
      this.dragEnd(ev);
    };
    this.scroller.addEventListener('pointermove', move);
    this.scroller.addEventListener('pointerup', up);
    this.scroller.addEventListener('pointercancel', up);
  }

  dragMove(e) {
    const d = this.drag;
    if (!d) return;
    if (d.kind === 'resize') {
      const pos = d.start.resize.axis === 'col' ? e.clientX : e.clientY;
      const size = Math.max(4, Math.min(4000, d.orig + pos - d.origPos));
      const ax = d.start.resize.axis === 'col' ? this.sheet.colAxis : this.sheet.rowAxis;
      ax.setSize(d.start.resize.index, size);
      d.size = size;
      this.updateSpacer();
      this.invalidate();
      return;
    }
    this.lastMoveEvent = e;
    this.applyDragMove(e);
    this.autoScroll();
  }

  applyDragMove(e) {
    const d = this.drag;
    const rect = this.scroller.getBoundingClientRect();
    const x = Math.max(this.rowHeaderW + 1, Math.min(this.viewW - 1, e.clientX - rect.left));
    const y = Math.max(COL_HEADER_H + 1, Math.min(this.viewH - 1, e.clientY - rect.top));
    const hit = this.hitAt(d.start.area === 'rowhead' ? Math.max(x, this.rowHeaderW + 1) : x, y);
    hit.area = d.start.area;
    d.last = hit;
    if (d.kind === 'fill') {
      this.fillPreview = this.fillTarget(hit);
      this.invalidate();
    } else this.handlers.onPointer?.('move', hit, e);
  }

  // Extend the selection along the dominant axis toward the pointer.
  fillTarget(hit) {
    const s = this.sel;
    const dr = hit.row < s.r0 ? hit.row - s.r0 : hit.row > s.r1 ? hit.row - s.r1 : 0;
    const dc = hit.col < s.c0 ? hit.col - s.c0 : hit.col > s.c1 ? hit.col - s.c1 : 0;
    if (!dr && !dc) return null;
    if (Math.abs(dr) >= Math.abs(dc)) {
      return dr > 0 ? { r0: s.r0, c0: s.c0, r1: hit.row, c1: s.c1 } : { r0: hit.row, c0: s.c0, r1: s.r1, c1: s.c1 };
    }
    return dc > 0 ? { r0: s.r0, c0: s.c0, r1: s.r1, c1: hit.col } : { r0: s.r0, c0: hit.col, r1: s.r1, c1: s.c1 };
  }

  autoScroll() {
    if (this.autoRaf) return;
    const step = () => {
      this.autoRaf = 0;
      const e = this.lastMoveEvent;
      if (!this.drag || !e) return;
      const rect = this.scroller.getBoundingClientRect();
      const x = e.clientX - rect.left;
      const y = e.clientY - rect.top;
      let dx = 0;
      let dy = 0;
      if (x > this.viewW - 10) dx = Math.min(40, x - this.viewW + 10);
      else if (x < this.rowHeaderW + this.frozenW() && this.drag.start.area !== 'rowhead') dx = -Math.min(40, this.rowHeaderW + this.frozenW() - x);
      if (y > this.viewH - 10) dy = Math.min(40, y - this.viewH + 10);
      else if (y < COL_HEADER_H + this.frozenH() && this.drag.start.area !== 'colhead') dy = -Math.min(40, COL_HEADER_H + this.frozenH() - y);
      if (!dx && !dy) return;
      this.scroller.scrollLeft += dx;
      this.scroller.scrollTop += dy;
      this.applyDragMove(e);
      this.autoRaf = requestAnimationFrame(step);
    };
    this.autoRaf = requestAnimationFrame(step);
  }

  dragEnd(e) {
    const d = this.drag;
    this.drag = null;
    if (!d) return;
    if (d.kind === 'resize') {
      if (d.size !== undefined && d.size !== d.orig) this.handlers.onResizeEnd?.(d.start.resize, d.size, d.orig);
      return;
    }
    if (d.kind === 'fill') {
      const target = this.fillPreview;
      this.fillPreview = null;
      this.invalidate();
      if (target) this.handlers.onFillEnd?.(target);
      return;
    }
    this.handlers.onPointer?.('up', d.last, e);
  }

  /* -------------------------------------------------------------- drawing */

  readTheme() {
    const cs = getComputedStyle(this.el);
    const v = (n, d) => cs.getPropertyValue(n).trim() || d;
    this.theme = {
      bg: v('--sh-cell-bg', '#ffffff'),
      grid: v('--sh-grid', '#e3e3e8'),
      text: v('--sh-text', '#17171c'),
      headBg: v('--sh-head-bg', '#f4f4f6'),
      headFg: v('--sh-head-fg', '#55555f'),
      headActiveBg: v('--sh-head-active-bg', '#e4e4f7'),
      headActiveFg: v('--sh-head-active-fg', '#3b3bb3'),
      headFullBg: v('--sh-head-full-bg', '#5b5bd6'),
      headFullFg: v('--sh-head-full-fg', '#ffffff'),
      headLine: v('--sh-head-line', '#d4d4dc'),
      selFill: v('--sh-sel-fill', 'rgba(91,91,214,0.10)'),
      selLine: v('--sh-sel-line', '#5b5bd6'),
      frozen: v('--sh-frozen', '#b9b9c4'),
      findFill: v('--sh-find-fill', 'rgba(250,204,21,0.35)'),
      findLine: v('--sh-find-line', '#e8710a'),
      border: v('--sh-cell-border', '#3f3f46'),
      font: v('--font-sans', 'system-ui, sans-serif'),
      dark: document.documentElement.dataset.theme === 'dark'
        || (document.documentElement.dataset.theme !== 'light' && window.matchMedia('(prefers-color-scheme: dark)').matches),
    };
  }

  draw() {
    const sh = this.sheet;
    const ctx = this.ctx;
    if (!sh || !this.viewW) return;
    if (!this.theme) this.readTheme();
    const t = this.theme;
    ctx.setTransform(this.dpr, 0, 0, this.dpr, 0, 0);
    ctx.fillStyle = t.bg;
    ctx.fillRect(0, 0, this.viewW, this.viewH);
    ctx.textBaseline = 'alphabetic';

    const RH = this.rowHeaderW;
    const fw = this.frozenW();
    const fh = this.frozenH();
    const sx = this.sx;
    const sy = this.sy;
    const fc = Math.min(sh.frozenCols, sh.cols);
    const fr = Math.min(sh.frozenRows, sh.rows);
    // visible index ranges of the scrollable parts
    const c0 = Math.max(fc, sh.colAxis.indexAt(fw + sx));
    const c1 = sh.colAxis.indexAt(sx + this.viewW - RH);
    const r0 = Math.max(fr, sh.rowAxis.indexAt(fh + sy));
    const r1 = sh.rowAxis.indexAt(sy + this.viewH - COL_HEADER_H);
    const quads = [
      { rows: [r0, r1], cols: [c0, c1], clip: [RH + fw, COL_HEADER_H + fh, this.viewW, this.viewH], ox: sx, oy: sy },
    ];
    if (fr > 0) quads.push({ rows: [0, fr - 1], cols: [c0, c1], clip: [RH + fw, COL_HEADER_H, this.viewW, COL_HEADER_H + fh], ox: sx, oy: 0 });
    if (fc > 0) quads.push({ rows: [r0, r1], cols: [0, fc - 1], clip: [RH, COL_HEADER_H + fh, RH + fw, this.viewH], ox: 0, oy: sy });
    if (fr > 0 && fc > 0) quads.push({ rows: [0, fr - 1], cols: [0, fc - 1], clip: [RH, COL_HEADER_H, RH + fw, COL_HEADER_H + fh], ox: 0, oy: 0 });
    for (const q of quads) this.drawQuad(q);
    this.drawHeaders(c0, c1, r0, r1, fc, fr, sx, sy);
    // frozen pane dividers
    ctx.strokeStyle = t.frozen;
    ctx.lineWidth = 2;
    ctx.beginPath();
    if (fr > 0) { ctx.moveTo(0, COL_HEADER_H + fh); ctx.lineTo(this.viewW, COL_HEADER_H + fh); }
    if (fc > 0) { ctx.moveTo(RH + fw, 0); ctx.lineTo(RH + fw, this.viewH); }
    ctx.stroke();
  }

  drawQuad(q) {
    const sh = this.sheet;
    const ctx = this.ctx;
    const t = this.theme;
    const [x0, y0, x1, y1] = q.clip;
    if (x1 <= x0 || y1 <= y0 || q.rows[1] < q.rows[0] || q.cols[1] < q.cols[0]) return;
    ctx.save();
    ctx.beginPath();
    ctx.rect(x0, y0, x1 - x0, y1 - y0);
    ctx.clip();
    const RH = this.rowHeaderW;
    const X = (c) => RH + sh.colAxis.pos(c) - q.ox;
    const Y = (r) => COL_HEADER_H + sh.rowAxis.pos(r) - q.oy;
    const [ra, rb] = q.rows;
    const [ca, cb] = q.cols;
    const xs = [];
    for (let c = ca; c <= cb + 1; c++) xs.push(X(c));
    const ys = [];
    for (let r = ra; r <= rb + 1; r++) ys.push(Y(r));

    // 1. cell fills and find highlights
    for (let r = ra; r <= rb; r++) {
      for (let c = ca; c <= cb; c++) {
        const cell = sh.cells.get(cellKey(r, c));
        const bg = cell?.s?.bg;
        if (bg) {
          ctx.fillStyle = bg;
          ctx.fillRect(xs[c - ca], ys[r - ra], xs[c - ca + 1] - xs[c - ca], ys[r - ra + 1] - ys[r - ra]);
        }
      }
    }
    if (this.find?.keys?.size) {
      ctx.fillStyle = t.findFill;
      for (let r = ra; r <= rb; r++) {
        for (let c = ca; c <= cb; c++) {
          if (this.find.keys.has(cellKey(r, c))) ctx.fillRect(xs[c - ca], ys[r - ra], xs[c - ca + 1] - xs[c - ca], ys[r - ra + 1] - ys[r - ra]);
        }
      }
    }
    // 2. selection tint (the active cell stays clear)
    const s = this.sel;
    if (s && !(s.r0 === s.r1 && s.c0 === s.c1)) {
      ctx.fillStyle = t.selFill;
      const sr0 = Math.max(s.r0, ra);
      const sr1 = Math.min(s.r1, rb);
      const sc0 = Math.max(s.c0, ca);
      const sc1 = Math.min(s.c1, cb);
      if (sr0 <= sr1 && sc0 <= sc1) {
        const rx0 = X(sc0);
        const rx1 = X(sc1 + 1);
        const ry0 = Y(sr0);
        const ry1 = Y(sr1 + 1);
        ctx.fillRect(rx0, ry0, rx1 - rx0, ry1 - ry0);
        if (s.ar >= ra && s.ar <= rb && s.ac >= ca && s.ac <= cb) {
          const cell = sh.cells.get(cellKey(s.ar, s.ac));
          ctx.fillStyle = cell?.s?.bg || t.bg;
          ctx.fillRect(X(s.ac), Y(s.ar), X(s.ac + 1) - X(s.ac), Y(s.ar + 1) - Y(s.ar));
        }
      }
    }
    // 3. grid lines
    ctx.strokeStyle = t.grid;
    ctx.lineWidth = 1;
    ctx.beginPath();
    for (const x of xs) { ctx.moveTo(Math.round(x) - 0.5, ys[0]); ctx.lineTo(Math.round(x) - 0.5, ys[ys.length - 1]); }
    for (const y of ys) { ctx.moveTo(xs[0], Math.round(y) - 0.5); ctx.lineTo(xs[xs.length - 1], Math.round(y) - 0.5); }
    ctx.stroke();
    // 4. text and borders
    for (let r = ra; r <= rb; r++) {
      for (let c = ca; c <= cb; c++) {
        const cell = sh.cells.get(cellKey(r, c));
        if (!cell) continue;
        const x = xs[c - ca];
        const y = ys[r - ra];
        const w = xs[c - ca + 1] - x;
        const hgt = ys[r - ra + 1] - y;
        if (cell.t !== 'z') this.drawText(cell, r, c, x, y, w, hgt, cb, xs, ca);
        const b = cell.s?.border;
        if (b) {
          ctx.strokeStyle = t.border;
          ctx.lineWidth = 1;
          ctx.beginPath();
          if (b.t) { ctx.moveTo(x - 1, Math.round(y) - 0.5); ctx.lineTo(x + w, Math.round(y) - 0.5); }
          if (b.b) { ctx.moveTo(x - 1, Math.round(y + hgt) - 0.5); ctx.lineTo(x + w, Math.round(y + hgt) - 0.5); }
          if (b.l) { ctx.moveTo(Math.round(x) - 0.5, y - 1); ctx.lineTo(Math.round(x) - 0.5, y + hgt); }
          if (b.r) { ctx.moveTo(Math.round(x + w) - 0.5, y - 1); ctx.lineTo(Math.round(x + w) - 0.5, y + hgt); }
          ctx.stroke();
        }
      }
    }
    // 5. overlays: formula references, copy marquee, find cursor, selection, fill preview
    const rectOf = (r) => {
      const ax0 = X(r.c0);
      const ay0 = Y(r.r0);
      return [ax0, ay0, X(Math.min(r.c1, sh.cols - 1) + 1) - ax0, Y(Math.min(r.r1, sh.rows - 1) + 1) - ay0];
    };
    this.highlights.forEach((hl) => {
      const [hx, hy, hw, hh] = rectOf(hl.range);
      ctx.fillStyle = `${hl.color}1a`;
      ctx.fillRect(hx, hy, hw, hh);
      ctx.strokeStyle = hl.color;
      ctx.lineWidth = 2;
      ctx.setLineDash(hl.dashed ? [4, 3] : []);
      ctx.strokeRect(hx + 1, hy + 1, hw - 2, hh - 2);
    });
    ctx.setLineDash([]);
    if (this.copyRange) {
      const [cx, cy, cw, ch] = rectOf(this.copyRange);
      ctx.strokeStyle = t.selLine;
      ctx.lineWidth = 2;
      ctx.setLineDash([5, 4]);
      ctx.strokeRect(cx + 0.5, cy + 0.5, cw - 2, ch - 2);
      ctx.setLineDash([]);
    }
    if (this.find?.current != null) {
      const k = this.find.current;
      const fr = Math.floor(k / 16384);
      const fc = k % 16384;
      if (fr >= ra && fr <= rb && fc >= ca && fc <= cb) {
        ctx.strokeStyle = t.findLine;
        ctx.lineWidth = 2;
        ctx.strokeRect(X(fc) + 1, Y(fr) + 1, X(fc + 1) - X(fc) - 3, Y(fr + 1) - Y(fr) - 3);
      }
    }
    if (s) {
      const [sx0, sy0, sw, shh] = rectOf(s);
      ctx.strokeStyle = t.selLine;
      ctx.lineWidth = 2;
      ctx.strokeRect(sx0, sy0, sw - 1, shh - 1);
      if (!(s.r0 === s.r1 && s.c0 === s.c1)) {
        ctx.lineWidth = 1;
        ctx.strokeRect(X(s.ac) + 1.5, Y(s.ar) + 1.5, X(s.ac + 1) - X(s.ac) - 4, Y(s.ar + 1) - Y(s.ar) - 4);
      }
      if (!this.hideFillHandle) {
        const hx = sx0 + sw - 1;
        const hy = sy0 + shh - 1;
        ctx.fillStyle = t.bg;
        ctx.fillRect(hx - 4, hy - 4, 8, 8);
        ctx.fillStyle = t.selLine;
        ctx.fillRect(hx - 3, hy - 3, 6, 6);
      }
    }
    if (this.fillPreview) {
      const [px, py, pw, ph] = rectOf(this.fillPreview);
      ctx.strokeStyle = t.selLine;
      ctx.lineWidth = 1;
      ctx.setLineDash([4, 3]);
      ctx.strokeRect(px + 0.5, py + 0.5, pw - 2, ph - 2);
      ctx.setLineDash([]);
    }
    ctx.restore();
  }

  fontOf(st) {
    const px = st?.size ? Math.round(st.size * 4 / 3) : 13;
    return `${st?.i ? 'italic ' : ''}${st?.b ? 'bold ' : ''}${px}px ${this.theme.font}`;
  }

  measure(font, text) {
    const k = `${font}\u0000${text}`;
    let w = this.textCache.get(k);
    if (w === undefined) {
      this.ctx.font = font;
      w = this.ctx.measureText(text).width;
      if (this.textCache.size > 20000) this.textCache.clear();
      this.textCache.set(k, w);
    }
    return w;
  }

  textColor(cell) {
    const st = cell.s;
    const t = this.theme;
    let color = cell._c || st?.color || null;
    const bg = st?.bg;
    if (!color) {
      if (bg && t.dark && luminance(bg) > 0.4) color = '#17171c';
      else if (bg && !t.dark && luminance(bg) < 0.08) color = '#f4f4f6';
      else color = cell.t === 'e' ? (t.dark ? '#ff9592' : '#b91c1c') : t.text;
    } else if (t.dark && !bg && luminance(color) < 0.05) {
      color = t.text; // explicit black on the dark canvas would be invisible
    }
    return color;
  }

  drawText(cell, r, c, x, y, w, hgt, cMax, xs, ca) {
    const sh = this.sheet;
    const ctx = this.ctx;
    let text = sh.display(cell);
    if (!text) return;
    const st = cell.s;
    const font = this.fontOf(st);
    const px = st?.size ? Math.round(st.size * 4 / 3) : 13;
    const align = st?.align || (cell.t === 'n' ? 'right' : cell.t === 'b' || cell.t === 'e' ? 'center' : 'left');
    const valign = st?.valign || 'bottom';
    const pad = 4;
    ctx.font = font;
    ctx.fillStyle = cell.pending ? this.theme.headFg : this.textColor(cell);
    let lines = [text];
    let clipW = w;
    if (st?.wrap) {
      lines = this.wrap(font, text, w - pad * 2);
    } else {
      if (text.includes('\n')) lines = [text.split('\n')[0]];
      const tw = this.measure(font, lines[0]);
      if (tw > w - pad * 2) {
        if (cell.t === 'n' && !cell.f?.startsWith('=TEXT')) {
          const hw = this.measure(font, '#') || 7;
          lines = ['#'.repeat(Math.max(1, Math.floor((w - pad * 2) / hw)))];
        } else if (align === 'left') {
          // overflow into empty neighbours to the right
          let cc = c + 1;
          while (cc <= cMax && clipW < tw + pad * 2 && !sh.cells.get(cellKey(r, cc))) {
            clipW = xs[cc - ca + 1] - x;
            cc++;
          }
          if (clipW > w) {
            ctx.fillStyle = this.theme.bg;
            ctx.fillRect(x + w - 1, y, clipW - w, hgt - 1);
            ctx.fillStyle = this.textColor(cell);
          }
        }
      }
    }
    const lineH = Math.round(px * 1.25);
    const blockH = lineH * lines.length;
    let ty;
    if (valign === 'top') ty = y + pad + px;
    else if (valign === 'middle') ty = y + (hgt - blockH) / 2 + px;
    else ty = y + hgt - blockH - 4 + px + 1;
    ctx.save();
    ctx.beginPath();
    ctx.rect(x + 1, y, clipW - 2, hgt - 1);
    ctx.clip();
    for (const line of lines) {
      const lw = this.measure(font, line);
      let tx = x + pad;
      if (align === 'right') tx = x + w - pad - lw;
      else if (align === 'center') tx = x + (w - lw) / 2;
      ctx.fillText(line, tx, ty);
      if (st?.u || st?.strike) {
        ctx.strokeStyle = ctx.fillStyle;
        ctx.lineWidth = 1;
        ctx.beginPath();
        if (st.u) { ctx.moveTo(tx, Math.round(ty + 2) + 0.5); ctx.lineTo(tx + lw, Math.round(ty + 2) + 0.5); }
        if (st.strike) { const my = Math.round(ty - px * 0.32) + 0.5; ctx.moveTo(tx, my); ctx.lineTo(tx + lw, my); }
        ctx.stroke();
      }
      ty += lineH;
      if (ty - px > y + hgt) break;
    }
    ctx.restore();
  }

  wrap(font, text, width) {
    const out = [];
    for (const para of text.split('\n')) {
      const words = para.split(/(\s+)/);
      let line = '';
      for (const word of words) {
        const next = line + word;
        if (line && this.measure(font, next.trimEnd()) > width) {
          out.push(line.trimEnd());
          line = word.trimStart();
          // hard-break very long words
          while (this.measure(font, line) > width && line.length > 1) {
            let n = line.length - 1;
            while (n > 1 && this.measure(font, line.slice(0, n)) > width) n--;
            out.push(line.slice(0, n));
            line = line.slice(n);
          }
        } else line = next;
      }
      out.push(line.trimEnd());
    }
    return out;
  }

  drawHeaders(c0, c1, r0, r1, fc, fr, sx, sy) {
    const sh = this.sheet;
    const ctx = this.ctx;
    const t = this.theme;
    const RH = this.rowHeaderW;
    const s = this.sel;
    ctx.font = `500 11.5px ${t.font}`;
    ctx.textBaseline = 'middle';
    const colsSel = s && s.r0 === 0 && s.r1 >= sh.rows - 1;
    const rowsSel = s && s.c0 === 0 && s.c1 >= sh.cols - 1;
    // column header strip
    ctx.fillStyle = t.headBg;
    ctx.fillRect(0, 0, this.viewW, COL_HEADER_H);
    const drawCols = (from, to, ox, clipX0, clipX1) => {
      if (to < from || clipX1 <= clipX0) return;
      ctx.save();
      ctx.beginPath();
      ctx.rect(clipX0, 0, clipX1 - clipX0, COL_HEADER_H);
      ctx.clip();
      for (let c = from; c <= to; c++) {
        const x = RH + sh.colAxis.pos(c) - ox;
        const w = sh.colAxis.size(c);
        const inSel = s && c >= s.c0 && c <= s.c1;
        if (inSel) {
          ctx.fillStyle = colsSel ? t.headFullBg : t.headActiveBg;
          ctx.fillRect(x, 0, w, COL_HEADER_H);
        }
        ctx.fillStyle = inSel ? (colsSel ? t.headFullFg : t.headActiveFg) : t.headFg;
        ctx.textAlign = 'center';
        if (w > 12) ctx.fillText(colName(c), x + w / 2, COL_HEADER_H / 2 + 1);
        ctx.strokeStyle = t.headLine;
        ctx.lineWidth = 1;
        ctx.beginPath();
        ctx.moveTo(Math.round(x + w) - 0.5, 0);
        ctx.lineTo(Math.round(x + w) - 0.5, COL_HEADER_H);
        ctx.stroke();
      }
      ctx.restore();
    };
    drawCols(0, fc - 1, 0, RH, RH + this.frozenW());
    drawCols(c0, c1, sx, RH + this.frozenW(), this.viewW);
    // row header strip
    ctx.fillStyle = t.headBg;
    ctx.fillRect(0, COL_HEADER_H, RH, this.viewH - COL_HEADER_H);
    const drawRows = (from, to, oy, clipY0, clipY1) => {
      if (to < from || clipY1 <= clipY0) return;
      ctx.save();
      ctx.beginPath();
      ctx.rect(0, clipY0, RH, clipY1 - clipY0);
      ctx.clip();
      for (let r = from; r <= to; r++) {
        const y = COL_HEADER_H + sh.rowAxis.pos(r) - oy;
        const hgt = sh.rowAxis.size(r);
        const inSel = s && r >= s.r0 && r <= s.r1;
        if (inSel) {
          ctx.fillStyle = rowsSel ? t.headFullBg : t.headActiveBg;
          ctx.fillRect(0, y, RH, hgt);
        }
        ctx.fillStyle = inSel ? (rowsSel ? t.headFullFg : t.headActiveFg) : t.headFg;
        ctx.textAlign = 'center';
        if (hgt > 9) ctx.fillText(String(r + 1), RH / 2, y + hgt / 2 + 1);
        ctx.strokeStyle = t.headLine;
        ctx.lineWidth = 1;
        ctx.beginPath();
        ctx.moveTo(0, Math.round(y + hgt) - 0.5);
        ctx.lineTo(RH, Math.round(y + hgt) - 0.5);
        ctx.stroke();
      }
      ctx.restore();
    };
    drawRows(0, fr - 1, 0, COL_HEADER_H, COL_HEADER_H + this.frozenH());
    drawRows(r0, r1, sy, COL_HEADER_H + this.frozenH(), this.viewH);
    // header edges and corner
    ctx.strokeStyle = t.headLine;
    ctx.beginPath();
    ctx.moveTo(0, COL_HEADER_H - 0.5);
    ctx.lineTo(this.viewW, COL_HEADER_H - 0.5);
    ctx.moveTo(RH - 0.5, 0);
    ctx.lineTo(RH - 0.5, this.viewH);
    ctx.stroke();
    ctx.fillStyle = t.headBg;
    ctx.fillRect(0, 0, RH - 1, COL_HEADER_H - 1);
    ctx.fillStyle = t.headLine;
    ctx.beginPath();
    ctx.moveTo(RH - 5, COL_HEADER_H - 5);
    ctx.lineTo(RH - 5, COL_HEADER_H - 13);
    ctx.lineTo(RH - 13, COL_HEADER_H - 5);
    ctx.closePath();
    ctx.fill();
    ctx.textBaseline = 'alphabetic';
    ctx.textAlign = 'left';
    // live column-resize guide
    if (this.drag?.kind === 'resize') {
      const rz = this.drag.start.resize;
      ctx.strokeStyle = t.selLine;
      ctx.lineWidth = 1;
      ctx.setLineDash([3, 3]);
      ctx.beginPath();
      if (rz.axis === 'col') {
        const x = Math.round(this.colX(rz.index) + sh.colAxis.size(rz.index)) - 0.5;
        ctx.moveTo(x, 0);
        ctx.lineTo(x, this.viewH);
      } else {
        const y = Math.round(this.rowY(rz.index) + sh.rowAxis.size(rz.index)) - 0.5;
        ctx.moveTo(0, y);
        ctx.lineTo(this.viewW, y);
      }
      ctx.stroke();
      ctx.setLineDash([]);
    }
  }
}
