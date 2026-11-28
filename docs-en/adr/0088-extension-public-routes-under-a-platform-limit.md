# ADR 0088: Extension Routes That Need No Sign-in Follow Their Method While the Whole Server Is Limited; Only Routes Declared Part of Signing In Are Served Under Any Limit

Date: 2026-11-28 · Status: Accepted (tightens the rule, from the implementation of [ADR 0071](0071-installable-extensions.md), that a plugin package's routes needing no sign-in are served under any limit; the compiled modules' public routes of [ADR 0085](0085-external-identity-connectors.md) decision 9 follow this page)

## Background

A module can set the whole server to read-only or locked with `Host.LimitPlatform` ([Extension Points](../dev/extensions.md#整台-server-的可用性)). While limited, the core classifies requests by route pattern: reads are served, changes are refused, and the recovery surface (signing in, health checks, the console shell, the connection of machines that already joined, and so on) is served under any limit.

A plugin package's public routes (callable without signing in, `access: public`) did not go through this classification: they were served under any limit, on the grounds that "a package that signs users in must keep working" (GWKS-477). So any public write route of a package still ran while the server was read-only or locked. Compiled modules had no public routes yet; step 2 of [ADR 0085](0085-external-identity-connectors.md) planned to give them `AccessPublic` and meant to carry the same exemption. "Needs no sign-in" says who may call a route, not whether it changes anything; it is the sign-in flow that a limited server has to keep.

## Decision

1. **Compiled modules' public routes: `api.AccessPublic`.** No authentication; limited per client address by the same rule as a plugin package's `public` routes (60 a minute by default, bursts of 30; over that, 429 `TOO_MANY_REQUESTS` with `Retry-After`). The chain never reads or renews a session, so the handler never learns who calls: with a valid session cookie, the handler sees no claims and the response carries no cookie. The route still names a feature or is marked a community route; a public route cannot be readable by the AI assistant. The route inventory shows `[access=public]`.
2. **Sign-in routes: `RouteSpec.SignIn`.** Only an `AccessPublic` route may set it, otherwise mounting panics. It marks the route as part of signing in: an identity connector's start and callback routes, the POST that carries a SAML response included. The route inventory shows `[sign-in]` last on the line.
3. **While limited, public routes follow their method.** A compiled module's public routes and a plugin package's `public` routes are classified like other routes under `/api/v1`: GET, HEAD and OPTIONS are reads and are served; other methods are changes and are refused while read-only and while locked. A compiled module's route that declares `SignIn` is recovery surface and is served under any limit, whatever its method.
4. **A refused public route answers the platform refusal at once.** 403 with the limit's code (`PLATFORM_READ_ONLY`, `PLATFORM_LOCKED`, or the notice the limiting module gives), without asking for authentication first. Other `/api/v1` routes authenticate before refusing so that a caller who has not signed in learns neither which routes exist nor that the server is limited; a public route is open to everyone, so there is nothing to hide.
5. **Plugin packages cannot declare sign-in routes yet.** It needs a new field on manifest routes, and that field changes enforcement; under [ADR 0071](0071-installable-extensions.md) decision 7 it raises the manifest schema (1 today), so it comes together with step 3 of [ADR 0085](0085-external-identity-connectors.md) (the manifest's `sign_in`) and the schema 2 planned by [ADR 0081](0081-role-gated-plugin-routes.md). Until then a plugin package's public POST is refused while the server is limited.

## Extension level

- Now: compiled modules have no public routes; a plugin package's public routes are served under any limit (an L3 route with an exemption hard-coded as one rule).
- Target: public routes and sign-in routes are route declarations (an L1 seam: `RouteSpec` fields and the manifest's access level), and the core enforces them under a limit from one classification table.
- Criteria ([ADR 0084](0084-extension-levels-and-table-kind-registry.md) decision 3): 1 the classification sits in one place, `classifyPlatformRoute` and `platformGuard`; 2 whether a route is part of signing in is the extension's to say, so the extension declares it; 3 the decision is on the per-request path and only looks up a table recorded at mount time, one map lookup; 4 and 5 do not apply; 6 a server with no extension behaves the same; 7 the contract cost is two values of `RouteSpec` and two route inventory markers, add-only.

## Trade-offs

- **Classifying by method vs serving every public route**: serving every public route is simpler, but it makes "needs no sign-in" a way around the limit: a package's or module's public write route changes data on a locked server. Classifying by method leaves only the writes declared part of signing in served under a limit.
- **Declaring sign-in routes vs serving public GETs only**: serving only GETs needs no new field, but a SAML callback is a POST, so it would block a real sign-in flow.
- **Adding the manifest field to plugin packages now vs later**: adding it now raises the manifest schema on its own, while step 3 raises it anyway; the cost of waiting is that a package's public POST callback is unavailable under a limit meanwhile, which affects nothing today since no identity connector ships as a plugin package.

## Consequences

- This tightens the behaviour of GWKS-477 (`02852bd00`): under a limit a plugin package's public POST is no longer served; its public GET still is. This build embeds no project root key yet, and no trusted plugin package has been published.
- An identity connector's (ADR 0085) start and callback routes all set `SignIn`.

## Implementation status

Implemented: `api.AccessPublic`, `RouteSpec.SignIn`, the mount-time checks, the route inventory markers, the classification and the immediate refusal in `platformGuard`, with tests and a contract test. Sign-in routes for plugin packages come with step 3 of ADR 0085.

<!-- translation-source-sha256: 14709db69bc4d609625cf55190c69a8c5d4fe3a1711910ec742dda64b2a74245 # ADR 0088: Extension Routes That Need No Sign-in Follow Their Method While the Whole Server Is Limited; Only Routes Declared Part of Signing In Are Served Under Any Limit -->
<!-- translation-source-sha256: 4fda9a7f800f500599c2a709b892ca953a4c4c2fb5fa659ac64980833af6ea04 ## Background -->
<!-- translation-source-sha256: fd395d392c6c57317bd46eca37ac0b864fe7694f49d0c08755fffa13637c282c ## Decision -->
<!-- translation-source-sha256: 0f8d8d3042b02a5252342703452b17a470de574c313e0bd76da663214c5aa148 ## Extension level -->
<!-- translation-source-sha256: e7ca6fa6225b3cfc4c99bc84786998b3219e180998af4dd6650882fac3a40fb7 ## Trade-offs -->
<!-- translation-source-sha256: d5eb37330b5e35c7434176780fed2b6c895adfe95d5521e090cc82e6c39c5857 ## Consequences -->
<!-- translation-source-sha256: b27a8ec0313beda07a6dddfe8c0b725ee5307d9578769a9b63ba2170c6ccff57 ## Implementation status -->
