// Structural checks for the MkDocs site without needing MkDocs installed: nav entries exist, every page is
// in the nav, relative links and #anchors resolve (anchors computed like Python-Markdown's toc extension),
// and the ReadTheDocs config has the required keys. Node >= 20, standard library only.
import { readFileSync, existsSync, readdirSync, statSync } from 'node:fs';
import { join, dirname, relative, resolve, sep } from 'node:path';
import { slugify } from './lib.mjs';

/**
 * Parses the `nav:` block of mkdocs.yml (the subset we use: "- Title: file.md", "- Title:" sections,
 * "- file.md"). Returns [{title, file, line}] for page entries.
 */
export function parseNav(yamlText) {
  const lines = yamlText.split(/\r?\n/);
  const start = lines.findIndex((l) => /^nav:\s*$/.test(l));
  if (start < 0) return [];
  const out = [];
  for (let i = start + 1; i < lines.length; i++) {
    const l = lines[i];
    if (/^\S/.test(l)) break; // next top-level key
    if (!l.trim() || l.trim().startsWith('#')) continue;
    const page = l.match(/^\s*-\s+(?:(?:"([^"]+)"|'([^']+)'|([^:]+?)):\s+)?([^\s:]\S*)\s*$/);
    // "- Section:" is a heading, not a page; URLs are external nav links.
    if (page && !page[4].endsWith(':') && !/^https?:/.test(page[4])) out.push({ title: page[1] ?? page[2] ?? page[3] ?? '', file: page[4].replace(/^["']|["']$/g, ''), line: i + 1 });
  }
  return out;
}

/** Reads a top-level scalar from a simple YAML file (`key: value`). */
export function yamlScalar(yamlText, key) {
  const m = yamlText.match(new RegExp(`^${key}:\\s*(.+?)\\s*$`, 'm'));
  return m ? m[1].replace(/^["']|["']$/g, '') : undefined;
}

/** Replaces fenced code blocks and inline code spans with blanks (keeping line numbers). */
export function stripCode(md) {
  return stripFences(md).replace(/(`+)(?:(?!\1)[^\n])*?\1/g, (s) => ' '.repeat(s.length));
}

/** Replaces fenced code blocks with empty lines (keeping line numbers); inline code is kept. */
export function stripFences(md) {
  const lines = md.split(/\r?\n/);
  let fence = null;
  const kept = lines.map((l) => {
    const f = l.match(/^\s{0,3}(`{3,}|~{3,})/);
    if (fence) {
      if (f && f[1][0] === fence[0] && f[1].length >= fence.length && /^\s*(`+|~+)\s*$/.test(l)) fence = null;
      return '';
    }
    if (f) { fence = f[1]; return ''; }
    return l;
  });
  return kept.join('\n');
}

/** Links in a Markdown document: inline links/images and reference definitions, outside code. */
export function extractLinks(md) {
  const text = stripCode(md);
  const out = [];
  text.split('\n').forEach((line, i) => {
    for (const m of line.matchAll(/!?\[(?:[^\]\\]|\\.)*\]\(\s*<?([^)\s>]+)>?(?:\s+(?:"[^"]*"|'[^']*'))?\s*\)/g)) out.push({ target: m[1], line: i + 1 });
    const def = line.match(/^\s{0,3}\[[^\]]+\]:\s*<?(\S+?)>?(?:\s+.*)?$/);
    if (def) out.push({ target: def[1], line: i + 1 });
  });
  return out;
}

/** Heading anchors of a Markdown page, as Python-Markdown's toc extension generates them. */
export function headingAnchors(md) {
  // Inline code in headings contributes its text to the slug (e.g. "### `smtp`" → "smtp").
  const lines = stripFences(md).split('\n');
  const anchors = new Set();
  const used = new Map();
  const add = (raw) => {
    let text = raw.trim();
    const attr = text.match(/\s*\{:?\s*#([\w-]+)[^}]*\}\s*$/);
    if (attr) { anchors.add(attr[1]); return; }
    text = text.replace(/\[([^\]]*)\]\([^)]*\)/g, '$1').replace(/[*`]/g, '').replace(/<[^>]+>/g, '');
    let id = slugify(text);
    if (used.has(id)) { const n = used.get(id) + 1; used.set(id, n); id = `${id}_${n}`; } else used.set(id, 0);
    anchors.add(id);
  };
  lines.forEach((l, i) => {
    const atx = l.match(/^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$/);
    if (atx) { add(atx[1]); return; }
    if (i > 0 && /^\s{0,3}(=+|-+)\s*$/.test(l) && lines[i - 1].trim() && !/^\s*([-*+]|\d+\.|\||#|>)/.test(lines[i - 1])) add(lines[i - 1]);
  });
  return anchors;
}

function listMarkdown(dir) {
  const out = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...listMarkdown(p));
    else if (name.endsWith('.md')) out.push(p);
  }
  return out;
}

/**
 * Checks the docs site rooted at `root` (directory with mkdocs.yml).
 * `readOnlyPrefixes` are docs-relative directories owned by others: their problems are warnings.
 * @returns {{errors: string[], warnings: string[], pages: number, links: number}}
 */
export function checkDocs(root, { readOnlyPrefixes = ['spec/'] } = {}) {
  const errors = []; const warnings = [];
  const cfgPath = join(root, 'mkdocs.yml');
  if (!existsSync(cfgPath)) return { errors: ['mkdocs.yml not found'], warnings, pages: 0, links: 0 };
  const cfg = readFileSync(cfgPath, 'utf8');
  if (!yamlScalar(cfg, 'site_name')) errors.push('mkdocs.yml: site_name is required');
  if (!/^theme:\s*\n\s+name:\s*readthedocs\s*$/m.test(cfg)) errors.push('mkdocs.yml: theme.name must be readthedocs (no extra theme packages)');
  if (/^plugins:/m.test(cfg)) errors.push('mkdocs.yml: plugins are not allowed (requirements pin mkdocs only)');
  const docsDir = resolve(root, yamlScalar(cfg, 'docs_dir') ?? 'docs');
  if (!existsSync(docsDir)) return { errors: [...errors, `docs_dir ${docsDir} not found`], warnings, pages: 0, links: 0 };
  const rel = (p) => relative(docsDir, p).split(sep).join('/');
  const owned = (r) => !readOnlyPrefixes.some((x) => r.startsWith(x));

  const nav = parseNav(cfg);
  if (!nav.length) errors.push('mkdocs.yml: nav is empty or missing');
  const navFiles = new Set();
  for (const n of nav) {
    if (navFiles.has(n.file)) errors.push(`mkdocs.yml:${n.line}: ${n.file} appears twice in nav`);
    navFiles.add(n.file);
    if (!existsSync(join(docsDir, n.file))) errors.push(`mkdocs.yml:${n.line}: nav entry ${n.file} does not exist`);
  }
  const pages = listMarkdown(docsDir);
  for (const p of pages) if (!navFiles.has(rel(p))) (owned(rel(p)) ? errors : warnings).push(`${rel(p)} is not in the nav`);

  const anchorCache = new Map();
  const anchorsOf = (file) => {
    if (!anchorCache.has(file)) anchorCache.set(file, headingAnchors(readFileSync(file, 'utf8')));
    return anchorCache.get(file);
  };
  let links = 0;
  for (const page of pages) {
    const r = rel(page);
    const report = owned(r) ? errors : warnings;
    const md = readFileSync(page, 'utf8');
    if (owned(r) && !/^#\s+\S/m.test(stripCode(md))) errors.push(`${r}: page needs a level-1 "# Title" heading`);
    for (const { target, line } of extractLinks(md)) {
      links++;
      const at = `${r}:${line}`;
      if (/^[a-z][a-z0-9+.-]*:/i.test(target)) {
        if (/^http:\/\//i.test(target) && !/^http:\/\/(localhost|127\.0\.0\.1)/.test(target)) report.push(`${at}: use https for ${target}`);
        continue;
      }
      if (target.startsWith('/')) { report.push(`${at}: absolute link ${target} breaks on ReadTheDocs (use a relative link, or code formatting for server URLs)`); continue; }
      const [pathPart, anchor] = target.split('#');
      let file = page;
      if (pathPart) {
        const decoded = decodeURIComponent(pathPart);
        file = resolve(dirname(page), decoded);
        if (!file.startsWith(docsDir + sep)) { report.push(`${at}: link ${target} leaves docs_dir`); continue; }
        if (!existsSync(file)) { report.push(`${at}: broken link ${target}`); continue; }
        if (statSync(file).isDirectory()) { report.push(`${at}: link ${target} points to a directory; link the .md file`); continue; }
      }
      if (anchor !== undefined && file.endsWith('.md') && !anchorsOf(file).has(anchor)) report.push(`${at}: anchor #${anchor} not found in ${rel(file)}`);
    }
  }

  // ReadTheDocs build configuration.
  const rtd = join(root, '.readthedocs.yaml');
  if (!existsSync(rtd)) errors.push('.readthedocs.yaml not found');
  else {
    const y = readFileSync(rtd, 'utf8');
    const need = [[/^version:\s*2\s*$/m, 'version: 2'], [/^\s+os:\s*ubuntu-24\.04\s*$/m, 'build.os: ubuntu-24.04'], [/^\s+python:\s*"3\.12"\s*$/m, 'build.tools.python: "3.12"'],
      [/^mkdocs:\s*\n\s+configuration:\s*mkdocs\.yml\s*$/m, 'mkdocs.configuration: mkdocs.yml']];
    for (const [re, what] of need) if (!re.test(y)) errors.push(`.readthedocs.yaml: missing ${what}`);
    const req = y.match(/requirements:\s*(\S+)/);
    if (!req) errors.push('.readthedocs.yaml: python.install requirements missing');
    else if (!existsSync(join(root, req[1]))) errors.push(`.readthedocs.yaml: ${req[1]} not found`);
    else {
      const pins = readFileSync(join(root, req[1]), 'utf8').split(/\r?\n/).map((l) => l.replace(/#.*/, '').trim()).filter(Boolean);
      if (pins.length !== 1 || !/^mkdocs==\d+\.\d+(\.\d+)?$/.test(pins[0])) errors.push(`${req[1]}: must pin exactly one requirement, mkdocs==X.Y.Z (got ${JSON.stringify(pins)})`);
    }
  }
  return { errors, warnings, pages: pages.length, links };
}
