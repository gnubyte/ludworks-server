import test from 'node:test';
import assert from 'node:assert/strict';
import { tokenize, highlight, highlightToHTML, resolveLanguage, languageLabel, listLanguages, detectLanguage, MAX_HIGHLIGHT_CHARS } from '../../assets/js/lib/highlight.js';
import { doc, toHTML } from './fakedom.mjs';

/** [type, value] pairs of the non-plain tokens. */
const typed = (code, lang) => tokenize(code, lang).filter((t) => t.type).map((t) => [t.type, t.value]);
const has = (code, lang, type, value) => typed(code, lang).some(([t, v]) => t === type && v === value);

const SAMPLES = {
  javascript: 'import x from "y";\nconst re = /a+b/gi; // c\nclass A extends B { async *gen() { yield `t${1}`; } }\nlet n = 0x1f + 1e3 + 10n;',
  typescript: 'interface P { name: string; age?: number }\nexport type K = keyof P;\n@Component()\nclass C implements P {}',
  json: '{"key": "value", "n": -1.5e3, "ok": true, "nil": null, "arr": [1, 2]}',
  python: '@dataclass\nclass P:\n    """doc"""\n    def f(self, x: int) -> str:\n        return f"{x!r}"  # c\nprint(rb"\\x00", None)',
  go: 'package main\n\nimport "fmt"\n\ntype T struct{ s string }\n\nfunc (t *T) M() error {\n\tfmt.Println(`raw`, \'r\', nil)\n\treturn nil\n}',
  rust: '#[derive(Debug)]\nstruct S<\'a> { r: &\'a str }\nfn main() { let s = r#"raw "x""#; println!("{}", s); }',
  java: '@Override\npublic static void main(String[] args) { var s = """\n  text\n  """; System.out.println(s); }',
  kotlin: 'data class P(val n: String)\nfun main() { val x: Int = 1; println("$x") }',
  c: '#include <stdio.h>\nint main(void) { char c = \'a\'; printf("%d\\n", sizeof(int)); return 0; }',
  cpp: '#include <vector>\ntemplate <typename T> class V { std::vector<T> v; auto s = R"x(raw)x"; };',
  csharp: 'using System;\nnamespace N { public record R(string S); class C { string v = @"C:\\x"; string i = $"{v}"; } }',
  php: '<?php\nfunction f($x) { echo "hi $x"; return null; } // c\n# hash\n?>',
  ruby: 'class Foo < Bar\n  def baz?(x) = @x ||= :sym\n  puts "#{x}" # c\nend',
  swift: 'import SwiftUI\nstruct V: View { @State var x = 1\n  var body: some View { Text("hi") } }',
  sql: 'SELECT u.id, COUNT(*) AS n FROM users u WHERE u.name = \'O\'\'Reilly\' AND x = :p -- c\nGROUP BY u.id;',
  bash: '#!/usr/bin/env bash\nset -euo pipefail\nNAME="x" ./run.sh --flag "$HOME" ${VAR:-d} | grep -v foo # c\nif [ -f a ]; then echo ok; fi',
  powershell: '<# block #>\nfunction Get-Thing { param([string]$Name) Write-Output "Hi $Name" -NoNewline; if ($a -eq 1) { $env:PATH } }',
  html: '<!DOCTYPE html>\n<div class="a" data-x=\'1\' hidden>&amp; <!-- c --><script>let a = "x";</script><style>.a{color:red}</style></div>',
  xml: '<?xml version="1.0"?>\n<root a="1"><![CDATA[ raw <x> ]]><child/></root>',
  css: '@media (max-width: 600px) { .a > b:hover, #id::before { color: #fff; margin: 0 1.5em !important; --v: calc(1px + 2px); } }',
  yaml: '---\nkey: value # c\nlist:\n  - name: "q"\n    on: true\n    n: 3.5\nanchor: &a x\nref: *a\nblock: |\n  text',
  toml: '# c\n[server]\nport = 8080\nname = "x"\n[[items]]\nwhen = 1979-05-27T07:32:00Z\nok = true',
  markdown: '# Title\n\n- [ ] task with **bold** and `code`\n> quote\n\n```js\nconst a = 1;\n```\n[link](https://x)',
  dockerfile: 'FROM golang:1.22 AS build\nARG V=1\nRUN go build -o /out ./... && \\\n    echo done\nCOPY --from=build /out /app\n# c',
  diff: 'diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-old\n+new\n context',
  ini: '; c\n[section]\nkey = value\nflag=true\nn = 3',
};

test('tokens always concatenate back to the input', () => {
  for (const [lang, code] of Object.entries(SAMPLES)) {
    assert.equal(tokenize(code, lang).map((t) => t.value).join(''), code, lang);
  }
  for (const lang of listLanguages().map((l) => l.id)) {
    // Garbage and unterminated constructs must not lose or duplicate characters.
    const junk = '"unterminated \'x `y /* z <!-- r#" <<<\n\t$$ @@ ##\\';
    assert.equal(tokenize(junk, lang).map((t) => t.value).join(''), junk, lang);
  }
});

test('JavaScript / TypeScript', () => {
  const js = SAMPLES.javascript;
  assert.ok(has(js, 'js', 'keyword', 'import'));
  assert.ok(has(js, 'js', 'string', '"y"'));
  assert.ok(has(js, 'js', 'string', '/a+b/gi'), 'regex literal');
  assert.ok(has(js, 'js', 'comment', '// c'));
  assert.ok(has(js, 'js', 'type', 'A'));
  assert.ok(has(js, 'js', 'string', '`t${1}`'));
  assert.ok(has(js, 'js', 'number', '0x1f'));
  assert.ok(has(js, 'js', 'number', '10n'));
  assert.ok(has('a / b / c', 'js', 'operator', '/'), 'division is not a regex');
  assert.ok(has('foo(1)', 'js', 'function', 'foo'));
  assert.ok(has('obj.type', 'ts', null, 'type') === false && !has('obj.type', 'ts', 'keyword', 'type'), 'member names are not keywords');
  assert.ok(has(SAMPLES.typescript, 'ts', 'keyword', 'interface'));
  assert.ok(has(SAMPLES.typescript, 'ts', 'type', 'string'));
  assert.ok(has(SAMPLES.typescript, 'ts', 'meta', '@Component'));
});

test('Python, Go, Rust, C-family', () => {
  assert.ok(has(SAMPLES.python, 'py', 'meta', '@dataclass'));
  assert.ok(has(SAMPLES.python, 'py', 'string', '"""doc"""'));
  assert.ok(has(SAMPLES.python, 'py', 'function', 'f'));
  assert.ok(has(SAMPLES.python, 'py', 'string', 'f"{x!r}"'));
  assert.ok(has(SAMPLES.python, 'py', 'string', 'rb"\\x00"'));
  assert.ok(has(SAMPLES.python, 'py', 'keyword', 'None'));
  assert.ok(has(SAMPLES.go, 'go', 'type', 'T'), 'type declared with "type"');
  assert.ok(has(SAMPLES.go, 'go', 'string', '`raw`'));
  assert.ok(has(SAMPLES.go, 'go', 'type', 'error'));
  assert.ok(has(SAMPLES.rust, 'rs', 'meta', '#[derive(Debug)]'));
  assert.ok(has(SAMPLES.rust, 'rs', 'string', 'r#"raw "x""#'));
  assert.ok(has(SAMPLES.rust, 'rs', 'function', 'println'), 'macro');
  assert.ok(has(SAMPLES.rust, 'rs', 'variable', "'a"), 'lifetime');
  assert.ok(has(SAMPLES.c, 'c', 'meta', '#include <stdio.h>'));
  assert.ok(has(SAMPLES.c, 'c', 'string', "'a'"));
  assert.ok(has(SAMPLES.cpp, 'c++', 'string', 'R"x(raw)x"'));
  assert.ok(has(SAMPLES.csharp, 'cs', 'string', '@"C:\\x"'));
  assert.ok(has(SAMPLES.csharp, 'cs', 'string', '$"{v}"'));
  assert.ok(has(SAMPLES.java, 'java', 'meta', '@Override'));
  assert.ok(has(SAMPLES.java, 'java', 'type', 'String'));
  assert.ok(has(SAMPLES.kotlin, 'kt', 'keyword', 'fun'));
  assert.ok(has(SAMPLES.swift, 'swift', 'meta', '@State'));
  assert.ok(has(SAMPLES.php, 'php', 'variable', '$x'));
  assert.ok(has(SAMPLES.php, 'php', 'meta', '<?php'));
  assert.ok(has(SAMPLES.ruby, 'rb', 'variable', '@x'));
  assert.ok(has(SAMPLES.ruby, 'rb', 'variable', ':sym'));
});

test('shells, SQL and config formats', () => {
  assert.ok(has(SAMPLES.bash, 'sh', 'meta', '#!/usr/bin/env bash'));
  assert.ok(has(SAMPLES.bash, 'sh', 'variable', 'NAME'));
  assert.ok(has(SAMPLES.bash, 'sh', 'variable', '$HOME') === false); // inside a string
  assert.ok(has(SAMPLES.bash, 'sh', 'variable', '${VAR:-d}'));
  assert.ok(has(SAMPLES.bash, 'sh', 'attr', '--flag'));
  assert.ok(has(SAMPLES.bash, 'sh', 'function', 'grep'));
  assert.ok(has(SAMPLES.bash, 'sh', 'keyword', 'then'));
  assert.ok(has(SAMPLES.powershell, 'ps1', 'function', 'Get-Thing'));
  assert.ok(has(SAMPLES.powershell, 'ps1', 'type', '[string]'));
  assert.ok(has(SAMPLES.powershell, 'ps1', 'operator', '-eq'));
  assert.ok(has(SAMPLES.powershell, 'ps1', 'variable', '$env:PATH'));
  assert.ok(has(SAMPLES.powershell, 'ps1', 'comment', '<# block #>'));
  assert.ok(has(SAMPLES.sql, 'sql', 'keyword', 'SELECT'));
  assert.ok(has(SAMPLES.sql, 'sql', 'string', "'O''Reilly'"));
  assert.ok(has(SAMPLES.sql, 'sql', 'variable', ':p'));
  assert.ok(has(SAMPLES.sql, 'sql', 'function', 'COUNT'));
  assert.ok(has('select 1', 'postgres', 'keyword', 'select'), 'case-insensitive keywords');
  assert.ok(has(SAMPLES.yaml, 'yml', 'attr', 'key'));
  assert.ok(has(SAMPLES.yaml, 'yml', 'keyword', 'true'));
  assert.ok(has(SAMPLES.yaml, 'yml', 'number', '3.5'));
  assert.ok(has(SAMPLES.yaml, 'yml', 'variable', '*a'));
  assert.ok(has(SAMPLES.yaml, 'yml', 'comment', '# c'));
  assert.ok(has(SAMPLES.toml, 'toml', 'type', '[server]'));
  assert.ok(has(SAMPLES.toml, 'toml', 'number', '1979-05-27T07:32:00Z'));
  assert.ok(has(SAMPLES.ini, 'ini', 'attr', 'key'));
  assert.ok(has(SAMPLES.dockerfile, 'dockerfile', 'keyword', 'FROM'));
  assert.ok(has(SAMPLES.dockerfile, 'dockerfile', 'string', 'golang:1.22'));
  assert.ok(has(SAMPLES.dockerfile, 'docker', 'attr', '--from=build'));
  assert.ok(has(SAMPLES.diff, 'diff', 'inserted', '+new'));
  assert.ok(has(SAMPLES.diff, 'patch', 'deleted', '-old'));
  assert.ok(has(SAMPLES.diff, 'diff', 'meta', '@@ -1,2 +1,2 @@'));
});

test('markup, CSS and Markdown', () => {
  assert.ok(has(SAMPLES.html, 'html', 'tag', 'div'));
  assert.ok(has(SAMPLES.html, 'html', 'attr', 'data-x'));
  assert.ok(has(SAMPLES.html, 'html', 'string', '"a"'));
  assert.ok(has(SAMPLES.html, 'html', 'comment', '<!-- c -->'));
  assert.ok(has(SAMPLES.html, 'html', 'meta', '&amp;'));
  assert.ok(has(SAMPLES.html, 'html', 'keyword', 'let'), 'embedded script');
  assert.ok(has(SAMPLES.html, 'html', 'attr', 'color'), 'embedded style');
  assert.ok(has(SAMPLES.xml, 'xml', 'string', '<![CDATA[ raw <x> ]]>'));
  assert.ok(has(SAMPLES.css, 'css', 'keyword', '@media'));
  assert.ok(has(SAMPLES.css, 'css', 'type', '.a'));
  assert.ok(has(SAMPLES.css, 'css', 'meta', ':hover'));
  assert.ok(has(SAMPLES.css, 'css', 'attr', 'color'));
  assert.ok(has(SAMPLES.css, 'css', 'number', '#fff'));
  assert.ok(has(SAMPLES.css, 'css', 'keyword', '!important'));
  assert.ok(has(SAMPLES.css, 'css', 'function', 'calc'));
  assert.ok(has(SAMPLES.css, 'css', 'variable', '--v'));
  assert.ok(has(SAMPLES.markdown, 'md', 'keyword', '# Title'));
  assert.ok(has(SAMPLES.markdown, 'md', 'string', '`code`'));
  assert.ok(has(SAMPLES.markdown, 'md', 'meta', '```js'));
});

test('aliases, labels and unknown languages', () => {
  const pairs = { js: 'javascript', JSX: 'javascript', ts: 'typescript', py: 'python', golang: 'go', rs: 'rust', 'c++': 'cpp', cs: 'csharp', 'c#': 'csharp',
    sh: 'bash', zsh: 'bash', ps1: 'powershell', pwsh: 'powershell', yml: 'yaml', htm: 'html', svg: 'xml', md: 'markdown', docker: 'dockerfile',
    patch: 'diff', conf: 'ini', kt: 'kotlin', rb: 'ruby', 'language-go': 'go', 'js {.line-numbers}': 'javascript' };
  for (const [alias, id] of Object.entries(pairs)) assert.equal(resolveLanguage(alias), id, alias);
  assert.equal(resolveLanguage('mermaid'), null);
  assert.equal(resolveLanguage(''), null);
  assert.equal(resolveLanguage(undefined), null);
  assert.equal(languageLabel('cpp'), 'C++');
  assert.equal(languageLabel('cs'), 'C#');
  assert.equal(languageLabel('mermaid'), 'mermaid');
  assert.deepEqual(tokenize('a < b', 'mermaid'), [{ type: null, value: 'a < b' }]);
  assert.deepEqual(tokenize('', 'js'), []);
  assert.ok(listLanguages().length >= 24);
  const big = 'x'.repeat(MAX_HIGHLIGHT_CHARS + 1);
  assert.equal(tokenize(big, 'js').length, 1, 'oversized input stays plain');
});

test('highlight() builds DOM spans; highlightToHTML() escapes', () => {
  const frag = highlight('if (a < b) { x = "<i>"; }', 'js', doc);
  const out = toHTML(frag);
  assert.match(out, /<span class="hl-keyword">if<\/span>/);
  assert.match(out, /<span class="hl-string">&quot;&lt;i&gt;&quot;<\/span>/);
  assert.equal(frag.textContent, 'if (a < b) { x = "<i>"; }');
  const s = highlightToHTML('<script>alert("x")</script>', 'html');
  assert.doesNotMatch(s, /<script/);
  assert.match(s, /<span class="hl-tag">script<\/span>/);
  assert.equal(highlightToHTML('<b>', 'unknown'), '&lt;b&gt;');
});

test('language detection', () => {
  assert.equal(detectLanguage('package main\n\nfunc main() {\n\tfmt.Println(1)\n}'), 'go');
  assert.equal(detectLanguage('def f(x):\n    return x'), 'python');
  assert.equal(detectLanguage('{"a": [1, 2]}'), 'json');
  assert.equal(detectLanguage('#include <stdio.h>\nint main() { printf("x"); }'), 'c');
  assert.equal(detectLanguage('SELECT a FROM b WHERE c = 1'), 'sql');
  assert.equal(detectLanguage('<!doctype html><html><body></body></html>'), 'html');
  assert.equal(detectLanguage('const x = () => 1;\nconsole.log(x)'), 'javascript');
  assert.equal(detectLanguage('just some words'), null);
  assert.equal(detectLanguage(''), null);
});

// The lexers themselves, without the size cap and time budget that would otherwise turn these
// large inputs into plain text and hide a regression.
const NO_LIMITS = { maxChars: Infinity, budgetMs: Infinity };

test('highlighting is linear on large and adversarial input', () => {
  const big = 'function f(a, b) { return a + b * 2; } // comment\nconst s = "str";\n'.repeat(12000);
  let t0 = performance.now();
  assert.ok(tokenize(big, 'js', NO_LIMITS).length > 1);
  assert.ok(performance.now() - t0 < 2000);
  const adversarial = { js: '= /[ ', rust: 'r#"', cpp: 'R"x(', css: 'a{b:', yaml: 'a ', markdown: '*a ', html: '<a b="', python: 'f"', bash: '"$', sql: "'" };
  for (const [lang, unit] of Object.entries(adversarial)) {
    const src = unit.repeat(100000);
    t0 = performance.now();
    const toks = tokenize(src, lang, NO_LIMITS);
    assert.equal(toks.map((t) => t.value).join('').length, src.length);
    assert.ok(performance.now() - t0 < 2000, `${lang} ${performance.now() - t0}ms`);
  }
});

// Regression: an unclosed "[" made the SQL bracket-identifier rule and the Markdown link rule
// rescan the rest of the line from every "[" (likewise "[a](" and "<http:" in Markdown), so one
// long line took O(n²): 240 kB of "[" froze a tab for ~40 s, the old 1 MB cap for ~11 minutes.
test('crafted bracket/link/autolink runs on one line stay linear', () => {
  const cases = [
    ['sql', '['], ['sql', '[a'], ['sql', '[', ']'],
    ['markdown', '['], ['markdown', '[', ']'], ['markdown', '[a]('], ['markdown', '<http:'], ['markdown', '!['],
    ['markdown', '[a] ('], ['markdown', '<https:x'], ['markdown', '`'], ['markdown', '``a'],
  ];
  for (const [lang, unit, tail = ''] of cases) {
    const src = unit.repeat(Math.ceil(200_000 / unit.length)) + tail;
    const t0 = performance.now();
    const toks = tokenize(src, lang, NO_LIMITS);
    const ms = performance.now() - t0;
    assert.equal(toks.map((t) => t.value).join(''), src, `${lang} ${unit} round-trips`);
    assert.ok(ms < 2000, `${lang} ${JSON.stringify(unit)}${tail} took ${ms.toFixed(0)}ms`);
  }
});

test('unclosed SQL [identifiers run to the end of the line', () => {
  assert.deepEqual(typed('SELECT [a b] FROM t', 'sql').filter(([t]) => t === 'variable'), [['variable', '[a b]']]);
  assert.deepEqual(typed('SELECT [oops FROM t\nWHERE x', 'sql').slice(0, 2), [['keyword', 'SELECT'], ['variable', '[oops FROM t']]);
  assert.ok(has('SELECT [oops FROM t\nWHERE x', 'sql', 'keyword', 'WHERE'), 'the next line is lexed normally');
});

/** Reference: the single regex the Markdown inline highlighter used before it became a linear scanner. */
function mdInlineReference(s) {
  const re = /(`+)[^`]*?\1|\*\*[^*\n]+\*\*|__[^_\n]+__|\*[^*\s][^*\n]*\*|_[^_\s][^_\n]*_|!?\[[^\]\n]*\]\([^)\s]*\)|<https?:[^>\s]*>|https?:\/\/[^\s<>)]+/g;
  const toks = [];
  const push = (type, value) => {
    if (!value) return;
    const last = toks[toks.length - 1];
    if (last && last.type === type) last.value += value;
    else toks.push({ type, value });
  };
  let pos = 0, m;
  while ((m = re.exec(s))) {
    if (m.index > pos) push(null, s.slice(pos, m.index));
    const t = m[0];
    if (t[0] === '`') push('string', t);
    else if (t.startsWith('**') || t.startsWith('__')) push('keyword', t);
    else if (t[0] === '*' || t[0] === '_') push('variable', t);
    else if (t[0] === '[' || t[0] === '!') {
      const close = t.indexOf('](');
      push('attr', t.slice(0, close + 1));
      push('string', t.slice(close + 1));
    } else push('string', t);
    pos = m.index + t.length;
  }
  if (pos < s.length) push(null, s.slice(pos));
  return toks;
}

test('markdown inline spans: the linear scanner matches the old regex exactly', () => {
  assert.deepEqual(tokenize('see [docs](https://x/y) and ![img](a.png), `code`, **b**, *i*, __u__, _e_, <https://a.b> https://c.d/e).', 'md'),
    mdInlineReference('see [docs](https://x/y) and ![img](a.png), `code`, **b**, *i*, __u__, _e_, <https://a.b> https://c.d/e).'));
  // Randomised differential check over the characters the rules care about (incl. Unicode spaces).
  const alphabet = ['`', '`', '*', '*', '_', '_', '!', '[', ']', '(', ')', '<', '>', 'h', 's', ':', '/', ' ', ' ', '　', '﻿', '\t', 'a', 'http', 'https://', '<http:', '<https:', '](', '``'];
  let seed = 20261026;
  const rnd = (n) => {
    seed = (seed * 1103515245 + 12345) & 0x7fffffff;
    return seed % n;
  };
  for (let iter = 0; iter < 20000; iter++) {
    let s = 'x '; // keeps block-level rules (lists, headings, quotes) out of the way
    for (let k = 1 + rnd(20); k > 0; k--) s += alphabet[rnd(alphabet.length)];
    const got = tokenize(s, 'markdown', NO_LIMITS);
    const want = mdInlineReference(s);
    if (JSON.stringify(got) !== JSON.stringify(want)) assert.deepEqual(got, want, JSON.stringify(s));
  }
});

test('size cap and time budget fall back to plain text', () => {
  assert.ok(MAX_HIGHLIGHT_CHARS <= 256 * 1024, 'highlighting runs on the main thread: keep the cap small');
  const big = 'a = 1;\n'.repeat(Math.ceil((MAX_HIGHLIGHT_CHARS + 1) / 7));
  assert.deepEqual(tokenize(big, 'js'), [{ type: null, value: big }]);
  assert.ok(tokenize(big, 'js', { maxChars: Infinity }).length > 1, 'callers may raise the cap explicitly');
  // A blown budget (deadline already passed) yields the input unchanged as one plain token.
  const mid = 'let x = [1, 2];\n'.repeat(2000);
  assert.deepEqual(tokenize(mid, 'js', { budgetMs: -1 }), [{ type: null, value: mid }]);
  assert.ok(tokenize(mid, 'js').length > 1, 'normal input finishes well within the default budget');
});
