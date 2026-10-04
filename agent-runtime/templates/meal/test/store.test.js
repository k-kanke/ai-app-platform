'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const http = require('node:http');
const { createStore } = require('../store');
const { createServer } = require('../server');

const tmp = () => fs.mkdtempSync(path.join(os.tmpdir(), 'meal-'));

test('answers persist across restarts and last write wins', () => {
  const dir = tmp();
  let s = createStore(dir);
  s.set('母', '2026-10-05', 'lunch', 'yes');
  s.set('母', '2026-10-05', 'lunch', 'no');
  s = createStore(dir); // simulate restart
  assert.strictEqual(s.get().answers['2026-10-05|lunch|母'], 'no');
});

test('none clears an answer', () => {
  const s = createStore(tmp());
  s.set('父', '2026-10-05', 'dinner', 'yes');
  s.set('父', '2026-10-05', 'dinner', 'none');
  assert.deepStrictEqual(s.get().answers, {});
});

test('rejects invalid input', () => {
  const s = createStore(tmp());
  assert.throws(() => s.set('誰か', '2026-10-05', 'lunch', 'yes'));
  assert.throws(() => s.set('母', 'bad', 'lunch', 'yes'));
  assert.throws(() => s.set('母', '2026-10-05', 'brunch', 'yes'));
  assert.throws(() => s.set('母', '2026-10-05', 'lunch', 'maybe'));
});

test('http contract: /healthz and PUT/GET /api/meals', async () => {
  const server = createServer(createStore(tmp()));
  await new Promise((r) => server.listen(0, r));
  const port = server.address().port;
  const req = (method, p, body) =>
    new Promise((resolve, reject) => {
      const r = http.request({ port, method, path: p, headers: { 'Content-Type': 'application/json' } }, (res) => {
        let d = '';
        res.on('data', (c) => (d += c));
        res.on('end', () => resolve({ status: res.statusCode, body: JSON.parse(d) }));
      });
      r.on('error', reject);
      if (body) r.write(JSON.stringify(body));
      r.end();
    });
  assert.strictEqual((await req('GET', '/healthz')).status, 200);
  const put = await req('PUT', '/api/meals', { member: '妹', date: '2026-10-06', slot: 'dinner', value: 'yes' });
  assert.strictEqual(put.status, 200);
  assert.strictEqual((await req('GET', '/api/meals')).body.answers['2026-10-06|dinner|妹'], 'yes');
  assert.strictEqual((await req('PUT', '/api/meals', { member: 'x' })).status, 400);
  server.close();
});
