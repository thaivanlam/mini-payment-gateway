# Learning path

How to read this repository in one or two days, deeply enough to defend every design decision in an interview.

This is not a summary of [README.md](../README.md) or [docs/architecture.md](architecture.md). Those two answer *what the system is*. This one answers *what to read, in what order, and what concept each piece of code teaches*. Every claim below is traceable to a file in this repository; where something could not be established from the code, it says so.

---

## Section 1 — The one-minute map

Re-read this page whenever you lose context.

A merchant calls an HMAC-signed HTTP API to place a hold on a card, then captures it, refunds it, or voids it. Every movement of money is written as a balanced group of entries in an append-only double-entry journal, never as a mutated balance column. Every state change queues a signed webhook in the same database transaction that moved the money, and a separate worker process delivers it with exponential backoff. Once a day a reconciliation job re-derives the day's totals from both the `transactions` table and the journal, and refuses to settle if the two disagree. The card processor is simulated, but the seam is a real port: it is called over the network, under a timeout and a circuit breaker, and it can decline, hang, or vanish.

| Process | Command | Responsibility |
|---|---|---|
| API | `cmd/api` | HTTP, HMAC auth, rate limit, idempotency, payment orchestration |
| Worker | `cmd/worker` | webhook delivery pool; `-job=reconcile` runs the daily close and exits `2` if the day does not reconcile |
| Migrate | `cmd/migrate` | applies the embedded goose migrations |
| Seed | `cmd/seed` | demo merchant (prints the secret once) plus sample transactions |

The three invariants that matter most:

1. **Within one entry group, debits equal credits** — enforced in `domain.EntryGroup.Validate`, re-checked in `LedgerRepo.InsertGroup`, and asserted over the whole database after every integration scenario.
2. **A retried money-moving request charges at most once** — one `SET NX` claim in Redis decides the winner; everyone else replays, waits, or is refused.
3. **`captured_amount ≤ amount` and `refunded_amount ≤ captured_amount`, always** — enforced in the domain, again by a `CHECK` constraint, and protected under concurrency by `SELECT … FOR UPDATE`.

---

## Section 2 — Reading order

Ordered by execution flow, not by directory. Start with the packages that import nothing, end with the wiring and the architecture notes.

For the same route one level down — which *functions* to read, in which order, and which ones each depends on — see [docs/code-reading-order.md](code-reading-order.md).

| # | File | Why here | Time |
|---|---|---|---|
| 1 | [internal/money/money.go](../internal/money/money.go) | Imports nothing. Teaches why money is `int64` minor units and why `Fee` truncates *down*. 60 lines, and every amount in the system is this type. | 10 min |
| 2 | [internal/domain/errors.go](../internal/domain/errors.go) | The sentinel-error vocabulary. Every layer above inspects these with `errors.Is`/`errors.As`; you cannot read the service layer without knowing them. | 10 min |
| 3 | [internal/domain/transaction.go](../internal/domain/transaction.go) | The state machine and every money rule. `allowedTransitions` is the single source of truth; nothing outside this file assigns to `Status`. | 30 min |
| 4 | [internal/domain/transaction_test.go](../internal/domain/transaction_test.go) | `TestAllowedTransitions` walks all 49 (from, to) pairs. Read the test before trusting your reading of the map. | 15 min |
| 5 | [internal/domain/ledger.go](../internal/domain/ledger.go) | The chart of accounts and the three entry-group constructors. `Validate` is the invariant, written once. | 30 min |
| 6 | [internal/domain/ledger_test.go](../internal/domain/ledger_test.go) | The worked examples: capture 100,000 at 2%, refund it, settle it. Confirms that a refund is *not* the mirror of a capture. | 15 min |
| 7 | [migrations/00002_create_transactions.sql](../migrations/00002_create_transactions.sql), [00003_create_ledger_entries.sql](../migrations/00003_create_ledger_entries.sql) | The same rules again, as `CHECK` constraints and an append-only trigger. Read them next to step 3 and 5 and note what is duplicated on purpose. | 15 min |
| 8 | [internal/repository/postgres.go](../internal/repository/postgres.go) | `Querier` and `WithTx`. The reason repository methods take a `Querier` explicitly is that the *caller* decides what runs inside a transaction. | 15 min |
| 9 | [internal/repository/transaction_repo.go](../internal/repository/transaction_repo.go) | `GetForUpdate` and the version-guarded `Update`. This is where the two concurrency mechanisms actually live. | 30 min |
| 10 | [internal/repository/ledger_repo.go](../internal/repository/ledger_repo.go) | `InsertGroup`, `Balance` as a fold, and `FindUnbalancedGroups` — the invariant expressed as SQL. | 25 min |
| 11 | [internal/acquirer/acquirer.go](../internal/acquirer/acquirer.go) | The port. Note the contract in the doc comment: a decline and an infrastructure failure are different kinds of answer. | 10 min |
| 12 | [internal/acquirer/breaker.go](../internal/acquirer/breaker.go), [guarded.go](../internal/acquirer/guarded.go) | Three-state breaker plus the timeout wrapper. `Guarded.do` is where "a decline must not trip the breaker" is implemented. | 25 min |
| 13 | [internal/service/payment_service.go](../internal/service/payment_service.go) | **The centre of the repository.** Read `Authorize` and `Capture` line by line and mark, for every statement, whether a lock or an open transaction is held. | 60 min |
| 14 | [internal/idempotency/store.go](../internal/idempotency/store.go) | `Begin` (the `SET NX` claim), `Complete`, `Release`, and `Fingerprint` with its JSON canonicalisation. | 25 min |
| 15 | [internal/transport/http/middleware_idem.go](../internal/transport/http/middleware_idem.go) | The four branches, plus the panic and 5xx release paths. The doc comment enumerates them in order. | 20 min |
| 16 | [internal/service/merchant_service.go](../internal/service/merchant_service.go) | `Authenticate` and `ComputeRequestSignature`. Read the comment explaining why the components are newline-delimited. | 25 min |
| 17 | [internal/transport/http/middleware_auth.go](../internal/transport/http/middleware_auth.go) | Where the body is buffered once and handed to both the signature check and the fingerprint. | 15 min |
| 18 | [internal/webhook/signer.go](../internal/webhook/signer.go) | The Stripe-shaped scheme `t=…,v1=…`, with the timestamp inside the signed string. | 15 min |
| 19 | [internal/repository/webhook_repo.go](../internal/repository/webhook_repo.go) | `ClaimDue` (`FOR UPDATE SKIP LOCKED`) and `ReleaseStale`. Twenty lines of SQL that make N workers safe. | 20 min |
| 20 | [internal/webhook/dispatcher.go](../internal/webhook/dispatcher.go) | Poller → channel → worker pool, and the shutdown ordering. | 30 min |
| 21 | [internal/service/reconciliation_service.go](../internal/service/reconciliation_service.go) | Two independent derivations of the same numbers, compared, and settlement gated on agreement. | 35 min |
| 22 | [internal/transport/http/response.go](../internal/transport/http/response.go), [router.go](../internal/transport/http/router.go) | The single error→status mapping, and the middleware order (which is a dependency order, not a style choice). | 20 min |
| 23 | [internal/app/app.go](../internal/app/app.go) | The whole object graph in one function. No globals, no `init()`. | 10 min |
| 24 | [test/integration/concurrency_test.go](../test/integration/concurrency_test.go), [idempotency_test.go](../test/integration/idempotency_test.go) | The behaviour the design exists for, asserted against a real database. | 40 min |
| 25 | [docs/architecture.md](architecture.md) | Read last. It will now read as a summary of things you already know, which is the point. | 20 min |

**The rule to apply after each file:** answer *"if I deleted this file, where exactly would the system be wrong?"* If the answer is "nowhere obvious", you have not understood the file yet — go back and find the failure it prevents.

---

## Section 3 — Tracing one request

`POST /api/v1/payments`, from the merchant's socket to the merchant's webhook endpoint. The annotation on each step is the thing to memorise: **what is held while this runs**.

**Step 1 — Request id and logging**
An id is assigned (or taken from an inbound `X-Request-Id`), stamped on the response header, and put in the context so every later log line and error body carries it.
File: `internal/transport/http/middleware_log.go:RequestID`
Holds: nothing.
If it fails: it cannot; there is no failure path.

**Step 2 — Panic recovery armed**
`Recoverer` wraps everything below. It re-panics on `http.ErrAbortHandler` rather than swallowing the http package's own signal.
File: `internal/transport/http/middleware_log.go:Recoverer`
Holds: nothing.
If it fails: a panic below becomes a `500` with a request id and a logged stack, not a dropped connection.

**Step 3 — Body buffered, signature verified**
The body is read once under `http.MaxBytesReader` and restored as a fresh reader, then the timestamp is parsed and checked against the clock-skew window (default 5 minutes), the merchant is loaded by API key, the `api_secret` is AES-GCM-decrypted, and the HMAC over `timestamp \n method \n path \n body` is compared with `hmac.Equal`.
File: `internal/transport/http/middleware_auth.go:Auth` → `internal/service/merchant_service.go:Authenticate`
Holds: nothing. One unlocked `SELECT` on `merchants`.
If it fails: `401 authentication_failed` — and a suspended merchant gets `403 merchant_suspended` *after* the signature check, so a bad signature never learns whether the account exists.

**Step 4 — Rate limit**
One pipelined `INCR` + `EXPIRE` against `rl:<merchant>:<window>` in Redis.
File: `internal/transport/http/middleware_ratelimit.go:RateLimit` → `internal/ratelimit/limiter.go:Allow`
Holds: nothing.
If it fails: **fails open.** A Redis error is logged and the request proceeds. Over the limit is `429` with `Retry-After`.

**Step 5 — Idempotency claim**
The buffered body is canonicalised (JSON re-encoded with sorted keys) and SHA-256'd into a fingerprint; `SET idem:<merchant>:<key> NX EX 86400` claims the key atomically.
File: `internal/transport/http/middleware_idem.go:Idempotency` → `internal/idempotency/store.go:Begin`
Holds: nothing.
If it fails: **fails closed.** A Redis error becomes a `500` — running without deduplication risks a double charge. Missing header → `400`. Lost claim → `409` in progress, `422` on a different body, or a byte-for-byte replay with `Idempotent-Replay: true` and the handler never runs.

**Step 6 — Decode and validate the DTO**
The JSON body becomes a `CreatePaymentRequest` and then an `AuthorizeInput`.
File: `internal/transport/http/handler_payment.go:Create` → `internal/transport/http/request.go`
Holds: nothing.
If it fails: `400 invalid_request` naming the offending field.

**Step 7 — Insert the transaction row**
`domain.NewTransaction` validates reference, amount and currency, then a single `INSERT` runs on the pool.
File: `internal/service/payment_service.go:Authorize` → `internal/repository/transaction_repo.go:Create`
Holds: only the implicit single-statement transaction of that one `INSERT`.
If it fails: a `23505` on `transactions_merchant_reference_key` becomes `ErrDuplicateReference`. The service then re-reads the row: if it is still `created` with the same amount and currency, this is a retry of an authorization that never got an answer and it is **resumed**; otherwise it is a genuine duplicate order → `409 duplicate_reference`.

**Step 8 — Call the acquirer** ← the decision the repository exists to teach
`Guarded.Authorize` checks the breaker, opens a 3-second `context.WithTimeout`, and calls the processor.
File: `internal/acquirer/guarded.go:do` → `internal/acquirer/mock.go:Authorize`
Holds: **nothing. No row lock, no open database transaction, no pooled connection.**
If it fails: a decline → step 9a; a timeout or an open circuit → `503` and the row is deliberately left at `created`.

**Step 9a — Declined**
`finalizeFailure` opens a transaction, locks the row, checks it is still `created`, calls `Fail(code)`, updates, and queues a `payment.failed` webhook.
File: `internal/service/payment_service.go:finalizeFailure`
Holds: **DB transaction + row lock**, for the duration of three statements.
If it fails: `402` with the acquirer's reason code. No ledger entries — a decline moves no money.

**Step 9b — Unknown outcome**
Logged at ERROR; the row stays `created`; `503 acquirer_unavailable` is returned and the idempotency key is released so a retry gets a real attempt.
File: `internal/service/payment_service.go:Authorize`, `ErrAcquirerUnavailable` branch
Holds: nothing.
If it fails: nothing to fail — this *is* the failure path. Asserting `failed` here would be a claim the gateway cannot support.

**Step 10 — Write-back**
`BEGIN`; `SELECT … FOR UPDATE` re-reads the row; the status is re-checked as `created`; `Authorize()` sets the acquirer ref, last4 and brand; `UPDATE … WHERE id = $1 AND version = $2`; the `payment.authorized` outbox row is inserted; `COMMIT`.
File: `internal/service/payment_service.go:Authorize` write-back closure → `internal/repository/transaction_repo.go:GetForUpdate`, `Update`
Holds: **DB transaction + row lock**, for four short statements and no network I/O.
If it fails: a concurrent finisher makes the re-check fail → `ErrConcurrentModification` → `409`. Rollback drops both the status change and the queued webhook together.

**Step 11 — Respond and store the idempotent answer**
The handler writes `201`; the recorder has captured the body; because the status is below 500, `store.Complete` writes `{status, body, fingerprint}` over the claim.
File: `internal/transport/http/middleware_idem.go`, after `next.ServeHTTP`
Holds: nothing.
If it fails: the store error is logged, not returned — the response is already on the wire, and a later retry re-runs the handler where the database constraints catch it.

**Step 12 — The worker claims the outbox row**
A separate process ticks every `WEBHOOK_POLL_INTERVAL`, first releasing anything stuck in `delivering` for over five minutes, then claiming due rows with a CTE that does `FOR UPDATE SKIP LOCKED` and flips them to `delivering` in the same statement.
File: `internal/webhook/dispatcher.go:poll` → `internal/repository/webhook_repo.go:ClaimDue`, `ReleaseStale`
Holds: a row lock **only for the duration of that one statement** — it runs on the pool, not inside `WithTx`.
If it fails: the error is logged and the next tick retries. Another worker's rows are skipped, not queued behind.

**Step 13 — Deliver**
The merchant is loaded, `webhook_secret_enc` is decrypted, and the payload is POSTed with `X-Signature: t=<unix>,v1=<hmac_sha256(secret, "<t>.<body>")>`.
File: `internal/webhook/dispatcher.go:deliver`, `post` → `internal/webhook/signer.go:Sign`
Holds: nothing. The job runs on a context detached from shutdown, bounded by the HTTP client timeout.
If it fails: `RecordFailure` increments the attempt count and schedules `now + 2^n` seconds with ±20% jitter; after `WEBHOOK_MAX_ATTEMPTS` the row is parked as `dead`. A merchant with no `webhook_url` is parked as `dead` immediately rather than retried six times.

**Step 14 — Delivered**
A 2xx marks the row `delivered`.
File: `internal/domain/webhook.go:RecordSuccess` → `internal/repository/webhook_repo.go:UpdateAttempt`
Holds: nothing.
If it fails: the persist error is logged; the stale-claim janitor eventually returns the row to the queue.

### Capture — what differs

1. Cheap pre-check on an **unlocked** read (`GetByMerchantAndID`), on a *copy* of the transaction (`preview := *txn`), so the real object is never mutated by a check.
2. `amount == 0` means "capture everything still uncapturable" — resolved before the acquirer call.
3. Acquirer `Capture` — no lock, no transaction.
4. Lock, then **re-apply the identical domain rule** to the fresh row. This is the double-check; the pre-check in step 1 is advisory only.
5. Inside the same transaction: update the row, insert the capture entry group (`debit acquirer_receivable`, `credit merchant_payable`, `credit platform_fee_revenue`), insert the outbox row.
6. If the acquirer succeeded but the double-check failed, an ERROR is logged with the acquirer reference and reconciliation is the safety net. That gap is acknowledged in the code, not hidden.

### Refund — what differs

1. Same shape, but `preview.Refund` guards against `RefundExceedsCaptured` instead.
2. The entry group has **two** legs, not three: `debit merchant_payable`, `credit acquirer_receivable`. The platform fee is not returned.
3. A partial refund leaves the status at `captured` (or `settled`); only a refund that brings `refunded_amount` up to `captured_amount` reaches the terminal `refunded` state.
4. Consequence, asserted in [test/integration/payment_flow_test.go](../test/integration/payment_flow_test.go): a fully refunded 150,000 leaves the merchant balance at **−3,000** — the fee it already owes.

### Void — what differs

1. `Void` is **not** behind the idempotency middleware (see the route groups in `internal/transport/http/router.go`); it is naturally idempotent because `voided` is terminal and a second call gets `409 invalid_state`.
2. `domain.Transaction.Void` refuses if `CapturedAmount > 0` — that is what refund is for.
3. **No ledger entries are written.** The journal records movements of value, not intentions, and a released hold moved nothing.
4. Still writes an outbox row, so the merchant learns the hold is gone.

---

## Section 4 — Design questions

Only "why is it done this way" questions. Each row names the file that answers it, the concept to look up, and what breaks if you get it wrong.

| Question | File that answers it | Concept to look up | Consequence of getting it wrong |
|---|---|---|---|
| Why is the acquirer called *outside* the database transaction and outside the row lock? | `internal/service/payment_service.go:Capture` | Lock hold time, connection-pool exhaustion | One slow issuer serialises every request touching that row and drains the pool; a slow dependency becomes an outage |
| Then why is a second, identical check needed *inside* the lock? | `internal/service/payment_service.go:Capture` (the `fresh.Capture` call) | Time-of-check to time-of-use (TOCTOU) | The row can change during the network call; the pre-check would authorise a double capture |
| Why does the idempotency record store a request *fingerprint* and not just the response? | `internal/idempotency/store.go:Fingerprint` | Idempotency keys, request canonicalisation | A client reusing one key from a config file would silently receive another payment's answer, and its real second payment would vanish |
| Why canonicalise the JSON before hashing instead of hashing the raw bytes? | `internal/idempotency/store.go:canonicalize` | JSON canonical form | Proxies and clients reorder keys and change whitespace; byte comparison would refuse legitimate retries as key reuse |
| Why does an in-flight duplicate get `409` rather than blocking until the first finishes? | `internal/transport/http/middleware_idem.go` (branch 3) | Fail-fast vs. queueing | Blocking ties up a server goroutine and a connection per duplicate; a double-click storm becomes a thread-pool outage |
| Why does a 5xx *release* the key while a 402 decline *keeps* it? | `internal/transport/http/middleware_idem.go`, `internal/idempotency/store.go:Release` | Definite vs. indeterminate outcome | Caching an unknown outcome for 24h leaves the merchant permanently unable to complete that payment; not caching a decline invites a retry storm at the processor |
| Why does the rate limiter fail **open** while idempotency fails **closed**? | `internal/transport/http/middleware_ratelimit.go`, `internal/transport/http/middleware_idem.go` | Failure-mode asymmetry | Fail-closed on the limiter turns a Redis blip into a payment outage; fail-open on idempotency turns it into duplicate charges |
| Why is the timestamp part of the *signed string* rather than just a header? | `internal/webhook/signer.go:Sign`, `internal/service/merchant_service.go:ComputeRequestSignature` | Replay attacks, signature binding | An attacker rewrites `t` on a captured payload and replays it forever; the freshness check becomes decorative |
| Why are the four signature components joined by `\n` instead of concatenated? | `internal/service/merchant_service.go:ComputeRequestSignature` | Canonical request, delimiter ambiguity | `(path "/a", body "b")` and `(path "/ab", body "")` sign identically — one signature is valid for two different requests. Proven in `internal/service/signature_test.go:TestComputeRequestSignatureIsUnambiguous` |
| Why is the query string deliberately excluded from the signature? | `internal/service/merchant_service.go:Authenticate` (signs `r.URL.Path`) | Canonicalisation cost | Signing it would force clients and server to agree on parameter ordering and encoding — a large surface for false rejections. The trade-off: query parameters are not authenticated |
| Why are merchant secrets **encrypted** rather than hashed? | `internal/secrets/cipher.go` package comment, `migrations/00001_create_merchants.sql` | Symmetric vs. one-way primitives | HMAC verification requires recomputing the signature, so it needs the secret; a bcrypt hash makes signed requests impossible and forces bearer tokens instead |
| Why does the webhook row go through an **outbox table** instead of an HTTP call in the handler? | `internal/service/payment_service.go:queueWebhook`, `internal/repository/webhook_repo.go:Create` | Transactional outbox, dual-write problem | Two failure modes reappear: a webhook announcing a rolled-back capture, and a committed capture whose notification died with the process |
| Why does the worker use `FOR UPDATE SKIP LOCKED` instead of plain `FOR UPDATE`? | `internal/repository/webhook_repo.go:ClaimDue` | Queue-in-a-database, lock contention | N workers queue behind each other on the same head-of-queue row; adding workers adds no throughput |
| Why flip the status to `delivering` in the *same statement* as the claim? | `internal/repository/webhook_repo.go:ClaimDue` (the CTE + `UPDATE`) | Atomic claim, visibility of stuck work | A crashed worker's rows would look due and be retried by everyone at once, instead of being visibly stuck and released by the janitor |
| Why release `delivering` rows after five minutes instead of trusting the worker to clean up? | `internal/webhook/dispatcher.go:staleClaimTimeout`, `internal/repository/webhook_repo.go:ReleaseStale` | Crash-only design, lease expiry | A `SIGKILL` strands the job forever; "kill the worker, lose no job" stops being true |
| Why is the journal protected by a **trigger** rather than by application discipline? | `migrations/00003_create_ledger_entries.sql` | Defence in depth, integrity at the storage layer | A migration, a support script or a curious `psql` session can rewrite history; the audit trail is only as strong as its weakest writer |
| Why is a balance computed as a fold over the journal instead of stored in a column? | `internal/repository/ledger_repo.go:Balance`, `internal/domain/ledger.go:ComputeBalance` | Double-entry bookkeeping, event sourcing | `UPDATE balance SET amount = amount + x` destroys the explanation for the number; a wrong balance becomes untraceable and a correction becomes a rewrite of history |
| Why does a refund **not** reverse the platform fee? | `internal/domain/ledger.go:NewRefundEntryGroup` | Revenue recognition | Mirroring the capture would give back revenue that was genuinely earned; the current rule is why a fully refunded transaction correctly leaves the merchant at `−fee` |
| Why is there a `version` column when `SELECT FOR UPDATE` already serialises writers? | `internal/repository/transaction_repo.go:Update` | Optimistic vs. pessimistic locking | Every writer *today* takes the lock, so this is defence in depth: it catches a future writer that reads without locking, and turns a silent lost update into `ErrConcurrentModification` |
| Why does a duplicate order rely on a **unique constraint** rather than a "does it exist?" check? | `migrations/00002_create_transactions.sql`, `test/integration/concurrency_test.go:TestConcurrentAuthorizeSameReference` | Check-then-act race, constraint-driven design | Ten concurrent requests all read "not found" and all insert; only the database can decide this atomically |
| Why does a card decline **not** trip the circuit breaker? | `internal/acquirer/guarded.go:do`, `internal/acquirer/breaker.go:Failure` | Signal vs. noise in health detection | A run of legitimately declined cards would open the breaker and take the gateway down while the processor is perfectly healthy |
| Why does an acquirer timeout leave the transaction at `created` instead of `failed`? | `internal/service/payment_service.go:Authorize`, `ErrAcquirerUnavailable` branch | Indeterminate outcomes, the two generals problem | Recording a definite failure for an unknown outcome strands a real hold on the cardholder's card with no record of it |
| Why does the half-open breaker allow exactly one probe? | `internal/acquirer/breaker.go:Allow` (`probeInFlgt`) | Circuit breaker, thundering herd | The whole waiting fleet hits a barely-recovered processor simultaneously and knocks it over again |
| Why is the pre-check run on a copy (`preview := *txn`) rather than on the transaction itself? | `internal/service/payment_service.go:Capture`, `Refund`, `Void` | Command-query separation, mutation-free validation | The advisory check would leave the in-memory object mutated, and the later authoritative check would run against a lie |
| Why is pagination cursor-based rather than `OFFSET`-based? | `internal/repository/transaction_repo.go:List` | Keyset pagination | `OFFSET 500` makes the database walk and discard 500 pages, and a row inserted meanwhile shifts every later page — rows get skipped or repeated |
| Why does `List` fetch `limit + 1` rows? | `internal/transport/http/handler_payment.go:List` | Has-more detection | The alternative is a second `COUNT(*)` over the whole filtered set on every page |
| Why does the reconciliation window filter on `transactions.captured_at` rather than the journal entry's own timestamp? | `internal/repository/ledger_repo.go:AggregateByMerchant` | Accounting date vs. event date | A refund booked today against yesterday's capture is reported as a discrepancy on *both* days |
| Why must reconciliation refuse to settle when the two sides disagree? | `internal/service/reconciliation_service.go:Run` (step 4 → `SettlementSkipped`) | Control totals, stop-the-line | Real money leaves the platform based on numbers nobody has verified, and the error is discovered after the payout |
| Why does the reconcile job exit with code `2`? | `cmd/worker/main.go`, `reconcile` branch | Cron exit-code semantics | A silent zero exit means an unreconciled day is never escalated to a human |
| Why does error→status mapping live in exactly one function? | `internal/transport/http/response.go:mapError` | Single point of truth, pinned by a table test | The same domain error becomes a `400` in one handler and a `500` in another; clients cannot branch reliably |
| Why does `Auth` sit before `RateLimit` and `RateLimit` before `Idempotency` in the chain? | `internal/transport/http/router.go:NewRouter` | Middleware ordering as a dependency graph | The limiter has no merchant identity to key on, and the idempotency middleware has neither the merchant nor the buffered body |
| Why is `/healthz` deliberately not checking the database? | `internal/transport/http/handler_system.go:Healthz` | Liveness vs. readiness | A database blip gets healthy processes killed by the orchestrator instead of letting them recover |
| Why is the full PAN never stored, not even encrypted? | `internal/domain/transaction.go` (type comment), `internal/acquirer/acquirer.go:Card` | PCI-DSS scope reduction | Data you do not store cannot leak or be subpoenaed out of a backup; storing a CVV after authorization is forbidden outright |
| Why `int64` minor units instead of a float or a decimal library? | `internal/money/money.go` package comment | Binary floating point, minor units | `0.1 + 0.2 != 0.3`; the rounding error is not reproducible across languages and a lost cent per transaction destroys trust faster than it destroys money |
| Why does `Fee` truncate instead of rounding to nearest? | `internal/money/money.go:Fee` | Rounding bias | Rounding up takes money from the merchant on every transaction — a systematic bias, not a random one |
| Why does the idempotency key include the merchant id? | `internal/idempotency/store.go:Key` | Tenant isolation of a shared namespace | One merchant can collide with — or probe for — another merchant's keys |
| Why does the worker detach the job context from the shutdown context? | `internal/webhook/dispatcher.go:worker` (`context.WithoutCancel`) | Graceful shutdown, in-flight work | A `SIGTERM` mid-POST leaves a delivery whose outcome nobody knows: possibly delivered, recorded as failed, and then delivered again |
| Why does the poller stop before the workers during shutdown? | `internal/webhook/dispatcher.go:Run` | Shutdown ordering | New rows keep being claimed while the process is dying, and each one is stranded in `delivering` |

---

## Section 5 — The invariants

Each of these is asserted by a test in this repository. The interesting column is the third one.

**1. Debits equal credits within every entry group.**
- Enforced by: `domain.EntryGroup.Validate` (at construction), `LedgerRepo.InsertGroup` (re-validated immediately before the write), `LedgerRepo.FindUnbalancedGroups` (a scan of the entire journal), and the `assertLedgerBalanced` helper that ends every integration scenario.
- **Multiple layers, on purpose.** The constructor protects the normal path; `InsertGroup` protects a caller that assembled a group by hand; `FindUnbalancedGroups` is the only defence against an entry that arrived some other way — which is exactly what `TestReconciliationDetectsAnUnbalancedGroup` simulates by inserting a single-legged group via raw SQL.
- Money goes wrong how: the journal stops being a journal. A balance is a fold over it, so an unbalanced group means someone is owed money that no one is charged for, and no report can ever say who.

**2. The journal is append-only.**
- Enforced by: a `BEFORE UPDATE OR DELETE` trigger in `migrations/00003_create_ledger_entries.sql`, plus the absence of any `Update`/`Delete` method on `LedgerRepo`.
- **Two layers.** Application discipline stops the code; the trigger stops a migration, a support script, or a `psql` session. `TestAuthorizeCaptureRefundFlow` ends by attempting a `DELETE` and asserting it errors.
- Money goes wrong how: history becomes editable, so a wrong number can be *hidden* rather than *corrected*. A correction must be a new opposite group, or the audit trail is fiction.

**3. `captured_amount ≤ amount`.**
- Enforced by: `Transaction.Capture` (`ErrCaptureExceedsAuthlzd`), the `transactions_captured_check` CHECK constraint, and `SELECT … FOR UPDATE` making the domain check meaningful under concurrency.
- **Three layers**, and each catches a different attacker: the domain catches a bad request, the lock catches a race, the CHECK catches a bug in either.
- Money goes wrong how: the platform charges more than the cardholder authorised — a chargeback and, at volume, a compliance problem.
- Asserted by `TestConcurrentPartialCapturesNeverOvercapture`: ten goroutines each try to take 30,000 from a 100,000 authorization, and `captured_amount` must end up exactly `30,000 × (number that reported success)`.

**4. `refunded_amount ≤ captured_amount`.**
- Enforced by: `Transaction.Refund` (`ErrRefundExceedsCaptured`), `transactions_refunded_check`, and the same row lock.
- Money goes wrong how: money is returned that was never taken — a direct loss, and one that reconciliation would only find after the fact.
- Asserted by `TestConcurrentRefundsNeverOverRefund`.

**5. One `(merchant_id, reference)` means one transaction.**
- Enforced by: the `transactions_merchant_reference_key` unique constraint alone. There is deliberately no application-level existence check, because a check-then-act cannot be atomic.
- Money goes wrong how: one customer order becomes two card holds.
- Asserted by `TestConcurrentAuthorizeSameReference` — ten concurrent requests with *different* idempotency keys, so only the database can stop them; exactly one `201`.

**6. A retried money-moving request charges at most once.**
- Enforced by: the `SET NX` claim in `idempotency.Store.Begin`, the fingerprint comparison, and the four branches of the middleware.
- Money goes wrong how: a double-clicked button or a retrying HTTP client charges the cardholder twice.
- Asserted by `TestConcurrentIdenticalRequestsChargeOnce` (8 concurrent identical requests, one transaction) and `TestIdempotentCaptureIsNotDoubleCharged`.

**7. A concurrent double capture produces exactly one entry group.**
- Enforced by: the lock-and-re-check ordering in `PaymentService.Capture`.
- Money goes wrong how: the ledger books the same capture N times while the card was charged once — the balance overstates what the merchant is owed.
- Asserted by `TestConcurrentCaptureWritesLedgerOnce` under `-race`: `CountEntryGroups(...) == 1`, `captured_amount == 100_000`, balance `98_000`.

**8. An unknown acquirer outcome is never recorded as a definite failure.**
- Enforced by: the `ErrAcquirerUnavailable` branch of `Authorize`, which returns without transitioning, and the 5xx key release in the idempotency middleware.
- Money goes wrong how: a hold that actually exists at the processor has no record on our side, so it is never voided and never captured — the cardholder's funds stay frozen.
- Asserted by `TestIdempotencyReleasesKeyAfterServerError`: after a timeout the row is still `created`, no journal lines exist, no cached answer remains, and a retry resumes the same transaction rather than creating a second one.

**9. Card data never leaves the request.**
- Enforced by: `Transaction` having no PAN field, `acquirer.Card` being confined to the request struct, and the webhook payload builder.
- Money goes wrong how: not money — scope. A stored PAN drags the database into the PCI audit boundary and makes any dump catastrophic.
- Asserted by `TestWebhookIsDeliveredAndVerifiable` (the delivered body contains neither the PAN nor `cvv`), `TestBuildEventPayloadOmitsCardData`, and `TestValidSignatureIsAccepted` (no response carries a secret).

**10. Settlement happens only on a day whose two records agree.**
- Enforced by: `Report.OK()` gating the call to `settle`, plus the per-transaction re-lock and status re-check inside `settle`.
- Money goes wrong how: a real payout is made from unverified numbers.
- Asserted by `TestReconciliationDetectsATamperedLedger` (`SettledCount == 0`, `SettlementSkipped == true`) and `TestReconciliationIsRepeatable` (a second run settles nothing).

---

## Section 6 — Concurrency, four timelines

### 6.1 `SELECT … FOR UPDATE` — two captures of the same transaction

Setup: transaction authorized for 100,000, `captured_amount = 0`, `version = 1`.

| T1 | T2 |
|---|---|
| `GetByMerchantAndID` (no lock): sees `captured_amount = 0` | |
| pre-check on the copy: 100,000 ≤ 100,000, OK | `GetByMerchantAndID` (no lock): also sees `captured_amount = 0` |
| acquirer `Capture(100_000)` — no lock held | pre-check passes too; acquirer `Capture(100_000)` — no lock held |
| `BEGIN`; `SELECT … FOR UPDATE` → **lock acquired** | `BEGIN`; `SELECT … FOR UPDATE` → **blocks here** |
| `fresh.Capture(100_000)` on the fresh row: OK | *(still blocked)* |
| `UPDATE … version = 1 → 2`; `INSERT` 3 ledger rows; `INSERT` outbox row | *(still blocked)* |
| `COMMIT` → **lock released** | |
| | `SELECT … FOR UPDATE` returns the *post-commit* row: `captured_amount = 100_000`, `version = 2` |
| | `fresh.Capture(100_000)` → `RemainingCapturable() == 0` → `ErrCaptureExceedsAuthlzd` |
| | `ROLLBACK`; ERROR log naming the acquirer ref; `422` to the client |

**Remove the lock and this line breaks:** `internal/service/payment_service.go:Capture`, the `fresh.Capture(amount, now)` double-check. Without `FOR UPDATE`, `GetForUpdate` degrades into a plain read and T2 reads the *pre-commit* snapshot — `captured_amount = 0` — so its check passes. Both transactions then insert a capture entry group: `CountEntryGroups` returns 2 and `TestConcurrentCaptureWritesLedgerOnce` fails on its decisive assertion. The `version` guard would still stop the second `UPDATE` (T2 holds `version = 1`, the row is at 2), so `captured_amount` stays correct — but the ledger insert happens *before* the update returns in program order only for the transaction that got there first; in the losing transaction the whole tx rolls back on the version error. The real damage is therefore reduced to a burned acquirer capture, which is precisely why the version column is called belt-and-braces and not a replacement.

### 6.2 The `version` column — a writer that skips the lock

Every writer in the current code takes `GetForUpdate` first (six call sites in `internal/service/`), so no existing test fails if you delete the version guard. This timeline is what the guard exists for: a future writer — a support script, a migration, a new endpoint — that reads without locking.

| T1 (correct writer) | T2 (reads without a lock) |
|---|---|
| | `GetByID` (no lock): `captured_amount = 0`, `version = 1` |
| `BEGIN`; `SELECT … FOR UPDATE` — nothing to wait for, T2 holds no lock | |
| `fresh.Capture(100_000)`; `UPDATE … WHERE id = $1 AND version = 1` → `version = 2` | |
| `INSERT` ledger group; `COMMIT` | |
| | `UPDATE … WHERE id = $1 AND version = 1` → **0 rows** |
| | `pgx.ErrNoRows` → `ErrConcurrentModification` → `409` |

**Remove the version guard and this line breaks:** `internal/repository/transaction_repo.go:Update` — the `AND version = $12` predicate and its `RETURNING`/`ErrNoRows` handling. T2's `UPDATE` would succeed and write back its stale in-memory copy, silently resetting `captured_amount` to 0 while the ledger still holds the capture entries. That is a classic lost update, and reconciliation would catch it the next day as a `captured_total` discrepancy — a day late, after the money has been reported.

### 6.3 `UNIQUE (merchant_id, reference)` — the same order submitted twice

Setup: ten concurrent `POST /payments` with the same `reference` and **ten different** `Idempotency-Key` values, so the middleware deduplicates nothing.

| T1 | T2 |
|---|---|
| idempotency: `SET NX` on key A → won | idempotency: `SET NX` on key B → won (different key) |
| `NewTransaction` validates | `NewTransaction` validates |
| `INSERT INTO transactions …` → row created | `INSERT INTO transactions …` → **blocks on the unique index** |
| acquirer `Authorize` (no lock) | *(still blocked — the index entry is uncommitted)* |
| `BEGIN`; lock; `UPDATE → authorized`; outbox; `COMMIT` | |
| | `INSERT` fails, `SQLSTATE 23505`, constraint `transactions_merchant_reference_key` |
| | mapped to `ErrDuplicateReference`; `GetByReference` shows status `authorized`, not `created`, so this is not a resumable retry → `409 duplicate_reference` |

**Remove the constraint and this line breaks:** `internal/repository/transaction_repo.go:Create`, the `isUniqueViolation` branch — it can never fire. `TestConcurrentAuthorizeSameReference` asserts exactly one `201` and `COUNT(*) == 1`; without the constraint all ten insert, all ten call the acquirer, and one customer order becomes ten card holds. Note what an application-level "does a row with this reference exist?" check would do here: all ten read "no" before any of them writes. Only the index is atomic.

### 6.4 `FOR UPDATE SKIP LOCKED` — two worker processes draining one queue

Setup: four due rows in `webhook_deliveries`; two worker processes poll on the same tick.

| Worker A | Worker B |
|---|---|
| `ClaimDue`: CTE selects rows 1–4 `FOR UPDATE SKIP LOCKED`, locks 1–4 | |
| `UPDATE … SET status = 'delivering'`, `RETURNING` rows 1–4 | `ClaimDue` runs concurrently: rows 1–4 are locked → **skipped**, returns empty |
| statement ends → locks released; rows are now `delivering`, so they no longer match `status IN ('pending','failed')` | next tick: `ClaimDue` returns empty again, and B stays available for genuinely new rows |
| POSTs row 1; endpoint returns 500 | |
| `RecordFailure`: `attempt_count = 1`, `status = 'failed'`, `next_attempt_at = now + 2^1 s ± 20%` | |
| | a later tick, after `next_attempt_at`: B claims row 1 and delivers it |

**Remove `SKIP LOCKED` and this line breaks:** `internal/repository/webhook_repo.go:ClaimDue`. With plain `FOR UPDATE`, Worker B's `SELECT` blocks on row 1 until A's statement finishes, then wakes, re-evaluates the predicate, finds `status = 'delivering'` and returns nothing — so B spent the whole time waiting to learn there was no work. Every additional worker adds latency instead of throughput, and the `Workers × 2` batch size in `dispatcher.poll` stops meaning anything. Now remove the `status = 'delivering'` flip as well and it gets worse: both workers claim the same row on the same tick and the merchant receives the same webhook twice.

Separately, if `ReleaseStale` did not exist, a worker `SIGKILL`ed between the claim and the POST leaves its rows at `delivering` forever — matching neither the due predicate nor the delivered state. `TestWebhookSurvivesWorkerRestart` asserts that no row is left in `delivering` after a worker stops.

---

## Section 7 — Failures and how they are handled

The third column is the argument, not the description.

| Failure | What the system does | Why not the opposite |
|---|---|---|
| Card declined | `402` with the acquirer's reason code; transaction → `failed`; no ledger entries; `payment.failed` queued | Treating a decline as a server error would retry a card the issuer already refused, and would trip the circuit breaker on a healthy processor. A decline is a *successful* call with a negative answer |
| Acquirer times out on authorize | `503`; row stays `created`; idempotency key released; ERROR logged | Marking it `failed` asserts something unknown. If the hold does exist, nothing will ever void or capture it and the cardholder's funds stay frozen. `created` keeps the row visible to both a retry and the reconciliation job |
| Acquirer times out on capture/refund | The error propagates; no ledger entries; no state change | Booking optimistically would put money in the journal that may not exist at the processor — the exact drift reconciliation is designed to detect, deliberately created by us |
| Acquirer succeeded but the in-lock double-check failed | `ROLLBACK`; ERROR log carrying the acquirer reference, amount and current status; reconciliation is the safety net | Committing anyway would break the state machine to make the books match a call we should never have made. The honest position is: the gap is real, it is logged with enough detail to resolve, and a real gateway resolves it against the processor's settlement file |
| Repeated infrastructure failures at the acquirer | Breaker opens after 5 consecutive failures; fails fast for 30s; then one probe | Without it every request still pays the full 3s timeout while the processor is down, so goroutines and pooled connections pile up — a downstream outage becomes ours |
| Many cards declined in a row | Breaker stays closed (`Guarded.do` calls `Success()` on a decline) | Opening here would let a merchant with bad traffic — or a fraud test — take the gateway offline for every other merchant |
| Redis down, rate limiting | **Fails open**: logged, request proceeds | Refusing real money movement to protect against a load problem we are not currently having trades a certain loss for a hypothetical one. The bounded downside is a temporarily unthrottled merchant |
| Redis down, idempotency | **Fails closed**: `500` | The opposite asymmetry, and the reason is the same kind of reasoning applied to a different payoff. Proceeding without deduplication risks double charges, which are worse than a brief outage and much harder to unwind |
| Idempotency key reused with a different body | `422 idempotency_key_reuse`, handler never runs | Guessing which body was meant either duplicates a charge or silently discards a real payment. Refusing loudly turns a client bug into a fixable error message |
| Second request with the same key still in flight | `409 request_in_progress` | Blocking until the first finishes holds a goroutine and a connection per duplicate; a double-click storm becomes a resource outage |
| Handler panics | `Recoverer` returns `500`; the idempotency middleware releases the key before re-panicking; stack logged | Leaving the key claimed would pin an unknown outcome for 24 hours. Note the ordering: the release happens in the idempotency middleware's own `defer`, *before* the panic reaches `Recoverer` |
| Worker killed mid-delivery | In-flight job finishes on a detached context; unstarted claims are handed back; anything stranded is released after 5 minutes | Cancelling the in-flight POST creates the worst case: the merchant may have received it, and we record it as failed and send it again |
| Merchant endpoint keeps failing | Retry with `2^n` seconds ± 20% jitter, up to `WEBHOOK_MAX_ATTEMPTS`, then `dead` | Infinite retries turn one broken endpoint into a permanent load source; no jitter makes a fleet of failures retry in lockstep and hammer the endpoint the moment it recovers |
| Merchant has no `webhook_url` | Parked as `dead` immediately | Six attempts against an address that does not exist is pure waste, and it buries real failures in the same queue |
| Reconciliation finds a discrepancy | Every discrepancy and every transaction of the affected merchant is logged; the report is still written; **settlement is skipped**; exit code `2` | Settling anyway pays out real money on numbers nobody verified. Skipping *quietly* is just as bad — the non-zero exit is what makes an unreconciled day reach a human |
| An entry group does not balance | Blocks settlement even when the daily totals happen to agree | The totals matching is a weaker property than the invariant holding. If the invariant is broken, the totals matching is a coincidence, not evidence |
| A merchant asks for another merchant's payment | `404`, not `403` | A `403` confirms the id exists, which is an enumeration oracle. Every read is scoped by `merchant_id` in the SQL itself (`GetByMerchantAndID`), not filtered afterwards |
| Unmapped internal error | `500` with a generic message and a request id | Echoing the internal message leaks table names, constraint names and query structure. The request id is what makes support possible without leaking |

---

## Section 8 — Background concepts worth learning

Priorities: **Must know** — you cannot read the code without it. **Should know** — you cannot defend the decision without it. **Nice to have** — it deepens the answer.

### Payments domain

| Concept | One-line definition | Live example in this repo | Priority |
|---|---|---|---|
| Authorize / capture / void / refund | The four card operations: hold funds, take them, release the hold, give them back | `internal/domain/transaction.go` — the four methods and `allowedTransitions` | Must know |
| Double-entry bookkeeping | Every event writes balanced debits and credits; a balance is derived, never stored | `internal/domain/ledger.go:NewCaptureEntryGroup` | Must know |
| Chart of accounts | The fixed set of accounts an event may touch, and what each one means | `internal/domain/ledger.go` constants; the table in `docs/architecture.md` | Must know |
| Minor units | Money as an exact integer count of the smallest unit, never a float | `internal/money/money.go` | Must know |
| Partial capture / partial refund | Taking or returning less than the full amount, without ending the lifecycle | `Transaction.Capture` and `Transaction.Refund` — note that only a *full* refund is terminal | Should know |
| Settlement and payout | Discharging the liability to the merchant and moving cash out | `internal/service/reconciliation_service.go:settle`, `NewSettlementEntryGroup` | Should know |
| Reconciliation / control totals | Deriving the same figure two independent ways and refusing to proceed if they differ | `internal/service/reconciliation_service.go:Run`, steps 1–4 | Should know |
| Basis points | 1 bps = 0.01%; the standard unit for platform fees | `money.Fee`, `PLATFORM_FEE_BPS` (default 200 = 2%) | Nice to have |
| Luhn checksum | The mod-10 check every card number satisfies | `internal/acquirer/mock.go:luhn` | Nice to have |
| Authorization validity window | Card networks expire holds after roughly a week | `domain.AuthorizationValidity` = 7 days | Nice to have |

### Distributed systems

| Concept | One-line definition | Live example in this repo | Priority |
|---|---|---|---|
| Idempotency keys | A client-supplied token making a retry safe to execute | `internal/idempotency/store.go`, `internal/transport/http/middleware_idem.go` | Must know |
| Transactional outbox | Writing the "send this message" row in the same transaction as the state change | `PaymentService.queueWebhook` + `WebhookRepo.Create` | Must know |
| The dual-write problem | Two systems written outside one transaction can always disagree | The problem the outbox above solves; the acquirer call is the case that *cannot* be solved this way | Must know |
| Indeterminate outcomes | A timeout says nothing about whether the remote side acted | The `ErrAcquirerUnavailable` branch of `Authorize` | Must know |
| At-least-once delivery | Retried delivery means the receiver must tolerate duplicates | `Dispatcher.deliver` retry loop; the merchant is expected to dedupe on `X-Webhook-Id` | Should know |
| Circuit breaker | Fail fast during a sustained outage instead of paying the timeout every time | `internal/acquirer/breaker.go` | Should know |
| Exponential backoff with jitter | Growing retry delays, randomised so a fleet does not retry in lockstep | `domain.BackoffDelay` — `2^n` seconds ±20% | Should know |
| Fail open vs. fail closed | Which way a dependency failure should tip, decided per dependency | Rate limiter vs. idempotency store — see Section 7 | Should know |
| Graceful shutdown | Stop intake, drain in-flight work, then exit | `Dispatcher.Run` ordering; `server.Shutdown` in `cmd/api/main.go` | Should know |
| Lease / stale claim recovery | A claim that expires so a crashed consumer's work returns to the queue | `WebhookRepo.ReleaseStale`, `staleClaimTimeout` | Should know |
| Backpressure | Letting a slow consumer slow the producer rather than buffering without bound | The unbuffered `jobs` channel in `Dispatcher.Run` | Nice to have |
| Liveness vs. readiness | "Is the process alive" and "can it serve traffic" are different questions | `internal/transport/http/handler_system.go:Healthz` vs. `Readyz` | Nice to have |

### Postgres

| Concept | One-line definition | Live example in this repo | Priority |
|---|---|---|---|
| `SELECT … FOR UPDATE` | A pessimistic row lock held until the surrounding transaction ends | `TransactionRepo.GetForUpdate` | Must know |
| Optimistic locking | A version column that makes a stale write fail instead of silently winning | `TransactionRepo.Update`, `AND version = $12` | Must know |
| Unique constraints as concurrency control | Letting the index decide, because check-then-act cannot be atomic | `transactions_merchant_reference_key` | Must know |
| `FOR UPDATE SKIP LOCKED` | Step over rows another session holds instead of queueing behind them | `WebhookRepo.ClaimDue` | Must know |
| CHECK constraints | Invariants the database enforces regardless of which client writes | `transactions_captured_check`, `transactions_refunded_check`, `ledger_amount_check` | Should know |
| Triggers for immutability | Blocking `UPDATE`/`DELETE` at the storage layer | `ledger_entries_no_mutation` in migration 00003 | Should know |
| SQLSTATE codes | Reacting to `23505`/`23514` by code rather than by matching error strings | `internal/repository/postgres.go:isUniqueViolation`, `isCheckViolation` | Should know |
| Keyset (cursor) pagination | Paging by comparing a `(created_at, id)` tuple against an index, not by `OFFSET` | `TransactionRepo.List` | Should know |
| Partial indexes | An index over only the rows a query cares about, so it stays small as the table grows | `idx_webhook_due … WHERE status IN ('pending','failed')` | Should know |
| Aggregate `FILTER` | Conditional aggregation in one pass | `LedgerRepo.Balance`, `AggregateByMerchant` | Nice to have |
| Reading `EXPLAIN (ANALYZE, BUFFERS)` | Confirming that an index is actually used, and that the fixture is representative | The measurement section of `docs/architecture.md` | Nice to have |
| Index selectivity | An index matching most rows is worse than a sequential scan | "What the numbers actually say" in `docs/architecture.md` | Nice to have |

### Go

| Concept | One-line definition | Live example in this repo | Priority |
|---|---|---|---|
| Error wrapping and sentinels | `%w` plus `errors.Is`/`errors.As` to inspect an error without string matching | `internal/domain/errors.go`, `internal/transport/http/response.go:mapError` | Must know |
| `context.Context` | Cancellation and deadlines threaded through every call | `Guarded.do`'s `WithTimeout`; the request context everywhere | Must know |
| Interfaces declared at the point of use | The consumer states what it needs; the producer does not export an interface | `transport/http.Authenticator`, `IdempotencyStore`, `RateLimiter` | Must know |
| `net/http` middleware | A `func(http.Handler) http.Handler` chain, where order encodes dependencies | `internal/transport/http/router.go` | Must know |
| `context.WithoutCancel` | Detaching work that must finish even though the parent was cancelled | `Dispatcher.worker`, `releaseKey`, `WithTx`'s rollback | Should know |
| `defer` + `recover` | Recovering a panic at exactly one boundary, and re-panicking after cleanup | `Recoverer`; the release-then-repanic in `internal/transport/http/middleware_idem.go` | Should know |
| `sync.WaitGroup` and channel fan-out | A poller feeding N workers, with a shutdown that waits for them | `Dispatcher.Run` | Should know |
| The race detector | `go test -race` catching unsynchronised access | `make test`; `TestBreakerIsRaceFree`, the concurrency suite | Should know |
| `log/slog` | Structured logging with a request-scoped logger in the context | `internal/transport/http/middleware_log.go:Logger`, `LoggerFrom` | Should know |
| Injecting the clock | A `now func() time.Time` field so time-dependent logic is testable | `PaymentService.now`, `Breaker.now` and the `fakeClock` in `internal/acquirer/breaker_test.go` | Should know |
| Table-driven tests | One test walking a matrix of cases | `TestAllowedTransitions` (49 pairs), `TestMapError` | Should know |
| `embed.FS` | Compiling the `.sql` files into the binary | `migrations/embed.go` | Nice to have |
| Build tags | Keeping the integration suite out of the default `go test ./...` | `//go:build integration` | Nice to have |

### Security

| Concept | One-line definition | Live example in this repo | Priority |
|---|---|---|---|
| HMAC request signing | Proving authenticity and integrity with a shared secret, not just a bearer token | `service.ComputeRequestSignature`, `MerchantService.Authenticate` | Must know |
| Canonical, delimited signed strings | Removing ambiguity so one signature cannot describe two requests | The `\n` separators, proven necessary by `TestComputeRequestSignatureIsUnambiguous` | Must know |
| Replay protection | Binding a signature to a moment in time and to one method and path | The clock-skew window; `t` inside the webhook signed string | Must know |
| Constant-time comparison | Comparing secrets without leaking how many bytes matched, via timing | `hmac.Equal` in `Authenticate`, `webhook.Verify`; `subtle.ConstantTimeCompare` in `AdminAuth` | Must know |
| Encryption at rest vs. hashing | Encrypt what you must recover; hash what you only need to verify | `internal/secrets/cipher.go` package comment | Must know |
| AES-256-GCM | Authenticated encryption: a tampered ciphertext fails the tag check rather than decrypting to garbage | `secrets.Cipher.Encrypt` / `Decrypt`, `TestDecryptRejectsTamperedCiphertext` | Should know |
| Show-once credentials | A secret returned exactly once and never retrievable afterwards | `MerchantService.Create`; `TestCreateMerchantReturnsSecretsOnce` | Should know |
| PCI-DSS scope reduction | Not storing what you do not need, so it cannot leak or be subpoenaed | No PAN/CVV field anywhere; only `card_last4` and `card_brand` | Should know |
| Tenant isolation in queries | Scoping by owner in the SQL itself rather than filtering after the read | `GetByMerchantAndID`; `TestMerchantCannotReadAnotherMerchantsPayment` | Should know |
| Not leaking existence | Answering `404` where `403` would confirm a resource exists | The same test as above | Should know |
| Request size limits | Bounding what an unauthenticated caller can make you buffer | `http.MaxBytesReader` in `readAndRestoreBody` | Nice to have |
| Keys outside the database | An encryption key in the environment (a KMS in production), never in the table it protects | `SECRET_ENC_KEY` in `config.Load` | Nice to have |

---

## Where to distrust this document

Three places where the code is subtler than any prose about it, and you should read the source directly:

1. **`PaymentService.Authorize`'s duplicate-reference resume branch.** Whether a failed `INSERT` is a retry to resume or a genuine duplicate depends on three conditions checked in sequence — read `internal/service/payment_service.go:Authorize` rather than trusting the summary.
2. **The interaction between the idempotency middleware's `defer`/`recover` and `Recoverer`.** The ordering of release, re-panic and response writing is only obvious from the code.
3. **`ClaimDue`'s CTE.** Which rows are locked, when the lock is released, and why the `UPDATE` must be in the same statement is a property of that specific SQL, not of `SKIP LOCKED` in general.

Test yourself: [docs/learning-quiz.md](learning-quiz.md).
