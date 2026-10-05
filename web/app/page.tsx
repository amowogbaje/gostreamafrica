export const dynamic = "force-dynamic";

async function apiStatus(): Promise<string> {
  const base = process.env.API_INTERNAL_URL ?? "http://api:8080";
  try {
    const res = await fetch(`${base}/readyz`, {
      cache: "no-store",
      signal: AbortSignal.timeout(2000),
    });
    const body = (await res.json()) as { status?: string };
    return body.status ?? "unknown";
  } catch {
    return "unreachable";
  }
}

export default async function Home() {
  const status = await apiStatus();
  return (
    <main>
      <h1>StreamAfrica</h1>
      <p>Foundations running. Nothing to watch yet.</p>
      <p>
        API status: <strong>{status}</strong>
      </p>
    </main>
  );
}
