'use client';
import { useRef, useState } from 'react';

export default function New() {
  const [name, setName] = useState('');
  const [prompt, setPrompt] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  // One key per form so a double tap / retry never creates two apps.
  const key = useRef(crypto.randomUUID());

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true); setErr('');
    const r = await fetch('/api/apps', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Idempotency-Key': key.current },
      body: JSON.stringify({ name, prompt }),
    });
    const body = await r.json().catch(() => ({}));
    if (r.ok) { location.href = `/apps/${body.app.id}`; return; }
    setErr(body.error ?? 'うまく送れませんでした'); setBusy(false);
  }

  return (
    <form onSubmit={submit}>
      <h1 style={{ fontSize: 22 }}>何が欲しいですか？</h1>
      <label htmlFor="n">名前</label>
      <input id="n" type="text" value={name} onChange={(e) => setName(e.target.value)} placeholder="例: ご飯管理" required />
      <label htmlFor="p">どんなものが欲しいか教えてください</label>
      <textarea id="p" value={prompt} onChange={(e) => setPrompt(e.target.value)} required
        placeholder="例: 昼と夜に家族が家でご飯を食べるかを、みんながスマホから入力できるようにしたい" />
      <p className="hint">むずかしい言葉は要りません。普通の言葉で大丈夫です。</p>
      {err && <p className="err">{err}</p>}
      <button className="btn primary" disabled={busy || !name.trim() || !prompt.trim()}>{busy ? '送っています…' : '作ってもらう'}</button>
    </form>
  );
}
