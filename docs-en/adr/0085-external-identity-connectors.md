# ADR 0085: External Identity: Plugins Are Identity Connectors, the Core Creates Accounts and Issues Sessions

Date: 2026-11-27 · Status: Accepted, steps 1 and 2 implemented, the rest not (adds to [ADR 0071](0071-installable-extensions.md) decisions 3, 11 and 19 and to [ADR 0081](0081-role-gated-plugin-routes.md); the extension level terms are in [ADR 0084](0084-extension-levels-and-table-kind-registry.md))

## Background

The management plane has only local password sign-in: `POST /api/v1/auth/login` checks `users.password_hash`, then creates a `management_sessions` row and hands out a cookie and a bearer token. Sites usually have their own identity system, require employees to sign in with it, and let it decide who is still employed: OIDC, SAML, LDAP, and the OAuth2 of office platforms such as DingTalk, Feishu and WeCom.

These identity sources all differ, and the variation is on the side of sites and publishers, so plugins are the right place to integrate them (ADR 0084 criterion 2). Plugins can already do half of it: a plugin route can be `public`, can answer HTML, and can set cookies for a single sign-on callback (`extapi.RouteDecl.Cookies`); the host capabilities also reserve `host.directory` (only the read permission `read:directory` exists, not implemented). The other half is missing in the core:

- a plugin cannot reach the user table, so it cannot find or create a core user for an external identity;
- a plugin cannot issue a core session; sessions, cookie attributes and revocation belong to the core;
- the sign-in page has no place for "sign in with ...";
- compiled modules only have `AccessRead`, `AccessAdmin` and `AccessAdminSession`, no unauthenticated route, so they cannot receive a callback;
- users cannot be "disabled": when an identity source says someone has left, the core cannot take away all of their access at once;
- ADR 0071 allows one synchronous call to a plugin at sign-in to check a password (LDAP), but never fixed its shape.

## Decision

1. **Identity connectors are plugins; accounts and sessions belong to the core.** A connector talks to the identity source: redirects, callbacks, checking assertions or passwords, syncing the directory. The core links external identities to core users, creates and disables users, and issues and revokes sessions. A plugin never reads `users`, `management_sessions` or password hashes, and cannot set the core's session cookie.

2. **An external identity is a row in a core table.** New table `user_identities`: `user_id`, `source` (the connector's source name, `[a-z][a-z0-9_]{1,31}`, exclusive to one plugin package or module, like role names in ADR 0081 decision 4), `subject` (an id in the identity source that never changes, not a username or email), `created_at`, `last_sign_in_at`; `(source, subject)` is unique. One core user can link several sources. This is a new fact that cannot be derived from existing data (`CLAUDE.md`, "新增持久化事实"). `users` gains `disabled_at` (empty means enabled).

3. **Two connector shapes.**
   - **Redirect** (OIDC, SAML, office-platform OAuth2):
     1. The sign-in page lists the entries (decision 6). A click goes first to the core's `GET /api/v1/auth/sign-in/start?source=…`. The core makes a random **browser binding**, stores it with the source name in a one-time cookie (`HttpOnly`, `Secure`, `SameSite=Lax`, valid for 10 minutes), then redirects the browser to the `start` route the connector declared, with a handle in the query: derived from the source and the binding, and impossible to turn back into the binding (an HMAC with a key derived from the master key);
     2. The connector takes the browser to the identity source, carrying the handle in its own state. The identity source returns to the connector's `public` callback route, and the connector checks the result itself (state, nonce, signature, token exchange);
     3. The connector calls the host's `accounts.sign_in` (decision 4) with `{source, subject, profile, handle}`. The core finds or creates the user by link (decision 5) and returns a one-time sign-in ticket bound to that handle;
     4. The connector redirects the browser to the core's `GET /api/v1/auth/complete?ticket=…`. The core checks that the ticket matches the binding cookie the browser brings: the ticket must have been issued for the cookie's source and bound to the handle computed from it; only then does it create the session and set the session cookie, delete the binding cookie and go to the console.

     A ticket is valid for 60 seconds, can be used once, is stored only as a hash, and is bound to the source and handle it was issued for. Redemption **trusts only the browser binding** and looks at neither `Sec-Fetch-Site` nor `Origin`: the top-level GET returning through the identity source is cross-site by nature, so `managementOriginAllowed` would refuse it, and a top-level GET carries no `Origin`, so that check binds nothing anyway. The binding stops login CSRF: a ticket an attacker obtained with their own identity is bound to the attacker's browser; when a victim is lured into opening the redemption link, the victim's browser has no matching cookie, and redemption is refused with `IDENTITY_TICKET_INVALID`. Connector routes do not need to set core cookies; their own state cookie is set through `Cookies` as usual.
   - **Password** (LDAP): at sign-in, when the core finds the username linked to a password source, it calls that connector's check (`POST /gwx/v1/accounts/verify`, 5-second timeout) synchronously within the sign-in request and gets one of three results: passed, failed, identity source unreachable. This is exactly the one synchronous call ADR 0071 decision 3 allows. Users without a link are checked against the local password as usual; when the identity source is unreachable, local users and existing sessions are unaffected, with no offline verification cache (ADR 0071 decision 19).

4. **Host capability `host.accounts`.** A plugin package requests the permission `write:accounts` (risk `high`) in its manifest, and only packages whose publisher certificate grants it may use it; a compiled module gets the same operations from `app.Host.Accounts()`. Every operation is scoped to the connector's own source name:
   - `sign_in(source, subject, profile, handle)` returns a ticket bound to `handle`, or refuses with a code;
   - `upsert(source, subject, profile)`: directory sync, creates or updates a user;
   - `disable(source, subject)` / `enable(source, subject)`: disable, restore;
   - `revoke_sessions(source, subject)`: on a sign-out from the identity source, ends all of this person's sessions;
   - `list(source, cursor, limit)`: pages through the users linked to this source, for reconciliation during a full sync.

   `profile` has only three fields, `username`, `display_name` and `email`, decoded strictly; it cannot carry a role.

   **Administrators are out of connectors' reach.** For a user whose `users.local_admin` is true or whose `users.role` is `admin`, `disable`, `enable`, `revoke_sessions`, and an `upsert` that would change the username are all refused with `IDENTITY_PROTECTED_USER`; disabling, restoring, renaming or signing out an administrator account can only be done by an administrator through core interfaces. `sign_in` is not affected: an administrator who explicitly linked an external identity can sign in with it. So a connector (including a compromised plugin package) cannot disable the local administrator or delete their sessions, and the floor of ADR 0071 decision 11 cannot be broken by a connector on purpose, not just hold while connectors are down. At implementation, contract tests will prove that neither a compiled module nor a fake plugin process can disable, restore, rename or sign out a local administrator or a platform administrator.

5. **The core decides whether to find or create a user.**
   - An existing `(source, subject)` link: that is the user. A sign-in never renames; the username changes only through a directory sync (`upsert`), under the same administrator protection.
   - No link: with the site policy `auth.external.provision` set to `auto` (the default), a new user is created, with role `user` and no local password; with `existing`, it is refused (`IDENTITY_NOT_PROVISIONED`), and only people an administrator linked beforehand can sign in.
   - **Never link automatically to an existing user by username or email**: that would let an identity source or plugin take over any local account of the same name (administrators included). Linking an existing user to an external identity is only done explicitly by an administrator through core interfaces.
   - When a new user's `username` collides with an existing user, it is refused (`IDENTITY_USERNAME_TAKEN{username}`); the connector picks another (for example with a source prefix) and retries, and the core never renames on its own.
   - An external identity can never make a user a platform administrator or local administrator: `profile` carries no role, and core roles are changed only by administrators; extra roles are pushed through `roles.bindings` of ADR 0081, which cannot produce `admin` either.

6. **Sign-in entries are declared by connectors and served by the core.** A plugin package declares `{source, label, icon, start}` under `sign_in` in its manifest (`start` must be a `public` `GET` route of the same package); a compiled module declares the same through `app.Host`. The core returns the entries available now from the unauthenticated `GET /api/v1/auth/sign-in-options`; an entry links to the core's `/api/v1/auth/sign-in/start?source=…` (decision 3), not to the connector's route, and it is not listed while its plugin is down or its feature is locked or hidden. The response also says whether local password sign-in is available (decision 8). The sign-in page only renders this list and decides nothing.

7. **Disabling takes effect at once; disconnects never disable.**
   - A disabled user: all sessions are deleted at once; sign-in, ticket redemption, platform tokens and inference API keys are all refused with `USER_DISABLED`. Data and keys are kept and work again after a restore (ADR 0071 decision 13: running workloads are not interrupted; disabling only affects new requests).
   - Time to take effect: sessions are read from the database per request, so deleting them takes effect at once on every server. Inference API keys and platform tokens each have an in-process cache (`apiKeyCacheTTL` of 5 minutes in `store/apikey_cache.go`, `platformTokenCacheTTL` of 1 minute in `store/platform_tokens.go`): the server process that disables the user advances the generation of both caches and evicts that user id's entries, so it takes effect at once in that process; other server processes in a cluster follow at the latest when their caches' TTLs expire. This bound is documented, and no cross-process invalidation channel is invented. If a cache's TTL ever grows, it must come with cross-process invalidation.
   - When a connector disconnects, is uninstalled or crashes: its entry disappears from the sign-in page and its callback route answers 503; existing sessions are unaffected; synced users are never disabled because of the disconnect, nor because a long time has passed without a sync. Disabling only comes from an explicit `disable` by a connector, or from an administrator.

8. **Local sign-in and the floor.** The site policy `auth.local_password` is `all` (the default) or `local_admin`. With `local_admin`, only users whose `users.local_admin` is true can sign in with a local password, and everyone else only through an external identity; the local administrator's password sign-in always works, as ADR 0071 decision 11 requires, so the administrator can still sign in when every connector is down.

9. **`public` routes for compiled modules.** `api.RouteAccess` gains `AccessPublic`: unauthenticated, rate-limited per client address (the same rules as `public` plugin routes), never a session write; the route must still name a feature or be marked a community route.

10. **Codes.** `IDENTITY_TICKET_INVALID` (the ticket does not exist, expired, was used, the source does not match, or it does not match the browser binding), `IDENTITY_PROTECTED_USER` (a connector tried to disable, restore, rename or sign out an administrator), `IDENTITY_NOT_PROVISIONED`, `IDENTITY_USERNAME_TAKEN{username}`, `IDENTITY_SOURCE_CONFLICT{source}` (the source name is owned by another package or module), `IDENTITY_SOURCE_UNAVAILABLE{source}` (signing in through this source is not possible now: no extension offers a sign-in through it, its feature is locked or hidden, or the identity source is unreachable during a password check), `USER_DISABLED`, `LOCAL_PASSWORD_DISABLED`.

## Extension level

- Now: L0. Local password sign-in only, no seam.
- Target: connectors at L3 (plugin packages), accounts and sessions an L1 mechanism of the core, sign-in entries at L2 (declared in the manifest, served by the core).
- Criteria (ADR 0084 decision 3):
  1. This ADR creates the seam;
  2. the variation comes from outside: every site's identity source differs;
  3. not on a hot path: sign-in is the one place ADR 0071 allows a synchronous plugin call, and every other request only reads the core's sessions and `disabled_at`;
  4. nothing runs in a GPU container or on the host;
  5. no real-machine verification is needed;
  6. for a server with no connector installed, behaviour is byte-for-byte the same as now: the sign-in page has only the password form, and `user_identities` is empty;
  7. contract cost: the five operations of `host.accounts` and the manifest's `sign_in` declaration, add-only.

## Trade-offs

- **The core issues tickets and sets cookies itself vs plugin routes set the session cookie**: the latter saves one redirect, but session cookie attributes (`HttpOnly`, `SameSite`, `Secure`, lifetime) and revocation would spread into every plugin, and plugins would hold session credentials they could use directly. A ticket can be redeemed once, within 60 seconds, and only in the browser that started the sign-in, so a leak costs little.
- **A browser binding vs checking where the request came from**: checking `Sec-Fetch-Site`/`Origin` saves a redirect, but the return leg of a redirect sign-in is a cross-site top-level GET by nature, so an origin check either refuses the normal flow or binds nothing. The browser binding costs one more redirect through the core and gives certain protection against login CSRF.
- **Automatic linking by username vs linking only by `(source, subject)`**: automatic linking eases migration, but anyone who controls usernames in the identity source could take over a local account of the same name. Migration is done by explicit administrator linking instead.
- **Disable on disconnect vs disable only when told**: disabling on disconnect would make the connector a single point of failure for the whole site, where one identity source outage locks everyone out; timely disabling of departures is the job of the connector's events or scheduled sync.
- **One general connector protocol vs OIDC built into the core**: built-in OIDC covers a lot, but DingTalk, Feishu and WeCom are not standard OIDC, nor are SAML and LDAP; the core handles only accounts and sessions, every protocol lives in connectors, and the core keeps one mechanism.

## Implementation status

Steps 1 and 2 are implemented (2026-11-27), for compiled modules only; steps 3 and 4 are not. Order, each step with contract tests (compiled modules through `internal/testenv/extmodule`, plugin packages against a fake plugin process, ADR 0071 decision 21):

1. **Implemented.** `user_identities` (migration 252) and `users.disabled_at`; enforcing disablement (sessions, sign-in, platform tokens, API keys); the administrator interfaces to disable, restore, list links, link and unlink (`/api/v1/users…`, see the [API reference](../reference/api.md#accounts账号与外部身份)); `auth.local_password`.
2. **Implemented.** The browser binding and `GET /api/v1/auth/sign-in/start`; tickets (`sign_in_tickets` of migration 253, kept in the database so any cluster member can redeem them) and `GET /api/v1/auth/complete`, which redirects (302) to `/login?code=<code>` on a refusal; the check that keeps administrators out of connectors' reach; `DeclareIdentitySources`, `OfferSignIn` and `Accounts()` of `app.Host`; `api.AccessPublic`; `GET /api/v1/auth/sign-in-options` and the sign-in page entries; `auth.external.provision` (see [Extension points](../dev/extensions.md#身份连接器) and the [API reference](../reference/api.md#通过身份连接器登录)). A compiled module can now implement a redirect connector.
3. The `write:accounts` permission for plugin packages, the `host.accounts` host interface and the manifest's `sign_in` declaration.
4. The synchronous check interface for password connectors.

<!-- translation-source-sha256: c6890feff7b100f8f0ad4a1f1451c4aafae99ba3eb06ddccfe3290923de07a68 # ADR 0085: External Identity: Plugins Are Identity Connectors, the Core Creates Accounts and Issues Sessions -->
<!-- translation-source-sha256: a9b89a0d37a27045da734b29e4c42fbe8c18e709426c5670c535c0a3536cf009 ## Background -->
<!-- translation-source-sha256: a17aa47aaacedab884010f709a0a0f3b9ccd0842fca70c3c7eee2d09657a0972 ## Decision -->
<!-- translation-source-sha256: faf40b805c5bc1b22d8a18f0d7b80320fcc208a69eef6823e055d6e845901ffc ## Extension level -->
<!-- translation-source-sha256: eaeee62586f9665447d7722bc0da9c3226e048659e0d3d461eaedb9575fdadbb ## Trade-offs -->
<!-- translation-source-sha256: e50a546687c29b6acfe798ed481ef8836604140db4ca8892a2d135a2059ab17c ## Implementation status -->
