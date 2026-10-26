// Parser/renderer conformance for lib/markdown.js (CommonMark core + GFM extensions).
import test from 'node:test';
import assert from 'node:assert/strict';
import { parse, parseInline, renderToHTML, renderToDOM, toPlainText, extractTitle, toggleTask } from '../../assets/js/lib/markdown.js';
import { doc, toHTML } from './fakedom.mjs';

// Compact HTML for assertions: no classes, no pretty-printing newlines.
// Pretty-print newlines only ever follow a tag; code content cannot contain a raw ">".
const html = (md, opts) => renderToHTML(md, opts).replace(/ class="[^"]*"/g, '').replace(/>\n/g, '>');
const cm = (md) => html(md, { breaks: false });

test('ATX headings', () => {
  assert.equal(html('# h1'), '<h1>h1</h1>');
  assert.equal(html('###### h6'), '<h6>h6</h6>');
  assert.equal(html('####### seven'), '<p>####### seven</p>');
  assert.equal(html('#5 bolt'), '<p>#5 bolt</p>');
  assert.equal(html('\\## foo'), '<p>## foo</p>');
  assert.equal(html('# foo *bar* \\*baz\\*'), '<h1>foo <em>bar</em> *baz*</h1>');
  assert.equal(html('## foo ##'), '<h2>foo</h2>');
  assert.equal(html('# foo#'), '<h1>foo#</h1>');
  assert.equal(html('### foo \\###'), '<h3>foo ###</h3>');
  assert.equal(html('#'), '<h1></h1>');
  assert.equal(html('   # indented'), '<h1>indented</h1>');
  assert.equal(html('    # code'), '<pre><code># code</code></pre>');
});

test('setext headings', () => {
  assert.equal(html('Foo *bar*\n========='), '<h1>Foo <em>bar</em></h1>');
  assert.equal(html('Foo\n---'), '<h2>Foo</h2>');
  assert.equal(cm('Foo\nbar\n==='), '<h1>Foo\nbar</h1>');
  assert.equal(html('---\nFoo\n---'), '<hr /><h2>Foo</h2>');
  assert.equal(html('- foo\n---'), '<ul><li>foo</li></ul><hr />');
});

test('paragraphs, soft and hard breaks', () => {
  assert.equal(html('aaa\n\nbbb'), '<p>aaa</p><p>bbb</p>');
  assert.equal(cm('aaa\n   bbb'), '<p>aaa\nbbb</p>');
  assert.equal(html('aaa\nbbb'), '<p>aaa<br />bbb</p>', 'notes semantics: newline is a visible break');
  assert.equal(cm('foo  \nbar'), '<p>foo<br />bar</p>');
  assert.equal(cm('foo\\\nbar'), '<p>foo<br />bar</p>');
  assert.equal(cm('foo\\'), '<p>foo\\</p>');
  assert.equal(cm('foo  '), '<p>foo</p>');
  assert.equal(cm('`code  \nspan`'), '<p><code>code   span</code></p>');
});

test('thematic breaks', () => {
  for (const s of ['***', '---', '___', ' - - -', '**  * ** * ** * **']) assert.equal(html(s), '<hr />', s);
  assert.equal(html('--'), '<p>--</p>');
  assert.equal(html('_ _ a'), '<p>_ _ a</p>');
});

test('emphasis and strong (delimiter algorithm)', () => {
  const cases = [
    ['*foo bar*', '<p><em>foo bar</em></p>'],
    ['a * foo bar*', '<p>a * foo bar*</p>'],
    ['foo*bar*', '<p>foo<em>bar</em></p>'],
    ['5*6*78', '<p>5<em>6</em>78</p>'],
    ['_foo bar_', '<p><em>foo bar</em></p>'],
    ['foo_bar_', '<p>foo_bar_</p>'],
    ['snake_case_name', '<p>snake_case_name</p>'],
    ['**foo bar**', '<p><strong>foo bar</strong></p>'],
    ['__foo bar__', '<p><strong>foo bar</strong></p>'],
    ['***strong emph***', '<p><em><strong>strong emph</strong></em></p>'],
    ['*foo **bar** baz*', '<p><em>foo <strong>bar</strong> baz</em></p>'],
    ['**foo*', '<p>*<em>foo</em></p>'],
    ['*foo**', '<p><em>foo</em>*</p>'],
    ['*foo**bar*', '<p><em>foo**bar</em></p>'],
    ['foo***bar***baz', '<p>foo<em><strong>bar</strong></em>baz</p>'],
    ['*(*foo*)*', '<p><em>(<em>foo</em>)</em></p>'],
    ['**a *b* c**', '<p><strong>a <em>b</em> c</strong></p>'],
    ['\\*not\\*', '<p>*not*</p>'],
    ['* a *', '<ul><li>a *</li></ul>'],
  ];
  for (const [md, want] of cases) assert.equal(html(md), want, md);
});

test('GFM strikethrough (double tilde only)', () => {
  assert.equal(html('~~del~~'), '<p><del>del</del></p>');
  assert.equal(html('a ~~b **c**~~ d'), '<p>a <del>b <strong>c</strong></del> d</p>');
  assert.equal(html('~5 to ~10'), '<p>~5 to ~10</p>');
  assert.equal(html('a ~~~not~~~'), '<p>a ~~~not~~~</p>');
  assert.equal(html('~~ spaced~~'), '<p>~~ spaced~~</p>');
});

test('code spans', () => {
  assert.equal(html('`foo`'), '<p><code>foo</code></p>');
  assert.equal(html('`` foo ` bar ``'), '<p><code>foo ` bar</code></p>');
  assert.equal(html('` `` `'), '<p><code>``</code></p>');
  assert.equal(html('`  `'), '<p><code>  </code></p>');
  assert.equal(html('`&amp; *x*`'), '<p><code>&amp;amp; *x*</code></p>');
  assert.equal(html('`unclosed'), '<p>`unclosed</p>');
  assert.equal(html('```foo``'), '<p>```foo``</p>');
  assert.equal(html('*a `*` b*'), '<p><em>a <code>*</code> b</em></p>');
});

test('fenced code blocks', () => {
  assert.equal(html('```js\nconst a = 1;\n```'), '<pre><code>const a = 1;</code></pre>');
  assert.equal(renderToHTML('```js\nx\n```').includes('class="language-js"'), true);
  assert.equal(html('````\n```\ninner\n```\n````'), '<pre><code>```\ninner\n```</code></pre>');
  assert.equal(html('~~~\n``` not closing\n~~~'), '<pre><code>``` not closing</code></pre>');
  assert.equal(html('```\nunclosed\n\nstill code'), '<pre><code>unclosed\n\nstill code</code></pre>');
  assert.equal(html('  ```\n  aaa\naaa\n    aaa\n  ```'), '<pre><code>aaa\naaa\n  aaa</code></pre>');
  assert.equal(html('``` a`b\nx\n```'), '<p>``` a`b<br />x</p><pre><code></code></pre>', 'backtick in info string: not a fence');
  assert.equal(html('```\n\tgo\ttabs\n```'), '<pre><code>\tgo\ttabs</code></pre>', 'tabs preserved');
  const ast = parse('~~~ python startline=3\ncode\n~~~');
  assert.equal(ast.children[0].lang, 'python');
  assert.equal(ast.children[0].info, 'python startline=3');
});

test('indented code blocks', () => {
  assert.equal(html('    a\n    b\n\n    c'), '<pre><code>a\nb\n\nc</code></pre>');
  assert.equal(cm('para\n    not code'), '<p>para\nnot code</p>');
  assert.equal(html('\tcode'), '<pre><code>code</code></pre>');
});

test('blockquotes', () => {
  assert.equal(html('> a\n> b'), '<blockquote><p>a<br />b</p></blockquote>');
  assert.equal(html('> a\nlazy'), '<blockquote><p>a<br />lazy</p></blockquote>');
  assert.equal(html('> > nested'), '<blockquote><blockquote><p>nested</p></blockquote></blockquote>');
  assert.equal(html('> - x\n> - y'), '<blockquote><ul><li>x</li><li>y</li></ul></blockquote>');
  assert.equal(html('> ```\n> code\n> ```'), '<blockquote><pre><code>code</code></pre></blockquote>');
  assert.equal(html('> a\n\n> b'), '<blockquote><p>a</p></blockquote><blockquote><p>b</p></blockquote>');
  assert.equal(html('>'), '<blockquote></blockquote>');
});

test('lists: nesting, ordering, tightness', () => {
  assert.equal(html('- a\n- b\n  - c\n    - d\n- e'), '<ul><li>a</li><li>b<ul><li>c<ul><li>d</li></ul></li></ul></li><li>e</li></ul>');
  assert.equal(html('1. a\n1. b'), '<ol><li>a</li><li>b</li></ol>');
  assert.equal(html('3. a\n4. b'), '<ol start="3"><li>a</li><li>b</li></ol>');
  assert.equal(html('- a\n+ b\n* c'), '<ul><li>a</li></ul><ul><li>b</li></ul><ul><li>c</li></ul>');
  assert.equal(html('1. a\n2) b'), '<ol><li>a</li></ol><ol start="2"><li>b</li></ol>');
  assert.equal(html('- a\n\n- b'), '<ul><li><p>a</p></li><li><p>b</p></li></ul>');
  assert.equal(html('- a\n  - b\n\n    c\n- d'), '<ul><li>a<ul><li><p>b</p><p>c</p></li></ul></li><li>d</li></ul>');
  assert.equal(cm('para\n- item'), '<p>para</p><ul><li>item</li></ul>');
  assert.equal(cm('para\n2. not a list'), '<p>para\n2. not a list</p>');
  assert.equal(html('-\n  foo'), '<ul><li>foo</li></ul>');
  assert.equal(html('- foo\n\n      code'), '<ul><li><p>foo</p><pre><code>code</code></pre></li></ul>');
  assert.equal(html('10. ten\n    - nested'), '<ol start="10"><li>ten<ul><li>nested</li></ul></li></ol>');
  assert.equal(html('- a\n - b\n  - c'), '<ul><li>a</li><li>b</li><li>c</li></ul>', 'CommonMark: under-indented markers are siblings');
  assert.equal(html('- a\n  - b\n    - c'), '<ul><li>a<ul><li>b<ul><li>c</li></ul></li></ul></li></ul>');
  assert.equal(html('*\tx'), '<ul><li>x</li></ul>');
});

test('GFM task lists record source lines', () => {
  const md = 'intro\n\n- [ ] one\n- [x] two\n  - [X] nested\n- [ ]\n- [ ]no space\n- plain';
  const list = parse(md).children[1];
  assert.equal(list.type, 'list');
  const [one, two, empty, nospace, plain] = list.children;
  assert.deepEqual([one.checked, one.taskLine], [false, 2]);
  assert.deepEqual([two.checked, two.taskLine], [true, 3]);
  assert.deepEqual([two.children[1].children[0].checked, two.children[1].children[0].taskLine], [true, 4]);
  assert.deepEqual([empty.checked, empty.taskLine, empty.children.length], [false, 5, 0]);
  assert.equal(nospace.checked, null);
  assert.equal(plain.checked, null);
  const out = renderToHTML('- [x] done\n- [ ] todo');
  assert.match(out, /<li class="md-task md-task-done" data-task-line="0"><input type="checkbox" class="md-task-checkbox" aria-label="done" checked="" disabled="" \/><div class="md-task-body">done<\/div><\/li>/);
  assert.match(out, /<ul class="md-list md-tasklist">/);
  // ordered task lists and tasks inside blockquotes
  assert.equal(parse('1. [x] a').children[0].children[0].checked, true);
  assert.equal(parse('> - [ ] q').children[0].children[0].children[0].taskLine, 0);
});

test('GFM tables', () => {
  assert.equal(
    html('| a | b | c |\n|:--|:-:|--:|\n| 1 | 2 | 3 |'),
    '<div><table><thead><tr><th align="left">a</th><th align="center">b</th><th align="right">c</th></tr></thead>'
      + '<tbody><tr><td align="left">1</td><td align="center">2</td><td align="right">3</td></tr></tbody></table></div>');
  assert.equal(html('a | b\n--- | ---\nx'), '<div><table><thead><tr><th>a</th><th>b</th></tr></thead><tbody><tr><td>x</td><td></td></tr></tbody></table></div>');
  assert.equal(html('| a |\n| - |\n| 1 | extra |'), '<div><table><thead><tr><th>a</th></tr></thead><tbody><tr><td>1</td></tr></tbody></table></div>');
  assert.equal(html('| x \\| y | `a\\|b` |\n|---|---|'), '<div><table><thead><tr><th>x | y</th><th><code>a|b</code></th></tr></thead></table></div>');
  assert.equal(html('| **b** | [l](/x) |\n|--|--|'), '<div><table><thead><tr><th><strong>b</strong></th><th><a href="/x">l</a></th></tr></thead></table></div>');
  assert.equal(html('| a | b |\n|---|---|\n| 1 | 2 |\n\nafter'), '<div><table><thead><tr><th>a</th><th>b</th></tr></thead><tbody><tr><td>1</td><td>2</td></tr></tbody></table></div><p>after</p>');
  assert.equal(html('| a | b |\n|---|---|\n> quote'), '<div><table><thead><tr><th>a</th><th>b</th></tr></thead></table></div><blockquote><p>quote</p></blockquote>');
  assert.equal(cm('| a | b |\n|---|'), '<p>| a | b |\n|---|</p>', 'cell count mismatch');
  assert.equal(html('abc\n---'), '<h2>abc</h2>', 'no pipe: setext, not table');
  assert.equal(html('intro\n| a |\n|---|'), '<p>intro</p><div><table><thead><tr><th>a</th></tr></thead></table></div>');
  assert.equal(html('> | a |\n> |---|\n> | 1 |'), '<blockquote><div><table><thead><tr><th>a</th></tr></thead><tbody><tr><td>1</td></tr></tbody></table></div></blockquote>');
  const t = parse('| a | b |\n| :-: | --- |\n| 1 | 2 |').children[0];
  assert.deepEqual(t.align, ['center', null]);
  assert.equal(t.children.length, 2);
  assert.equal(t.children[1].line, 2);
});

test('links and images', () => {
  assert.equal(html('[a](/url "title")'), '<p><a href="/url" title="title">a</a></p>');
  assert.equal(html("[a](/url 'title')"), '<p><a href="/url" title="title">a</a></p>');
  assert.equal(html('[a](</my url>)'), '<p><a href="/my%20url">a</a></p>');
  assert.equal(html('[a](foo(and(bar)))'), '<p><a href="foo(and(bar))">a</a></p>');
  assert.equal(html('[a](https://x.com)'), '<p><a href="https://x.com" target="_blank" rel="noopener noreferrer">a</a></p>');
  assert.equal(html('[a]()'), '<p><a href="">a</a></p>');
  assert.equal(html('[link [foo [bar]]](/uri)'), '<p><a href="/uri">link [foo [bar]]</a></p>');
  assert.equal(html('[foo [bar](/uri)](/uri)'), '<p>[foo <a href="/uri">bar</a>](/uri)</p>');
  assert.equal(html('[*em* `code`](/u)'), '<p><a href="/u"><em>em</em> <code>code</code></a></p>');
  assert.equal(html('![alt *x*](/img.png "t")'), '<p><img src="/img.png" alt="alt x" loading="lazy" decoding="async" referrerpolicy="no-referrer" title="t" /></p>');
  assert.equal(html('[![img](/i.png)](/target)'), '<p><a href="/target"><img src="/i.png" alt="img" loading="lazy" decoding="async" referrerpolicy="no-referrer" /></a></p>');
  assert.equal(html('[not a link]'), '<p>[not a link]</p>');
  assert.equal(html('[a](/u "unclosed'), '<p>[a](/u &quot;unclosed</p>');
  assert.equal(html('[a](/u\\)x)'), '<p><a href="/u)x">a</a></p>');
});

test('reference links', () => {
  assert.equal(html('[foo]\n\n[foo]: /url "T"'), '<p><a href="/url" title="T">foo</a></p>');
  assert.equal(html('[Foo][BAR]\n\n[bar]: /u'), '<p><a href="/u">Foo</a></p>');
  assert.equal(html('[foo][]\n\n[foo]: /u'), '<p><a href="/u">foo</a></p>');
  assert.equal(html('![pic][p]\n\n[p]: /p.png'), '<p><img src="/p.png" alt="pic" loading="lazy" decoding="async" referrerpolicy="no-referrer" /></p>');
  assert.equal(html('[x]: /a\n[x]: /b\n\n[x]'), '<p><a href="/a">x</a></p>', 'first definition wins');
  assert.equal(html('[undefined][nope]'), '<p>[undefined][nope]</p>');
  assert.equal(html('[foo]: /url'), '', 'definition alone renders nothing');
});

test('autolinks and GFM bare links', () => {
  assert.equal(html('<https://a.b/c?d=e>'), '<p><a href="https://a.b/c?d=e" target="_blank" rel="noopener noreferrer">https://a.b/c?d=e</a></p>');
  assert.equal(html('<me@example.com>'), '<p><a href="mailto:me@example.com" rel="noopener noreferrer">me@example.com</a></p>');
  assert.equal(html('see https://example.com/path.'), '<p>see <a href="https://example.com/path" target="_blank" rel="noopener noreferrer">https://example.com/path</a>.</p>');
  assert.equal(html('www.commonmark.org/a?b=1, more'), '<p><a href="http://www.commonmark.org/a?b=1" target="_blank" rel="noopener noreferrer">www.commonmark.org/a?b=1</a>, more</p>');
  assert.equal(html('(http://x.com/a_(b))'), '<p>(<a href="http://x.com/a_(b)" target="_blank" rel="noopener noreferrer">http://x.com/a_(b)</a>)</p>');
  assert.equal(html('http://x.com/&amp;'), '<p><a href="http://x.com/" target="_blank" rel="noopener noreferrer">http://x.com/</a>&amp;</p>');
  assert.equal(html('mail foo.bar@x-y.example.org.'), '<p>mail <a href="mailto:foo.bar@x-y.example.org" rel="noopener noreferrer">foo.bar@x-y.example.org</a>.</p>');
  assert.equal(html('a@b'), '<p>a@b</p>');
  assert.equal(html('`https://in.code`'), '<p><code>https://in.code</code></p>');
  assert.equal(html('foohttp://x.com'), '<p>foohttp://x.com</p>', 'needs a word boundary');
  assert.equal(html('www.under_score.com'), '<p>www.under_score.com</p>');
  assert.equal(html('[https://inner.com](/outer)'), '<p><a href="/outer">https://inner.com</a></p>', 'no links in links');
  assert.equal(html('**https://x.com**'), '<p><strong><a href="https://x.com" target="_blank" rel="noopener noreferrer">https://x.com</a></strong></p>');
  const a = parseInline('go to www.x.org now');
  assert.equal(a[1].autolink, 'bare');
});

test('backslash escapes and entities', () => {
  assert.equal(html('\\!\\"\\#\\$\\%\\&\\\'\\(\\)\\*\\+\\,\\-\\.\\/\\:\\;\\<\\=\\>\\?\\@\\[\\\\\\]\\^\\_\\`\\{\\|\\}\\~'),
    '<p>!&quot;#$%&amp;&#39;()*+,-./:;&lt;=&gt;?@[\\]^_`{|}~</p>');
  assert.equal(html('\\a\\b'), '<p>\\a\\b</p>');
  const ch = (c) => String.fromCharCode(c);
  assert.equal(html('&amp; &copy; &AElig; &#35; &#x22; &#0; &bogus; &#xD800;'), `<p>&amp; ${ch(0xa9)} ${ch(0xc6)} # &quot; ${ch(0xfffd)} &amp;bogus; ${ch(0xfffd)}</p>`);
  assert.equal(html('a' + ch(0) + 'b'), '<p>a' + ch(0xfffd) + 'b</p>');
});

test('raw HTML is literal text', () => {
  assert.equal(html('<div onclick="x()">hi</div>'), '<p>&lt;div onclick=&quot;x()&quot;&gt;hi&lt;/div&gt;</p>');
  assert.equal(html('a <b>bold</b>'), '<p>a &lt;b&gt;bold&lt;/b&gt;</p>');
  assert.equal(html('<!-- c -->'), '<p>&lt;!-- c --&gt;</p>');
});

test('options: heading ids, source lines, highlighting', () => {
  const out = renderToHTML('# Hello World\n\n## Hello World', { headingIds: true });
  assert.match(out, /<h1 id="md-hello-world">/);
  assert.match(out, /<h2 id="md-hello-world-1">/);
  assert.match(renderToHTML('a\n\n- b', { sourceLines: true }), /<ul class="md-list" data-line="2">/);
  const hl = renderToHTML('```js\nconst x = 1;\n```', { highlight: true });
  assert.match(hl, /<span class="hl-keyword">const<\/span>/);
  const header = renderToHTML('```go\nx\n```', { codeHeader: true });
  assert.match(header, /<span class="md-code-lang">Go<\/span><button type="button" class="md-code-copy"/);
});

test('renderToDOM builds equivalent DOM and wires task toggles', () => {
  const md = '# T\n\n- [ ] a\n- [x] b\n\n```py\nx = 1\n```\n\n| a |\n|---|\n| [l](https://x.io) |';
  const calls = [];
  const frag = renderToDOM(md, doc, { onTaskToggle: (line, checked) => calls.push([line, checked]), highlight: false, codeHeader: false });
  const dom = toHTML(frag).replace(/\n/g, '');
  const str = renderToHTML(md, { highlight: false, codeHeader: false, onTaskToggle: () => {} }).replace(/\n/g, '').replace(/ \/>/g, '>');
  assert.equal(dom, str.replace(/ disabled=""/g, ''));
  const inputs = [];
  const walk = (n) => { if (n.nodeName === 'INPUT') inputs.push(n); (n.childNodes || []).forEach(walk); };
  walk(frag);
  assert.equal(inputs.length, 2);
  inputs[1].dispatch('change', { target: { checked: false } });
  inputs[0].dispatch('change', { target: { checked: true } });
  assert.deepEqual(calls, [[3, false], [2, true]]);
});

test('toggleTask rewrites only the checkbox', () => {
  const md = 'x\r\n- [ ] a\n  - [x] b\n> 1. [ ] c\n- [ ]\n\n* plain [ ] text';
  assert.equal(toggleTask(md, 1, true), 'x\r\n- [x] a\n  - [x] b\n> 1. [ ] c\n- [ ]\n\n* plain [ ] text');
  assert.equal(toggleTask(md, 2, false), 'x\r\n- [ ] a\n  - [ ] b\n> 1. [ ] c\n- [ ]\n\n* plain [ ] text');
  assert.equal(toggleTask(md, 3, true), 'x\r\n- [ ] a\n  - [x] b\n> 1. [x] c\n- [ ]\n\n* plain [ ] text');
  assert.equal(toggleTask(md, 4, true), 'x\r\n- [ ] a\n  - [x] b\n> 1. [ ] c\n- [x]\n\n* plain [ ] text');
  assert.equal(toggleTask(md, 6, true), md, 'not a task line');
  assert.equal(toggleTask(md, 99, true), md);
  assert.equal(toggleTask(md, -1, true), md);
  assert.equal(toggleTask(md, 1, false), md, 'already unchecked');
  // every taskLine reported by the parser must be toggleable
  const doc2 = '- [ ] a\n- [ ] b\n  - [ ] c\n\n> - [ ] d';
  const lines = [];
  const walk = (n) => { if (n.type === 'listItem' && n.checked !== null) lines.push(n.taskLine); (n.children || []).forEach(walk); };
  walk(parse(doc2));
  assert.deepEqual(lines, [0, 1, 2, 4]);
  let cur = doc2;
  for (const l of lines) cur = toggleTask(cur, l, true);
  assert.equal(cur, '- [x] a\n- [x] b\n  - [x] c\n\n> - [x] d');
});

test('toPlainText and extractTitle', () => {
  const md = '# My *Title*\n\nSome **bold** text with a [link](https://x.com) and `code`.\n\n- [ ] task\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n![pic](/p.png)';
  assert.equal(toPlainText(md), 'My Title Some bold text with a link and code. task a b 1 2 pic');
  assert.equal(toPlainText(md, 20), 'My Title Some bold' + String.fromCharCode(0x2026));
  assert.ok(toPlainText(md, 20).length <= 20);
  assert.equal(toPlainText(''), '');
  assert.equal(extractTitle('\n\n  \n## Hello *there*\nmore'), 'Hello there');
  assert.equal(extractTitle('first line\nsecond line'), 'first line');
  assert.equal(extractTitle('- [ ] buy milk\n- eggs'), 'buy milk');
  assert.equal(extractTitle('```\n\nfn main()\n```'), 'fn main()');
  assert.equal(extractTitle('---\n\n> quoted'), 'quoted');
  assert.equal(extractTitle(''), '');
  assert.equal(extractTitle('x'.repeat(500)).length, 200);
  const big = 'word '.repeat(300000);
  const t0 = performance.now();
  assert.ok(toPlainText(big, 200).length <= 200);
  assert.ok(performance.now() - t0 < 200, 'snippets only parse a prefix');
});

test('performance: 1 MB note parses and renders in linear-ish time', () => {
  const unit = '# Heading\n\nSome *emphasis* and **strong** text with `code` and a [link](https://example.com "t") plus https://bare.example.org/path.\n\n- [ ] task one\n- [x] task two\n  - nested _item_\n\n> quote\n\n| a | b |\n|---|:-:|\n| 1 | 2 |\n\n```js\nconst x = { a: 1 }; // c\n```\n\n';
  const big = unit.repeat(Math.ceil(1_000_000 / unit.length));
  const t0 = performance.now();
  const ast = parse(big);
  const t1 = performance.now();
  renderToHTML(ast, { highlight: true });
  const t2 = performance.now();
  assert.ok(ast.children.length > 10000);
  assert.ok(t1 - t0 < 2500, `parse took ${t1 - t0}ms`);
  assert.ok(t2 - t1 < 2500, `render took ${t2 - t1}ms`);
});

test('pathological inputs stay fast and never overflow the stack', () => {
  const cases = {
    stars: '*a '.repeat(100000),
    brackets: '['.repeat(100000),
    links: '[a](b '.repeat(50000),
    titles: '[a](b "x '.repeat(50000),
    backticks: '`a ``b '.repeat(50000),
    angle: '[a](<b '.repeat(50000),
    nestedQuotes: '> '.repeat(20000) + 'x',
    nestedEmph: '*'.repeat(50000) + 'a' + '*'.repeat(50000),
    nestedImages: '!['.repeat(30000) + 'x' + '](y)'.repeat(30000),
    nestedLinks: '['.repeat(30000) + 'x' + '](y)'.repeat(30000),
    entities: '&a'.repeat(100000),
    emails: 'a@b'.repeat(100000),
    tildes: '~~a '.repeat(100000),
    spaces: 'a' + ' '.repeat(200000) + 'b \nc',
    deepList: Array.from({ length: 300 }, (_, i) => ' '.repeat(i * 2) + '- x').join('\n'),
  };
  for (const [name, md] of Object.entries(cases)) {
    const t0 = performance.now();
    renderToHTML(md);
    const ms = performance.now() - t0;
    assert.ok(ms < 3000, `${name} took ${ms}ms`);
  }
});

// Regression: Files renders a shared .md with highlight on; a ```sql fence holding one long line
// of "[" took O(n²) in the highlighter (60 kB ≈ 3 s, the old 1 MB cap ≈ 11 minutes).
test('highlighted code fences with crafted bracket runs render fast', () => {
  for (const [lang, body] of [['sql', '['.repeat(200_000)], ['md', '[a]('.repeat(50_000)], ['markdown', '<http:'.repeat(40_000)]]) {
    const t0 = performance.now();
    const out = renderToHTML('```' + lang + '\n' + body + '\n```\n', { highlight: true });
    const ms = performance.now() - t0;
    assert.ok(out.length >= body.length, `${lang} rendered`);
    assert.ok(ms < 2000, `${lang} fence took ${ms.toFixed(0)}ms`);
  }
});

test('one document shares a single highlighting budget across its code blocks', () => {
  const block = 'select 1;\n'.repeat(Math.ceil(150_000 / 10));
  const fence = '```sql\n' + block + '```\n';
  // Two top-level blocks: together over the per-document budget, so the second stays plain.
  let out = renderToHTML(fence + '\n' + fence, { highlight: true });
  assert.equal(out.split('<pre').length - 1, 2);
  const highlighted = (s) => s.split('<pre').slice(1).map((p) => p.includes('hl-keyword'));
  assert.deepEqual(highlighted(out), [true, false]);
  // Nested blocks (quotes/lists copy the render context) must not each get a fresh budget.
  out = renderToHTML('> ' + fence.trimEnd().split('\n').join('\n> ') + '\n\n- x\n\n  ' + fence.trimEnd().split('\n').join('\n  ') + '\n', { highlight: true });
  assert.deepEqual(highlighted(out), [true, false]);
  // Small documents are unaffected.
  assert.deepEqual(highlighted(renderToHTML('```sql\nselect 1;\n```\n\n```sql\nselect 2;\n```\n', { highlight: true })), [true, true]);
});
