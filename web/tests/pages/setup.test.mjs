// First-run setup page rendered on a small DOM.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { installDom, settle } from '../files/minidom.mjs';

installDom();
globalThis.location ??= new URL('https://nas.local/setup');
globalThis.history ??= { state: null, replaceState() {} };
globalThis.CSS ??= { escape: (s) => String(s).replace(/["\\]/g, '\\$&') };
// minidom has no selector lists ("input,select,textarea") or ":scope > x" (forms.js uses both);
// emulate them for this page only.
{
  const proto = globalThis.Element.prototype;
  proto.checkValidity ??= () => true; // the email field's constraint check
  const { matches, querySelector } = proto;
  const list = (sel) => (typeof sel === 'string' && sel.includes(',') ? sel.split(',').map((s) => s.trim()) : null);
  proto.matches = function (sel) { const l = list(sel); return l ? l.some((s) => matches.call(this, s)) : matches.call(this, sel); };
  proto.querySelector = function (sel) {
    if (typeof sel === 'string' && sel.startsWith(':scope > ')) {
      const rest = sel.slice(9);
      return this.childNodes.find((c) => c.nodeType === 1 && c.matches(rest)) || null;
    }
    const l = list(sel);
    if (!l) return querySelector.call(this, sel);
    for (const s of l) { const el = querySelector.call(this, s); if (el) return el; }
    return null;
  };
}
const ui = await import('../../assets/js/core/ui.js');
const { mount } = await import('../../assets/js/pages/setup.js');

// Regression: the username input was pre-filled with value "admin", so typing a name appended
// to it ("adminpat") and an unnoticed default became the administrator's login name.
test('the administrator username is suggested by a placeholder, not pre-filled', async () => {
  const container = document.createElement('div');
  document.body.append(container);
  const posted = [];
  const ctx = {
    ui, settings: { server_name: 'Ludworks' }, query: {}, setTitle() {},
    api: { post: async (path, body) => { posted.push({ path, body }); return {}; } },
    router: { navigate: async () => {} }, completeLogin: async () => {},
  };
  mount(container, ctx);
  await settle(3);
  const username = container.querySelectorAll('input').find((i) => i.getAttribute('name') === 'username');
  assert.ok(Boolean(username), 'username input rendered');
  assert.equal(username.value ?? '', '', 'no pre-filled value');
  assert.equal(username.getAttribute('placeholder'), 'admin');

  // Walk the wizard. Validation still demands a username: with everything else valid and the
  // name left empty, the admin step never advances and nothing is posted.
  const form = container.querySelector('form');
  const named = (n) => container.querySelectorAll('input').find((i) => i.getAttribute('name') === n);
  const submit = async () => {
    form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await settle(2);
  };
  named('setup_token').value = 'tok';
  await submit(); // → admin step
  named('email').value = 'pat@example.com';
  named('password').value = 'correct horse battery';
  named('confirm').value = 'correct horse battery';
  await submit();
  await submit();
  await submit();
  assert.equal(posted.length, 0, 'blocked on the empty username');

  // Typing a name yields exactly that name ("pat", not "adminpat").
  named('username').value = 'pat';
  await submit(); // → server step
  await submit(); // → POST /setup
  assert.equal(posted.length, 1);
  assert.equal(posted[0].path, '/setup');
  assert.equal(posted[0].body.username, 'pat');
});
