# ADR 0037: Playground HTML Previews Run in an Opaque-Origin Sandbox With Network Access Disabled by Default

Date: 2026-10-08. Status: Accepted.

In Playground conversations, code blocks with the language `html` / `htm` / `xhtml` / `svg`, and replies consisting entirely of an HTML document, can be previewed in a drawer on the right. Preview content is model output and may be determined by prompt injection, retrieved documents, or the upstream model itself, so it is treated as untrusted code:

- **Isolation**: the iframe uses `srcdoc` with `sandbox="allow-scripts allow-modals allow-downloads"` and **never adds `allow-same-origin`**. This gives the page an opaque origin, preventing access to the console's cookies, localStorage, and DOM. Its requests to the console are cross-site, so the `SameSite=Lax` session cookie is not included. `allow-forms`, `allow-popups`, and `allow-top-navigation` are also excluded. This layer remains unchanged regardless of the network setting.
- **Network**: by default, a CSP is injected at the very beginning of the page's `<head>`. Scripts, styles, images, fonts, and media must be inline or loaded over HTTPS (pages referencing CDN libraries continue to work); `connect-src 'none'` blocks fetch / XHR / WebSocket / EventSource / beacon, and all plain HTTP subresources are blocked. Any CSP supplied by the page can only add restrictions. Administrators can enable "Allow Playground HTML previews to make network requests" under "Settings → Security" (`store.SecurityPolicy.PlaygroundPreviewNetwork`, delivered as `html_preview.allow_network` by `GET /api/v1/playground/models`); when enabled, no network restrictions are injected.
- **Allow eval and wasm**: inline scripts are already allowed, so prohibiting `eval` provides no security benefit and only breaks Vue's in-browser compilation, Alpine.js, and wasm pages.
- **Bootstrap script**: this runs before page scripts and replaces `localStorage` / `sessionStorage` / `document.cookie` with in-memory implementations (accessing them from an opaque origin throws `SecurityError`). It delegates clicks on HTTP(S) links to the parent page, which, under the default policy, displays a confirmation page with the full URL before opening it with `noopener,noreferrer`. Under the default policy, it also sends CSP violations to the parent page through `postMessage`; the preview header uses these messages to explain which address was blocked and that an administrator must enable access. The parent page accepts only messages whose `event.source` is this iframe and whose structure matches the fixed schema, rendering their content as text.
- **Rendering time**: render only once, after the code block closes (or the reply ends). Show source code during generation to avoid rerunning page scripts for every chunk.
- **No "Open in new window" option**: a `blob:` URL inherits the console's origin, allowing scripts in a new tab to call management APIs with the user's session. "Download" provides the original file; opening it locally with `file://` is the user's own action.

## Tradeoffs and Consequences

- The policy runs in the viewer's own browser. It protects the viewer's browser from being exploited by model output to access the internal network and console; it does not enforce server-side permissions. You can modify only your own copy of the page.
- The default policy does not prevent data exfiltration: `img-src https:` still allows pages to embed content in image URLs and send it to external HTTPS sites. It prevents requests from the viewer's browser to the internal network and the console itself.
- Residual behavior that neither CSP nor sandbox can control:
  - Scripts can navigate the iframe itself (`location = ...`, `<meta http-equiv="refresh">`), causing a GET request. Under the default policy, the parent page resets the preview and displays a notice when it detects any `load` after the first. Subframe navigation does not carry `SameSite=Lax` or default cookies.
  - WebRTC STUN/ICE and DNS prefetching are not restricted by CSP.
- When CDNs are unreachable in an offline internal cluster, pages still render but lack external libraries. This is expected behavior.
- Storage fallbacks exist only in memory and are lost on reload; the header explains this.
- A `srcdoc` document inherits the parent page's CSP. The console currently sets no CSP; if one is added later, verify that it does not inadvertently relax or disrupt the preview policy.

## Rejected Alternatives

- Add `allow-same-origin` to make localStorage work natively: together with `allow-scripts`, it gives the page the console's origin and lets it remove its own sandbox, effectively handing the session to model output.
- Use a separate preview domain (similar to an artifact sandbox domain): this requires deployers to configure an additional domain and certificate, which is impractical for common single-binary deployments accessed directly by IP over HTTP. An opaque origin already provides equivalent session isolation.
- Provide separate HTTP/HTTPS network levels: administrators need two clear states, "blocked" or "allowed." An intermediate level (HTTPS only) still reaches HTTPS intranet services and only adds complexity.
- Debounce and rerender every chunk: this reruns scripts, replays animations, and can execute a partially written `<script>`.

<!-- translation-source-sha256: 4937783aee9fdcb55fd1767bcff8f1ab5c525704e0f7dff917a73327a9da984a # ADR 0037: Playground HTML Previews Run in an Opaque-Origin Sandbox With Network Access Disabled by Default -->
<!-- translation-source-sha256: d3936060d79b5ea85941a5af176b1f1de15baca5e1fb4627a53bc14fdc04fc0a ## Tradeoffs and Consequences -->
<!-- translation-source-sha256: f77f08368534f0de6c5b99158c9b8d42ef3a6f7f8a37521c0c2c5cabb6613ea8 ## Rejected Alternatives -->
