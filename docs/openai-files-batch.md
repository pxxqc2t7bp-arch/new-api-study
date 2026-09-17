# OpenAI-compatible Files and Batch

The gateway implements local `Files` and `Batch` resources for upstreams that
do not expose native OpenAI Batch APIs.

## Endpoints

- `POST /v1/files` with multipart fields `purpose=batch` and `file`
- `GET /v1/files`
- `GET /v1/files/{file_id}`
- `DELETE /v1/files/{file_id}`
- `GET|HEAD /v1/files/{file_id}/content`
- `POST /v1/batches`
- `GET /v1/batches`
- `GET /v1/batches/{batch_id}`
- `POST /v1/batches/{batch_id}/cancel`

Files and batches are scoped to the API token that created them. Input JSONL
supports at most 50,000 requests and 200 MiB by default. Every line must use
`POST`, have a unique `custom_id`, match the batch's declared endpoint, contain
a JSON object body with a model, and be non-streaming.

Supported endpoints are `/v1/chat/completions`, `/v1/responses`,
`/v1/embeddings`, `/v1/images/generations`, and `/v1/videos`.

Batch items re-enter the normal gateway authentication, routing, plugin, usage,
logging, and billing path. They use normal online model prices; the gateway
does not advertise or apply an upstream Batch discount.

## Persistence

File bytes are stored under `BATCH_STORAGE_DIR` (default
`/data/new-api/batches`) with random storage keys, mode `0600`, atomic rename,
and a persisted SHA-256 digest. The input digest is checked before a batch is
created. Database rows store metadata and item state, not channel credentials.

The master-node system task runner claims batches with leases. An expired
lease resets stale running items to pending for crash recovery. Cancellation
lets the current claimed chunk finish and prevents pending items from starting.

Configuration:

- `BATCH_MAX_FILE_MB` (default `200`)
- `BATCH_MAX_LINES` (default `50000`)
- `BATCH_WORKER_CONCURRENCY` (default `4`)
- `BATCH_RETENTION_HOURS` (default `720`)
- `BATCH_REQUEST_TIMEOUT_SECONDS` (default `900`)
