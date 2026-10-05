// Server-side access to the Control Plane. The browser never talks to it directly.
export const CP = process.env.CONTROL_PLANE_URL ?? 'http://localhost:8080';

export type Operation = { id: string; kind: string; state: string; step: string; prompt: string; error?: string; userMessage?: string };
export type AppView = {
  id: string; name: string; phase: string; url?: string; operation?: Operation;
  strategy?: string; liveRelease?: number; prevRelease?: number; draftState?: string; previewUrl?: string; canRestoreData?: boolean;
};

export async function listApps(): Promise<AppView[]> {
  const r = await fetch(`${CP}/api/v1/apps`, { cache: 'no-store' });
  if (!r.ok) throw new Error(`control plane: ${r.status}`);
  return (await r.json()).apps ?? [];
}

export async function getApp(id: string): Promise<AppView | null> {
  const r = await fetch(`${CP}/api/v1/apps/${encodeURIComponent(id)}`, { cache: 'no-store' });
  if (r.status === 404) return null;
  if (!r.ok) throw new Error(`control plane: ${r.status}`);
  return r.json();
}

// User-facing wording: no Kubernetes / Git / image vocabulary.
export const PHASE_LABEL: Record<string, { text: string; tone: 'ok' | 'wait' | 'bad' }> = {
  QUEUED: { text: '順番を待っています', tone: 'wait' },
  AGENT_STARTING: { text: '準備しています', tone: 'wait' },
  GENERATING: { text: '作っています', tone: 'wait' },
  TESTING: { text: '動きを確認しています', tone: 'wait' },
  STARTING: { text: '起動しています', tone: 'wait' },
  READY: { text: '使えます', tone: 'ok' },
  PREVIEW: { text: 'お試し版ができました', tone: 'wait' },
  FAILED: { text: 'うまくいきませんでした', tone: 'bad' },
  DELETING: { text: '削除しています', tone: 'wait' },
};
