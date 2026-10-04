import { getApp } from '@/lib/cp';
import { notFound } from 'next/navigation';
import AppView from './view';

export const dynamic = 'force-dynamic';

export default async function Page({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const app = await getApp(id);
  if (!app) notFound();
  return <AppView initial={app} />;
}
