'use strict';
// Placeholder app produced by the offline stub agent.
const http = require('node:http');
const fs = require('node:fs');
const page = () => `<!doctype html><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>新しいアプリ</title><body style="font:18px system-ui;padding:24px"><h1>新しいアプリ</h1>
<p>ご依頼:</p><pre style="white-space:pre-wrap">${(fs.existsSync('REQUEST.txt') ? fs.readFileSync('REQUEST.txt', 'utf8') : '').replace(/</g, '&lt;')}</pre>`;
http.createServer((req, res) => {
  if (req.url === '/healthz') { res.writeHead(200); return res.end('ok'); }
  res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
  res.end(page());
}).listen(Number(process.env.PORT || 8080));
