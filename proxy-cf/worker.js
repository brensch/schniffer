// Cloudflare Worker implementation of the schniffer proxy.
//
// Identical wire contract to proxy/main.go: POST /fetch with
//   Authorization: Bearer <PROXY_SECRET>
//   {"requests":[{"url","method","headers","body"}, ...]}
// returns
//   {"responses":[{"status","headers","body","error","elapsed_ms"}, ...],"region":"cf-<colo>"}
//
// Why Workers instead of Cloud Run: Workers bill CPU time, not wall time.
// Most of our request is spent in `await fetch(rec.gov)` — that's pure I/O
// wait, billed at 0. Cloud Run bills the full duration including the wait.
// At 2s polling that means ~$5/mo on Workers vs ~$30/mo on Cloud Run.
//
// Compression: Cloudflare's edge auto-applies gzip/br/zstd based on the
// client's Accept-Encoding header. No code needed here. Measured payloads
// drop from ~560 KB raw to ~7 KB gzip / ~4 KB brotli / ~4.5 KB zstd.

const SUBREQUEST_LIMIT = 45; // free tier is 50, paid 1000; leave headroom

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === '/healthz') {
      return new Response('ok');
    }
    if (url.pathname !== '/fetch') {
      return new Response('not found', { status: 404 });
    }
    if (request.method !== 'POST') {
      return new Response('method not allowed', { status: 405 });
    }

    const auth = request.headers.get('Authorization') || '';
    const expected = 'Bearer ' + (env.PROXY_SECRET || '');
    if (!env.PROXY_SECRET || auth !== expected) {
      return new Response('unauthorized', { status: 401 });
    }

    let batch;
    try {
      batch = await request.json();
    } catch (e) {
      return new Response('bad json: ' + String(e), { status: 400 });
    }
    if (!batch || !Array.isArray(batch.requests) || batch.requests.length === 0) {
      return new Response('no requests', { status: 400 });
    }
    if (batch.requests.length > SUBREQUEST_LIMIT) {
      return new Response(`too many requests in batch (max ${SUBREQUEST_LIMIT})`, { status: 400 });
    }

    const responses = await Promise.all(batch.requests.map(r => doOne(r)));

    return Response.json({
      responses,
      region: 'cf-' + (request.cf?.colo || 'unk'),
    });
  },
};

async function doOne(spec) {
  const t0 = Date.now();
  try {
    const init = {
      method: spec.method || 'GET',
      headers: spec.headers || {},
      // Bypass any CF edge caching of upstream — we want fresh availability.
      cf: { cacheTtl: 0, cacheEverything: false },
    };
    if (spec.body) init.body = spec.body;

    const upstream = await fetch(spec.url, init);
    const text = await upstream.text();

    // Flatten headers to {name: [values]} for parity with proxy/main.go,
    // which marshals from Go's http.Header.
    const headers = {};
    for (const [k, v] of upstream.headers) {
      if (headers[k]) headers[k].push(v); else headers[k] = [v];
    }

    return {
      status: upstream.status,
      headers,
      body: text,
      elapsed_ms: Date.now() - t0,
    };
  } catch (e) {
    return {
      error: 'upstream: ' + String(e),
      elapsed_ms: Date.now() - t0,
    };
  }
}
