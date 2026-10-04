'use strict';
const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
const { createStore } = require('./store');

// App Contract: PORT from the platform, persistent data only under /data.
const PORT = Number(process.env.PORT || 8080);
const DATA_DIR = process.env.DATA_DIR || '/data';

function createServer(store) {
  return http.createServer((req, res) => {
    const url = new URL(req.url, 'http://x');

    if (url.pathname === '/healthz') return json(res, 200, { ok: true });

    if (url.pathname === '/api/meals' && req.method === 'GET') {
      return json(res, 200, store.get());
    }

    if (url.pathname === '/api/meals' && req.method === 'PUT') {
      let body = '';
      req.on('data', (c) => {
        body += c;
        if (body.length > 10_000) req.destroy();
      });
      req.on('end', () => {
        try {
          const { member, date, slot, value } = JSON.parse(body);
          store.set(member, date, slot, value);
          json(res, 200, store.get());
        } catch (e) {
          json(res, 400, { error: e.message });
        }
      });
      return;
    }

    if (req.method === 'GET') {
      const rel = url.pathname === '/' ? 'index.html' : url.pathname.slice(1);
      const file = path.join(__dirname, 'public', path.normalize(rel));
      if (!file.startsWith(path.join(__dirname, 'public'))) return json(res, 404, {});
      return fs.readFile(file, (err, data) => {
        if (err) return json(res, 404, { error: 'not found' });
        res.writeHead(200, { 'Content-Type': mime(file) });
        res.end(data);
      });
    }
    json(res, 405, { error: 'method not allowed' });
  });
}

function json(res, code, obj) {
  res.writeHead(code, { 'Content-Type': 'application/json; charset=utf-8' });
  res.end(JSON.stringify(obj));
}
const mime = (f) =>
  ({ '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css' })[path.extname(f)] ||
  'application/octet-stream';

if (require.main === module) {
  const server = createServer(createStore(DATA_DIR));
  server.listen(PORT, () => console.log(`meal app listening on :${PORT}, data in ${DATA_DIR}`));
  process.on('SIGTERM', () => server.close(() => process.exit(0)));
}

module.exports = { createServer };
