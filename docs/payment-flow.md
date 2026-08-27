# Payment flow

## Transaction state machine

Implemented in [`internal/domain/transaction.go`](../internal/domain/transaction.go). Every status change goes through `TransitionTo`; there is no direct assignment to `Status` anywhere else in the codebase.

```mermaid
stateDiagram-v2
    [*] --> created: POST /payments

    created --> authorized: acquirer approves
    created --> failed: acquirer declines

    authorized --> captured: capture()
    authorized --> voided: void()
    authorized --> failed: capture declined

    captured --> captured: partial capture
    captured --> settled: reconciliation
    captured --> captured: partial refund
    captured --> refunded: refund in full

    settled --> refunded: refund in full

    failed --> [*]
    voided --> [*]
    refunded --> [*]
```

Rules layered on top of the transitions:

- `captured_amount` may never exceed `amount` — enforced in the domain **and** by a `CHECK` constraint.
- `refunded_amount` may never exceed `captured_amount` — same, twice.
- An authorization older than 7 days cannot be captured (`ErrAuthorizationExpired`).
- A partial refund leaves the status alone; only a refund that brings `refunded_amount` up to `captured_amount` reaches the terminal `refunded` state. Otherwise "refunded" would mean two different things.
- A transaction with any captured amount cannot be voided — that is what refund is for.

## Authorize

```mermaid
sequenceDiagram
    autonumber
    participant M as Merchant
    participant API
    participant R as Redis
    participant DB as Postgres
    participant A as Acquirer

    M->>API: POST /payments
    API->>API: verify HMAC (timestamp + method + path + body)
    API->>API: reject if |now - timestamp| > 5 min
    API->>R: SET idem:<merchant>:<key> NX EX 86400

    alt key completed with the same fingerprint
        API-->>M: stored response, Idempotent-Replay: true
    else key in progress
        API-->>M: 409 request_in_progress
    else key completed with a different fingerprint
        API-->>M: 422 idempotency_key_reuse
    else claim won
        API->>DB: INSERT transaction (created)
        Note over DB: UNIQUE (merchant_id, reference)<br/>rejects a duplicate order here,<br/>before any money is touched

        rect rgb(240, 248, 255)
            Note over API,A: no lock held, no transaction open
            API->>A: Authorize(amount, card)
        end

        alt approved
            API->>DB: BEGIN
            API->>DB: SELECT ... FOR UPDATE
            API->>DB: re-check status == created
            API->>DB: UPDATE -> authorized, version+1
            API->>DB: INSERT webhook_deliveries (payment.authorized)
            API->>DB: COMMIT
            API->>R: store {201, body, fingerprint}
            API-->>M: 201 authorized
        else declined
            API->>DB: UPDATE -> failed (failure_code)
            API->>DB: INSERT webhook_deliveries (payment.failed)
            API->>R: store {402, body, fingerprint}
            API-->>M: 402 + reason code
        else no answer (timeout)
            Note over API: status stays `created`.<br/>The hold may or may not exist;<br/>asserting failure would be a lie.
            API->>R: DEL key (releases the claim)
            API-->>M: 503 acquirer_unavailable
        end
    end
```

## Capture

The shape that matters: **pre-check → network → lock and re-check → write**.

```mermaid
sequenceDiagram
    autonumber
    participant M as Merchant
    participant API
    participant DB as Postgres
    participant A as Acquirer

    M->>API: POST /payments/{id}/capture
    API->>DB: SELECT transaction (no lock)
    API->>API: pre-check: capturable? amount within remaining?
    Note right of API: advisory only —<br/>the row can change while we are on the network

    rect rgb(240, 248, 255)
        API->>A: Capture(acquirer_ref, amount)
    end

    API->>DB: BEGIN
    API->>DB: SELECT ... FOR UPDATE
    Note right of DB: a concurrent capture blocks here
    API->>API: double-check the domain rules on the fresh row

    alt still capturable
        API->>DB: UPDATE captured_amount, version+1
        API->>DB: INSERT ledger entries (debits == credits)
        API->>DB: INSERT webhook_deliveries (payment.captured)
        API->>DB: COMMIT
        API-->>M: 200 captured
    else already captured by someone else
        API->>DB: ROLLBACK
        Note right of API: ERROR log:<br/>money moved at the acquirer but not in our books.<br/>Reconciliation is the safety net.
        API-->>M: 422 / 409
    end
```

## Webhook delivery

```mermaid
sequenceDiagram
    autonumber
    participant P as Poller
    participant Q as jobs channel
    participant W as Worker (xN)
    participant DB as Postgres
    participant M as Merchant endpoint

    loop every 2s
        P->>DB: release rows stuck in 'delivering' > 5 min
        P->>DB: SELECT ... WHERE status IN ('pending','failed')<br/>AND next_attempt_at <= now()<br/>FOR UPDATE SKIP LOCKED
        DB-->>P: batch (status -> 'delivering')
        P->>Q: send (blocks when every worker is busy)
    end

    W->>Q: receive
    W->>DB: load merchant, decrypt webhook_secret
    W->>M: POST payload<br/>X-Signature: t=<unix>,v1=hmac_sha256(secret, "t.body")

    alt 2xx
        W->>DB: status = delivered
    else non-2xx or transport error
        W->>DB: attempt_count++<br/>next_attempt_at = now + 2^n ±20% jitter
        Note right of DB: after 6 attempts: status = dead
    end
```

On `SIGINT`/`SIGTERM`: the poller stops first so no new rows are claimed, the channel closes, and workers finish what they hold on a context detached from the shutdown signal — bounded by the HTTP client timeout and a 30s overall deadline. A job is either completed or released; never abandoned half-done.

### Verifying a webhook, as a merchant

```
X-Signature: t=1756288800,v1=5257a869e7ecebeda32affa62cdca3fa51cad7e77a0e56ff536d0ce8e108d8bd
```

```go
mac := hmac.New(sha256.New, []byte(webhookSecret))
mac.Write([]byte(timestamp))
mac.Write([]byte("."))
mac.Write(body)
expected := hex.EncodeToString(mac.Sum(nil))

// constant time, and reject anything older than your tolerance
ok := hmac.Equal([]byte(expected), []byte(v1)) && time.Since(signedAt) < 5*time.Minute
```

The timestamp is part of the signed string, so rewriting it to defeat the freshness check invalidates the signature.

## Reconciliation and settlement

```mermaid
flowchart TD
    A["worker -job=reconcile -date=YYYY-MM-DD"] --> B["Sum transactions captured that day<br/>per merchant: captured, refunded, fees, net"]
    A --> C["Sum ledger_entries for the same transactions"]
    B --> D{"Do the two sides agree?"}
    C --> D
    D -- no --> E["Log every discrepancy and<br/>every transaction involved"]
    E --> F["Write the report, exit code 2.<br/>Nothing is settled."]
    D -- yes --> G{"Is every entry group balanced?"}
    G -- no --> E
    G -- yes --> H["For each captured transaction:<br/>lock, transition to settled,<br/>book the payout entry group"]
    H --> I["Queue payment.settled webhooks"]
    I --> J["Write settlement-YYYY-MM-DD.json and .csv"]
```

The non-zero exit code is what makes the job usable from cron: a day that does not reconcile is a day somebody has to look at, and settlement does not run on numbers nobody has checked.
