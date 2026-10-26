// Security tests: Markdown from notes is untrusted (imports, mail-in, shared folders).
import test from 'node:test';
import assert from 'node:assert/strict';
import { renderToHTML, renderToDOM, safeLinkHref, safeImageSrc } from '../../assets/js/lib/markdown.js';
import { doc } from './fakedom.mjs';

const ch = (c) => String.fromCharCode(c);

/** Every href/src in the output, decoded from HTML attribute escaping. */
function urls(out) {
  const res = [];
  for (const m of out.matchAll(/\s(href|src)="([^"]*)"/g)) res.push(m[2].replace(/&amp;/g, '&').replace(/&quot;/g, '"').replace(/&#39;/g, "'"));
  return res;
}

const ALLOWED_TAGS = new Set(['p', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'em', 'strong', 'del', 'code', 'pre', 'a', 'img', 'br', 'hr',
  'ul', 'ol', 'li', 'blockquote', 'table', 'thead', 'tbody', 'tr', 'th', 'td', 'div', 'span', 'input', 'button']);
const ALLOWED_ATTRS = new Set(['href', 'src', 'alt', 'title', 'class', 'target', 'rel', 'loading', 'decoding', 'referrerpolicy', 'type',
  'checked', 'disabled', 'aria-label', 'data-task-line', 'data-lang', 'data-line', 'start', 'align', 'id']);

function assertNoActiveContent(out, label) {
  // Parse the renderer's own markup: every "<" must start a well-formed allowed tag whose
  // attributes are all on the allow-list (so text can never smuggle in markup or handlers).
  const tagRe = /<(\/?)([a-z0-9]+)((?:\s+[a-z-]+="[^"<>]*")*)\s*(\/?)>/g;
  let tagCount = 0;
  for (const m of out.matchAll(tagRe)) {
    tagCount++;
    assert.ok(ALLOWED_TAGS.has(m[2]), `${label}: unexpected tag <${m[2]}>`);
    for (const a of m[3].matchAll(/([a-z-]+)="/g)) assert.ok(ALLOWED_ATTRS.has(a[1]), `${label}: unexpected attribute ${a[1]}`);
  }
  assert.equal((out.match(/</g) || []).length, tagCount, `${label}: stray "<" in output`);
  for (const u of urls(out)) {
    const norm = u.replace(/[\x00-\x20]/g, '').normalize('NFKC').toLowerCase();
    assert.doesNotMatch(norm, /^(?:javascript|vbscript|data|file|livescript|mocha):/, `${label}: dangerous url ${u}`);
  }
}

const dangerousLinks = [
  '[x](javascript:alert(1))',
  '[x](JaVaScRiPt:alert(1))',
  '[x](javascript&#58;alert(1))',
  '[x](&#106;avascript:alert(1))',
  '[x](&#x6A;avascript:alert(1))',
  '[x](java&#9;script:alert(1))',
  '[x](java&#10;script:alert(1))',
  '[x](&#1;javascript:alert(1))',
  '[x](<javascript:alert(1)>)',
  '[x]( javascript:alert(1))',
  '[x](vbscript:msgbox(1))',
  '[x](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)',
  '[x](file:///etc/passwd)',
  '<javascript:alert(1)>',
  '<vbscript:alert(1)>',
  '[x][r]\n\n[r]: javascript:alert(1)',
  '[x](\\javascript:alert(1))',
  '[x](' + ch(0xff4a) + ch(0xff41) + ch(0xff56) + ch(0xff41) + 'script:alert(1))',
  '[x](' + ch(0x202e) + 'javascript:alert(1))',
  '[x](' + ch(0x200b) + 'javascript:alert(1))',
];

test('dangerous link schemes are rendered as plain text', () => {
  for (const md of dangerousLinks) {
    const out = renderToHTML(md);
    assertNoActiveContent(out, md);
    assert.doesNotMatch(out, /<a\s[^>]*href="[^"]*script/i, md);
  }
  assert.equal(renderToHTML('[click me](javascript:alert(1))'), '<p>click me</p>\n');
});

test('dangerous image sources are dropped (alt text kept)', () => {
  const cases = ['![a](javascript:alert(1))', '![a](data:image/svg+xml;base64,PHN2Zz4=)', '![a](http://insecure/x.png)',
    '![a](//evil.example/x.png)', '![a](file:///x.png)', '![a](vbscript:x)', '![a](<//evil.example/x.png>)'];
  for (const md of cases) {
    const out = renderToHTML(md);
    assert.doesNotMatch(out, /<img/, md);
    assert.match(out, /<p>a<\/p>/, md);
  }
  // Backslashes are percent-encoded, so "/\host" stays a same-origin path instead of "//host".
  for (const md of ['![a](/\\evil.example/x.png)', '![a](\\\\evil.example\\x.png)']) {
    const [src] = urls(renderToHTML(md));
    assert.match(src, /^\/?%5C/, md);
  }
  assert.match(renderToHTML('![a](https://cdn.example/x.png)'), /<img class="md-img" src="https:\/\/cdn.example\/x.png"/);
  assert.match(renderToHTML('![a](/api/v1/notes/attachments/abc)'), /src="\/api\/v1\/notes\/attachments\/abc"/);
  assert.match(renderToHTML('![a](blob:https://h/uuid)'), /src="blob:https:\/\/h\/uuid"/);
});

test('resolveImage output is re-validated', () => {
  const evil = renderToHTML('![a](/x.png)', { resolveImage: () => 'javascript:alert(1)' });
  assert.doesNotMatch(evil, /<img/);
  const ok = renderToHTML('![a](/x.png)', { resolveImage: (s) => s + '?access_token=t' });
  assert.match(ok, /src="\/x.png\?access_token=t"/);
  const dropped = renderToHTML('![a](/x.png)', { resolveImage: () => null });
  assert.equal(dropped, '<p>a</p>\n');
});

test('raw HTML and HTML-looking text are escaped', () => {
  const cases = ['<script>alert(1)</script>', '<img src=x onerror=alert(1)>', '<svg/onload=alert(1)>', '<a href="javascript:x">y</a>',
    '<iframe src="https://evil"></iframe>', '<style>*{display:none}</style>', '&lt;script&gt;alert(1)&lt;/script&gt;',
    '<<script>>', '<div\nonclick=alert(1)>', '`<script>` and ```\n<script>\n```', '<math><mi xlink:href="javascript:x">'];
  for (const md of cases) {
    const out = renderToHTML(md, { highlight: true, codeHeader: true });
    assertNoActiveContent(out, md);
    assert.doesNotMatch(out, /<(script|img|svg|iframe|style|math)\b/i, md);
  }
});

test('attribute injection via titles, alt text, info strings and URLs', () => {
  const t = renderToHTML('[x](https://a.example "\\" onmouseover=\\"alert(1)")');
  assertNoActiveContent(t, 'title');
  assert.match(t, /title="&quot; onmouseover=&quot;alert\(1\)"/);
  const alt = renderToHTML('![" onerror="alert(1)](https://a.example/x.png)');
  assertNoActiveContent(alt, 'alt');
  assert.match(alt, /alt="&quot; onerror=&quot;alert\(1\)"/);
  const info = renderToHTML('```"><script>alert(1)</script>\ncode\n```', { codeHeader: true, highlight: true });
  assertNoActiveContent(info, 'info');
  const url = renderToHTML('[x](https://a.example/"onmouseover="alert(1))');
  assertNoActiveContent(url, 'url');
  assert.match(url, /href="https:\/\/a.example\/%22onmouseover=%22alert\(1\)"/);
  const sq = renderToHTML("[x](https://a.example/'><script>)");
  assertNoActiveContent(sq, 'single quote');
});

test('external links get noopener/noreferrer; internal links do not open new tabs', () => {
  assert.match(renderToHTML('[a](https://x.example)'), /<a href="https:\/\/x.example" target="_blank" rel="noopener noreferrer">/);
  assert.match(renderToHTML('[a](//x.example)'), /target="_blank" rel="noopener noreferrer"/);
  assert.match(renderToHTML('[a](mailto:a@b.co)'), /<a href="mailto:a@b.co" rel="noopener noreferrer">/);
  assert.equal(renderToHTML('[a](/notes/123)'), '<p><a href="/notes/123">a</a></p>\n');
  assert.equal(renderToHTML('[a](#section)'), '<p><a href="#section">a</a></p>\n');
});

test('url classifiers', () => {
  for (const u of ['https://x', 'http://x', 'mailto:a@b', '/rel', 'rel/path', '#frag', '?q=1', '//host/x']) assert.equal(safeLinkHref(u), u, u);
  for (const u of ['javascript:x', ' javascript:x', 'JAVASCRIPT:x', 'data:x', 'vbscript:x', 'blob:x', 'ftp://x', 'weird:x', 'java' + ch(9) + 'script:x', 'a:b/c']) {
    assert.equal(safeLinkHref(u), null, JSON.stringify(u));
  }
  for (const u of ['https://x/y.png', '/api/x', 'img.png', 'blob:https://h/1']) assert.equal(safeImageSrc(u), u, u);
  for (const u of ['http://x/y.png', 'data:image/png;base64,AA', '//x/y.png', '/\\x/y.png', 'javascript:x', '']) assert.equal(safeImageSrc(u), null, JSON.stringify(u));
  assert.equal(safeLinkHref(42), null);
});

test('renderToDOM never produces handlers or unsafe attributes', () => {
  const md = dangerousLinks.join('\n\n') + '\n\n<img src=x onerror=alert(1)>\n\n![a](javascript:x)\n\n```js\nalert(1)\n```\n\n- [ ] task';
  const frag = renderToDOM(md, doc, { onTaskToggle: () => {} });
  const check = (n) => {
    if (n.nodeType === 1) {
      assert.ok(!['SCRIPT', 'IFRAME', 'STYLE', 'OBJECT', 'EMBED', 'SVG'].includes(n.nodeName), n.nodeName);
      for (const { name, value } of n.attributes) {
        assert.ok(!/^on/i.test(name), `handler attribute ${name}`);
        assert.notEqual(name, 'style');
        if (name === 'href' || name === 'src') assert.doesNotMatch(value.toLowerCase(), /^(javascript|data|vbscript):/);
      }
    }
    n.childNodes.forEach(check);
  };
  check(frag);
});

test('deeply nested structures do not crash the renderer', () => {
  const md = '> '.repeat(5000) + 'x\n\n' + '- '.repeat(3000) + 'y\n\n' + '['.repeat(20000) + 'z' + ']'.repeat(20000) + '\n\n' + '**'.repeat(20000) + 'w';
  const out = renderToHTML(md);
  assertNoActiveContent(out, 'nested');
  assert.match(out, /x/);
});
