# Code reading order — function by function

[docs/learning-path.md](learning-path.md) tells you which **files** to open and in what order. This document goes one level down: **which functions to read, in which order, and which functions you must already understand before each one makes sense.**

The order is neither alphabetical nor top-to-bottom within a file. It follows one rule: never read a function whose callees you have not read yet. Every function below is reached only after everything it depends on is already in your head, so you are never guessing what a call does.

Read it with the repository open. Each row names a real function; the link goes to its first line.

- **About 140 functions in total.** Roughly 55 carry the design; the rest are scanners, constructors and helpers you can skim — they are listed explicitly under [Functions you can safely skip](#functions-you-can-safely-skip-on-a-first-pass).
- **Suggested budget:** Pass 0–3 in one sitting (~3h), Pass 4–7 in a second (~4h), Pass 8–12 in a third (~3h).

---

## Contents

- [How to read a function in this repo](#how-to-read-a-function-in-this-repo)
- [Pass 0 — The shape of the process](#pass-0--the-shape-of-the-process-20-min)
- [Pass 1 — Vocabulary: functions that call nothing](#pass-1--vocabulary-functions-that-call-nothing-40-min)
- [Pass 2 — The state machine](#pass-2--the-state-machine-50-min)
- [Pass 3 — The double-entry ledger](#pass-3--the-double-entry-ledger-50-min)
- [Pass 4 — Persistence](#pass-4--persistence-60-min)
- [Pass 5 — The acquirer port](#pass-5--the-acquirer-port-40-min)
- [Pass 6 — The centre: PaymentService](#pass-6--the-centre-paymentservice-90-min)
- [Pass 7 — Idempotency](#pass-7--idempotency-50-min)
- [Pass 8 — Authentication and signing](#pass-8--authentication-and-signing-50-min)
- [Pass 9 — Webhooks](#pass-9--webhooks-60-min)
- [Pass 10 — Reconciliation](#pass-10--reconciliation-45-min)
- [Pass 11 — The HTTP edge](#pass-11--the-http-edge-50-min)
- [Pass 12 — Wiring and the tests that prove it](#pass-12--wiring-and-the-tests-that-prove-it-50-min)
- [Call chains worth tracing end to end](#call-chains-worth-tracing-end-to-end)
- [Functions you can safely skip on a first pass](#functions-you-can-safely-skip-on-a-first-pass)
- [Checkpoints](#checkpoints)

---

## How to read a function in this repo

Four questions, applied to every function in the tables below. The interesting functions here all turn on the last two.

1. **What does it return on the happy path?** One sentence.
2. **What are its failure modes, and who handles them?** Almost everything returns a sentinel from [errors.go](../internal/domain/errors.go); the handler is almost always `mapError`.
3. **Is a lock or an open database transaction held while it runs?** For anything in `internal/service`, this *is* the design.
4. **If I deleted it, what would break, and how would I find out?** If the answer is "a test fails", name the test. If it is "nothing obvious", read it again.

One convention makes the rest much easier. A `Querier` parameter means *the caller decides* whether the call runs inside a transaction: `s.db.Pool` as the argument means no transaction, `tx` means inside one. Grep for that argument and the transaction boundaries become visible without reading a line of SQL.

---

## Pass 0 — The shape of the process (20 min)

Do not try to understand anything yet. You are building a map so the later passes have somewhere to attach. Skim four functions and answer only "what is constructed, and in what order".

| # | Function | Read for |
|---|---|---|
| 0.1 | [`cmd/api/main.go:run`](../cmd/api/main.go#L26) | The lifecycle: load config → build the object graph → build the router → serve → graceful shutdown. Note the `signal.NotifyContext` near the top — one context cancels everything below it. |
| 0.2 | [`app.New`](../internal/app/app.go#L51) | The entire object graph in one function. No globals, no `init()`. The order of construction is the dependency order of the whole system. |
| 0.3 | [`http.NewRouter`](../internal/transport/http/router.go#L36) | The middleware chain and the route table. The doc comment explains why the order is a dependency order, not a style choice: `Auth` buffers the body, `RateLimit` needs the merchant, `Idempotency` needs both. |
| 0.4 | [`cmd/worker/main.go:run`](../cmd/worker/main.go#L34) | The second process. It has two jobs — the dispatcher loop, and `-job=reconcile`, which exits `2` when the day does not reconcile. |

**Checkpoint:** you can name the four binaries and say which of them opens an HTTP listener. Nothing more.

---

## Pass 1 — Vocabulary: functions that call nothing (40 min)

Leaves in the call graph. Every later pass uses them, so they are cheap now and expensive to guess at later.

| # | Function | What it teaches | Depends on |
|---|---|---|---|
| 1.1 | [`money.Currency.Valid`](../internal/money/money.go#L31) | The closed set of currencies, and `minorUnits` as the reason `Format` needs to exist at all. | — |
| 1.2 | [`money.Fee`](../internal/money/money.go#L68) | **Read the truncation direction.** `amount * bps / 10000` in integer arithmetic always rounds *down*, so rounding never takes from the merchant. One line, and it is a business decision. | 1.1 |
| 1.3 | [`money.Format`](../internal/money/money.go#L48) | Minor units → display string. Confirms the representation is converted only at the edge. | 1.1 |
| 1.4 | The sentinel block in [`errors.go`](../internal/domain/errors.go#L11) | Not a function — read it as vocabulary. Every layer above inspects these with `errors.Is`. You cannot read the service layer without them. | — |
| 1.5 | [`domain.Invalid`](../internal/domain/errors.go#L84) + [`ValidationError.Unwrap`](../internal/domain/errors.go#L81) | Why a field-level error is both a distinct type (so the handler can name the offending field) *and* `errors.Is(err, ErrValidation)` (so callers can switch on the class). This pairing recurs everywhere. | 1.4 |
| 1.6 | [`NewDeclinedError`](../internal/domain/errors.go#L54) + [`DeclinedError.Error`](../internal/domain/errors.go#L48) | A decline is a *typed answer from the acquirer*, not an infrastructure failure. Pass 5 and Pass 6 both hinge on that distinction. | 1.4 |
| 1.7 | [`secrets.randomString`](../internal/secrets/generate.go#L26) and the three one-liners above it | `crypto/rand`, and the key prefixes (`pk_test_`, `sk_test_`, `whsec_`). Two minutes. | — |
| 1.8 | [`secrets.NewCipher`](../internal/secrets/cipher.go#L30) → [`Encrypt`](../internal/secrets/cipher.go#L46) → [`Decrypt`](../internal/secrets/cipher.go#L56) | AES-256-GCM with a fresh nonce prepended to each ciphertext. Read it now, because Pass 8 calls `Decrypt` on **every authenticated request**. | — |

**Self-check:** why is `Fee` integer division rather than rounding to nearest? Who loses the fraction, and is that deliberate?

---

## Pass 2 — The state machine (50 min)

`internal/domain/transaction.go`, in dependency order rather than file order. `TransitionTo` is the choke point: read it before any method that calls it and the rest of the file reads itself.

| # | Function | What it teaches | Depends on |
|---|---|---|---|
| 2.1 | [`allowedTransitions`](../internal/domain/transaction.go#L36) | The single source of truth for the status graph. Draw it on paper before continuing; the terminal states are exactly those with an empty slice. | — |
| 2.2 | [`Status.IsTerminal`](../internal/domain/transaction.go#L47) / [`Status.Valid`](../internal/domain/transaction.go#L50) | Terminality is *derived* from the map, not kept as a second list that could drift out of sync with it. | 2.1 |
| 2.3 | [`Transaction.CanTransitionTo`](../internal/domain/transaction.go#L118) | A pure lookup, no side effects — which is why the service layer can use it as a cheap pre-check. | 2.1 |
| 2.4 | [`Transaction.TransitionTo`](../internal/domain/transaction.go#L131) | **The choke point.** Every status change in the codebase goes through it; nothing outside this file assigns to `t.Status`. Verify that claim yourself: `grep -rn "\.Status = " internal/`. | 2.3 |
| 2.5 | [`NewTransaction`](../internal/domain/transaction.go#L84) | The only way to build a valid transaction. Note which invariants are checked here and which are deferred to the database `CHECK` constraints. | 1.1, 2.1 |
| 2.6 | [`RemainingCapturable`](../internal/domain/transaction.go#L140) / [`RemainingRefundable`](../internal/domain/transaction.go#L145) | Two subtractions that every money rule below is expressed in terms of. Trivial to read, central to Pass 6. | — |
| 2.7 | [`AuthorizationExpiresAt`](../internal/domain/transaction.go#L151) / [`AuthorizationExpired`](../internal/domain/transaction.go#L160) | The 7-day capture window, and why `now` is a parameter rather than a call to `time.Now()` — testability without a clock abstraction. | — |
| 2.8 | [`Transaction.Authorize`](../internal/domain/transaction.go#L166) | The simplest transition method; read it as the template. Transition first, then mutate fields, then stamp `UpdatedAt`. | 2.4 |
| 2.9 | [`Transaction.Capture`](../internal/domain/transaction.go#L204) | **The most important method in the package.** Five guards in order: positive amount → status is `authorized` *or* `captured` → authorization not expired → amount within `RemainingCapturable` → transition. The transition only fires on the *first* capture, so later partial captures grow the amount while the status stays put. | 2.4, 2.6, 2.7 |
| 2.10 | [`Transaction.Refund`](../internal/domain/transaction.go#L237) | The asymmetry with `Capture`: a partial refund leaves the status alone, and only a refund that brings `RefundedAmount` up to `CapturedAmount` reaches the terminal `refunded`. The doc comment says why — otherwise "refunded" would mean two different things. | 2.9 |
| 2.11 | [`Void`](../internal/domain/transaction.go#L189) / [`Fail`](../internal/domain/transaction.go#L179) / [`Settle`](../internal/domain/transaction.go#L260) | `Void` carries a guard the transition map cannot express: you cannot void a partially captured transaction. | 2.4 |
| 2.12 | [`NetPayable`](../internal/domain/transaction.go#L271) | `captured − fee − refunded`. Pass 10 re-derives this same number from the journal and compares. | 1.2 |
| 2.13 | `TestAllowedTransitions` in [transaction_test.go](../internal/domain/transaction_test.go) | Walks every (from, to) pair. Read the test before trusting your reading of the map. | all above |

**Self-check:** capture 60,000 of a 100,000 authorization, then capture 50,000 more. Which guard in 2.9 rejects the second call, and what status is the row left in?

---

## Pass 3 — The double-entry ledger (50 min)

Read `Validate` before the constructors. Every constructor ends by calling it, so it is the contract they are all written against.

| # | Function | What it teaches | Depends on |
|---|---|---|---|
| 3.1 | The account constants and [`MerchantPayableAccount`](../internal/domain/ledger.go#L46) | The chart of accounts. Per-merchant liability accounts are a *string convention*, `merchant_payable:<uuid>` — cheap, and the reason `IsMerchantPayable` has to exist. | — |
| 3.2 | [`EntryGroup.Totals`](../internal/domain/ledger.go#L108) | The fold: sum the debits, sum the credits. | — |
| 3.3 | [`EntryGroup.Validate`](../internal/domain/ledger.go#L79) | **The invariant, written exactly once:** non-empty, one currency, one group id, `sum(debit) == sum(credit)`. Everything else in this package exists to satisfy this function. | 3.2 |
| 3.4 | [`NewCaptureEntryGroup`](../internal/domain/ledger.go#L124) | Three entries: debit `acquirer_receivable` the full amount, credit `merchant_payable:<id>` the net, credit `platform_fee_revenue` the fee. Check the arithmetic against 3.3 yourself — and note the fee entry is omitted entirely when the fee is zero, which is why `Validate` must accept a two-entry group. | 3.3, 1.2 |
| 3.5 | [`NewRefundEntryGroup`](../internal/domain/ledger.go#L185) | **Not the mirror of a capture.** The platform fee is not returned, because the processing already happened. A policy decision hard-coded here; the README lists making it configurable as future work. | 3.3 |
| 3.6 | [`NewSettlementEntryGroup`](../internal/domain/ledger.go#L228) | Moves the merchant liability out to cash. Pass 10 is its only caller. | 3.3, 2.12 |
| 3.7 | [`ComputeBalance`](../internal/domain/ledger.go#L280) / [`Balance.Available`](../internal/domain/ledger.go#L277) | A balance is a fold over entries, never a stored column. The same fold appears again as SQL in 4.10 — compare the two and confirm they agree. | 3.2 |
| 3.8 | The worked examples in [ledger_test.go](../internal/domain/ledger_test.go) | Capture 100,000 at 2%, refund it, settle it, with every expected entry spelled out. The fastest way to check your reading of 3.4–3.6. | all above |
| 3.9 | [`00003_create_ledger_entries.sql`](../migrations/00003_create_ledger_entries.sql) | The append-only trigger: the same invariant enforced a second time, against a direct `psql` session. Read it next to 3.3 and note what is duplicated *on purpose*. | 3.3 |

**Self-check:** capture 100,000 with a 2% fee, then refund 40,000. Write out all five entries by hand, confirm each group balances, and state the merchant's payable balance at the end.

---

## Pass 4 — Persistence (60 min)

The only package that writes SQL. Read `WithTx` first — it defines what "inside a transaction" means everywhere else.

| # | Function | What it teaches | Depends on |
|---|---|---|---|
| 4.1 | [`Querier`](../internal/repository/postgres.go#L19) | Three methods, satisfied by both `*pgxpool.Pool` and `pgx.Tx`. This one interface is why every repository method can run inside *or* outside a transaction, at the caller's choice. | — |
| 4.2 | [`DB.WithTx`](../internal/repository/postgres.go#L64) | `BEGIN`, run the closure, `COMMIT` or `ROLLBACK` — including on panic. **Every transaction boundary in the system is a call to this function**, so `grep -rn "WithTx" internal/` gives you the complete list. There are fewer than you would expect. | 4.1 |
| 4.3 | [`isUniqueViolation`](../internal/repository/postgres.go#L98) / [`isCheckViolation`](../internal/repository/postgres.go#L107) | Postgres error codes translated into domain sentinels at the boundary. This is why no layer above imports `pgconn`. | 1.4 |
| 4.4 | [`scanTransaction`](../internal/repository/transaction_repo.go#L233) | Read once, then never again — every read method below funnels through it. | — |
| 4.5 | [`TransactionRepo.Create`](../internal/repository/transaction_repo.go#L31) | Note the unique-violation branch: `(merchant_id, reference)` is what makes a duplicate order impossible *before* any money moves. | 4.3, 4.4 |
| 4.6 | [`GetByID`](../internal/repository/transaction_repo.go#L60) / [`GetByMerchantAndID`](../internal/repository/transaction_repo.go#L67) / [`GetByReference`](../internal/repository/transaction_repo.go#L73) | Three unlocked reads. `GetByMerchantAndID` is the tenancy check: a merchant cannot reach another merchant's transaction by guessing a UUID. | 4.4 |
| 4.7 | [`GetForUpdate`](../internal/repository/transaction_repo.go#L81) | **`SELECT ... FOR UPDATE`** — one of the two concurrency mechanisms. Only ever called with a `tx`, never with the pool; check that yourself. | 4.4 |
| 4.8 | [`TransactionRepo.Update`](../internal/repository/transaction_repo.go#L93) | **The other mechanism.** `WHERE version = $n`, and `rows_affected == 0` becomes `ErrConcurrentModification`. Belt and braces: the row lock protects writers who take it, the version column catches anyone who did not. | 4.3 |
| 4.9 | [`LedgerRepo.InsertGroup`](../internal/repository/ledger_repo.go#L28) | Inserts every entry of a group. There is no update path and no delete path — not in this file, not anywhere. | 3.3 |
| 4.10 | [`LedgerRepo.Balance`](../internal/repository/ledger_repo.go#L54) | `SUM(credit) − SUM(debit)` as SQL. Compare with `ComputeBalance` (3.7): one definition, two implementations, one of them pushed into the database. | 3.7 |
| 4.11 | [`FindUnbalancedGroups`](../internal/repository/ledger_repo.go#L175) | `GROUP BY entry_group_id HAVING sum(debit) <> sum(credit)` — the invariant of 3.3, asked of the entire table. Every integration test ends by calling this. | 3.3 |
| 4.12 | [`AggregateByMerchant`](../internal/repository/ledger_repo.go#L221) | The journal side of reconciliation. **Read the doc comment on the window**: it filters on `transactions.captured_at`, not on the entry timestamp, so both sides of the reconciliation slice the day identically. A refund booked today against yesterday's capture belongs to yesterday's batch. | — |
| 4.13 | [`TransactionRepo.List`](../internal/repository/transaction_repo.go#L151) | Keyset (cursor) pagination on `(created_at, id)`, not `OFFSET` — stable under concurrent inserts. | 4.4 |
| 4.14 | [`MerchantRepo.GetByAPIKey`](../internal/repository/merchant_repo.go#L59) | Two lines, but it sits on the hot path of every authenticated request (Pass 8). | — |
| 4.15 | [`00005_create_indexes.sql`](../migrations/00005_create_indexes.sql) | Each index next to the query it serves — 4.12, 4.13, and `ClaimDue` in Pass 9. The measurements, including the two queries where an index did *not* help, are in [architecture.md](architecture.md#what-the-numbers-actually-say). | 4.12, 4.13 |

**Self-check:** two goroutines call `GetForUpdate` on the same row inside their own transactions. What happens to the second one, and at which line does it resume?

---

## Pass 5 — The acquirer port (40 min)

| # | Function | What it teaches | Depends on |
|---|---|---|---|
| 5.1 | [`Acquirer` interface](../internal/acquirer/acquirer.go#L59) | Four methods, and a doc comment drawing the line the whole package rests on: **a decline is an answer; a timeout is not.** | 1.6 |
| 5.2 | [`Breaker.Allow`](../internal/acquirer/breaker.go#L66) | The gate. Read it before the state transitions — it is where `open` actually refuses traffic. | — |
| 5.3 | [`Breaker.refreshLocked`](../internal/acquirer/breaker.go#L108) | `open` → `half-open` once the cooldown elapses. Note the naming convention: a `Locked` suffix means "the caller already holds the mutex". | 5.2 |
| 5.4 | [`Breaker.Success`](../internal/acquirer/breaker.go#L86) / [`Failure`](../internal/acquirer/breaker.go#L97) | The three-state machine in full: a success in `half-open` closes the breaker, a failure reopens it immediately. | 5.3 |
| 5.5 | [`Guarded.do`](../internal/acquirer/guarded.go#L83) | **The one function to read slowly in this package.** Timeout wrapper, breaker check, and the classification: a `*DeclinedError` returns *without* calling `Failure()`, so declines never trip the breaker. A merchant testing declined cards must not take the acquirer offline for everyone else. | 5.1, 5.2, 5.4 |
| 5.6 | [`Guarded.Authorize`](../internal/acquirer/guarded.go#L37) | Then skim `Capture`, `Refund`, `Void` — all four are the same three lines wrapped around `do`. | 5.5 |
| 5.7 | [`Mock.simulate`](../internal/acquirer/mock.go#L113) | The failure injection: configurable decline and timeout rates, plus the three magic PANs from the README. | — |
| 5.8 | [`Mock.Authorize`](../internal/acquirer/mock.go#L54) → [`validateCard`](../internal/acquirer/mock.go#L206) → [`luhn`](../internal/acquirer/mock.go#L233) | Why all three test cards are Luhn-valid: they have to exercise the simulator, not the input validation. | 5.7 |
| 5.9 | `TestBreaker*` in [breaker_test.go](../internal/acquirer/breaker_test.go) | Confirms 5.5. The test asserting that declines do not trip the breaker is the one to find. | 5.5 |

**Self-check:** the breaker is `open` and a request arrives. Which function returns the error, and what status does the merchant see? Trace it into `mapError`.

---

## Pass 6 — The centre: PaymentService (90 min)

Everything so far exists so that these four methods can be short. Read `Authorize` and `Capture` line by line; the other two are variations.

**Before you start:** take a pen and, for every statement in `Capture`, mark whether a row lock is held and whether a database transaction is open. That annotation *is* the design.

| # | Function | What it teaches | Depends on |
|---|---|---|---|
| 6.1 | [`PaymentService.Capture`](../internal/service/payment_service.go#L193) | **Read this before `Authorize`** — it is the cleanest statement of the three-step pattern. (1) Cheap pre-check against a `preview` copy, no lock. (2) The acquirer call with *nothing* held. (3) `WithTx` → `GetForUpdate` → **re-apply `fresh.Capture` to the row as it now is** → `Update` → `InsertGroup` → `queueWebhook` → commit. Step 3's double-check is what actually decides; step 1 is only advisory. Find the ERROR log for "acquirer capture succeeded but transaction moved on" and work out why that case has to exist. | 2.9, 3.4, 4.2, 4.7, 4.8, 4.9, 5.6 |
| 6.2 | [`PaymentService.Authorize`](../internal/service/payment_service.go#L74) | The same shape plus two concerns. The row is inserted *before* the acquirer call, so the unique reference constraint rejects a duplicate order before money is touched. And the resume branch: a duplicate reference still in `created` means the previous attempt never got an answer, so it is resumed rather than refused — but only when amount and currency match. | 6.1, 4.5, 4.6 |
| 6.3 | The `switch` on `acqErr` inside `Authorize` ([line 112](../internal/service/payment_service.go#L112)) | Three-way classification, and the middle branch is the interesting one: on `ErrAcquirerUnavailable` the row stays `created` and the error is returned. Asserting a failure we cannot prove would be worse than leaving the row for a retry or for reconciliation. | 1.6, 5.1 |
| 6.4 | [`finalizeFailure`](../internal/service/payment_service.go#L393) | The decline path: mark failed and queue the webhook, in one transaction. | 2.11, 4.2 |
| 6.5 | [`queueWebhook`](../internal/service/payment_service.go#L414) | **Takes `tx pgx.Tx`, not a `Querier`** — that signature is the outbox pattern enforced by the compiler. The webhook row cannot be written outside the transaction that moved the money. | 4.2 |
| 6.6 | [`BuildEventPayload`](../internal/service/payment_service.go#L449) | The payload is frozen at queue time, not rebuilt at delivery time. A webhook describes the event as it happened, not the row as it is now. | — |
| 6.7 | [`PaymentService.Refund`](../internal/service/payment_service.go#L272) | The same three steps as 6.1 with `NewRefundEntryGroup`. Read it quickly and note only what differs. | 6.1, 3.5 |
| 6.8 | [`PaymentService.Void`](../internal/service/payment_service.go#L338) | The one operation that writes **no ledger entries at all** — releasing a hold moves no money. If you cannot explain why, go back to Pass 3. | 6.1, 2.11 |
| 6.9 | [`Get`](../internal/service/payment_service.go#L382) / [`List`](../internal/service/payment_service.go#L387) | Pass-throughs. Thirty seconds. | 4.6, 4.13 |

**Self-check:** ten goroutines capture the same transaction at once. How many reach the acquirer, how many reach `GetForUpdate`, how many write an entry group? Name the exact line that rejects the ninth — then read [concurrency_test.go](../test/integration/concurrency_test.go) and check your answer.

---

## Pass 7 — Idempotency (50 min)

| # | Function | What it teaches | Depends on |
|---|---|---|---|
| 7.1 | [`idempotency.Key`](../internal/idempotency/store.go#L66) | Namespaced by merchant, so two merchants can use the same key string without colliding. | — |
| 7.2 | [`canonicalize`](../internal/idempotency/store.go#L172) → [`sortValue`](../internal/idempotency/store.go#L187) | JSON re-encoded with keys sorted, recursively. Read this before `Fingerprint`. | — |
| 7.3 | [`Fingerprint`](../internal/idempotency/store.go#L167) | SHA-256 of the canonicalised body. Byte-comparing raw bodies would break on proxies that reorder keys or change whitespace, and would refuse legitimate retries. | 7.2 |
| 7.4 | [`Store.Begin`](../internal/idempotency/store.go#L75) | **The atomic claim** — a single `SET NX EX`, so exactly one of N simultaneous retries wins. Then four return paths: claim won; key expired between SETNX and GET (treated as in-flight, deliberately conservative); fingerprint mismatch; still in progress. | 7.3 |
| 7.5 | [`Store.Complete`](../internal/idempotency/store.go#L129) / [`Release`](../internal/idempotency/store.go#L153) | Overwrite the claim with the stored response, or drop the key so a retry can run. | 7.4 |
| 7.6 | [`Idempotency` middleware](../internal/transport/http/middleware_idem.go#L43) | **The six branches are enumerated in the doc comment above the function — read that first, then match each branch to its code.** Two details deserve attention: the `defer`/`recover` that releases the key *before* the panic travels on to `Recoverer`, and the `status >= 500` release. Caching a 5xx for 24 hours would leave the merchant permanently unable to complete that payment. | 7.4, 7.5 |
| 7.7 | [`releaseKey`](../internal/transport/http/middleware_idem.go#L128) | `context.WithoutCancel`: the client may have disconnected, but the key must be released regardless. | 7.6 |
| 7.8 | [middleware_idem_test.go](../internal/transport/http/middleware_idem_test.go) | All four outcomes against a fake store — the fastest confirmation that you read 7.6 correctly. | 7.6 |

**Self-check:** same key, same body, and the first request is still running. What status code comes back, and which of `Begin`'s return paths produced it?

---

## Pass 8 — Authentication and signing (50 min)

| # | Function | What it teaches | Depends on |
|---|---|---|---|
| 8.1 | [`ComputeRequestSignature`](../internal/service/merchant_service.go#L210) | **Read the doc comment in full.** The newline delimiters are not decoration: with plain concatenation, `(path "/a", body "b")` and `(path "/ab", body "")` produce identical signed bytes. Exported so `cmd/seed` and `scripts/sign.sh` agree with a single definition. | — |
| 8.2 | [`MerchantService.Authenticate`](../internal/service/merchant_service.go#L149) | The order of checks: headers present → timestamp parseable → **drift within the skew window** (replay protection) → load merchant → decrypt secret → `hmac.Equal` → merchant active. Note the constant-time compare and the comment on the timing leak that `==` would create. | 8.1, 1.8, 4.14 |
| 8.3 | [`readAndRestoreBody`](../internal/transport/http/middleware_auth.go#L100) | `MaxBytesReader`, read once, then hand the handler a fresh reader over the same bytes. The body can only be read once off the network — this is how three consumers share it. | — |
| 8.4 | [`Auth` middleware](../internal/transport/http/middleware_auth.go#L36) | Buffers the body, verifies, then puts *both* the merchant and the raw body into the context. That stored body is exactly what the idempotency middleware fingerprints. | 8.2, 8.3 |
| 8.5 | [`MerchantFrom`](../internal/transport/http/middleware_auth.go#L69) / [`rawBodyFrom`](../internal/transport/http/middleware_auth.go#L76) | The typed context keys — trivial, but they are the seam between 8.4 and Pass 7. | 8.4 |
| 8.6 | [`AdminAuth`](../internal/transport/http/middleware_auth.go#L86) | Why merchant provisioning uses a bearer token rather than HMAC: a merchant that does not exist yet has no signing key. Still a constant-time compare. | — |
| 8.7 | [`MerchantService.Create`](../internal/service/merchant_service.go#L75) | Generate key, secret and webhook secret; encrypt; store; **return the plaintext exactly once**. No endpoint can retrieve it afterwards. | 1.7, 1.8 |
| 8.8 | [`ratelimit.Limiter.Allow`](../internal/ratelimit/limiter.go#L49) | A fixed window keyed by `now / window`, with `INCR` and `EXPIRE` pipelined into one round trip. Read the comment on why `EXPIRE` is re-set on every call rather than only the first — it removes the immortal-counter failure mode. Then read the honest limitation in the README: the window boundary can let through 2× the limit. | — |
| 8.9 | `TestComputeRequestSignature*` in [signature_test.go](../internal/service/signature_test.go) | The test that demonstrates the concatenation ambiguity 8.1 avoids. | 8.1 |

**Self-check:** an attacker replays a valid signed request ten minutes later. Which check in 8.2 stops them, and what is the configured window?

---

## Pass 9 — Webhooks (60 min)

| # | Function | What it teaches | Depends on |
|---|---|---|---|
| 9.1 | [`NewWebhookDelivery`](../internal/domain/webhook.go#L53) | The outbox row: event, payload, first attempt due immediately. | — |
| 9.2 | [`BackoffDelay`](../internal/domain/webhook.go#L69) | `2^n` seconds with ±20% jitter. The jitter is what stops N failed deliveries from retrying in lockstep and hammering an endpoint that is just coming back. Note that `*rand.Rand` is a parameter — deterministic in tests. | — |
| 9.3 | [`RecordFailure`](../internal/domain/webhook.go#L86) / [`RecordSuccess`](../internal/domain/webhook.go#L99) | The retry state machine: schedule the next attempt, or — at `maxAttempts` — park the delivery as `dead` for a human, rather than retrying forever. | 9.2 |
| 9.4 | [`webhook.Sign`](../internal/webhook/signer.go#L32) → [`computeSignature`](../internal/webhook/signer.go#L64) | Stripe's scheme, `t=<unix>,v1=<hex>`, with the timestamp **inside** the signed string. If `t` were only a header field, an attacker could replay a captured payload forever by rewriting it. | — |
| 9.5 | [`webhook.Verify`](../internal/webhook/signer.go#L41) → [`parseHeader`](../internal/webhook/signer.go#L72) | The merchant's side of the same scheme. `SystemHandler.WebhookReceiver` and the integration suite both use it. | 9.4 |
| 9.6 | [`WebhookRepo.Create`](../internal/repository/webhook_repo.go#L28) | The insert that `queueWebhook` (6.5) performs inside the money transaction. | 6.5 |
| 9.7 | [`WebhookRepo.ClaimDue`](../internal/repository/webhook_repo.go#L52) | **`FOR UPDATE SKIP LOCKED`** — twenty lines of SQL that make N worker processes safe with no coordinator: each claims a disjoint batch instead of blocking on the same rows. | 4.1 |
| 9.8 | [`WebhookRepo.ReleaseStale`](../internal/repository/webhook_repo.go#L112) | The janitor. A worker killed mid-delivery leaves a claim behind; this releases it after a timeout, instead of every worker retrying it at once. | 9.7 |
| 9.9 | [`Dispatcher.Run`](../internal/webhook/dispatcher.go#L81) | **Read the shutdown ordering carefully.** Poller and workers start; on `ctx.Done()` it waits for the poller, whose deferred `close(jobs)` then drains the workers, all under a shutdown timeout. Closing the channel from the producer side is what makes that drain correct. | — |
| 9.10 | [`Dispatcher.poll`](../internal/webhook/dispatcher.go#L122) | Ticker → `ReleaseStale` → `ClaimDue` → fan out. Note the `select` on the send: on cancellation it hands the row *back* so a restarted worker picks it up at once. | 9.7, 9.8 |
| 9.11 | [`Dispatcher.worker`](../internal/webhook/dispatcher.go#L156) | `context.WithoutCancel` again: once a job is claimed it runs to completion even during shutdown, bounded by the HTTP timeout. Compare with 7.7 — same reasoning, different problem. | 9.9 |
| 9.12 | [`deliver`](../internal/webhook/dispatcher.go#L168) → [`post`](../internal/webhook/dispatcher.go#L207) → [`fail`](../internal/webhook/dispatcher.go#L234) / [`persist`](../internal/webhook/dispatcher.go#L248) | The attempt itself: sign, POST, classify the response, persist the outcome. Straightforward once 9.3 and 9.4 are in place. | 9.3, 9.4 |

**Self-check:** the process is killed between `ClaimDue` and the HTTP POST. Which function un-sticks that row, and how long does the merchant wait for the webhook?

---

## Pass 10 — Reconciliation (45 min)

| # | Function | What it teaches | Depends on |
|---|---|---|---|
| 10.1 | [`Report.OK`](../internal/service/reconciliation_service.go#L61) | The gate: no discrepancies **and** no unbalanced groups. Settlement is conditional on this returning true. | — |
| 10.2 | [`ReconciliationService.Run`](../internal/service/reconciliation_service.go#L107) | Five numbered steps in the code. The point is that steps 1 and 2 are **two independent derivations of the same numbers** — one from `transactions`, one from the journal — computed without reference to each other and only then compared in step 3. A single derivation would prove nothing except that the code agrees with itself. | 4.12, 2.12, 1.2 |
| 10.3 | Step 4 of `Run` — the `FindUnbalancedGroups` call | Checks the invariant across the **whole journal**, not just the day being closed. Yesterday's corruption must not settle today. | 4.11 |
| 10.4 | [`settle`](../internal/service/reconciliation_service.go#L236) | Reached only when everything agrees. Per transaction: transition to settled and book the settlement group, one transaction each. | 2.11, 3.6, 4.2 |
| 10.5 | [`logOffendingTransactions`](../internal/service/reconciliation_service.go#L288) | What makes a failed close debuggable at 3am: the specific transactions behind a mismatched total, not just the total. | — |
| 10.6 | [`write`](../internal/service/reconciliation_service.go#L309) | The report file under `reports/`. Together with `cmd/worker`'s exit code `2`, this is what a scheduler alerts on. | — |
| 10.7 | [`SettlementReport`](../internal/service/reconciliation_service.go#L365) | The read model behind `GET /reports/settlement`. | — |

**Self-check:** the ledger says a merchant is owed 98,000 while the transactions table implies 99,000. Trace what `Run` does, in order, and say whether anything settles that day.

---

## Pass 11 — The HTTP edge (50 min)

Deliberately last. By now every handler is three lines you could have predicted.

| # | Function | What it teaches | Depends on |
|---|---|---|---|
| 11.1 | [`mapError`](../internal/transport/http/response.go#L75) | **The single domain-error → HTTP-status mapping, in exactly one place.** `errors.As` for the typed errors first (they carry a code and a field), then `errors.Is` for the sentinels. Pinned by a table test. | 1.4, 1.5, 1.6 |
| 11.2 | [`writeError`](../internal/transport/http/response.go#L63) / [`writeJSON`](../internal/transport/http/response.go#L45) | The error envelope, and why 5xx logs at ERROR while 4xx logs at INFO — a client mistake is not an incident. | 11.1 |
| 11.3 | [`decodeJSON`](../internal/transport/http/request.go#L27) | `DisallowUnknownFields`, a size limit, and every decode failure turned into a field-level error. A typo in a field name is rejected, not silently ignored. | 1.5 |
| 11.4 | [`CreatePaymentRequest.ToInput`](../internal/transport/http/request.go#L83) | The DTO → domain-input boundary: where the wire format stops and the domain begins. `service.AuthorizeInput` knows nothing about JSON. | 11.3 |
| 11.5 | [`encodeCursor`](../internal/transport/http/request.go#L187) / [`decodeCursor`](../internal/transport/http/request.go#L191) | The opaque cursor that 4.13 consumes. | 4.13 |
| 11.6 | [`RequestID`](../internal/transport/http/middleware_log.go#L28) → [`Logger`](../internal/transport/http/middleware_log.go#L87) → [`withLogger`](../internal/transport/http/middleware_log.go#L116) / [`LoggerFrom`](../internal/transport/http/middleware_log.go#L121) | How a request-scoped logger reaches every layer without being threaded through as a parameter, and how `X-Request-Id` ends up in every error body. | — |
| 11.7 | [`responseRecorder.WriteHeader`](../internal/transport/http/middleware_log.go#L56) / [`Write`](../internal/transport/http/middleware_log.go#L63) / [`statusCode`](../internal/transport/http/middleware_log.go#L75) | The `capture` flag: one recorder both logs the status and buffers the body for the idempotency store. Read this if 7.6 felt like magic. | 7.6 |
| 11.8 | [`Recoverer`](../internal/transport/http/middleware_log.go#L132) | Sits *above* `Idempotency` in the chain — which is exactly why 7.6 has to release the key itself before re-panicking. | 7.6, 11.6 |
| 11.9 | [`PaymentHandler.Create`](../internal/transport/http/handler_payment.go#L77) → [`Capture`](../internal/transport/http/handler_payment.go#L179) → [`toPaymentResponse`](../internal/transport/http/handler_payment.go#L56) | Decode, call the service, map the error, encode. Read one carefully and skim the rest — `Get`, `List`, `Refund`, `Void` are the same shape. | 11.1, 11.4, 6.1 |
| 11.10 | [`LedgerHandler.Balance`](../internal/transport/http/handler_ledger.go#L115) / [`Entries`](../internal/transport/http/handler_ledger.go#L54) | The read side of Pass 3. | 4.10 |
| 11.11 | [`SystemHandler.Readyz`](../internal/transport/http/handler_system.go#L43) | Readiness pings Postgres and Redis under a 2s timeout and returns 503 if either is down; liveness ([`Healthz`](../internal/transport/http/handler_system.go#L37)) does neither. That distinction is what stops a sick dependency from triggering a restart loop on a healthy process. | — |
| 11.12 | [`SystemHandler.WebhookReceiver`](../internal/transport/http/handler_system.go#L77) | The demo merchant endpoint, verifying with 9.5. It exists so the loop can be closed end to end without a second service. | 9.5 |

---

## Pass 12 — Wiring and the tests that prove it (50 min)

| # | Function | What it teaches | Depends on |
|---|---|---|---|
| 12.1 | [`config.Load`](../internal/config/config.go#L66) | Every knob in one place, validated at startup. Failing fast beats a nil dereference at 2am. | — |
| 12.2 | [`Config.RequireAdminToken`](../internal/config/config.go#L178) | A production guard that refuses to boot without a real admin token. | 12.1 |
| 12.3 | [`app.New`](../internal/app/app.go#L51) — again, properly this time | Re-read Pass 0.2 now that you know every type it constructs. The construction order *is* the dependency graph. | everything |
| 12.4 | [payment_flow_test.go](../test/integration/payment_flow_test.go) | Authorize → capture → refund against real Postgres and real Redis. Every scenario ends with the same assertion: no unbalanced group anywhere in the journal. | 6.1, 4.11 |
| 12.5 | [concurrency_test.go](../test/integration/concurrency_test.go) | Ten goroutines, one transaction, under `-race`: exactly one success and exactly one entry group. This is the test 6.1 exists for. | 6.1 |
| 12.6 | [idempotency_test.go](../test/integration/idempotency_test.go) | All four outcomes, end to end. | 7.6 |
| 12.7 | [reconciliation_test.go](../test/integration/reconciliation_test.go) | Including the case where the day does *not* reconcile and nothing settles. | 10.2 |

---

## Call chains worth tracing end to end

Once the passes are done, trace these with a finger on the screen. Each crosses every layer, and each is a question you should expect to be asked.

**1. `POST /payments` — the full path**

```
NewRouter → RequestID → Logger → Recoverer
  → Auth → readAndRestoreBody → MerchantService.Authenticate
                                  → GetByAPIKey → Cipher.Decrypt
                                  → ComputeRequestSignature → hmac.Equal
  → RateLimit → Limiter.Allow
  → Idempotency → Fingerprint → canonicalize
                → Store.Begin  (SET NX)
  → PaymentHandler.Create → decodeJSON → CreatePaymentRequest.ToInput
      → PaymentService.Authorize
          → NewTransaction → TransactionRepo.Create
          → Guarded.Authorize → do → Breaker.Allow → Mock.Authorize
          → DB.WithTx
              → GetForUpdate → Transaction.Authorize → TransactionRepo.Update
              → queueWebhook → BuildEventPayload → WebhookRepo.Create
  → toPaymentResponse → writeJSON
  → Store.Complete
```

**2. Capture, and the concurrency argument.** `PaymentService.Capture`, steps 1/2/3, with `Transaction.Capture` called **twice**: once on the `preview` copy with no lock held, once on `fresh` inside the lock. Be able to say what each call is for and which one is authoritative.

**3. A webhook, from money movement to delivery.** `queueWebhook` (inside the money transaction) → `WebhookRepo.Create` → `Dispatcher.poll` → `ReleaseStale` → `ClaimDue` → `worker` → `deliver` → `Sign` → `post` → `RecordSuccess`/`RecordFailure` → `BackoffDelay` → `UpdateAttempt`.

**4. A balance.** `LedgerHandler.Balance` → `LedgerService.Balance` → `LedgerRepo.Balance` (the SQL fold), against `domain.ComputeBalance` (the in-memory fold). One definition, two implementations, pinned together by the ledger tests.

**5. The daily close.** `cmd/worker run -job=reconcile` → `Run` → `ListCapturedBetween` **and** `AggregateByMerchant` → compare → `FindUnbalancedGroups` → `settle`, or `SettlementSkipped` → exit code.

---

## Functions you can safely skip on a first pass

Open them only when a test fails inside one. They are mechanical, and time spent here is time not spent on Pass 6.

- Every `scan*` helper: `scanTransaction`, `scanMerchant`, `scanDelivery`, `scanLedgerEntry`.
- Every `New*` constructor that only assigns struct fields: `NewPaymentService`, `NewLedgerRepo`, `NewTransactionRepo`, `NewMerchantHandler` and their siblings.
- [internal/repository/types.go](../internal/repository/types.go) — two type conversions.
- The `internal/config` env helpers: `env`, `envInt`, `envFloat`, `envBool`, `envDuration`, `decodeKey`.
- [internal/domain/merchant.go](../internal/domain/merchant.go): `containsRune`, `hasHTTPScheme`.
- [internal/acquirer/mock.go](../internal/acquirer/mock.go): `newRef`, `last4`, `brandOf`, `float64`, `int63n`, `delay`, `hang`.
- [internal/app/logging.go](../internal/app/logging.go), [internal/app/migrate.go](../internal/app/migrate.go), [internal/cache/redis.go](../internal/cache/redis.go) — startup plumbing.
- `cmd/seed`, `cmd/migrate` — worth *running*, not reading.

---

## Checkpoints

If you can answer these without opening the file, that pass is done.

| After | Question |
|---|---|
| Pass 2 | Name every status from which `Capture` can legally be called, and the guard that rejects the others. |
| Pass 3 | Write the entries for a capture of 100,000 at 2%, and say which account each side lands in. |
| Pass 4 | What is the difference between `GetByID` and `GetForUpdate`, and what breaks if `Capture` uses the wrong one? |
| Pass 5 | Why does a declined card not trip the circuit breaker, and where is that decided? |
| Pass 6 | Which line rejects the second of two concurrent captures, and what state is the row left in? |
| Pass 7 | Same idempotency key, different body — what status, and why is refusing better than guessing? |
| Pass 8 | Why newline delimiters in the signed string? Give the two-request collision. |
| Pass 9 | Why `SKIP LOCKED` rather than a `claimed_by` column and a `WHERE` clause? |
| Pass 10 | Why derive the day's totals twice instead of once? |
| Pass 11 | Where does `domain.ErrAuthorizationExpired` become a 409, and how many places would have to change to make it a 422? |

Then take [docs/learning-quiz.md](learning-quiz.md).
