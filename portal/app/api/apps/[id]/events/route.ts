import { CP } from '@/lib/cp';

export const dynamic = 'force-dynamic';

// Pass the Control Plane's SSE stream through to the browser unchanged.
export async function GET(req: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const last = req.headers.get('Last-Event-ID');
  const upstream = await fetch(`${CP}/api/v1/apps/${encodeURIComponent(id)}/events`, {
    headers: last ? { 'Last-Event-ID': last } : {}, signal: req.signal, cache: 'no-store',
  });
  if (!upstream.ok || !upstream.body) return new Response('upstream error', { status: 502 });
  return new Response(upstream.body, {
    headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache, no-transform', 'X-Accel-Buffering': 'no' },
  });
}
