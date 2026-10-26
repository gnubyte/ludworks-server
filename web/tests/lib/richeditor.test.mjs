// DOM <-> Markdown for the rich editor, tested against the fake DOM (the serializer only needs
// nodeType/nodeName/childNodes/nodeValue/getAttribute).
import test from 'node:test';
import assert from 'node:assert/strict';
import { domToMarkdown, markdownToEditorDOM, escapeMarkdown, plainTextToMarkdown, popupPosition, shortcutCommand } from '../../assets/js/lib/richeditor.js';
import { renderToHTML, parse } from '../../assets/js/lib/markdown.js';
import { doc, toHTML, parseHTML } from './fakedom.mjs';

const ch = (c) => String.fromCharCode(c);

/** Markdown → editing DOM → Markdown (what getMarkdown() returns after setMarkdown()). */
function roundTrip(md, opts) {
  const root = doc.createElement('div');
  root.appendChild(markdownToEditorDOM(md, doc, opts));
  return domToMarkdown(root);
}
const editorHTML = (md) => toHTML(markdownToEditorDOM(md, doc));
const paste = (html) => domToMarkdown(parseHTML(html), { pasted: true });
/** Builds editor DOM directly from HTML (simulates what the browser leaves in the surface). */
const fromDOM = (html) => domToMarkdown(parseHTML(html));
const rendered = (md) => renderToHTML(md);

// Canonical Markdown: the serializer reproduces it byte for byte.
const CANONICAL = [
  '# Title\n\nHello **bold**, *em*, ~~del~~, `code` and [a link](https://x.example "T").',
  '## Sub\n\n### Subsub\n\n#### Small',
  '- a\n- b\n  - c\n    - d\n- e',
  '1. one\n2. two\n3. three',
  '7. seven\n8. eight',
  '- [ ] todo\n- [x] done\n  - [ ] nested todo\n- [ ]',
  '> quoted **text**\n>\n> - list in quote',
  '> outer\n>\n> > inner',
  '```js\nconst a = `x`;\n```',
  '````\n```\nfence inside\n```\n````',
  '```\n```',
  '```go\nfunc main() {\n\tfmt.Println("tabs")\n}\n```',
  '| a | b | c |\n| :--- | :---: | ---: |\n| 1 | **2** | `x\\|y` |',
  '| only |\n| --- |',
  '---',
  'line one\nline two',
  'para\n\n&nbsp;\n\nafter an empty line',
  'visit https://example.com/a_b. or www.example.org, mail me@x.io',
  '<https://angle.example/path>',
  '![alt text](/api/v1/notes/attachments/abc "caption")',
  '[![img](/i.png)](https://target.example)',
  '\\# not heading\n\n1\\. not a list\n\n\\- not a bullet\n\n\\> not a quote',
  '\\*not em\\* a_b_c \\[x\\](y) \\`tick\\`',
  '**bold *nested em* bold**',
  '***both***',
  '- a\n\n* b',
  '-\n  - nested only',
  '- item\n  ```\n  code in item\n  ```',
  '1. a\n\n   second paragraph\n\n2. b',
  'C:\\Users\\path and 2 * 3 and a ~ b and 1 < 2',
  'Price: $5 & 10% off; email@ notes!',
  '- level1\n  - level2\n    - level3\n      - [x] deep task',
  '> # heading in quote\n>\n> text',
];

test('canonical Markdown round-trips byte for byte', () => {
  for (const md of CANONICAL) assert.equal(roundTrip(md), md, JSON.stringify(md));
});

// Non-canonical inputs: normalized once, then stable; semantics preserved where noted.
const NORMALIZED = [
  ['Setext\n======', '# Setext'],
  ['Sub\n---', '## Sub'],
  ['* * *', '---'],
  ['__strong__ _em_', '**strong** *em*'],
  ['a  \nb\\\nc', 'a\nb\nc'],
  ['# heading #', '# heading'],
  ['+ plus\n+ list', '- plus\n- list'],
  ['1) paren', '1. paren'],
  ['- a\n\n- b', '- a\n- b'],
  ['    indented code', '```\nindented code\n```'],
  ['~~~py\nx\n~~~', '```py\nx\n```'],
  ['[ref]\n\n[ref]: https://r.example', '[ref](https://r.example)'],
  ['<b>raw</b>', '\\<b>raw</b>'],
  ['&copy; &amp;', String.fromCharCode(0xa9) + ' &'],
  ['[evil](javascript:alert(1)) text', 'evil text'],
  ['![x](data:image/png;base64,AA)', 'x'],
  ['a\n\n\n\nb', 'a\n\nb'],
];

test('non-canonical Markdown normalizes once and is then stable', () => {
  for (const [md, want] of NORMALIZED) {
    const once = roundTrip(md);
    assert.equal(once, want, JSON.stringify(md));
    assert.equal(roundTrip(once), once, 'idempotent: ' + JSON.stringify(md));
  }
});

test('serialization preserves rendered meaning', () => {
  const corpus = [...CANONICAL, '**a**b *c*d', 'x**y**z', 'snake_case and *a*b', 'a*b*c', '***a** b*', '*a **b***',
    '**a**[l](/x)', '[**a** b](/x)', 'H~~2~~O', '`a` `` b`c ``', '1. a\n1. b\n1. c', '- a\n  1. b\n  2. c',
    '| a |\n|---|\n| line1 line2 |', '> - [x] q\n>   - [ ] r', 'end with backslash \\\\', 'tab\there'];
  for (const md of corpus) {
    const out = roundTrip(md);
    assert.equal(rendered(out), rendered(md), `${JSON.stringify(md)} -> ${JSON.stringify(out)}`);
  }
});

test('idempotence over a mixed corpus', () => {
  const corpus = [...CANONICAL, ...NORMALIZED.map((x) => x[0]), '- a\n\n  b\n- c', '> a\nlazy', '- x\n---', '1. a\n\n10. b',
    '* [x] star task', '- [ ]\n- [ ]', '![a](/x.png) ![b](/y.png)', 'mixed **bold _and em_ here**', '`code` at start', '[x]: /dangling',
    '| a | b |\n|---|---|\n| 1 |', 'a <https://x.y> b', 'email first.last@example.co.uk.', '#hashtag not heading', '1986\\. A great year'];
  for (const md of corpus) {
    const a = roundTrip(md);
    assert.equal(roundTrip(a), a, JSON.stringify(md));
  }
});

test('literal text typed in the editor is escaped so it stays literal', () => {
  const literals = ['# not a heading', '1. not a list', '- not a bullet', '+ nor this', '> not quote', '*not em*', '_not em_', '**not strong**',
    '~~not del~~', '`not code`', '[not](link)', '![not](image)', '<https://not.autolink>', 'a\\b', 'trailing \\', '&amp; stays',
    '---', '===', '| a | b |', '1) paren', '\\*', '<div>', 'a_b_c', '2 * 3 * 4', 'x ~ y', ch(0xa0) + 'nbsp'];
  for (const text of literals) {
    const md = fromDOM(`<p>${text.replace(/&/g, '&amp;').replace(/</g, '&lt;')}</p>`);
    const out = rendered(md).replace(/\n$/, '');
    const expectText = text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').split(ch(0xa0)).join(' ').trim();
    assert.equal(out, `<p>${expectText}</p>`, `${JSON.stringify(text)} -> ${JSON.stringify(md)}`);
  }
});

test('marks: whitespace moves outside delimiters and adjacent marks are merged', () => {
  assert.equal(fromDOM('<p><strong>bold </strong>text</p>'), '**bold** text');
  assert.equal(fromDOM('<p>a<em> b</em></p>'), 'a *b*');
  assert.equal(fromDOM('<p><b>x</b><strong>y</strong></p>'), '**xy**');
  assert.equal(fromDOM('<p><strong> </strong>z</p>'), 'z');
  assert.equal(fromDOM('<p><strong></strong>z<em></em></p>'), 'z');
  assert.equal(fromDOM('<p><b><i>both</i></b></p>'), '***both***');
  assert.equal(fromDOM('<p><s>gone</s> <del>also</del> <strike>too</strike></p>'), '~~gone~~ ~~also~~ ~~too~~');
  assert.equal(fromDOM('<p><a href="https://a.example">x <b>y</b></a> z</p>'), '[x **y**](https://a.example) z');
  assert.equal(fromDOM('<p><code>a`b</code> <code>`x</code></p>'), '``a`b`` `` `x ``');
  assert.equal(fromDOM('<p>Hey!<a href="/x">[y]</a></p>'), 'Hey\\![\\[y\\]](/x)');
  assert.equal(fromDOM('<p><a href="https://x.example">https://x.example</a></p>'), '<https://x.example>');
  assert.equal(fromDOM('<p><a href="https://x.example" data-md-autolink="bare">https://x.example</a>!</p>'), 'https://x.example!');
  assert.equal(fromDOM('<p>a<a href="https://x.example" data-md-autolink="bare">https://x.example</a></p>'), 'a<https://x.example>');
  assert.equal(fromDOM('<p><a href="mailto:a@b.co" data-md-autolink="email">a@b.co</a></p>'), 'a@b.co');
  assert.equal(fromDOM('<p><a href="javascript:alert(1)">bad</a></p>'), 'bad');
  assert.equal(fromDOM('<p><a href="/p a t h(1">x</a></p>'), '[x](/p%20a%20t%20h\\(1)');
  assert.equal(fromDOM(`<p>a${ch(0x200b)}b</p>`), 'ab', 'zero-width spaces are editor artefacts');
});

test('editor DOM shape', () => {
  assert.equal(editorHTML('- [x] a\n- [ ] b'), '<ul><li data-checked="true">a</li><li data-checked="false">b</li></ul><p><br></p>');
  assert.equal(editorHTML('para\n\n&nbsp;'), '<p>para</p><p><br></p>');
  assert.equal(editorHTML('a\nb'), '<p>a<br>b</p>');
  assert.equal(editorHTML('```js\nx\n```'), '<pre class="re-code" spellcheck="false" data-lang="js"><code>x\n</code></pre><p><br></p>');
  assert.equal(editorHTML('| a |\n|:-:|\n| |'), '<table><thead><tr><th data-align="center">a</th></tr></thead><tbody><tr><td data-align="center"><br></td></tr></tbody></table><p><br></p>');
  assert.equal(editorHTML('![a](/x.png)'), '<p><img src="/x.png" data-md-src="/x.png" alt="a"></p>');
  assert.equal(toHTML(markdownToEditorDOM('![a](/x.png)', doc, { resolveImage: (s) => s + '?t=1' })), '<p><img src="/x.png?t=1" data-md-src="/x.png" alt="a"></p>');
  assert.equal(toHTML(markdownToEditorDOM('![a](/x.png)', doc, { resolveImage: () => 'javascript:x' })), '<p><img src="/x.png" data-md-src="/x.png" alt="a"></p>', 'unsafe resolver output ignored');
  assert.equal(editorHTML('[x](javascript:alert(1))'), '<p>x</p>');
  assert.equal(editorHTML('www.x.org'), '<p><a href="http://www.x.org" data-md-autolink="bare">www.x.org</a></p>');
  assert.equal(toHTML(markdownToEditorDOM('', doc)), '<p><br></p>');
  assert.equal(toHTML(markdownToEditorDOM('# x', doc, { trailingParagraph: false })), '<h1>x</h1>');
});

test('browser-generated DOM is tolerated', () => {
  assert.equal(fromDOM('<div>line1</div><div>line2</div>'), 'line1\n\nline2');
  assert.equal(fromDOM('text at root<br><p>para</p>'), 'text at root\n\npara');
  assert.equal(fromDOM('<p><span style="font-weight:bold">x</span><font color="red">y</font></p>'), 'xy', 'styles ignored outside paste');
  assert.equal(domToMarkdown(parseHTML('<p><span style="font-weight:bold">x</span></p>'), { styles: true }), '**x**');
  assert.equal(fromDOM('<ul><li>a</li><ul><li>nested</li></ul><li>b</li></ul>'), '- a\n  - nested\n- b', 'invalid nesting from old execCommand');
  assert.equal(fromDOM('<p>a<br><br></p>'), 'a', 'trailing placeholder breaks');
  assert.equal(fromDOM('<p><br></p><p>x</p><p><br></p><p><br></p>'), 'x', 'leading/trailing empty paragraphs dropped');
  assert.equal(fromDOM('<h2>T<br>U</h2>'), '## T U');
  assert.equal(fromDOM('<h1></h1>'), '#');
  assert.equal(fromDOM('<pre class="re-code"><code>a\n<span class="hl-keyword">b</span>\n\n</code></pre>'), '```\na\nb\n\n```');
  assert.equal(fromDOM('<pre><code>x</code></pre><pre><code>y</code></pre>'), '```\nx\n```\n\n```\ny\n```');
  assert.equal(fromDOM('<ul><li data-checked="true"><br><ul><li>child</li></ul></li></ul>'), '- [x]\n  - child');
  assert.equal(fromDOM('<ol start="4"><li>x</li></ol><ol><li>y</li></ol>'), '4. x\n\n1) y', 'adjacent lists stay separate');
  assert.equal(fromDOM('<ul><li>a<ul><li><br></li><li>b</li></ul></li></ul>'), '- a\n\n  -\n  - b', 'empty first nested item cannot follow text');
  assert.equal(fromDOM('<ul><li>a<ol start="3"><li>b</li></ol></li></ul>'), '- a\n\n  3. b');
  assert.equal(fromDOM('<blockquote>bare text</blockquote>'), '> bare text');
  assert.equal(fromDOM('<table><tr><td>no thead</td><td>x|y</td></tr><tr><td>1</td></tr></table>'), '| no thead | x\\|y |\n| --- | --- |\n| 1 |  |');
});

test('pasted HTML is reduced to the Markdown subset', () => {
  // Google Docs wraps everything in a non-bold <b> and uses inline styles.
  assert.equal(paste('<b style="font-weight:normal" id="docs-internal-guid-1"><p><span style="font-weight:700">Bold</span> and <span style="font-style:italic">italic</span> and <span style="text-decoration:line-through">gone</span></p></b>'),
    '**Bold** and *italic* and ~~gone~~');
  // GitHub task lists.
  assert.equal(paste('<ul class="contains-task-list"><li class="task-list-item"><input type="checkbox" disabled checked> done</li><li class="task-list-item"><p><input type="checkbox" disabled> loose</p></li></ul>'),
    '- [x] done\n- [ ] loose');
  // Dangerous and irrelevant content disappears.
  assert.equal(paste('<h2>T</h2><script>alert(1)</script><style>p{}</style><iframe src="x"></iframe><p onclick="x()">A <a href="javascript:alert(1)">bad</a> <a href="https://ok.example">ok</a> <a href="#local">frag</a></p>'),
    '## T\n\nA bad [ok](https://ok.example) frag');
  assert.equal(paste('<p><img src="data:image/png;base64,AA" alt="inline"> <img src="https://i.example/a.png" alt="remote" onerror="x()"></p>'), 'inline ![remote](https://i.example/a.png)');
  assert.equal(paste('<p><img src="images/rel.png" alt="rel"> <img src="blob:https://o/1" alt="blob"> <img src="/api/v1/notes/attachments/abc" alt="ours"></p>'),
    'rel blob ![ours](/api/v1/notes/attachments/abc)', 'foreign relative/blob images keep only alt text');
  assert.equal(paste('<p><img src="/x?t=1" data-md-src="/api/v1/notes/attachments/q" alt="e"></p>'), '![e](/api/v1/notes/attachments/q)', 'copied from our editor');
  // Whitespace collapsing, empty paragraphs (Word), nested lists, code blocks with language classes.
  assert.equal(paste('<p>   lots   of\n   whitespace   </p><p>&nbsp;</p><p class="MsoNormal">next<o:p></o:p></p>'), 'lots of whitespace\n\nnext');
  assert.equal(paste('<ol start="3"><li>three<ol><li>nested</li></ol></li><li>four</li></ol>'), '3. three\n   1. nested\n4. four');
  assert.equal(paste('<pre class="language-python"><code>def f():\n    return 1\n</code></pre>'), '```python\ndef f():\n    return 1\n```');
  assert.equal(paste('<div class="highlight"><pre><span>a</span>\n<span>b</span></pre></div>'), '```\na\nb\n```');
  assert.equal(paste('<table><thead><tr><th style="text-align:right">N</th><th>Name</th></tr></thead><tbody><tr><td>1</td><td><b>x</b><br>y</td></tr></tbody></table>'),
    '| N | Name |\n| ---: | --- |\n| 1 | **x** y |');
  assert.equal(paste('<blockquote><p>q1</p><p>q2</p></blockquote>'), '> q1\n>\n> q2');
  assert.equal(paste('<h1>A <code>b</code></h1><hr><dl><dt>term</dt><dd>def</dd></dl>'), '# A `b`\n\n---\n\nterm\n\ndef');
  assert.equal(paste('<p>a <kbd>Ctrl</kbd>+<kbd>C</kbd></p>'), 'a `Ctrl`+`C`');
  assert.equal(paste('<!-- c --><p>x<!-- d -->y</p>'), 'xy');
  assert.equal(paste('<svg><text>no</text></svg><math><mi>x</mi></math><p>ok</p>'), 'ok');
  // Output always round-trips safely through the renderer.
  const nasty = paste('<p><a href="java&#x09;script:alert(1)">t</a><img src="javascript:x" alt="i"><a href=" https://ok.example ">sp</a></p>');
  assert.doesNotMatch(rendered(nasty), /javascript/i);
});

test('plain-text paste heuristics', () => {
  assert.equal(plainTextToMarkdown('# heading\n- list'), '# heading\n- list', 'markdown stays markdown');
  assert.equal(plainTextToMarkdown('See **this** and [that](https://x.y)'), 'See **this** and [that](https://x.y)');
  assert.equal(plainTextToMarkdown('function f() {\n  return 1;\n}\n'), '```javascript\nfunction f() {\n  return 1;\n}\n```');
  assert.equal(plainTextToMarkdown('a_b *c* [d]'), 'a_b \\*c\\* \\[d\\]');
  assert.equal(plainTextToMarkdown('Address\nLine 2\n\nNew para'), 'Address\nLine 2\n\nNew para');
  assert.equal(plainTextToMarkdown('   '), '');
  assert.equal(escapeMarkdown('1. x\n# y\n\n\nz *w*'), '1\\. x\n\\# y\n\nz \\*w\\*');
  const text = 'Literal *stars* _under_ `ticks` [br] <tag> & &amp; # hash\n1) item';
  assert.equal(parse(escapeMarkdown(text)).children.length, 1);
  assert.equal(rendered(escapeMarkdown(text)), '<p>Literal *stars* _under_ `ticks` [br] &lt;tag&gt; &amp; &amp;amp; # hash<br />1) item</p>\n');
});

test('large documents serialize quickly', () => {
  const unit = '# H\n\nText with **bold**, *em*, `code`, [link](https://x.example) and https://bare.example.\n\n- [ ] a\n- [x] b\n  - c\n\n> q\n\n| a | b |\n| --- | --- |\n| 1 | 2 |\n\n```js\nconst x = 1;\n```\n\n';
  const big = unit.repeat(Math.ceil(300_000 / unit.length));
  const root = doc.createElement('div');
  root.appendChild(markdownToEditorDOM(big, doc));
  const t0 = performance.now();
  const md = domToMarkdown(root);
  const ms = performance.now() - t0;
  assert.equal(md, big.trimEnd());
  assert.ok(ms < 3000, `serialize took ${ms}ms`);
});

// Regression (e2e): the code block's language picker + copy button were placed 6px inside the
// block's top edge, so the 38px tall tools covered the first line of code while typing in it.
test('code block tools stay in the label strip and never cover the first code line', () => {
  const host = { top: 100, left: 0, right: 600 };
  const scroll = { top: 0, left: 0, width: 600 };
  const pre = { top: 300, right: 560, bottom: 400, left: 40 };
  const strip = 29; // pre.re-code padding-top (2.1em at 0.86 × 16px)
  for (const height of [38, 30, 24, 12]) {
    const size = { width: 160, height };
    const pos = popupPosition('header', pre, host, scroll, size, { strip });
    const contentTop = pre.top - host.top + strip; // where the first line of code starts
    assert.ok(pos.top + height <= contentTop, `tools of height ${height} end at ${pos.top + height} > ${contentTop}`);
    assert.ok(pos.top >= pre.top - host.top - height, 'tools stay attached to the block');
    assert.equal(pos.left, pre.right - host.left - size.width - 6, 'right-aligned to the block');
  }
  // Short tools fit inside the strip entirely (no overlap with the block above either).
  const inside = popupPosition('header', pre, host, scroll, { width: 160, height: 20 }, { strip });
  assert.ok(inside.top >= pre.top - host.top);
  // Scroll offsets and horizontal clamping still apply.
  const scrolled = popupPosition('header', pre, host, { top: 250, left: 0, width: 600 }, { width: 160, height: 38 }, { strip });
  assert.equal(scrolled.top, pre.top - host.top + 250 + strip - 38 - 2);
  const clamped = popupPosition('header', { ...pre, right: 900 }, host, scroll, { width: 160, height: 38 }, { strip });
  assert.equal(clamped.left, 600 - 160 - 4);
  // Other placements are unchanged.
  assert.deepEqual(popupPosition('below', { top: 10, bottom: 30, left: 50, right: 90 }, host, scroll, { width: 100, height: 30 }), { top: 30 - 100 + 6, left: 50 });
});

// Regression (e2e): Ctrl/Cmd+K with the caret in a code block opened the link editor (links are
// impossible there) and swallowed the app's command palette shortcut.
test('editor shortcuts: Ctrl/Cmd+K is left to the page inside code blocks', () => {
  assert.equal(shortcutCommand('k', 'KeyK'), 'link');
  assert.equal(shortcutCommand('k', 'KeyK', { inCode: true }), null);
  assert.equal(shortcutCommand('b', 'KeyB', { inCode: true }), 'bold');
  assert.equal(shortcutCommand('z', 'KeyZ', { shift: true }), 'redo');
  assert.equal(shortcutCommand('&', 'Digit7', { shift: true }), 'orderedList');
  assert.equal(shortcutCommand('c', 'KeyC', { shift: true }), 'codeBlock');
  assert.equal(shortcutCommand('q', 'KeyQ'), null);
});
