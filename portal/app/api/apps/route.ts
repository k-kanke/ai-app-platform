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
  return forward(await fetch(`${CP}/api/v1/apps`, {
    method: 'POST', body: await req.text(), cache: 'no-store',
    headers: { 'Content-Type': 'application/json', ...(key ? { 'Idempotency-Key': key } : {}) },
  }));
}
