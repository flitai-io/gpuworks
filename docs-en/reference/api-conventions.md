# HTTP API Conventions

All endpoints under `/api/v1` follow the conventions in this document. Follow them when adding or modifying endpoints. `make check-api-conventions`
(`scripts/apiconv`) checks the aspects that can be determined from types. For individual endpoint descriptions, see [api.md](api.md).

## Example: Sign In and Query the Inference Service List

GPUWorks groups inference service configuration and running instances under deployments. Use `GET /api/v1/deployments` to query the deployment list.
The results also include deployments that are preparing, stopped, or failed. This differs from [`GET /v1/models`](openai-gateway.md), which inference clients use to select models.

Run the following example on a machine that can reach the console. You need **Bash, curl 7.76 or later, jq**, and an account that can sign in to the console.
The commands use the JWT returned by sign-in (a management session token), not an inference token (for example, `gw-…`).

### 1. Set the address and sign in

Replace the example address with your actual **console address**, keeping the protocol and port. Remove page paths such as `/dashboard` and the trailing `/`.
The default installation port is `8800`. If you use a custom listen address or reverse proxy, use the address you access in your browser.

```bash
API_URL='http://192.0.2.10:8800'
USERNAME='admin'
read -r -s -p 'GPUWorks password: ' PASSWORD
printf '\n'

TOKEN=''
LOGIN_RESPONSE=$(jq -n --arg username "$USERNAME" --arg password "$PASSWORD" \
  '{username: $username, password: $password}' |
  curl --fail-with-body -sS "$API_URL/api/v1/auth/login" \
    -H 'Content-Type: application/json' --data-binary @-) &&
TOKEN=$(printf '%s' "$LOGIN_RESPONSE" | jq -er '.token')
unset PASSWORD
```

Set `USERNAME` to your console username. A successful sign-in returns `token` and `user`. The command stores `token` in a variable for subsequent requests.
If sign-in fails, run `printf '%s\n' "$LOGIN_RESPONSE"` to inspect `error` and `code` (see [Errors](#错误)). Resolve the error before querying.

### 2. Query the deployment list

After signing in successfully, run the following in the same terminal:

```bash
curl --fail-with-body -sS \
  "$API_URL/api/v1/deployments?page=1&page_size=20" \
  -H "Authorization: Bearer $TOKEN"
```

A successful response returns HTTP `200`. The following example shows key fields. The actual response also includes fields for the model, node, health status, and more:

```json
{
  "items": [
    {
      "id": 7,
      "name": "qwen3-chat",
      "served_name": "qwen3",
      "desired_state": "active",
      "runtime_state": "running",
      "replicas": 1,
      "ready_replicas": 1
    }
  ],
  "page": 1,
  "page_size": 20,
  "total": 1
}
```

| Field | How to Interpret It |
|---|---|
| `items` | Deployment records on this page. `[]` means that the current page has no records. |
| `page` / `page_size` / `total` | Current page, records per page, and total matching deployments. There is another page when `page × page_size < total`. |
| `id` | Deployment ID, used to query management endpoints such as `GET /api/v1/deployments/{id}`. |
| `name` / `served_name` | `name` is the display name in the management UI. `served_name` is the `model` name to specify in inference requests. |
| `desired_state` / `runtime_state` | Desired state and current runtime state, respectively. To determine whether a deployment can serve requests, also check its ready replicas; do not rely only on `active`. |
| `replicas` / `ready_replicas` | The deployment's replica and ready replica counts. Prefill/decode (PD) disaggregation deployments sum the units of each role, with the same names and values as the list row; the details response also has `pd.roles` giving the same numbers split by role; do not treat an omitted value as `0`. |

`page` starts at `1`, and `page_size` ranges from `1`–`200`. Without pagination parameters, the response returns all matching deployments, with more complete individual records than the paginated list.
An empty list also returns HTTP `200`. A failed request returns an error object, which must not be treated as an empty list.

### 3. Query deployment details

Replace `7` with an actual `id` from `items` in the previous step:

```bash
curl --fail-with-body -sS "$API_URL/api/v1/deployments/7" \
  -H "Authorization: Bearer $TOKEN"
```

The details response is the deployment object itself, without an `items` wrapper. It includes the deployment's configuration and instance information. To call the model, use
`served_name` and follow the authentication and request format in [OpenAI-Compatible Gateway](openai-gateway.md).

## Field Naming

- Use snake_case for all JSON field names: `node_uuid`, `memory_total_bytes`, `time_ms`. Go struct fields must have
  a `json` tag. Do not rely on the default encoding of field names, which produces names such as `TimeMS` and `Entries`.
- This restriction does not apply to map keys that represent data, such as label names, environment variable names, or metric labels.
- Include units in numeric field names: `_bytes`, `_gib`, `_ms`, `_seconds`, `_pct`.
- End timestamps with `_at` (RFC 3339 string) or `_ms` (Unix milliseconds). Do not mix these two forms for the same type of timestamp within an endpoint.

## Collections

An endpoint that returns a collection of objects of the same type returns an object with the elements in `items`. **Do not return a bare array**:

```json
{"items": [{"uuid": "…", "name": "gpu3"}], "total": 1}
```

- `total` is the total number of matches. Without pagination, it equals the length of `items`. With pagination, it is the total number of matches after filtering, not just the count on this page.
- Paginated responses (`page`/`page_size`) also include `page` and `page_size`, corresponding to `dto.ListPage`. Non-paginated responses correspond to `dto.List`.
- Cursor pagination (event timelines and log searches) cannot always provide `total`. It can return only `items`, together with the cursor fields or instructions needed to navigate pages.
- Information related to the entire collection but not to any individual element, such as summaries and filter options, belongs in sibling fields of `items`. Examples include
  `overview` in `GET /api/v1/gpus` and `labels` in `GET /api/v1/nodes?page=1`.
- An empty collection is `"items": []`, not `null`, and must not be omitted.
- Filtering and sorting parameters take effect with or without pagination and **must not be silently ignored**. Return 400 (`LIST_FILTER_INVALID`) for filter values that the endpoint does not support,
  rather than returning the entire unfiltered collection.
- The criterion is whether the main response content is a collection of objects of the same type. Arrays within an object's own properties, such as `points` in a curve or
  `gpus` in a node, are part of that object and are not subject to this restriction.

## Opaque Fields

The check script cannot inspect field names inside response fields typed as `any`, `map[string]any`, or `json.RawMessage`.
Use these fields only for complete snapshots passed through unchanged, such as a deployment's `catalog_selection_snapshot`. The snapshot itself must also follow snake_case.
Prefer named DTOs for new endpoints. The existing count is constrained by the `api_opaque_response_fields` budget in `scripts/arch-constraints.txt`, which can only decrease, never increase.

## Errors

Every management API error response is a JSON object containing `error` and a stable `code`, with optional `params`.
For example, an incorrect username or password during sign-in returns HTTP `401`:

```json
{"error": "invalid username or password", "code": "INVALID_CREDENTIALS"}
```

| Field | Purpose |
|---|---|
| `error` | A human-readable error description in English. Do not build handling branches by matching this text. |
| `code` | A stable error identifier that programs can use to distinguish causes. Every error has one. |
| `params` | Context such as the fields and values involved in the error. Clients can use it with `code` for localized messages. Omitted when not provided. |

When the server gives no specific cause for an error, it fills in a **generic code** by HTTP status:

| HTTP Status | Generic `code` | HTTP Status | Generic `code` |
|---|---|---|---|
| `400` | `BAD_REQUEST` | `422` | `UNPROCESSABLE` |
| `401` | `UNAUTHORIZED` | `424` | `FAILED_DEPENDENCY` |
| `403` | `FORBIDDEN` | `429` | `TOO_MANY_REQUESTS` |
| `404` | `NOT_FOUND` | `500` | `INTERNAL_ERROR` |
| `405` | `METHOD_NOT_ALLOWED` | `501` | `NOT_IMPLEMENTED` |
| `408` | `REQUEST_TIMEOUT` | `502` | `BAD_GATEWAY` |
| `409` | `CONFLICT` | `503` | `SERVICE_UNAVAILABLE` |
| `410` | `GONE` | `504` | `GATEWAY_TIMEOUT` |
| `413` | `REQUEST_TOO_LARGE` | `507` | `INSUFFICIENT_STORAGE` |
| `415` | `UNSUPPORTED_MEDIA_TYPE` | `416` | `RANGE_NOT_SATISFIABLE` |

Any other 4xx uses `BAD_REQUEST`, and anything else uses `INTERNAL_ERROR`. A generic code states only the category; the specifics are in `error`, for example:

```json
{"error": "deployment 7 not found", "code": "NOT_FOUND"}
```

The console shows a generic code as the localized category followed by the original `error` text (such as "Not found: deployment 7 not found"), and a specific code with its own text.

### Common HTTP Statuses and Error Codes

The following table lists common management API cases. The HTTP status indicates the error category, while `code` indicates the specific cause. They do not have a one-to-one mapping.

| HTTP Status | Common `code` | Meaning and Action |
|---|---|---|
| `400` | `FIELD_INVALID` | A request field or the JSON request body does not meet requirements. Check `params.field` and `error`. |
| `400` | `LIST_PAGE_INVALID` / `LIST_FILTER_INVALID` | Invalid pagination parameters, filter values, or sort fields. Adjust them to the range supported by the endpoint. |
| `401` | `INVALID_CREDENTIALS` | The sign-in username or password is incorrect. Check the credentials. |
| `401` | `AUTHENTICATION_REQUIRED` | Management credentials are missing, invalid, or expired. Sign in again or use a valid platform token. |
| `403` | `ADMINISTRATOR_REQUIRED` | The operation requires the administrator role. Use an account with the required permissions. |
| `403` | `PLATFORM_TOKEN_READ_ONLY` / `PLATFORM_TOKEN_NOT_ALLOWED` | A read-only platform token cannot write, or this endpoint allows only signed-in sessions. Use credentials that meet the endpoint's requirements. |
| `403` | `MANAGEMENT_ORIGIN_REJECTED` | Origin validation failed for the management request. Check the browser entry point and reverse proxy configuration. |
| `404` / `405` | A specific code, or the generic `NOT_FOUND` / `METHOD_NOT_ALLOWED` | The resource or route does not exist / the HTTP method is not supported. Check the address, resource ID, and method. |
| `409` | Depends on the operation | The resource's current state or a concurrent change prevents the operation. Read the error details first and query the resource state again. |
| `413` | Endpoints using generic JSON decoding return `FIELD_INVALID` | The request body exceeds the endpoint's limit. The generic management JSON request body limit is 1 MiB. Upload and similar endpoints follow their own limits. |
| `429` | `LOGIN_THROTTLED` | Sign-in attempts are too frequent. Wait for the number of seconds specified by the `Retry-After` response header before retrying. |
| `500` / `503` | Depends on the failure; `INTERNAL_ERROR` / `SERVICE_UNAVAILABLE` when there is no specific cause | An internal service error occurred / the service is currently unavailable. Retain the error content and check the control plane logs. `AUTH_NOT_CONFIGURED` means management authentication is not configured (`503`). |

Clients first check the HTTP status, then read `code` and `params`. For a generic or unrecognized code, they must still retain and display `error`.
These conventions apply to the management API. For the inference gateway's error format, see [OpenAI-Compatible Gateway](openai-gateway.md). Error codes are defined in
`internal/server/api/errors.go`, the per-area `errors_<area>.go` files (such as `errors_images.go` for the image catalog and update sources), and `internal/server/messages` (generic codes in `messages/http.go`). Individual endpoints may also return business-specific error codes.

The cluster enrollment listener also uses `CLUSTER_TLS_REQUIRED` (426, enrollment requires a completed HTTPS handshake), `CLUSTER_REQUEST_INVALID` (400), `CLUSTER_INVITATION_REQUIRED` (401, enrollment needs the invitation bearer), `CLUSTER_IDENTITY_MISMATCH` (409), `CLUSTER_ADMISSION_REJECTED` (403), and `CLUSTER_UNAVAILABLE` (503). Issuing an invitation uses the existing management session; a non-administrator still receives `ADMINISTRATOR_REQUIRED`. While a control-plane cluster upgrades its schema, every member answers business requests (including `/api/v1/readyz`) with 503 `CLUSTER_UPGRADE_IN_PROGRESS` and `Retry-After`; the request was not processed, and the members recover on their own when the upgrade ends.

A member of a control-plane cluster answers 503 (code `SERVICE_UNAVAILABLE`) when it cannot commit a write: only the member holding the database leader can commit it and this member is not that one, or this member's business writes are closed. Nothing is committed, and the cluster entry can send the request to another member. A member that has not opened business answers business paths with 503 and the plain-text body `member business service is not ready`, not JSON.

## Checks

`make check-api-conventions` performs type checks on `internal/server/api`, identifies the value written by every `writeJSON` / `writeSSE`
call, and reports:

- `envelope`: The value written is a slice or array and should be changed to `dto.List` / `dto.ListPage`.
- `snake_case`: Reachable struct fields or keys in map literals written inline do not encode as snake_case.

This check is included in `make test-unit`.

<!-- translation-source-sha256: 9430ec1d83972f42a839d9a62667c21c0b2c4958de6d971ca86affe1f9247305 # HTTP API Conventions -->
<!-- translation-source-sha256: 4eedc84c97a083698f30bb59136ba3a9e08cd89a5611541a493d9654395a8762 ## Example: Sign In and Query the Inference Service List -->
<!-- translation-source-sha256: 8d2742a2083cf96a822ce3c5a005fb3891cf9e0f527914ade32b9fe192f518bb ### 1. Set the address and sign in -->
<!-- translation-source-sha256: ddbb1404108c865cad52751d004f8438237ad649fdbaca37901f25fcdfdbd356 ### 2. Query the deployment list -->
<!-- translation-source-sha256: f98a51cc0720b18e83462d778d5ddef4dc184a1f124804888469462360d88af3 ### 3. Query deployment details -->
<!-- translation-source-sha256: b2b28f9336d8fdf644052737b8bc8b5ee59c2c607cfd8436b6e796987801f4fa ## Field Naming -->
<!-- translation-source-sha256: 91f772294d3153eda1852c479296e5d88259cadd4a779ade09b249f546cda4f2 ## Collections -->
<!-- translation-source-sha256: f48d1e262baadf4e58cd76b542e243659f052e731b0b0c9089ff9ec9f5f92d51 ## Opaque Fields -->
<!-- translation-source-sha256: 45246930eb15fb28bdee8f1e45cdc066df26656c641684fb62651b59316c2f53 ## Errors -->
<!-- translation-source-sha256: cea858b690cb14c48c1577245f75c103933935a09c6b866d39dd7b8784d5dbac ### Common HTTP Statuses and Error Codes -->
<!-- translation-source-sha256: bdd75ee5112cec32ed080569cff9a49c8a785efea72efb8951bb812243add9a4 ## Checks -->
<!-- translation-localized-code-block: bae0e4c7d262a34ee3ef790d5e20acb056000102304e33d31fffd72989b4b11c 575be17f25555cdcb9a913a995f63b3814eab0f2391908631ca49f5fa2d37787 Translate password input prompt -->
