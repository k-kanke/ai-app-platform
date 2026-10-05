import { CP } from '@/lib/cp';

export const dynamic = 'force-dynamic';

export async function POST(req: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const key = req.headers.get('Idempotency-Key');
  const r = await fetch(`${CP}/api/v1/apps/${encodeURIComponent(id)}/approve`, {
    method: 'POST', body: await req.text(), cache: 'no-store',
    headers: { 'Content-Type': 'application/json', ...(key ? { 'Idempotency-Key': key } : {}) },
  });
  return new Response(await r.text(), { status: r.status, headers: { 'Content-Type': 'application/json' } });
}
