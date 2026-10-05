'use client';
import { useEffect, useRef, useState } from 'react';
import { PHASE_LABEL, type AppView } from '@/lib/cp';

type Ev = { id: number; phase: string; message: string };
const STEPS = [
  ['AGENT_STARTING', '準備'], ['GENERATING', '作成'], ['TESTING', '確認'], ['STARTING', '起動'], ['READY', '完成'],
] as const;

export default function View({ initial }: { initial: AppView }) {
  const [app, setApp] = useState(initial);
  const [log, setLog] = useState<Ev[]>([]);
  const [prompt, setPrompt] = useState('');
  const [err, setErr] = useState('');
  const key = useRef(crypto.randomUUID());

  async function refresh() {
    const r = await fetch(`/api/apps/${app.id}`, { cache: 'no-store' });
    if (r.ok) setApp(await r.json());
  }

  // Live progress over SSE (proxied by the Portal); EventSource resumes via Last-Event-ID.
  useEffect(() => {
    const es = new EventSource(`/api/apps/${app.id}/events`);
    es.addEventListener('progress', (m) => {
      const e: Ev = JSON.parse((m as MessageEvent).data);
      setLog((l) => [...l.slice(-50), e]);
      refresh();
    });
    return () => es.close();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [app.id]);

  const s = PHASE_LABEL[app.phase] ?? { text: app.phase, tone: 'wait' as const };
  const busy = app.operation && (app.operation.state === 'PENDING' || app.operation.state === 'RUNNING');
  const idx = STEPS.findIndex(([p]) => p === app.phase);
  const failedChange = app.phase === 'READY' && app.operation?.state === 'FAILED';
  const failedCreate = app.phase === 'FAILED';
  const reason = app.operation?.userMessage;
  const [retryPrompt, setRetryPrompt] = useState(app.operation?.prompt ?? '');
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const retryKey = useRef(crypto.randomUUID());

  async function retry(e: React.FormEvent) {
    e.preventDefault(); setErr('');
    const r = await fetch(`/api/apps/${app.id}/retry`, {
      method: 'POST', headers: { 'Content-Type': 'application/json', 'Idempotency-Key': retryKey.current },
      body: JSON.stringify({ prompt: retryPrompt }),
    });
    if (r.ok) { retryKey.current = crypto.randomUUID(); setLog([]); refresh(); }
    else setErr((await r.json().catch(() => ({}))).error ?? 'うまく送れませんでした');
  }

  async function remove() {
    setDeleting(true); setErr('');
    const r = await fetch(`/api/apps/${app.id}`, { method: 'DELETE' });
    if (r.ok) { location.href = '/'; return; }
    setDeleting(false);
    setErr((await r.json().catch(() => ({}))).error ?? '削除できませんでした');
  }

  async function change(e: React.FormEvent) {
    e.preventDefault(); setErr('');
    const r = await fetch(`/api/apps/${app.id}/changes`, {
      method: 'POST', headers: { 'Content-Type': 'application/json', 'Idempotency-Key': key.current },
      body: JSON.stringify({ prompt }),
    });
    if (r.ok) { setPrompt(''); key.current = crypto.randomUUID(); refresh(); }
    else setErr((await r.json().catch(() => ({}))).error ?? 'うまく送れませんでした');
  }

  return (
    <>
      <h1 style={{ fontSize: 22 }}>{app.name}</h1>
      <div className={`status ${s.tone}`}>● {s.text}</div>

      {busy && (
        <div className="card">
          <ul className="steps">
            {STEPS.map(([p, label], i) => (
              <li key={p} className={i === idx ? 'now' : ''}>{i < idx ? '✓' : i === idx ? '●' : '○'} {label}</li>
            ))}
          </ul>
          <p className="hint">できあがるまで、このページを開いたままでも、閉じても大丈夫です。</p>
        </div>
      )}

      {app.phase === 'READY' && app.url && <p><a className="btn primary" href={app.url}>アプリを開く</a></p>}
      {failedChange && (
        <p className="err">前回の変更はうまくいかなかったので、元のアプリに戻しました。{reason ? <><br />{reason}</> : '言い方を変えてもう一度試せます。'}</p>
      )}
      {failedCreate && !busy && (
        <form onSubmit={retry} className="card">
          <p className="err" style={{ marginTop: 0 }}>{reason ?? 'うまく作れませんでした。'}</p>
          <label htmlFor="r" style={{ marginTop: 0 }}>内容を直して、もう一度作れます</label>
          <textarea id="r" value={retryPrompt} onChange={(e) => setRetryPrompt(e.target.value)} required />
          {err && <p className="err">{err}</p>}
          <button className="btn primary" disabled={!retryPrompt.trim()}>もう一度作る</button>
        </form>
      )}

      {app.phase === 'READY' && !busy && (
        <form onSubmit={change} className="card">
          <label htmlFor="c" style={{ marginTop: 0 }}>変えたいところを教えてください</label>
          <textarea id="c" value={prompt} onChange={(e) => setPrompt(e.target.value)} placeholder="例: 文字をもっと大きくして" required />
          {err && <p className="err">{err}</p>}
          <button className="btn primary" disabled={!prompt.trim()}>変更をお願いする</button>
        </form>
      )}

      {!busy && (
        <div style={{ marginTop: 32 }}>
          {!confirmDelete ? (
            <button className="btn" onClick={() => setConfirmDelete(true)}>このアプリを削除する</button>
          ) : (
            <div className="card">
              <p style={{ marginTop: 0 }}><b>「{app.name}」を削除します。</b><br />入力したデータも消えて、元に戻せません。</p>
              {err && <p className="err">{err}</p>}
              <div className="btns">
                <button className="btn" disabled={deleting} onClick={() => setConfirmDelete(false)}>やめる</button>
                <button className="btn primary" style={{ background: '#c0392b', borderColor: '#c0392b' }} disabled={deleting} onClick={remove}>
                  {deleting ? '削除しています…' : '削除する'}
                </button>
              </div>
            </div>
          )}
        </div>
      )}

      {log.length > 0 && (
        <div className="log">{log.filter((e) => e.message).slice(-8).reverse().map((e) => <div key={e.id}>{e.message}</div>)}</div>
      )}
    </>
  );
}
