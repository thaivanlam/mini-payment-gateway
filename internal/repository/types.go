package repository

import "github.com/thaivanlam/mini-payment-gateway/internal/money"

// Small conversion helpers so the SQL scanning code reads uniformly. Postgres
// stores money as BIGINT and currency as TEXT; the domain uses named types.
func moneyAmount(v int64) money.Amount { return money.Amount(v) }

func currencyOf(v string) money.Currency { return money.Currency(v) }
