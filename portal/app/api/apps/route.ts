import { CP } from '@/lib/cp';

export const dynamic = 'force-dynamic';

async function forward(r: Response) {
  return new Response(await r.text(), { status: r.status, headers: { 'Content-Type': 'application/json' } });
}

export async function GET() {
  return forward(await fetch(`${CP}/api/v1/apps`, { cache: 'no-store' }));
}

export async function POST(req: Request) {
  const key = req.headers.get('Idempotency-Key');
  // PORTAL_STRATEGY=release makes apps created from this Portal use drafts, previews and releases.
  const body = JSON.parse(await req.text());
  if (process.env.PORTAL_STRATEGY && !body.strategy) body.strategy = process.env.PORTAL_STRATEGY;
  return forward(await fetch(`${CP}/api/v1/apps`, {
    method: 'POST', body: JSON.stringify(body), cache: 'no-store',
    headers: { 'Content-Type': 'application/json', ...(key ? { 'Idempotency-Key': key } : {}) },
  }));
}
