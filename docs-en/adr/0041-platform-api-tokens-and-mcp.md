# ADR 0041: Platform API Tokens and the MCP Service Share One Credential

Date: 2026-10-11 · Status: Accepted

## Context

The management API accepts only login sessions (JWT or Cookie, expiring after 24 hours), leaving scripts and external AI tools without a long-lived, revocable credential. Existing API keys (`gw-`) are used only for the `/v1` inference gateway and do not represent a user.
The AI assistant's tools now read platform APIs as the user asking the question (`aihelper.APIReader`). The same tools are suitable for exposing to external tools such as Claude Code and Cursor through MCP, but require a credential that can represent a user.

## Decision

1. **Add platform API tokens, separate from inference API keys**: the plaintext starts with `gwp_` (256 random bits); store only its SHA-256 and a display prefix in a separate `platform_tokens` table.
   The random string has enough entropy to avoid argon2; each request requires one indexed lookup.
2. **Use the same token for both management APIs and MCP**, without a separate MCP credential.
3. **A token represents its creator**: authorize every request against the user's current role in the user table, then apply the token scope: `read` (GET/HEAD and `POST /api/v1/mcp`) or `write` (the same access as a login). The default is `read`.
4. **No token can manage tokens** (`/api/v1/platform-tokens` accepts login sessions only), preventing a read-only token from issuing itself a read/write token.
5. **Parse authentication once in the outer `managementAuth` layer** and place the identity in the request context; `claimsFromRequest` reads the context first, while its callers and `managementAdmin` remain unchanged.
   Routes declare token scope instead of middleware checking paths: attach `sessionOnly` to token management endpoints and `readTokensRead` to the other management routes, but not to `POST /api/v1/mcp` (the tools are read-only).
6. **MCP uses Streamable HTTP and returns JSON only**: the protocol lives in the leaf package `internal/server/mcp`, which depends on no other package in this repository. The tools are `aihelper.BuiltinTools()`,
   sharing `Tool.Run` with the agent loop (the same errors and truncation). They read APIs as the caller through `APIReader` and are read-only regardless of token scope. An administrator controls the switch, which is off by default.
7. **Cache validation results briefly** (1 minute, keyed by token hash): previously, every token-bearing request read both the token row and the user row, and each MCP read required another validation. Revocation through this store takes effect immediately,
   and a read concurrent with revocation cannot put a revoked token back into the cache.

## Tradeoffs

- Do not reuse the `api_keys` table by adding a “purpose” column: inference keys do not belong to users and sit on the gateway's hot path with argon2 and caching, while management tokens must bind to users and scopes. Combining them would compromise both semantics and performance choices.
- Do not implement fine-grained permissions (per resource or endpoint): the platform has only admin / user roles, and two scope levels already cover “read-only access for AI tools” and “read/write access for automation scripts.” Extend `scope` if needed.
- Do not enable SSE streaming: all tools perform one-shot reads, so JSON responses suffice and no sessions need to be maintained.
- Revocation deletes the token without retaining a revocation record; auditing relies on slog entries for creation, revocation, and MCP tool calls.

## Consequences

- A new table and migration 137; setting key `mcp.enabled`; one more package dependency for `api` (`mcp`, architecture budget `api_fanout` 39→40).
- Documentation: [Connect Through MCP](../how-to/use-mcp.md), [API Reference: Platform Tokens](../reference/api.md#platform-tokens平台-api-令牌).

<!-- translation-source-sha256: dddffec5ac5fea2108e8931f38247c8f2f7d9842dff30bf72f0fa1d8ef5d6bbf # ADR 0041: Platform API Tokens and the MCP Service Share One Credential -->
<!-- translation-source-sha256: 03af6a1ba6b43e69dc872d66159dbd8f23aa89e160202fab97c287eb2e2f6739 ## Context -->
<!-- translation-source-sha256: 522884db17ffaa5569c6d029226ffe92fcd1865b2e7f6a3f6d32e08fb1670b98 ## Decision -->
<!-- translation-source-sha256: 13755bf68d76cffa8a863ee31d0500b7542520511ef03aca5e3471206bcfd8eb ## Tradeoffs -->
<!-- translation-source-sha256: e2b2c461e7db50bc6f4830ed1bad9955b716ce9da40a29e710c098507ad5978f ## Consequences -->
