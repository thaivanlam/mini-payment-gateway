# Learning quiz

Twenty questions, easiest first, five in each of four groups: state machine, idempotency, ledger, concurrency and failure. None of them asks you to recall a function name or a column name — they ask what happens, and why the alternative was rejected.

Answers are hidden so you can read straight through without spoiling them. Companion to [docs/learning-path.md](learning-path.md).

---

## State machine

**Question 1.** A merchant authorizes 100,000 and then captures 40,000. What is the transaction's status, and what happens if they capture another 40,000?

<details><summary>Answer</summary>

The status is `captured` after the first partial capture, and the second capture succeeds — `captured_amount` becomes 80,000 while the status stays `captured`.

`internal/domain/transaction.go:Capture` transitions `authorized → captured` only on the *first* capture; after that it simply grows `CapturedAmount`. It accepts a capture when the status is either `authorized` or `captured`, which is what makes repeated partial captures possible without a self-transition in `allowedTransitions`. Verified by `TestPartialCaptures` in `test/integration/payment_flow_test.go`.
</details>

**Question 2.** Why can a partially captured transaction not be voided?

<details><summary>Answer</summary>

Because money has already moved, and a void is defined as releasing a hold that was never taken. `Transaction.Void` refuses outright when `CapturedAmount > 0`, before even consulting the state machine — the operation for returning money that was captured is a refund, which books ledger entries. A void books none. Allowing it would mean a state change that silently discards captured funds with no journal entry to explain it.
</details>

**Question 3.** What happens if you refund 50,000 of a 150,000 capture, and why is the status not `refunded`?

<details><summary>Answer</summary>

`refunded_amount` becomes 50,000 and the status stays `captured`. Only a refund that brings `refunded_amount` up to `captured_amount` reaches the terminal `refunded` state.

The reasoning is in the doc comment on `Transaction.Refund`: if a partial refund also produced status `refunded`, then "refunded" would mean two different things — "some money came back" and "this transaction is finished" — and no consumer of the API could tell them apart. `TestAuthorizeCaptureRefundFlow` asserts both halves.
</details>

**Question 4.** Eight days after authorizing, a merchant tries to capture. What do they get, and why does this rule exist in the domain rather than in the handler?

<details><summary>Answer</summary>

`409 authorization_expired`. `Transaction.Capture` calls `AuthorizationExpired(now)`, which compares against `AuthorizedAt + AuthorizationValidity` (7 days), mirroring how card networks expire holds.

It lives in the domain because the rule is about the transaction, not about HTTP: the same check has to fire for the auto-capture path inside `Authorize`, for any future internal caller, and for the pre-check and the in-lock double-check alike. A handler-level check would be bypassed by all of those. Note that this is one rule *not* duplicated as a database constraint — it depends on `now`, which a CHECK constraint cannot see.
</details>

**Question 5.** Why is there no `TransitionTo` call anywhere outside `internal/domain/transaction.go`, and what would go wrong if one handler assigned `txn.Status = "captured"` directly?

<details><summary>Answer</summary>

`allowedTransitions` is the single source of truth, and `TestAllowedTransitions` walks all 49 (from, to) pairs to pin it. A direct assignment bypasses the matrix entirely.

Concretely: it would let a `failed` or `voided` transaction — both terminal — become `captured`, so a declined card would produce capture ledger entries. The state machine test would still pass, because it tests the domain method, not the handler; the bug would surface as a reconciliation discrepancy or a chargeback. The rule "every status change goes through `TransitionTo`" is what makes one passing test cover the whole codebase.
</details>

---

## Idempotency

**Question 6.** A merchant sends the same request with the same `Idempotency-Key` twice, the first having completed. Does the handler run the second time?

<details><summary>Answer</summary>

No. `store.Begin` loses the `SET NX` race, reads the stored record, sees state `completed` and a matching fingerprint, and the middleware writes the stored status code and body back verbatim with `Idempotent-Replay: true` — `next.ServeHTTP` is never called.

`TestIdempotentRetryReplaysAndChargesOnce` asserts that the replayed body is identical and that only one transaction row exists.
</details>

**Question 7.** Two requests with the same `Idempotency-Key` arrive at the same instant and the first has not finished. What does the second receive, and why not simply wait for the first?

<details><summary>Answer</summary>

`409 request_in_progress` (`idempotency.ErrInFlight`, mapped in `internal/transport/http/response.go`).

Waiting would hold a server goroutine, a connection and possibly a pooled database connection for the entire duration of the first request — which includes a card-processor call of up to three seconds. A double-clicked checkout button, multiplied across merchants, turns that into resource exhaustion. Answering immediately with a retryable status pushes the wait to the client, where it costs nothing. `TestIdempotencyInFlightRequestIsRejected` covers it.
</details>

**Question 8.** Why does a `422 idempotency_key_reuse` exist at all? What is wrong with just replaying the stored response when the key matches?

<details><summary>Answer</summary>

Because "same key" would then mean "return whatever I returned last time", regardless of what was actually asked.

A client that reuses a key by accident — a fixed value in a config file, a UUID generated once per process — would receive a completely unrelated payment's response, and its genuine second payment would never be created. Neither side would notice until the merchant's books disagreed. The fingerprint (a SHA-256 of the canonicalised body) is what makes "same key, different request" detectable, and refusing it loudly turns a silent data-loss bug into an error message. `TestIdempotencyKeyReuseWithDifferentBody`.
</details>

**Question 9.** Why is the request body canonicalised — JSON re-parsed and re-encoded with sorted keys — before being hashed, rather than hashing the raw bytes?

<details><summary>Answer</summary>

Because HTTP clients, SDKs and proxies legitimately reorder JSON object keys and change whitespace between a request and its retry. Hashing raw bytes would classify a genuine retry as a different body and return `422`, blocking exactly the case idempotency exists to serve.

`canonicalize` in `internal/idempotency/store.go` unmarshals into `any` and re-marshals (Go's `encoding/json` sorts map keys), recursing through nested objects and arrays. Non-JSON bodies fall back to hashing the raw bytes. `TestFingerprintIsStableAcrossFormatting` and `TestFingerprintDetectsRealDifferences` pin both directions.
</details>

**Question 10.** The acquirer times out and the API answers `503`. Why is the idempotency key deleted, when a `402` decline on the very same endpoint is cached and replayed?

<details><summary>Answer</summary>

Because the two answers differ in kind, not in severity.

A decline is a *definite* answer from a healthy processor about that card. It is a real result and caching it is correct — a retry gets the same refusal without bothering the issuer again (`TestIdempotencyCachesDefiniteFailures`).

A timeout is an *indeterminate* outcome: the hold may or may not exist. Caching that for the 24-hour TTL would leave the merchant permanently unable to complete this payment — every retry would replay the same 503. So the middleware releases the key on any status ≥ 500 and on a panic, and the retry gets a real attempt. `TestIdempotencyReleasesKeyAfterServerError` additionally checks that the retry *resumes* the still-`created` transaction rather than creating a second one.
</details>

---

## Ledger

**Question 11.** A capture of 100,000 with a 2% platform fee. What is written to the journal, and what is the merchant's balance afterwards?

<details><summary>Answer</summary>

Three lines in one entry group: `debit acquirer_receivable 100,000`, `credit merchant_payable:<id> 98,000`, `credit platform_fee_revenue 2,000`. Debits (100,000) equal credits (98,000 + 2,000).

The balance is 98,000, and it is not stored anywhere — `LedgerRepo.Balance` computes `SUM(credit) − SUM(debit)` over `merchant_payable:<id>`. If the fee were zero, the group would have two legs instead of three (`TestNewCaptureEntryGroupWithoutFee`).
</details>

**Question 12.** You fully refund that 100,000 capture. What is the merchant's balance now, and why is it not zero?

<details><summary>Answer</summary>

It is **−2,000**, and that is correct.

The refund books only two legs — `debit merchant_payable 100,000`, `credit acquirer_receivable 100,000` — because the platform fee is not returned: the processing genuinely happened. So the merchant was credited 98,000 and debited 100,000, leaving it owing the platform the 2,000 fee it already earned. This is exactly why a refund is not the mirror image of a capture. `TestAuthorizeCaptureRefundFlow` asserts `−3,000` for the same shape at 150,000.

`NewRefundEntryGroup` names this as a policy decision, and the README's "what I would do next" lists making it a switch.
</details>

**Question 13.** Why is a balance recomputed from the journal on every read instead of being kept in a column that each capture updates?

<details><summary>Answer</summary>

Because a column answers *how much* and destroys *why*. `UPDATE balance SET amount = amount + 98000` discards the explanation the moment it runs, so a number that turns out to be wrong is untraceable and a correction is a rewrite of history rather than a new, auditable event.

With a journal, every figure folds back to the entry group that produced it, corrections are new opposite groups, and reconciliation is possible at all. The cost is real and acknowledged: reading a balance is an aggregate, not a column read. At this scale it is one index scan; past a few million entries the stated fix is a materialised view refreshed at settlement — still not a mutable balance.
</details>

**Question 14.** You connect with `psql` and run `UPDATE ledger_entries SET amount = 1 WHERE id = 5`. What happens, and why is application-level discipline not considered enough?

<details><summary>Answer</summary>

It raises an exception. `migrations/00003_create_ledger_entries.sql` installs a `BEFORE UPDATE OR DELETE … FOR EACH ROW` trigger that unconditionally raises `ledger_entries is append-only`.

Application discipline only binds code that goes through `LedgerRepo` — which deliberately has no `Update` and no `Delete`. It does not bind a migration, a support script, an ops one-liner, or a future service written by someone who did not read this document. An audit trail is only as strong as its weakest writer, so the guarantee is placed where every writer must pass. `TestAuthorizeCaptureRefundFlow` ends by asserting that a `DELETE` errors.

(`TRUNCATE` still works, because it does not fire row triggers — which is what lets the integration tests reset between scenarios.)
</details>

**Question 15.** Reconciliation finds that the day's captured total from `transactions` is 100,000 while the journal says 112,345. What does the job do, and why does it not simply settle the smaller figure?

<details><summary>Answer</summary>

It records a `Discrepancy`, logs every transaction belonging to the affected merchant, sets `SettlementSkipped`, writes the report anyway, settles **nothing**, and `cmd/worker` exits with code `2`.

Settling the smaller figure would be picking a number without knowing which record is wrong — and settlement pays real money out and moves transactions to a terminal-ish state. The disagreement itself is the finding: the two records are produced by the same code path, so if they differ, something is wrong that nobody understands yet. The non-zero exit is what turns that into a human's problem rather than a line in yesterday's log. `TestReconciliationDetectsATamperedLedger` asserts `SettledCount == 0`.
</details>

---

## Concurrency and failure

**Question 16.** Ten goroutines capture the same 100,000 authorization simultaneously. How many succeed, how many entry groups are written, and where exactly do the other nine fail?

<details><summary>Answer</summary>

One succeeds; one entry group is written. All ten pass the unlocked pre-check and all ten call the acquirer — the pre-check is explicitly advisory. They then all reach `db.WithTx`, where `SELECT … FOR UPDATE` serialises them one at a time.

The winner captures and commits. Each loser's `SELECT … FOR UPDATE` returns the row *as it now is*, with `captured_amount = 100,000`, so the in-lock `fresh.Capture(...)` fails with `ErrCaptureExceedsAuthlzd` (or `ErrInvalidStateTransition` / `ErrConcurrentModification`) and its transaction rolls back. `TestConcurrentCaptureWritesLedgerOnce` asserts exactly one success and `CountEntryGroups == 1`, under `-race`.
</details>

**Question 17.** Why is the card processor called *before* the row lock is taken rather than inside the same transaction, given that this creates a window where the row can change?

<details><summary>Answer</summary>

Because the call is network I/O with a three-second timeout. Holding a row lock and a pooled connection across it means one slow issuer serialises every other request touching that row and drains the connection pool — that is how a slow dependency becomes an outage.

The window it creates is handled, not ignored: the same domain rule is re-applied to the freshly locked row, so the pre-check being stale is harmless. What remains is one honest gap — the acquirer succeeded and the double-check then failed, so money moved at the processor but not in our books. That case is logged at ERROR with the acquirer reference, and daily reconciliation is the safety net. A real gateway resolves it against the processor's settlement file, which is why reconciliation exists rather than being an afterthought.
</details>

**Question 18.** Ten concurrent `POST /payments` use the same `reference` but ten *different* idempotency keys. What stops ten card holds, and why would an application-level "does this reference exist?" check not work?

<details><summary>Answer</summary>

The `UNIQUE (merchant_id, reference)` constraint, and nothing else — the idempotency middleware deduplicates nothing here, because the keys differ.

An application check cannot work because check-then-act is not atomic: all ten read "no such reference" before any of them writes, and all ten proceed. Only the unique index makes the decision at a single point in time. `TransactionRepo.Create` catches SQLSTATE `23505` on that specific constraint name and maps it to `ErrDuplicateReference`; the service then decides whether it is a resumable retry or a real duplicate. `TestConcurrentAuthorizeSameReference` asserts exactly one `201` and one row.
</details>

**Question 19.** Redis goes down. What happens to a `POST /payments`, and why do the rate limiter and the idempotency store behave differently?

<details><summary>Answer</summary>

The rate limiter **fails open** — the error is logged and the request proceeds. The idempotency store **fails closed** — the error becomes a `500`, so the request never reaches the handler.

The asymmetry is deliberate and is about which failure is worse. Failing closed on the limiter would refuse real money movement to protect against a load problem that is not currently happening: a certain loss traded for a hypothetical one, with the bounded downside of a temporarily unthrottled merchant. Failing open on idempotency would run money-moving handlers with no deduplication at all, which risks double charges — worse than a brief outage and far harder to unwind afterwards.

Since the limiter sits before the idempotency middleware in the chain, a Redis outage means the request passes the limiter and then fails at the idempotency claim.
</details>

**Question 20.** A webhook worker is `SIGKILL`ed after claiming three deliveries but before POSTing any of them. What happens to those three rows, and how would the answer differ on a graceful `SIGTERM`?

<details><summary>Answer</summary>

On `SIGKILL` the rows sit at `delivering`, matching neither the due predicate (`status IN ('pending','failed')`) nor a finished state. Nothing retries them until a poller — any worker's — runs `ReleaseStale`, which flips anything in `delivering` whose `updated_at` is older than five minutes back to `failed` with `next_attempt_at = now`. They are then claimed and delivered normally. Without that janitor the rows would be stranded forever, and "kill the worker, lose no job" would be aspirational.

On `SIGTERM` the shutdown path handles it directly: the poller stops first so no new rows are claimed, any row it is holding when the context cancels is handed straight back via `release`, the `jobs` channel closes, and workers finish what they already hold on a context detached with `context.WithoutCancel` — bounded by the HTTP client timeout and the overall `ShutdownTimeout`. That detachment matters: cancelling a POST already in flight produces the worst case, where the merchant may have received the callback while we record it as failed and send it again. `TestWebhookSurvivesWorkerRestart` asserts that no row is left in `delivering` after a worker stops.
</details>
