# Inspector clients

Two reference clients for the read API at `/api/v1`. Inspector embeds both (`embed.go`) and serves them with the env's domain filled in:

| Client | Served at |
|---|---|
| Userscript | `https://inspector.<domain>/userscript/roll-inspector.user.js` |
| DevTools extension (zip) | `https://inspector.<domain>/extension/roll-inspector.zip` |

The UI page "API & extensions" has the install steps. Both clients read only; neither can purge, flush or change a setting. User documentation lives in `roll-docker-stack/docs/inspector/api-and-extensions.md`.

## Userscript

`userscript/roll-inspector.user.js` runs on `https://*.<domain>/*` except Inspector itself, once per top-level page.

1. It calls `GET /api/v1/page?url=<page url>` through `GM_xmlhttpRequest`, and falls back to `fetch` (which works because v1 sends `Access-Control-Allow-Origin: *`).
2. The trace lands just after the page, so it retries at 250 ms, 500 ms, 1 s and 2 s (five tries).
3. It draws a badge in a shadow root, so the shop's CSS cannot reach it: Varnish result, PHP time, SQL calls and N+1 count. The popover shows the cost strip and the three hottest items, each linking to the trace in Inspector.
4. "Hide on this site" is stored with `GM_setValue`; the manager menu entry "Show the Inspector badge on <host>" undoes it.

It builds the badge with DOM APIs and `textContent`. Pure helpers sit in the factory so `node --test` can load them.

## DevTools extension

`extension/` is a Manifest V3 extension (Chrome or Edge 110+). The "Roll Inspector" panel lists the inspected page's document and XHR/fetch requests from `chrome.devtools.network`.

- **Join.** Each row reads `X-Varnish` from the response and asks `/api/v1/page?url=...&xid=...`, an exact match.
- **Discovery.** The panel takes the inspected page's own hostname and tries `inspector.<host without its first label>`, then `inspector.<host>`. A candidate counts once `/api/v1/` answers and names the env. Only requests on the domain Inspector confirmed get looked up; rows on other hosts show "other site". The options page can set an address by hand, and it wins.
- **Permissions.** `https://*.test/*` by default. "Allow every site" on the options page asks for `<all_urls>` at runtime, for envs on another domain.
- **Install.** Unzip, open `chrome://extensions`, switch on Developer mode, Load unpacked, pick `roll-inspector`.

## Build your own client

Everything under `/api/v1` is GET, HEAD or OPTIONS, JSON, `Cache-Control: no-store`, errors as `{error, hint}`. There is no cookie, token or origin check, so `fetch` works from any page. The consequence: any site open in the same browser can read the shop's traces and cache too.

| Route | Returns |
|---|---|
| `/api/v1/` | `{env, domain, version, inspectorUrl, api, endpoints[]}`; use it to discover and confirm the env |
| `/api/v1/openapi.json` | OpenAPI 3.1 of every route with its query parameters |
| `/api/v1/overview` | service health tones and warnings with copyable fixes |
| `/api/v1/page?url=&xid=` | latest timeline entry for a URL or Varnish xid: HIT/MISS/PASS and why, lanes, trace summary, top hot items, deep links; 404 with a reason when nothing matches yet |
| `/api/v1/requests?url=&xid=&limit=` | recent timeline entries |
| `/api/v1/traces?route=&url=&min_ms=&sort=&limit=` | trace summaries with per-category `times` |
| `/api/v1/traces/{id}`, `/analysis` | summary plus analysis; `?spans=1` adds spans |
| `/api/v1/hotspots`, `/api/v1/traces/metrics` | routes, operations, repeats; PHP series |
| `/api/v1/<kind>/...` | the service reads (`info`, `health`, `metrics`, browse and search) for varnish, redis, search, rabbitmq |
| `/api/v1/stream?sub=` | server-sent events for live panels |

Streams start work on the shop's services (Valkey MONITOR, varnishlog). Each stream accepts only the filters it reads (400 otherwise), `/api/v1` may start at most 2 filtered variants (429 beyond that), and a source stopped on the Performance page answers 409 for 10 minutes. Stop streams when your panel closes. `HEAD /api/v1/stream?sub=` checks a subscription and starts nothing.

Minimal client:

```js
const base = 'https://inspector.example.test/api/v1';
const info = await fetch(`${base}/`).then(r => r.json());   // confirms the env
const page = await fetch(`${base}/page?url=${encodeURIComponent(location.href)}`);
if (page.status === 404) { /* no trace yet: retry after 250 ms, 500 ms, 1 s, 2 s */ }
const { links } = await page.json();                        // links.bottlenecks opens the UI
```

Start from `/api/v1/` to confirm the env, then call `/api/v1/page` for the page in front of you, and link to `links.bottlenecks` for the detail.
