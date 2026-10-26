import { test } from 'node:test';
import assert from 'node:assert/strict';
import { otherSignIns, describeAgent } from '../../assets/js/apps/settings/sessions.js';

// Regression (e2e): "Sign out other sessions" deleted every non-current row of GET /me/sessions,
// which also lists personal access tokens (kind "token"), so a backup script's API token was
// revoked although the dialog promised that personal API tokens are managed separately.
test('settings sessions: bulk sign-out targets sign-in sessions only, never API tokens', () => {
  const rows = [
    { id: 'cur', kind: 'session', current: true, user_agent: 'Mozilla/5.0 (Windows NT 10.0) Chrome/130.0' },
    { id: 'phone', kind: 'session', current: false, device_name: 'Pixel 9' },
    { id: 'laptop', kind: 'session', current: false },
    { id: 'pat', kind: 'token', current: false, device_name: 'backup script' },
  ];
  assert.deepEqual(otherSignIns(rows).map((s) => s.id), ['phone', 'laptop']);
  assert.deepEqual(otherSignIns([rows[0], rows[3]]), []);
  assert.deepEqual(otherSignIns(null), []);
  // Signed in with a token (e.g. an API client viewing its own list): the current token is not "other".
  assert.deepEqual(otherSignIns([{ id: 't', kind: 'token', current: true }, rows[1]]).map((s) => s.id), ['phone']);
  assert.equal(describeAgent(rows[0].user_agent), 'Chrome · Windows');
});
