// Safe XML parser / serializer, including hostile inputs.
import test from 'node:test';
import assert from 'node:assert/strict';
import { childElements, el, firstChild, parseXml, serializeXml, textOf } from '../../assets/js/lib/vault/xml.js';

const code = (c) => (e) => e.name === 'VaultError' && e.code === c;

test('parses elements, attributes, entities, CDATA, comments and PIs', () => {
  const doc = parseXml(
    '﻿<?xml version="1.0" encoding="utf-8"?>\n<!-- lead -->\n<root a="1" b=\'x &amp; &quot;y&quot;\'>\n' +
      '  <item id="&#65;&#x42;">t &lt;&gt;&amp;&apos;&quot; &#x1F600;</item>\n  <empty/>\n  <c><![CDATA[<raw> & ]]></c>\n' +
      '  <?pi data?><ws>  </ws><!-- mid --><nl>a\r\nb\rc&#13;</nl>\n</root>\n<!-- trail -->',
  );
  assert.equal(doc.name, 'root');
  assert.equal(doc.attrs.a, '1');
  assert.equal(doc.attrs.b, 'x & "y"');
  assert.equal(firstChild(doc, 'item').attrs.id, 'AB');
  assert.equal(textOf(firstChild(doc, 'item')), 't <>&\'" \u{1F600}');
  assert.deepEqual(firstChild(doc, 'empty').children, []);
  assert.equal(textOf(firstChild(doc, 'c')), '<raw> & ');
  assert.equal(textOf(firstChild(doc, 'ws')), '  '); // whitespace-only leaf text is significant
  assert.equal(textOf(firstChild(doc, 'nl')), 'a\nb\nc\r'); // EOL normalisation, &#13; survives
  assert.equal(childElements(doc).length, 5); // item, empty, c, ws, nl
  assert.equal(doc.children.length, 5); // indentation whitespace between them dropped
  assert.equal(Object.getPrototypeOf(doc.attrs), null);
});

test('serializer escapes and round-trips', () => {
  const tree = el('r', { q: 'a"b<c>&\n\t' }, el('t', null, 'x<y>&z\r\n]]>'), el('e'), el('m', null, el('n', null, 'v')));
  const xml = serializeXml(tree);
  assert.match(xml, /^<\?xml version="1.0" encoding="utf-8" standalone="yes"\?>\n<r q="a&quot;b&lt;c&gt;&amp;&#10;&#9;">/);
  const back = parseXml(xml);
  assert.equal(back.attrs.q, 'a"b<c>&\n\t');
  assert.equal(textOf(firstChild(back, 't')), 'x<y>&z\r\n]]>');
  assert.deepEqual(firstChild(back, 'e').children, []);
  // characters that XML 1.0 cannot represent are dropped rather than emitted
  assert.equal(textOf(parseXml(serializeXml(el('r', null, 'a\u0000b\u0007c￿d')))), 'abcd');
  assert.equal(serializeXml(el('a', null, 'x'), { declaration: false, indent: '' }), '<a>x</a>');
});

test('rejects DOCTYPE, entity declarations and undefined entities', () => {
  const bomb =
    '<?xml version="1.0"?><!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">]><lolz>&lol2;</lolz>';
  assert.throws(() => parseXml(bomb), /DOCTYPE/);
  assert.throws(() => parseXml('<!DOCTYPE x SYSTEM "file:///etc/passwd"><x/>'), code('xml_invalid'));
  assert.throws(() => parseXml('<x><!ENTITY e "v"></x>'), code('xml_invalid'));
  assert.throws(() => parseXml('<x>&xxe;</x>'), /entity/);
  assert.throws(() => parseXml('<x a="&ext;"/>'), /entity/);
  assert.throws(() => parseXml('<x>a & b</x>'), code('xml_invalid'));
  assert.throws(() => parseXml('<x>&#0;</x>'), /character reference/);
  assert.throws(() => parseXml('<x>&#xD800;</x>'), /character reference/);
  assert.throws(() => parseXml('<x>&#99999999;</x>'), code('xml_invalid'));
  assert.throws(() => parseXml('<x>&#x110000;</x>'), code('xml_invalid'));
});

test('rejects malformed documents', () => {
  for (const bad of [
    '', 'text', '<a>', '<a></b>', '<a><b></a></b>', '<a/><b/>', '<a x=1/>', '<a x="1" x="2"/>', '<a x="<"/>',
    '<a>]]></a>', 'junk<a/>', '<a/>junk', '<a><!-- open</a>', '<a><![CDATA[x</a>', '<a x="1"y="2"/>', '<?xml version="1.0"?><a><?xml x?></a>',
  ]) {
    assert.throws(() => parseXml(bad), (e) => e.name === 'VaultError', JSON.stringify(bad));
  }
});

test('enforces depth, element, attribute and size limits', () => {
  const deep = '<a>'.repeat(300) + '</a>'.repeat(300);
  assert.throws(() => parseXml(deep), code('limit_exceeded'));
  assert.equal(parseXml(deep, { maxDepth: 400 }).name, 'a');
  // very deep documents never recurse on the JS stack
  const veryDeep = '<a>'.repeat(100000) + '</a>'.repeat(100000);
  assert.throws(() => parseXml(veryDeep), code('limit_exceeded'));
  const many = `<r>${'<i/>'.repeat(1001)}</r>`;
  assert.throws(() => parseXml(many, { maxElements: 1000 }), code('limit_exceeded'));
  const attrs = `<r ${Array.from({ length: 70 }, (_, i) => `a${i}="1"`).join(' ')}/>`;
  assert.throws(() => parseXml(attrs), code('limit_exceeded'));
  assert.throws(() => parseXml(`<r>${'x'.repeat(2000)}</r>`, { maxLength: 1000 }), code('limit_exceeded'));
  // a "huge entity" attempt via a long numeric reference is just an invalid reference
  assert.throws(() => parseXml(`<r>&#${'9'.repeat(5000)};</r>`), code('xml_invalid'));
  // serializing a deep tree is iterative too
  let node = el('leaf', null, 'v');
  for (let i = 0; i < 20000; i++) node = el('n', null, node);
  assert.ok(serializeXml(node, { indent: '' }).length > 20000 * 7);
});
