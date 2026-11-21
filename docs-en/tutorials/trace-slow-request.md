# Investigate a Slow Response

When a model answers successfully but takes a long time to start or finish its response, use request history to see which stage takes the time. In this tutorial, you make a Playground request, find its record, interpret its timing, and use its request ID to find the same record again.

All screenshots in this tutorial show the English UI and the same completed example request. Request IDs and timings are examples; your run may differ.

## Before You Begin

- You have permission to open **Playground** and **Monitor Center**.
- You have a chat service that streams answers successfully in the Playground, and you can view its request history.
- If the engine provides per-request timing, you can distinguish stages such as queuing and prefill. Otherwise, you can still see the overall wait and output durations.

This tutorial uses a single-node, single-replica service named `mac-gpt-oss`, running `mlx-community/gpt-oss-20b-MXFP4-Q8` with vLLM `0.31.0` on an Apple GPU. Per-request timing is enabled, and the example uses low reasoning effort. Select your own service as you follow along. You do not need to complete other tutorials, and your answer and timings do not need to match the example.

## 1. Send an Easily Identifiable Request

1. Open **Playground → Chat**, click **New conversation** (the plus sign), and select your service. This example uses `mac-gpt-oss`.
2. Click **Parameters** to the right of the model selector. Select **Maximum Output Tokens** and enter `2048`. Keep **Stream options → Report usage (include_usage)** enabled. For this example, click **Low Reasoning Effort** under **Extra Parameters**. The output limit does not mean the model necessarily generates that many tokens.
3. Enter the following prompt, note the current time, and click **Send**.

   ```text
   Write an approximately 600-word beginner's guide for colleagues using large language models for the first time. Use six sections: what it is, suitable tasks, how to phrase a question, how to add context, how to verify results, and how to protect sensitive information. Give one concrete example in each section. Do not use a table.
   ```

![Request setup: mac-gpt-oss selected, maximum output tokens set to 2048, low reasoning effort, and a prompt requesting a six-section introduction](../assets/screenshots/en/tutorial-trace-single-request.png)

4. Wait for the answer to finish and confirm that there is no request error. Notice whether the main delay occurs before the answer starts or while it continues to stream. The example request completes normally; the following steps use its details to determine the time spent in each stage.

If the request fails or you stop it manually, resolve the issue and try again. This tutorial analyzes a request that completes normally.

## 2. Find the Request in the Service History

1. Open **Monitor Center** and click the service you selected in the service list.
2. Select **6h** at the top of the page and switch to **History**.
3. Enter a time range covering the request in **From** and **To**, pressing Enter to confirm each field. Use the format `MM-DD HH:mm`. This example uses `10-24 02:30` through `10-24 02:34`.
4. Select **Completed** for the result, enter `5` seconds in **End-to-end ≥**, and click **Reload**. The example request takes about `83.3 s`, so it meets this filter.

![History filters: the request time range, completed results, and an end-to-end duration of at least 5 seconds locate the Playground request for mac-gpt-oss](../assets/screenshots/en/tutorial-trace-single-history.png)

5. Find the request by its time and **Playground** caller, then click its row to open **Request details**. The example's received time is `10-24 02:31:26`.

If there are no results, clear the minimum end-to-end duration and check the time range first. Your request might take less than 5 seconds. Use this individual request's timings for the following analysis; the averages at the top of the page describe all requests in the selected period.

## 3. Identify Where Most of the Time Goes

1. Check the service name at the top of the page, then check **Received**, **Caller**, and **Replica** at the top of the details. Click the copy button beside **Request ID** and save it. The example ID is `r10441`.
2. At the bottom of the details, confirm **Status 200** and **finish stop**, then check **total** and the duration of each stage.
3. Compare the stages and identify the longest one. The table below uses the exact recorded values for this request; the screenshot rounds longer stages to seconds or minutes.

| Stage | Recorded value in this example | Meaning |
|---|---|---|
| Gateway | `3 ms` | Time the gateway spends dispatching the request |
| Engine preprocessing | `959 ms` | Time before the first generated token that is not attributed to engine queuing or prompt computation |
| Prefill | `7036 ms` | Time the engine spends processing the input prompt |
| Thinking | `1424 ms` | Time between the first generated token and the first answer token |
| Answer | `73759 ms` | Time from the first answer token to the last text chunk |
| Engine postprocessing | `73 ms` | Time after the last text chunk until the request ends |

![Details for request r10441: 135 input tokens, 1042 output tokens, distinct prefill, thinking, and answer stages, status 200, and normal completion](../assets/screenshots/en/tutorial-trace-single-detail.png)

This request has `135` input tokens (`32` cached), `1042` output tokens, and `16` thinking tokens. The first generated token arrives `3 + 959 + 7036 = 7998 ms`, or about `8.00 s`, after the gateway receives the request. Thinking lasts another `1424 ms`, so the first answer token arrives after `9422 ms`, or about `9.42 s`. The full request takes `83254 ms`, or about `83.25 s`.

**Most of this request's time is spent generating the answer**: the answer stage takes about `73.76 s`, or approximately `88.6%` of the total duration. The first answer token arrives at about `9.42 s`, so the full `83.25 s` cannot be described as waiting for the answer to start. Draw your own conclusion from the longest stage in your request.

In this example, **Thinking** falls between the first generated token and the first answer token. If your engine does not separate queuing and prefill, the page shows a combined **Waiting for first token** stage. Interpret the stages actually shown; do not attribute the combined wait directly to the network. **Engine decode time** in the details is for comparison and must not be added again to the sum of the colored stages. For complete definitions, see [Request Timeline and Stage Breakdown](../reference/request-timeline.md#1-记录了哪些时间点).

## 4. Find the Same Record by Request ID

1. Close the details and return to the **Monitor Center** home page.
2. Paste the request ID you saved in section 3 into the **Request ID / trace ID** search box at the upper right, then press Enter. This example uses `r10441`; replace it with your own ID.
3. Click the matching record in the **Request** panel on the right. Confirm that its **Request ID**, **Deployment**, **Received** time, and total duration match the record you just examined.

![Request ID search: r10441 found within the last hour, showing a Playground call at 02:31:26 and a total duration of about 1.4 minutes](../assets/screenshots/en/tutorial-trace-single-search.png)

The search covers the last hour by default. If it shows **Nothing found**, click **Widen to the last 24 hours**. If the request still does not appear, check the ID, request time, and whether the record was saved. Request history has retention and sampling limits; a missing record does not prove that a request never occurred. See [Data Sources and Limitations in the Monitoring Guide](../how-to/monitor-and-logs.md#2-看请求视图与请求分布).

**Completion criteria**: You can find your request in the service history and find the same record again by request ID. You can explain the relationship between waiting for the first token, answer generation, and total duration, and identify where most of the time goes based on the actual stages.

<!-- translation-source-sha256: 131b98049849213fd6a59ae4a536ddb3f94c394fee2cc73466a0f75491a1a2e1 # Investigate a Slow Response -->
<!-- translation-source-sha256: f16f850e1fcd57d573bff55a9848c888ae4d42d8c2b95bfcf8ae04fa7be94103 ## Before You Begin -->
<!-- translation-source-sha256: 031d5cf8fbb58a4c5f536c5f0f213ad1334c988fe41f718b9b2830dc5a2d51ce ## 1. Send an Easily Identifiable Request -->
<!-- translation-source-sha256: bad89ecccfff66f33794655e0dbe381efd99303e191bf59a79c8b80560b1dcae ## 2. Find the Request in the Service History -->
<!-- translation-source-sha256: 5c912e4c6a039c64a244ed1a60aa89acbeeacb914bac687ee2b4973ddf85969a ## 3. Identify Where Most of the Time Goes -->
<!-- translation-source-sha256: 6064f2dec52c0ec39fea4a00358ddb6e786d12b80dbc4c470f603870b50552d9 ## 4. Find the Same Record by Request ID -->
<!-- translation-localized-code-block: ef092705e802a3c2b5efba757885a4728f51cf78a1626fcf6dafa0d330f42a71 51273292cd140c06a4abb1450496b25a9397b1f06030de564aa066a69f02cc7f Reviewed English translation of the visible natural-language Playground prompt; the fenced text format and six requested sections are unchanged. -->
