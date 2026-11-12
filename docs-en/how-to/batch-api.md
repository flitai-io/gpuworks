# Run Batches: the Batch API and the Console

This guide explains how to hand a large set of inference requests to the platform to run asynchronously: prepare a JSONL file, upload it, create a batch, follow its progress, download the results, cancel it, and point it at a deployment whose yield policy is yield anytime so it uses only idle GPUs.

**Use case**: work with many requests that do not need an immediate answer, such as generating answers for a dataset, labeling in bulk, or computing embeddings in bulk. Requests that need streamed output or millisecond latency do not belong in a batch; call the [OpenAI-Compatible Gateway](../reference/openai-gateway.md) directly.

**Prerequisites**:

- A running deployment whose served model name you know. A batch routes by the `model` in each request line, exactly as a direct gateway call does; you can also choose one model when creating the batch so that all requests go to it, see [Choosing the Model](#指定模型).
- An inference token (starting with `gw-`), created on the **Inference tokens** card of the **API Tokens** page; the plaintext is shown only once. The `/v1` routes require it by default, including when the server has no inference token at all; only after an administrator turns on "Accept /v1 inference requests without a token" under **Settings → Security** does `/v1` accept requests without one, see [Gateway Authentication](../reference/openai-gateway.md#authentication).
- Creating or cancelling a batch in the console requires an administrator (the create button is disabled for other roles); other users can view their own batches in the console.
- For the interface specification (object fields, error codes, limits), see [Complete REST API Routes](../reference/api.md#batches批处理任务).

## What a Batch Does

A batch is a set of requests from one JSONL file. The flow is always three steps:

1. Upload the JSONL file (`POST /v1/files`). The platform validates it while receiving it and, if it passes, returns a file ID.
2. Create a batch from that file (`POST /v1/batches`) and get a batch ID.
3. When the batch ends, fetch the results from the output file and the error file.

Three endpoints are supported: `/v1/chat/completions`, `/v1/completions`, and `/v1/embeddings`; all requests in one batch must use the same endpoint. Streaming (`"stream": true`) is not supported. The completion window is `24h` by default and can be set to a whole number of hours from `1h` to `72h` when creating the batch (see [Completion Window](#完成时限)): requests without a result once that time has passed since the batch was created are treated as expired.

A batch's requests take the same gateway path as a client's request: a replica is chosen by `model`, a sleeping deployment is woken when necessary, and usage and request records are counted. They therefore appear in the request views and reset the deployment's idle timer.

## 1. Prepare the Input File

The input is JSON Lines, in a file named `*.jsonl` (another extension is refused on upload with 400 `invalid_file_extension`): each non-blank line is one request with these fields.

| Field | Requirement |
|---|---|
| `custom_id` | A non-empty string, unique within the file. Results are matched back to requests by it |
| `method` | Always `POST` |
| `url` | One of `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`; the `endpoint` given when creating the batch must equal the `url` of every line |
| `body` | An object whose `model` is a non-empty string and which is not `"stream": true` |

Example `requests.jsonl` (in the real file each request takes one line):

```jsonl
{"custom_id":"q-001","method":"POST","url":"/v1/chat/completions","body":{"model":"qwen3-offline","messages":[{"role":"user","content":"Explain what a KV cache is in one sentence."}],"max_tokens":128}}
{"custom_id":"q-002","method":"POST","url":"/v1/chat/completions","body":{"model":"qwen3-offline","messages":[{"role":"user","content":"Translate hello into French."}],"max_tokens":32}}
```

File limits: 200 MB, 50000 requests (non-blank lines), and up to 16 MiB per line. The file is validated line by line while it uploads, and the first error rejects it with the line number, for example `Line 2: custom_id must be a non-empty string`. A rejected upload leaves no file behind.

## 2. Run a Batch with curl

The commands below use the environment variables `GPUWORKS_URL` and `GPUWORKS_API_KEY`, written as in the [first-model tutorial](../tutorials/getting-started.md#6-调用并验证回答).

```bash
GPUWORKS_URL=http://192.0.2.10:8800
read -r -s -p 'Paste your inference token: ' GPUWORKS_API_KEY
printf '\n'
```

### Upload the File

```bash
curl -sS "$GPUWORKS_URL/v1/files" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -F purpose=batch \
  -F file=@requests.jsonl
```

On success it returns 200 and a file object; note the `id`:

```json
{"id":"file-3f9c2a7e1b6d4c8a90e5d721","object":"file","bytes":412,"created_at":1790000000,"filename":"requests.jsonl","purpose":"batch","status":"processed"}
```

`purpose` can only be `batch`.

### Create the Batch

```bash
curl -sS "$GPUWORKS_URL/v1/batches" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"input_file_id":"file-3f9c2a7e1b6d4c8a90e5d721","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"purpose":"demo"}}'
```

It returns the batch object, with the initial status `validating`:

```json
{"id":"batch_5a1e9d03c7b24f6e8d2a4b19","object":"batch","endpoint":"/v1/chat/completions","errors":null,"input_file_id":"file-3f9c2a7e1b6d4c8a90e5d721","completion_window":"24h","status":"validating","output_file_id":null,"error_file_id":null,"created_at":1790000010,"in_progress_at":null,"expires_at":1790086410,"finalizing_at":null,"completed_at":null,"failed_at":null,"expired_at":null,"cancelling_at":null,"cancelled_at":null,"request_counts":{"total":2,"completed":0,"failed":0},"metadata":{"purpose":"demo"}}
```

`metadata` is optional: at most 16 keys, keys up to 64 characters, values up to 512 characters, all strings.

### Poll the Progress

```bash
BATCH_ID=batch_5a1e9d03c7b24f6e8d2a4b19
while true; do
  resp=$(curl -sS "$GPUWORKS_URL/v1/batches/$BATCH_ID" -H "Authorization: Bearer $GPUWORKS_API_KEY")
  echo "$resp" | jq -c '{status, request_counts}'
  case $(echo "$resp" | jq -r .status) in
    completed|failed|expired|cancelled) break ;;
  esac
  sleep 10
done
```

`request_counts` is persisted about every 5 seconds, so the counts move in steps rather than request by request; a request that is still being retried counts as neither `completed` nor `failed`.

### Download the Results

After the batch ends, `output_file_id` points to the output file of successful requests and `error_file_id` to the error file of failed requests; each is `null` when there is no such request.

```bash
OUT=$(echo "$resp" | jq -r .output_file_id)
curl -sS "$GPUWORKS_URL/v1/files/$OUT/content" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" -o output.jsonl
```

The file format is described in [Reading the Result Files](#结果文件怎么读).

## 3. Use the OpenAI Python SDK

The official SDK only needs `base_url` pointed at GPUWorks' `/v1`:

```python
import time
from openai import OpenAI

client = OpenAI(base_url="http://192.0.2.10:8800/v1", api_key="gw-...")

batch_file = client.files.create(file=open("requests.jsonl", "rb"), purpose="batch")

batch = client.batches.create(
    input_file_id=batch_file.id,
    endpoint="/v1/chat/completions",
    completion_window="24h",
    metadata={"purpose": "demo"},
)

while batch.status not in ("completed", "failed", "expired", "cancelled"):
    time.sleep(10)
    batch = client.batches.retrieve(batch.id)
    print(batch.status, batch.request_counts)

if batch.output_file_id:
    client.files.content(batch.output_file_id).write_to_file("output.jsonl")
if batch.error_file_id:
    client.files.content(batch.error_file_id).write_to_file("errors.jsonl")
```

Cancel with `client.batches.cancel(batch.id)` and list with `client.batches.list(limit=20)`.

## 4. Run from the Console

The **Batches** page in the **Inference Services** group of the left menu runs a batch without code. **Call it through the API** at the top of the page expands to the curl and SDK calls above, for the address you opened the console from; the usage note on the **API Keys** page points to these endpoints too.

1. Click **Create batch**, choose the `.jsonl` file, and submit. The batch calls the endpoint the lines' `url` names, with nothing else to choose; when the lines' `url`s differ, the error names the first line that differs. The file is validated line by line on upload here too, and an error names the line and the reason. The **Model** drop-down sets the model every request uses (see [Choosing the Model](#指定模型)), and **Completion window** changes the batch's time limit (see [Completion Window](#完成时限)). The choices in the dialog all come from the server (`GET /api/v1/batches/dictionary`): the endpoints a sample can call, the range and default of the completion window, the file size and request limits, the order of the status filter, the output retention in days, and the models that can be chosen with the state of each; the console works none of them out itself, so what the dialog offers agrees with the server's check at creation. Without a file at hand, pick the sample type (chat, completions, or embeddings) and a deployment under **Sample file** in the dialog: only models that support this endpoint can be chosen (each model in the dictionary carries the endpoints it supports), and the default is the first of them (the best state); when a **Model** is chosen and it supports this endpoint, the sample uses it directly. Click **Download sample** for a 1000-request `.jsonl` sample (10000 requests for embeddings, each input two prompts joined) (a mix of tasks such as math reasoning, translation, summarization, writing and code, in English and Chinese, with `max_tokens` 4096 for chat and completions, enough to keep a GPU busy for a while; the deployment's context length must hold it plus the prompt, or the inference engine refuses the requests; the same model gives the same content every time), or **Use sample** to create a batch with it as is.
2. The list shows each batch's ID, status, endpoint, model (shown only when one was chosen, otherwise "Per file line"), progress (completed out of total, with the failure count), creation time, and expiry time; use **Filter by status** to narrow it.
3. Click a row to open the detail drawer: summary, request counts, the timeline of stages, metadata, and **Download input**, **Download output**, and **Download errors**. A failed batch (`failed`) also shows the failure reason; when it has an output or error file, the drawer says when those files are deleted, from the batch's `output_expires_at`.
4. While a batch can be cancelled (its `cancellable` is `true`, that is, `validating` or `in_progress`), click **Cancel batch** and confirm in the page. A `finalizing` batch is writing its result files and cannot be cancelled, so the drawer shows no cancel button.

A batch created in the console belongs to the signed-in user and, like one created with an API token, can be viewed through `/v1/batches` with any inference token of the same user. Administrators see every user's batches; other users see only their own.

## Choosing the Model

The `body.model` on each line of the input file decides which deployment it goes to. To hand the whole file to one model, choose the model when creating the batch instead of editing the file:

- **Console**: the **Model** drop-down in the create dialog (optional; leave it empty to **use the model on each line of the file**).
- **`/v1/batches`**: one more field in the request body, `model`, a gpuworks extension; with the official SDK, put it in `extra_body={"model": "qwen3-offline"}`. Omitted, each line uses its own model.

```bash
curl -sS "$GPUWORKS_URL/v1/batches" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"input_file_id":"file-3f9c2a7e1b6d4c8a90e5d721","endpoint":"/v1/chat/completions","completion_window":"24h","model":"qwen3-offline"}'
```

With a model chosen, the platform sets the `body.model` of every request (retries included) to that name just before sending it and leaves the rest of the request alone; each line of the file still needs a `body.model` (upload validation is unchanged), whatever it says. The batch object gains a `model` field (the console's list and detail show it too), absent when none was chosen.

The chosen model must be the served model name of a deployment (running or stopped; one being deleted does not count); an unknown one returns 400 (`BATCH_MODEL_UNKNOWN` in the console, `model_not_found` on `/v1`) and no batch is created. When the model's deployment does not support the batch's endpoint (for example a chat batch that names an embeddings-only model), it also returns 400 (`BATCH_MODEL_ENDPOINT` in the console, with `params` `{model, endpoint}`; `model_endpoint_unsupported` on `/v1`), by the same rule the gateway uses to decide whether a deployment exposes that API (the model's inference API declaration).

The model names, states and supported endpoints in the drop-down come from `GET /api/v1/batches/dictionary` and follow the same rule as the check at creation. The states match the Inference Services page and are ordered by how soon the model can serve:

The console model picker defaults to dictionary entries with `ready=true` (serving or automatically wakeable); the sample picker uses the same filter. Select “Show models that need to wait” to choose a starting, held, offline, failed or stopped model explicitly; existing waiting warnings still apply. Turning the option off clears a waiting selection. The asynchronous waiting rules for API requests and models named in each file line still apply.

| State | Meaning | Requests of the batch for this model |
|---|---|---|
| `running` Running | A replica is awake | Handled normally |
| `asleep` Asleep | Every replica is asleep and a request can wake it (a sleep the platform made by yielding, or a manual sleep that lets requests wake it) | The gateway holds the request, wakes the deployment, and lets it through once awake, as for any request |
| `starting` Starting | Being placed, pulling its image, or starting | Retried until it starts serving |
| `asleep_held` Asleep (manual) | Put to sleep by you or a schedule and requests do not wake it (`sleep_held` is `true` in the deployment detail) | Retried until someone wakes it or the batch expires |
| `offline` Node offline | The node of the replica is disconnected | Retried until the node recovers or the batch expires |
| `failed` Failed | The replica failed to start | Retried until someone fixes it and starts it again, or the batch expires |
| `stopped` Stopped | The deployment is stopped | Waits until someone starts it or the batch expires |

The last four states need someone to act. The batch is still created, but the create response carries `warnings` (the console shows a notice, and the drop-down shows a hint under it when such a model is chosen): for `asleep_held` it is `model_sleep_held` (`BATCH_MODEL_SLEEP_HELD` in the console, with `params` `{model}`), and for the other three it is `model_waits` (`BATCH_MODEL_WAITS` in the console, with `params` `{model, state}`, where `state` is a state value from the table above, translated by the console into the interface language). When several deployments share a served name, the best state of them counts and the supported endpoints are the union.

**Checking the models in the file when no model is chosen**: when the batch is created, the platform reads the stored input file once, takes the distinct `body.model` values of its lines, and looks each up by the rules above. If some model is served by no deployment (one being deleted does not count), the creation is refused with HTTP 400; no batch is created and the uploaded file stays, ready for the next creation:

```json
{"error":{"message":"The input file names models that no deployment serves: A, B. Set a model to send every request to, or deploy these models first","type":"invalid_request_error","param":"input_file_id","code":"model_not_found"}}
```

The `/v1` message names at most 5 models and ends with `, ...` when there are more; the console has a code of its own, `BATCH_FILE_MODELS_UNKNOWN`, with `params` `{models, more}` (`models` is the list of the first few names and `more` how many further ones there are; the lookups stop after a few dozen unknown models, so `more` may be a lower bound), and the console joins the names in the interface language. To fix it, choose a model so that all requests go to it, or deploy the models in the file first. When every model in the file has a deployment but some do not support the batch's endpoint, it also returns 400 (`model_endpoint_unsupported` on `/v1`; `BATCH_FILE_MODELS_ENDPOINT` in the console, with `params` `{models, more, endpoint}`). When some models in the file are in a state that needs someone to act (the last four rows of the table above), the batch is still created, and the create response carries one `warnings` entry per state: `model_sleep_held` for held sleeps, and `model_waits` for offline, failed and stopped ones (`param` is `input_file_id` and it lists those models; with a model chosen, `param` is `model`); in the console's batch object these are `BATCH_FILE_MODELS_SLEEP_HELD` (`params` `{models, more}`) and `BATCH_FILE_MODELS_WAIT` (`params` `{models, more, state}`), and with a model chosen `BATCH_MODEL_SLEEP_HELD` and `BATCH_MODEL_WAITS`. With a model chosen the models in the file are not checked (they are replaced), only the chosen one is.

For a deployment stopped after the batch was created, the requests wait for it to start again (the gateway answers 404 for a stopped deployment; when the batch confirms that the model's deployment is merely stopped, it treats this as something to wait for, not a failure); for a deployment deleted after the batch was created, the requests get a 404 one by one and go to the error file.

After you choose a file in the console, the browser also reads it (when it is at most 50 MB) and lists every model it uses, judging which have no deployment by the model names in the dictionary. With no model chosen, if some model has no available deployment, a warning names those models and says the batch cannot be created; with a model chosen, the models in the file are replaced by it and only a notice is shown. The check in the browser is an early hint; the server's check at creation decides, and files over 50 MB are not checked in the browser.

## Completion Window

The completion window decides how long a batch has from its creation: `expires_at` is the creation time plus the window, and requests without a result by then are treated as expired (see [Statuses, Cancellation, and Expiry](#状态取消与过期)). The default is `24h`; for a tighter or looser limit, set it to a whole number of hours from `1h` to `72h`:

- **Console**: **Completion window** in the create dialog (hours, 1–72, 24 by default); the format hint in the dialog follows it.
- **`/v1/batches`**: `completion_window` in the request body is `"24h"` (OpenAI's value) or `"1h"` to `"72h"` (a gpuworks extension; with the official SDK you can override the parameter with `extra_body={"completion_window": "48h"}`).
- Anything else (`0h`, `73h`, `1d`, `90m`, `24H`, leading zeros or fractions) returns 400 `invalid_completion_window` (`BATCH_REQUEST_INVALID` in the console, with `params.field` set to `completion_window`).

The batch object and the console detail both show its `completion_window`; in the console the range and the default of this field come from the dictionary's `completion_window_hours` (`min`, `max`, `default`). The retention of the output and error files (30 days by default, the dictionary's `output_retention_days`) counts from when the batch ends and does not depend on the window.

## Statuses, Cancellation, and Expiry

| Status | Meaning |
|---|---|
| `validating` | Created and waiting for an execution slot; at most 32 batches run at once across the server |
| `in_progress` | Requests are being sent |
| `finalizing` | Every request has a result; the output and error files are being written |
| `completed` | Ended; every request has a result (success or failure) |
| `failed` | A platform fault, such as the input file disappearing; `errors` gives the reason |
| `expired` | The completion window (24 hours by default) passed after creation; requests without a result go to the error file with the code `batch_expired` |
| `cancelling` | Cancellation requested: no new requests are sent, and requests already in flight get up to 2 more minutes |
| `cancelled` | Cancellation finished; the results so far are in the output and error files |

Cancel with `POST /v1/batches/{id}/cancel` (in the console, **Cancel batch** in the detail drawer). Only `validating` and `in_progress` batches can be cancelled; a `finalizing` batch is writing its result files and ends as it is, and so does one that has already ended, and cancelling either returns 409 `batch_not_cancellable` (`BATCH_NOT_CANCELLABLE` in the console). The console's batch object carries `cancellable`, decided by the server, and the drawer shows the cancel button by it.

A batch's `expires_at` is its creation time plus its completion window (24 hours by default). A server restart does not lose batches: unfinished ones resume from the latest progress checkpoint, and requests that already have a result are not sent again.

## Reading the Result Files

The output file and the error file are both JSON Lines, one line per `custom_id`, in completion order rather than input order; use `custom_id` to match a line to its request.

Each line of the output file (`purpose` is `batch_output`) is a successful request:

```json
{"id":"batch_req_8c3d51f0a2e7","custom_id":"q-001","response":{"status_code":200,"request_id":"req-7b2f","body":{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"The KV cache stores the keys and values of processed tokens so they are not recomputed."},"finish_reason":"stop"}]}},"error":null}
```

Each line of the error file is a request that failed for good:

```json
{"id":"batch_req_1d90be44c6a3","custom_id":"q-002","response":{"status_code":400,"request_id":"req-91ac","body":{"error":{"message":"max_tokens is too large","type":"BadRequestError","code":400}}},"error":{"code":"400","message":"max_tokens is too large"}}
```

- When the model server answers 4xx, `response` keeps the full answer, and `error.code` and `error.message` come from the model server's error object. When that object has no `code`, `error.code` falls back to its `type`, then to `http_<status code>`.
- Other error codes: `batch_expired` (no result before expiry) and `response_too_large` (the answer exceeds the size the gateway keeps for a batch).
- The output file exists only if at least one request succeeded, and the error file only if at least one request failed.
- Output and error files are kept for 30 days after the batch ends and then deleted (the batch keeps its counts); reading one afterwards returns 404. The console's batch object gives the deletion time as `output_expires_at`; a time in the past means the files are gone, and it is absent while the batch is still running or when it wrote neither file. Input files are kept until you delete them.

Delete an input file with `DELETE /v1/files/{id}`; while a batch that has not ended still reads it, the call returns 409 `file_in_use`.

## Which Failures Are Retried Automatically

The platform separates "the request itself is wrong" from "the platform cannot serve it right now":

| Situation | Handling |
|---|---|
| The answer is HTTP 200 but some choice has `finish_reason` `abort` (a yield-anytime deployment preempted by a higher level) | Retried automatically; not a failure |
| 503 (the deployment is asleep and did not wake within the hold, has no replica yet, its replica failed to start, or its node is offline), 429, 408, any other 5xx, or a connection error | Retried automatically; not a failure |
| 404, and the deployment of this model is merely stopped | Retried automatically (waiting for the deployment to start); not a failure |
| Any other 4xx: 400 invalid request, 404 model not found or the deployment does not expose this API, 401, and so on | Final failure: written to the error file and counted in `failed` |

Retries use exponential backoff from 2 seconds, doubling up to 60 seconds, honoring any `Retry-After` in the answer and adding jitter, until the batch expires. Without a chosen model, a file naming a model nothing serves is refused at creation; for a model that is deleted after creation, every line gets a 404 quickly and ends in the error file instead of waiting until expiry.

**Why it is retrying**: while a batch runs, its detail in the console lists every model that has requests waiting to be retried: the state it is in right now (the same as the state table in [Choosing the Model](#指定模型)), how many requests are waiting, since when, and the gateway's latest answer (HTTP status and original text). In the API this is the `waiting` field of a single batch object (`GET /api/v1/batches/{id}`), each item `{model, state, status, detail, since, requests}`; it is absent when no request is waiting, and the list does not carry it. The information lives only in the executor's memory, and after a server restart it reappears with the next retry.

## Batches in Usage and Request Records

The requests a batch sends show the source **Batch** (`source=batch`) in request records, usage analytics and monitoring, so they can be filtered on their own and do not mix with ordinary API calls. A batch created with an API key is counted under that key; one created in the console has no key and is counted as **Batch (user name)** under the user who created it.

## Pairing with Yield Anytime

Point a batch at a deployment whose yield policy is **yield anytime** and it runs on idle GPUs only: when a real-time model needs the cards the batch deployment yields its memory, and it comes back automatically once the cards are free. For how to set the policy on the deployment, see [Yield policy](scheduling-center.md#出让策略); for the rules, see [Yield Policy: How Deployments Share a Set of GPUs](../explanation/service-levels.md).

1. When creating the deployment, turn on **sleep capable** and set the **yield policy** to **yield anytime** (or change it later under **Scheduling policy** in the deployment details).
2. Put this deployment's model name in the `model` of the JSONL, and follow the same steps as above.

How a batch interacts with a yield-anytime deployment:

- **On-demand wake**: when the deployment is asleep or has yielded its cards, a batch request is held by the gateway and triggers a wake like any other request, and is let through once the deployment is awake. A request is held for at most the deployment's `hold_max_sec` (1 hour by default for yield anytime); a request that times out gets 503, and the batch retries it automatically.
- **Counts as activity**: batch requests reset the idle timer and appear in the request records, unlike health probes. The deployment therefore does not yield for being idle in the middle of a batch.
- **Retry after preemption**: when a higher-level deployment needs the cards, the yield-anytime deployment yields immediately and in-flight requests are cut off; the client sees HTTP 200 with `finish_reason: "abort"`. The batch recognizes this signal and sends those requests again until the batch expires; they are not counted as failures. The deployment recovers automatically once the cards are free, and the batch continues.
- **When the cards stay taken**: if the real-time model stays busy and the yield-anytime deployment never gets the cards, requests keep being retried until the batch expires, and the remaining requests land in the error file with `batch_expired`. When you need a more predictable finish time, move the batch to a deployment that never yields automatically or yields on a turn, or shrink the batch.

## Scope and Limits

- Files and batches belong to the user that owns the API token that created them; any inference token of the same user sees them, and other users get 404. Administrators see every user's batches in the console.
- Concurrency: 8 concurrent requests per batch and 64 in-flight requests across all batches.
- Files are stored under `batch-files/` in the server data directory and use server disk; `GET /v1/files` lists your own files, so delete input files you no longer need.
- The completion window is a whole number of hours from `1h` to `72h`, `24h` by default.

## Troubleshooting

| Symptom | What to check |
|---|---|
| Upload returns 400 with `Line N` | Fix the file by the line number and error code, for example `duplicate_custom_id`, `streaming_not_supported`, `invalid_url` |
| Creating a batch returns 400 `endpoint_mismatch` | `endpoint` must equal the `url` of every line in the file |
| Creating a batch returns 400 `model_not_found` (`BATCH_MODEL_UNKNOWN` in the console) with `param` `model` | No deployment serves the chosen `model`; check the served model name and whether the deployment was deleted |
| Creating a batch returns 400 `model_not_found` (`BATCH_FILE_MODELS_UNKNOWN` in the console) with `param` `input_file_id` | With no `model` chosen, the models of the file (listed in the message) are served by no deployment; choose a `model` to receive every request, or deploy these models first; the file need not be uploaded again |
| Creating a batch returns 400 `model_endpoint_unsupported` (`BATCH_MODEL_ENDPOINT` or `BATCH_FILE_MODELS_ENDPOINT` in the console) | The model does not support the batch's endpoint, for example a chat batch using an embeddings model; choose a model that supports the endpoint, or use an endpoint the model supports |
| Creating a batch returns 400 `invalid_completion_window` | `completion_window` must be a whole number of hours from `1h` to `72h`, such as `"48h"` |
| Creating a batch returns 400 `file_not_found` | The file ID is wrong, or the file is not a `purpose=batch` file of the same user |
| The batch stays in `validating` | 32 batches are already executing across the server; it is waiting for a slot |
| The batch stays in `in_progress` and the counts do not move | Open the batch detail and read "Requests are waiting to be retried": which model, in what state, and the gateway's latest answer. Fix and restart a failed deployment, start a stopped one, and wake a manually slept one or let requests wake it; for a deployment that is asleep or has yielded its cards, check the deployment timeline and the scheduling analysis. Requests are being retried and do not count as failures |
| All requests land in the error file with status 404 | `model` does not match the deployment's served model name |
| Reading the output file returns 404 | The batch ended more than 30 days ago and the file was deleted, or no request succeeded so the output file never existed |
| 401 `invalid_api_key` | Check `Authorization: Bearer <key>` |
