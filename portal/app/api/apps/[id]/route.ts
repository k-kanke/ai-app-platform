import { CP } from '@/lib/cp';

export const dynamic = 'force-dynamic';

export async function GET(_: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const r = await fetch(`${CP}/api/v1/apps/${encodeURIComponent(id)}`, { cache: 'no-store' });
  return new Response(await r.text(), { status: r.status, headers: { 'Content-Type': 'application/json' } });
}

// Deleting from the Portal also removes the app's data (purge). The nightly backup is the safety net.
export async function DELETE(_: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const r = await fetch(`${CP}/api/v1/apps/${encodeURIComponent(id)}?purge=true`, { method: 'DELETE', cache: 'no-store' });
  return new Response(await r.text(), { status: r.status, headers: { 'Content-Type': 'application/json' } });
}
