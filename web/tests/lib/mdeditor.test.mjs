// Pure text operations behind the Markdown source editor. "|" marks the caret, "[...]" a selection.
import test from 'node:test';
import assert from 'node:assert/strict';
import { textOps } from '../../assets/js/lib/mdeditor.js';

const { parseLine, buildLine, wrapSelection, insertLink, toggleLines, continueOnEnter, indentLines, insertBlock, toggleCodeFence } = textOps;

/** "a[bc]d" → {value:'abcd', start:1, end:3}; "ab|c" → caret at 2. */
function state(s) {
  const a = s.indexOf('['), b = s.indexOf(']');
  if (a >= 0 && b > a && s.indexOf('|') < 0) return { value: s.slice(0, a) + s.slice(a + 1, b) + s.slice(b + 1), start: a, end: b - 1 };
  const c = s.indexOf('|');
  return { value: s.slice(0, c) + s.slice(c + 1), start: c, end: c };
}
function show(r) {
  if (!r) return null;
  if (r.start === r.end) return r.value.slice(0, r.start) + '|' + r.value.slice(r.start);
  return r.value.slice(0, r.start) + '[' + r.value.slice(r.start, r.end) + ']' + r.value.slice(r.end);
}
const run = (fn, s, ...args) => { const st = state(s); return show(fn(st.value, st.start, st.end, ...args)); };

test('parseLine / buildLine are inverse on common prefixes', () => {
  for (const line of ['plain', '- item', '* item', '  + nested', '1. one', '12) twelve', '- [ ] task', '- [x] done', '> quote', '> > - [ ] deep',
    '## heading', '  > 3. x', '#nohash', '- ', '- [ ] ']) {
    assert.equal(buildLine(parseLine(line)), line, line);
  }
  // Bare markers gain their separating space.
  assert.equal(buildLine(parseLine('-')), '- ');
  assert.equal(buildLine(parseLine('1.')), '1. ');
  const p = parseLine('> 3. [x] text');
  assert.deepEqual([p.quote, p.list.type, p.list.num, p.task, p.text], ['> ', 'ol', 3, true, 'text']);
});

test('wrapSelection toggles inline markers', () => {
  assert.equal(run(wrapSelection, 'say [hello] there', '**'), 'say **[hello]** there');
  assert.equal(run(wrapSelection, 'say **[hello]** there', '**'), 'say [hello] there');
  assert.equal(run(wrapSelection, 'say [**hello**] there', '**'), 'say [hello] there');
  assert.equal(run(wrapSelection, 'x |y', '*', 'it'), 'x *[it]*y');
  assert.equal(run(wrapSelection, '[ padded ]', '*'), ' *[padded]* ');
  assert.equal(run(wrapSelection, '**[bold]**', '*'), '***[bold]***', 'italic inside bold is added, not removed');
  assert.equal(run(wrapSelection, '***[both]***', '*'), '**[both]**');
  assert.equal(run(wrapSelection, 'a [b`c] d', '`'), 'a `` [b`c] `` d');
  assert.equal(run(wrapSelection, '[one\n\ntwo]', '~~'), '~~[one~~\n\n~~two]~~');
  assert.equal(run(wrapSelection, '[   ]', '**'), null);
});

test('insertLink selects the URL or label', () => {
  assert.equal(run(insertLink, 'see [docs] now'), 'see [docs]([https://]) now');
  assert.equal(run(insertLink, 'see |now'), 'see [[link]](https://)now');
  assert.equal(run(insertLink, '[https://x.example]'), '[|](https://x.example)');
});

test('toggleLines: lists, tasks, quotes, headings', () => {
  assert.equal(run(toggleLines, 'a|b', 'ul'), '- a|b');
  assert.equal(run(toggleLines, '- a|b', 'ul'), 'a|b');
  assert.equal(run(toggleLines, '[one\ntwo\nthree]', 'ol'), '[1. one\n2. two\n3. three]');
  assert.equal(run(toggleLines, '[- one\n- two]', 'ol'), '[1. one\n2. two]', 'converts bullets');
  assert.equal(run(toggleLines, '[1. one\n2. two]', 'ol'), '[one\ntwo]');
  assert.equal(run(toggleLines, '- it|em', 'task'), '- [ ] it|em');
  assert.equal(run(toggleLines, '- [x] it|em', 'task'), 'it|em');
  assert.equal(run(toggleLines, '[a\n\nb]', 'quote'), '[> a\n>\n> b]');
  assert.equal(run(toggleLines, '[> a\n>\n> b]', 'quote'), '[a\n\nb]');
  assert.equal(run(toggleLines, 'Ti|tle', 'heading', 2), '## Ti|tle');
  assert.equal(run(toggleLines, '## Ti|tle', 'heading', 2), 'Ti|tle');
  assert.equal(run(toggleLines, '## Ti|tle', 'heading', 1), '# Ti|tle');
  assert.equal(run(toggleLines, '- it|em', 'heading', 1), '# it|em', 'heading replaces list marker');
  assert.equal(run(toggleLines, '|', 'ul'), '- |');
});

test('continueOnEnter: lists, quotes, fences, indentation', () => {
  assert.equal(run(continueOnEnter, '- item|'), '- item\n- |');
  assert.equal(run(continueOnEnter, '- it|em'), '- it\n- |em');
  assert.equal(run(continueOnEnter, '  * nested|'), '  * nested\n  * |');
  assert.equal(run(continueOnEnter, '9. nine|'), '9. nine\n10. |');
  assert.equal(run(continueOnEnter, '3) x|'), '3) x\n4) |');
  assert.equal(run(continueOnEnter, '- [x] done|'), '- [x] done\n- [ ] |');
  assert.equal(run(continueOnEnter, '> quote|'), '> quote\n> |');
  assert.equal(run(continueOnEnter, '> - q|'), '> - q\n> - |');
  assert.equal(run(continueOnEnter, '- a\n- |'), '- a\n|', 'empty item ends the list');
  assert.equal(run(continueOnEnter, '- a\n- [ ] |'), '- a\n|');
  assert.equal(run(continueOnEnter, '> a\n> |'), '> a\n|');
  assert.equal(run(continueOnEnter, '```js|'), '```js\n|\n```', 'smart fence');
  assert.equal(run(continueOnEnter, '```|\nexisting\n```'), null, 'a dangling fence below closes it already');
  assert.equal(run(continueOnEnter, '```\n    code|\n```'), '```\n    code\n    |\n```', 'auto-indent inside fences');
  assert.equal(run(continueOnEnter, '```\n- not a list|\n```'), null);
  assert.equal(run(continueOnEnter, '    indented|'), '    indented\n    |');
  assert.equal(run(continueOnEnter, 'plain|'), null);
  assert.equal(run(continueOnEnter, '|- item'), null, 'caret before the marker');
  assert.equal(run(continueOnEnter, '[- a]'), null, 'selection: browser default');
});

test('indentLines: list nesting and plain indentation', () => {
  assert.equal(run(indentLines, '- a\n- b|'), '- a\n  - b|');
  assert.equal(run(indentLines, '1. a\n2. b|'), '1. a\n   2. b|', 'ordered parent needs 3 spaces');
  assert.equal(run(indentLines, '- a\n  - b|', true), '- a\n- b|');
  assert.equal(run(indentLines, '1. a\n   - b|', true), '1. a\n- b|');
  assert.equal(run(indentLines, '- a\n  - b\n    - c|', true), '- a\n  - b\n  - c|');
  assert.equal(run(indentLines, 'text|'), 'text  |');
  assert.equal(run(indentLines, '```\nx|\n```'), '```\nx    |\n```');
  assert.equal(run(indentLines, '[a\nb]'), '[  a\n  b]');
  assert.equal(run(indentLines, '[  a\n\tb\nc]', true), '[a\nb\nc]');
});

test('insertBlock and code fences keep blank-line separation', () => {
  assert.equal(run(insertBlock, 'para|', '---'), 'para\n\n---|\n');
  assert.equal(run(insertBlock, 'a\n|\nb', '---'), 'a\n\n---|\n\nb');
  assert.equal(run(insertBlock, '|', '| a |\n| --- |', 2), '| |a |\n| --- |\n');
  assert.equal(run(toggleCodeFence, '|'), '```\n|\n```\n');
  assert.equal(run(toggleCodeFence, 'x\n[a\nb]\ny'), 'x\n\n```\n[a\nb]\n```\n\ny');
  assert.equal(run(toggleCodeFence, '[has ``` inside]'), '````\n[has ``` inside]\n````\n');
});
