# ADR 0085: 外部身份：插件做身份连接器，核心建立账号与签发会话

日期：2026-11-27 · 状态：已接受，第 1 步已实施，其余未实施（补充 [ADR 0071](0071-installable-extensions.md) 决策 3、11、19 与 [ADR 0081](0081-role-gated-plugin-routes.md)；扩展等级的说法见 [ADR 0084](0084-extension-levels-and-table-kind-registry.md)）

## 背景

管理面只有本地密码登录：`POST /api/v1/auth/login` 校验 `users.password_hash`，成功后建一条 `management_sessions` 并下发 Cookie 与 Bearer 令牌。站点普遍已有自己的身份系统，要求员工用它登录，并由它决定谁还在职：OIDC、SAML、LDAP，以及钉钉、飞书、企业微信这类办公平台的 OAuth2。

这些身份源各不相同，变化在站点与发布者那边，适合由插件对接（ADR 0084 准则 2）。插件已经能做一半：插件路由可以是 `public`，可以回 HTML，可以为单点登录回调设置 Cookie（`extapi.RouteDecl.Cookies`）；宿主能力里也留了 `host.directory`（目前只有只读权限 `read:directory`，未实现）。缺的一半在核心：

- 插件拿不到用户表，无法为一个外部身份找到或建立核心用户；
- 插件不能签发核心会话，会话、Cookie 的属性与吊销都属于核心；
- 登录页没有放「用某某登录」的位置；
- 编译进来的模块只有 `AccessRead`、`AccessAdmin`、`AccessAdminSession`，没有不认证的路由，接不住回调；
- 用户没有「停用」：身份源说某人离职时，核心没有办法让他立即失去全部访问；
- ADR 0071 允许登录时同步调用插件校验密码（LDAP），但没有定形状。

## 决策

1. **身份连接器是插件，账号与会话属于核心。** 连接器负责与身份源对话：跳转、回调、校验断言或密码、同步目录。核心负责：外部身份到核心用户的链接、建立与停用用户、签发与吊销会话。插件永远读不到 `users`、`management_sessions`、密码哈希，也不能设置核心的会话 Cookie。

2. **外部身份是核心表里的一行。** 新表 `user_identities`：`user_id`、`source`（连接器的来源名，`[a-z][a-z0-9_]{1,31}`，按插件包或模块独占，同 ADR 0081 决策 4 的角色名）、`subject`（身份源里不会变的 id，不用用户名、邮箱）、`created_at`、`last_sign_in_at`，`(source, subject)` 唯一。一个核心用户可以链接多个来源。这是无法从现有数据推导的新事实（`CLAUDE.md`「新增持久化事实」）。`users` 增加 `disabled_at`（空为启用）。

3. **两种连接器形状。**
   - **跳转式**（OIDC、SAML、OAuth2 办公平台）：
     1. 登录页列出入口（决策 6），用户点击后先到核心的 `GET /api/v1/auth/sign-in/start?source=…`。核心生成一个随机的**浏览器绑定**：设置一个一次性的 `HttpOnly`、`Secure`、`SameSite=Lax`、10 分钟有效的 Cookie 存放它，再把浏览器重定向到连接器声明的 `start` 路由，查询参数里带一个由绑定派生、不能反推出绑定的句柄（以 master key 派生的密钥做 HMAC）；
     2. 连接器把浏览器带去身份源，句柄随自己的 state 往返；身份源带回结果到连接器的 `public` 回调路由，连接器自己校验（state、nonce、签名、换取令牌）；
     3. 连接器调用宿主的 `accounts.sign_in`（决策 4），交出 `{source, subject, profile, handle}`；核心按链接找到或建立用户（决策 5），返回一个绑定到这个句柄的一次性登录票据；
     4. 连接器把浏览器重定向到核心的 `GET /api/v1/auth/complete?ticket=…`。核心核对票据与浏览器带来的绑定 Cookie 是否对应，一致才建立会话、设置会话 Cookie，再删掉绑定 Cookie、跳到控制台。

     票据 60 秒内有效、只能用一次、只存哈希，绑定签发时的来源与句柄。兑换**只认浏览器绑定**，不看 `Sec-Fetch-Site` 与 `Origin`：经身份源跳回的这次顶层 GET 本来就是跨站的，`managementOriginAllowed` 会拒绝它；而顶层 GET 不带 `Origin`，那个检查也绑定不了任何东西。绑定挡住登录 CSRF：攻击者用自己的身份换到的票据绑定在攻击者的浏览器上，诱导受害者打开兑换链接时，受害者的浏览器没有对应的 Cookie，兑换以 `IDENTITY_TICKET_INVALID` 拒绝。连接器的路由不需要设置核心 Cookie，自己的 state Cookie 照常经 `Cookies` 设置。
   - **密码式**（LDAP）：登录时核心查到用户名链接到某个密码式来源，就在登录请求里同步调用该连接器的校验接口（`POST /gwx/v1/accounts/verify`，超时 5 秒），只得到「通过、不通过、身份源不可达」三种结果。这正是 ADR 0071 决策 3 允许的那一处同步调用。没有链接的用户照常校验本地密码；身份源不可达时，本地用户与已有会话不受影响，不做离线校验缓存（ADR 0071 决策 19）。

4. **宿主能力 `host.accounts`。** 插件包在清单里请求权限 `write:accounts`（风险 `high`），只有发布者证书授予了它的包才能用；编译模块经 `app.Host.Accounts()` 得到同一组操作。操作都以连接器自己的来源名为范围：
   - `sign_in(source, subject, profile, handle)` 返回绑定到 `handle` 的票据，或带码拒绝；
   - `upsert(source, subject, profile)`：目录同步，建立或更新用户；
   - `disable(source, subject)` / `enable(source, subject)`：停用、恢复；
   - `revoke_sessions(source, subject)`：身份源发来登出时，结束这个人的全部会话；
   - `list(source, cursor, limit)`：分页读本来源链接过的用户，用于全量同步时对账。

   `profile` 只有 `username`、`display_name`、`email` 三个字段，严格解码；不能带角色。

   **管理员不受连接器管辖。** 对 `users.local_admin` 为真或 `users.role` 为 `admin` 的用户，`disable`、`enable`、`revoke_sessions`，以及会改变用户名的 `upsert`，一律以 `IDENTITY_PROTECTED_USER` 拒绝；管理员账号的停用、恢复、改名与踢下线只能由管理员在核心接口里操作。`sign_in` 不受这一条影响：管理员显式链接过外部身份后可以用它登录。这样，连接器（包括被攻破的插件包）不能停用本地管理员或删掉其会话，ADR 0071 决策 11 的兜底不会被连接器主动破坏，而不只是在连接器掉线时成立。实施时由契约测试证明：编译模块和假插件进程都不能停用、恢复、改名或踢下线本地管理员与平台管理员。

5. **找到还是建立用户，规则由核心定。**
   - 已有 `(source, subject)` 链接：就是这个用户；`username` 变了时按站点策略更新或保留。
   - 没有链接：站点策略 `auth.external.provision` 为 `auto`（默认）时建立新用户，角色为 `user`，没有本地密码；为 `existing` 时拒绝（`IDENTITY_NOT_PROVISIONED`），只有管理员事先链接过的人能登录。
   - **永不按用户名或邮箱自动链接到已有用户**：那等于让身份源或插件接管任何同名本地账号（含管理员）。把已有用户链接到外部身份只能由管理员在核心接口里显式操作。
   - 新用户的 `username` 与已有用户冲突时拒绝（`IDENTITY_USERNAME_TAKEN{username}`），由连接器换一个（例如加来源前缀）后重试，核心不自动改名。
   - 外部身份永远不能让一个用户成为平台管理员或本地管理员：`profile` 不带角色，核心角色只能由管理员修改；额外的角色经 ADR 0081 的 `roles.bindings` 推送，同样不能产生 `admin`。

6. **登录页入口由连接器声明，核心下发。** 插件包在清单的 `sign_in` 里声明 `{source, label, icon, start}`（`start` 必须是本包一条 `public` 的 `GET` 路由），编译模块经 `app.Host` 声明同样的内容。核心在不认证的 `GET /api/v1/auth/sign-in-options` 里返回当前可用的入口，入口的链接是核心的 `/api/v1/auth/sign-in/start?source=…`（决策 3），不是连接器的路由；连接器所在插件掉线或功能被锁定、隐藏时不列出。它还返回本地密码登录是否可用（决策 8）。登录页只按这个列表渲染，不自己判断。

7. **停用立即生效，掉线不停用。**
   - 停用的用户：所有会话立即删除；登录、票据兑换、平台令牌与推理 API key 都以 `USER_DISABLED` 拒绝。数据与 key 保留，恢复后照常可用（ADR 0071 决策 13：不中断正在运行的负载，停用只影响新的请求）。
   - 生效时限：会话按请求读库，删除后在所有 server 上立即失效。推理 API key 与平台令牌各有一个进程内缓存（`store/apikey_cache.go` 的 `apiKeyCacheTTL` 为 5 分钟，`store/platform_tokens.go` 的 `platformTokenCacheTTL` 为 1 分钟）：执行停用的那个 server 进程同时推进两个缓存的 generation、按用户 id 清掉缓存条目，所以在本进程立即失效；集群里的其他 server 进程最晚在各自缓存的 TTL 到期时失效。这个上限写进文档，不另造跨进程的失效通道。将来某个缓存的 TTL 变长时，必须同时给出跨进程失效。
   - 连接器掉线、卸载或崩溃时：它的入口从登录页消失，回调路由回 503；已建立的会话不受影响；已同步的用户不会因为掉线被停用，也不会因为过了多久没同步而被停用。停用只来自连接器明确的 `disable`，或管理员的操作。

8. **本地登录与兜底。** 站点策略 `auth.local_password` 取 `all`（默认）或 `local_admin`。为 `local_admin` 时，只有 `users.local_admin` 为真的用户能用本地密码登录，其余人只能走外部身份；本地管理员的密码登录永远可用，与 ADR 0071 决策 11 一致，所以所有连接器都掉线时管理员仍能登录。

9. **编译模块的 `public` 路由。** `api.RouteAccess` 增加 `AccessPublic`：不认证，按客户端地址限流（规则同插件路由的 `public`），链路从不读取、也从不续期会话；路由仍须指明功能或标为社区路由。连接器的起始与回调路由标 `RouteSpec.SignIn`，整台服务端受限时只有这些公开路由照常放行，其他公开路由按方法分类，见 [ADR 0088](0088-extension-public-routes-under-a-platform-limit.md)。

10. **码。** `IDENTITY_TICKET_INVALID`（票据不存在、过期、已使用、来源不符，或与浏览器绑定不对应）、`IDENTITY_PROTECTED_USER`（连接器试图停用、恢复、改名或踢下线管理员）、`IDENTITY_NOT_PROVISIONED`、`IDENTITY_USERNAME_TAKEN{username}`、`IDENTITY_SOURCE_CONFLICT{source}`（来源名已被别的包或模块拥有）、`IDENTITY_SOURCE_UNAVAILABLE{source}`（密码式校验时身份源不可达）、`USER_DISABLED`、`LOCAL_PASSWORD_DISABLED`。

## 扩展等级

- 现状：L0。只有本地密码登录，没有接缝。
- 目标：连接器为 L3（插件包），账号与会话是核心的 L1 机制，登录入口为 L2（清单声明，核心下发）。
- 准则（ADR 0084 决策 3）：
  1. 本 ADR 建立接缝；
  2. 变化来自外部：每个站点的身份源不同；
  3. 不在热路径上：登录是 ADR 0071 允许同步调用插件的唯一一处，其余请求只读核心的会话与 `disabled_at`；
  4. 不在 GPU 容器或宿主上执行；
  5. 不需要真机验证；
  6. 对没有安装连接器的 server，行为与现在逐字节相同：登录页只有密码框，`user_identities` 为空；
  7. 契约成本：`host.accounts` 的五个操作与清单的 `sign_in` 声明，只增不改。

## 取舍

- **核心签发票据、自己设置 Cookie vs 让插件路由设置会话 Cookie**：后者省一次跳转，但会话 Cookie 的属性（`HttpOnly`、`SameSite`、`Secure`、有效期）与吊销会散到每个插件里，插件还会拿到能直接用的会话凭据。票据只能兑换一次、只有 60 秒、只能在发起登录的浏览器上兑换，泄露的代价小。
- **浏览器绑定 vs 校验请求来源**：校验 `Sec-Fetch-Site`/`Origin` 不需要多一次跳转，但跳转式登录的回程本来就是跨站的顶层 GET，来源检查要么拒绝正常流程，要么什么也绑定不了。浏览器绑定多一次经核心的跳转，换来对登录 CSRF 的确定防护。
- **按用户名自动链接 vs 只按 `(source, subject)`**：自动链接对迁移友好，但任何能控制身份源用户名的人都能接管同名本地账号。迁移改为管理员显式链接。
- **掉线即停用 vs 只在明确停用时停用**：掉线即停用会让连接器成为整个站点的单点，一次身份源故障就把所有人锁在门外；离职停用的及时性由连接器的事件或定时同步负责。
- **一个通用连接器协议 vs 核心内置 OIDC**：内置 OIDC 覆盖面广，但钉钉、飞书、企业微信都不是标准 OIDC，SAML 与 LDAP 也不是；核心只管账号与会话，各种协议都在连接器里，核心保持一种机制。

## 实施状态

第 1 步已实施（2026-11-27）；第 2 步的 `api.AccessPublic` 与 `RouteSpec.SignIn` 已按 [ADR 0088](0088-extension-public-routes-under-a-platform-limit.md) 实施（2026-11-28），第 2 步其余部分与第 3–4 步未实施。顺序，每一步都带契约测试（编译模块走 `internal/testenv/extmodule`，插件包走假插件进程，ADR 0071 决策 21）：

1. **已实施。** `user_identities`（迁移 252）与 `users.disabled_at`；停用的执行（会话、登录、平台令牌、API key）；管理员的停用、恢复、查看链接、链接与删除链接接口（`/api/v1/users…`，见 [API 参考](../reference/api.md#accounts账号与外部身份)）；`auth.local_password`。
2. 浏览器绑定与 `GET /api/v1/auth/sign-in/start`、票据与 `GET /api/v1/auth/complete`，管理员不受连接器管辖的检查，`app.Host.Accounts()`，`api.AccessPublic`（**已实施**，连同 `RouteSpec.SignIn`，见 ADR 0088），`GET /api/v1/auth/sign-in-options` 与登录页入口。到这一步，编译模块就能实现跳转式连接器。
3. 插件包的 `write:accounts` 权限、`host.accounts` 宿主接口与清单的 `sign_in` 声明。
4. 密码式连接器的同步校验接口。
