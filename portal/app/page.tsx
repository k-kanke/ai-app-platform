import { listApps, PHASE_LABEL } from '@/lib/cp';

export const dynamic = 'force-dynamic';

export default async function Home() {
  let apps;
  try {
    apps = await listApps();
  } catch {
    return <p className="err">いま接続できません。少し待ってからもう一度開いてください。</p>;
  }
  return (
    <>
      <h1 style={{ fontSize: 22 }}>あなたのアプリ</h1>
      {apps.length === 0 && <p className="hint">まだありません。下のボタンから作ってみましょう。</p>}
      {apps.map((a) => {
        const s = PHASE_LABEL[a.phase] ?? { text: a.phase, tone: 'wait' as const };
        return (
          <div className="card" key={a.id}>
            <h2>{a.name}</h2>
            <div className={`status ${s.tone}`}>● {s.text}</div>
            <div className="btns">
              {a.phase === 'READY' && a.url && <a className="btn primary" href={a.url}>開く</a>}
              <a className="btn" href={`/apps/${a.id}`}>{a.phase === 'READY' ? '変更する' : '様子を見る'}</a>
            </div>
          </div>
        );
      })}
      <a className="btn primary" href="/new">＋ 新しいものを作る</a>
    </>
  );
}
