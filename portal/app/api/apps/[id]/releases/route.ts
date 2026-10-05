import { CP } from '@/lib/cp';

export const dynamic = 'force-dynamic';

export async function GET(_: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const r = await fetch(`${CP}/api/v1/apps/${encodeURIComponent(id)}/releases`, { cache: 'no-store' });
  return new Response(await r.text(), { status: r.status, headers: { 'Content-Type': 'application/json' } });
}
