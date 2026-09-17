# Open WebUI v0.11.3 API contract used by M2

Status: source-verified on 2026-09-17, locally exercised against an `httptest` implementation of this contract, and not yet exercised against an authorized live Open WebUI installation. This document fixes the narrow downstream contract used by `internal/owu`; it is not a general Open WebUI API wrapper.

## Fixed version and identity

The adapter supports exactly Open WebUI `0.11.3` for M2. Startup identity verification performs these two reads against the configured, fixed base URL:

| Purpose | Request | Required response fields |
| --- | --- | --- |
| version and deployment | `GET /api/version` without the OWU credential | `version`, `deployment_id` |
| token owner | `GET /api/v1/auths/` with `Authorization: Bearer <token>` | `id`; `email`, `name`, and `role` are diagnostic only |

The route prefixes are registered in [`main.py`](https://github.com/open-webui/open-webui/blob/v0.11.3/backend/open_webui/main.py#L833-L838), and the version route returns both fields in [`main.py`](https://github.com/open-webui/open-webui/blob/v0.11.3/backend/open_webui/main.py#L2552-L2557). The authenticated session route and response are in [`auths.py`](https://github.com/open-webui/open-webui/blob/v0.11.3/backend/open_webui/routers/auths.py#L237-L304).

`site_id` is an opaque SHA-256 binding over the canonical configured base URL, including any path prefix, and `deployment_id`. This prevents a stored target ID from being carried to another URL that happens to report the same deployment identifier. `account_id` is the `id` from the authenticated session response. After installation, both values must equal the persisted installation identity before any target operation is allowed.

The session response can echo a token. The adapter decodes only identity fields and never exposes or persists that returned token.

## Chat routes and bodies

The adapter implements only these routes:

| Operation | Request | Response |
| --- | --- | --- |
| create | `POST /api/v1/chats/new` with `{"chat": <chat-document>}` | one `ChatResponse` |
| read | `GET /api/v1/chats/{id}` | one `ChatResponse` owned/readable by the authenticated user |
| update | `POST /api/v1/chats/{id}` with `{"chat": <chat-document>}` | one `ChatResponse` |

`ChatForm` contains a free-form `chat` object plus optional `variables` and `folder_id`; `ChatResponse` contains `id`, `user_id`, `title`, the full `chat` object, timestamps, archive/pin/folder state and other metadata. The fixed definitions are in [`models/chats.py`](https://github.com/open-webui/open-webui/blob/v0.11.3/backend/open_webui/models/chats.py#L252-L299). The create, read and update handlers are in [`routers/chats.py`](https://github.com/open-webui/open-webui/blob/v0.11.3/backend/open_webui/routers/chats.py#L771-L797), [`routers/chats.py`](https://github.com/open-webui/open-webui/blob/v0.11.3/backend/open_webui/routers/chats.py#L1318-L1350), and [`routers/chats.py`](https://github.com/open-webui/open-webui/blob/v0.11.3/backend/open_webui/routers/chats.py#L1353-L1393).

Create uses a server-generated UUID. The caller cannot choose the chat ID: the route passes `str(uuid4())` into storage. A trustworthy target ID therefore exists only after a complete, valid create response has been decoded and its owner and operation marker have been checked.

The adapter does not expose list, search, import, delete, message-delete, archive, arbitrary method/path, or arbitrary base-URL methods. A caller may pass a chat ID to the internal read/update method only after storage has resolved it from a trusted binding/allow-list record; model input is never passed through as a target ID.

## Chat document conversion and validation

The written chat document maintains both Open WebUI representations:

```json
{
  "title": "Source title",
  "history": {
    "currentId": "target-message-id",
    "messages": {
      "target-message-id": {
        "id": "target-message-id",
        "parentId": null,
        "childrenIds": [],
        "role": "user",
        "content": "exact text"
      }
    }
  },
  "messages": []
}
```

The full graph is authoritative. `history.messages` is a message-ID map, `parentId` and `childrenIds` are reciprocal, `history.currentId` identifies the active leaf, and top-level `messages` is the active branch view. Open WebUI derives its stored `current_message_id` from these fields in [`models/chats.py`](https://github.com/open-webui/open-webui/blob/v0.11.3/backend/open_webui/models/chats.py#L404-L420). Its create path stores the chat dict and dual-writes history messages in [`models/chats.py`](https://github.com/open-webui/open-webui/blob/v0.11.3/backend/open_webui/models/chats.py#L542-L590).

The bridge uses distinct deterministic target message IDs and stores source message/node IDs, channel and original text parts in a namespaced per-message metadata object. `content` is the exact concatenation of the source text parts; the original parts remain available for lossless readback verification. An operation marker is stored in a namespaced chat-level object.

Readback rejects rather than repairs:

- a map key that differs from the contained message ID;
- missing or invalid role/content/children fields;
- missing nodes, duplicate children, non-reciprocal parent/child links, cycles or unreachable nodes;
- a missing/unknown `currentId`;
- an active `messages` view whose IDs, order, roles or contents differ from the selected history branch;
- a graph/view inconsistency even when visible IDs and text happen to look the same.

All graph nodes are included in the normalized projection. Nodes without the bridge metadata are marked unmanaged, so a native Open WebUI continuation or branch cannot be mistaken for bridge-owned content.

If a user edits managed text in Open WebUI, the saved bridge parts metadata naturally becomes stale. The adapter keeps the source/target mapping, takes the actual OWU `content` as one text part, and lets the sync planner report a target-content conflict. Stale metadata cannot restore or mask the old body, and the target is not misreported as unreadable merely because the user edited it.

Updates begin from a full current read and preserve unknown chat fields, unknown per-message fields and unmanaged nodes. The bridge never infers deletion from a missing source message. The v0.11.3 model also merges incoming history with stored history and does not infer deletes in this route; see [`models/chats.py`](https://github.com/open-webui/open-webui/blob/v0.11.3/backend/open_webui/models/chats.py#L717-L754).

The strict conflict hash is canonical JSON over the complete relevant response, including unknown envelope/chat/message metadata. Only clock/computed response fields (`created_at`, `updated_at`, `last_read_at`, `context_usage`) are excluded. Write-before-read equality uses this hash. Post-write verification compares the complete desired chat document, graph and stable response metadata, allowing only the server-owned chat document `id`, current operation receipt field, and the same documented volatile response fields to differ. Archive, pin and folder changes are always conflict relevant.

## Concurrency and outcome semantics

The v0.11.3 read/update route has no `ETag`, `If-Match`, version parameter, revision precondition or other compare-and-swap contract. The storage method takes a row lock only inside Open WebUI while applying the update; it cannot condition that update on the bridge's earlier GET. Therefore the bridge can perform a write-before-read comparison and write-after-readback, but it cannot close the race in which the user edits the chat after the last GET and before the POST. A mismatch on response/readback becomes conflict or reconciliation; M2 must not claim atomic protection from concurrent OWU UI edits.

Writes use these result classes:

- A complete 2xx JSON response with matching target/owner and exact operation marker is a receipt, but success is finalized only after strict GET readback.
- Transport timeout/disconnect, truncated or oversized 2xx, malformed 2xx, wrong response identity, or a missing operation marker is `outcome_unknown`.
- HTTP 400 and 5xx are also `outcome_unknown`. In v0.11.3, create/update storage may commit before later event publication finishes, so an error response is not always proof that nothing was stored.
- HTTP 408 and 429 are `outcome_unknown`, since an intermediary can emit them after the application has received the request.
- Authentication/authorization/validation statuses that prove rejection before the handler are terminal failures.

Create `outcome_unknown` returns no target ID and is never blindly retried. The server chooses the ID, and v0.11.3 exposes no verified marker lookup route. List/search adoption is intentionally forbidden because it could inspect or claim unrelated chats. The durable operation remains `needs_reconciliation`; without a trusted receipt ID, automatic reconciliation cannot discover the created row. This is an explicit at-most-one-attempt safety boundary, not exactly-once create.

An update with a known allowed target can be reconciled by GET: the readback must contain the exact operation marker and strictly match the desired snapshot. External convergence to the same text without the marker is not credited to the operation.

HTTP redirects are rejected and never followed, preventing the bearer credential from crossing to another origin. Responses are size-bounded, the client has a total timeout, request/response bodies are absent from errors, and status errors contain no token, URL, response body or chat text.

## Evidence and remaining external gap

The source files inspected from the fixed `v0.11.3` tag had these SHA-256 hashes during this review:

| Source | SHA-256 |
| --- | --- |
| `backend/open_webui/main.py` | `e5cbc9326266a7c0983061ecf8b792184f91e13a0e245c3e520549ec0a2978e1` |
| `backend/open_webui/routers/auths.py` | `160d4572ca7a8a5c71eaeb09b14b29b958164a2ac5fb79b45f052971af1047ed` |
| `backend/open_webui/routers/chats.py` | `13ce859cf31ffbdb64e1cfac46ae0dd578bb7865a15d446f33e278f4cbbf0640` |
| `backend/open_webui/models/chats.py` | `aab391c385484005f929f9d807dc9b2db1f86479c5aad7227fd8771535348907` |

Local tests cover the exact paths and body shapes, version/account/site mismatch, fixed bearer use, redirects, response limits, redacted errors, definite versus unknown outcomes, create/read/update readback, deterministic conversion, unknown metadata preservation, native branches, malformed graphs and strict/canonical comparison.

No OWU credential or authorized dedicated test environment was available in this implementation run. Still pending are a real v0.11.3 identity check, API-key permission behavior, create/update/readback against a dedicated chat, actual page rendering, and an induced live timeout/concurrent UI edit. Those are external acceptance items and are not implied by the source review or mock server results.
