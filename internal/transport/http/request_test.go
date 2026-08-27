package http

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/money"
)

func TestCreatePaymentRequestToInput(t *testing.T) {
	merchantID := uuid.New()
	base := CreatePaymentRequest{
		Reference: " ORDER-1 ",
		Amount:    150_000,
		Currency:  "vnd",
		Card:      CardRequest{Number: "4242424242424242", ExpMonth: 12, ExpYear: 2030, CVV: "123"},
		Capture:   true,
		Metadata:  map[string]string{"customer_id": "cus_123"},
	}

	input, err := base.ToInput(merchantID)
	require.NoError(t, err)

	assert.Equal(t, merchantID, input.MerchantID)
	assert.Equal(t, "ORDER-1", input.Reference, "the reference is trimmed")
	assert.Equal(t, money.VND, input.Currency, "the currency is upper-cased")
	assert.Equal(t, money.Amount(150_000), input.Amount)
	assert.True(t, input.Capture)
	assert.Equal(t, "cus_123", input.Metadata["customer_id"])
}

func TestCreatePaymentRequestValidation(t *testing.T) {
	valid := CreatePaymentRequest{
		Reference: "ORDER-1",
		Amount:    1000,
		Currency:  "VND",
		Card:      CardRequest{Number: "4242424242424242", ExpMonth: 12, ExpYear: 2030, CVV: "123"},
	}

	tests := []struct {
		name    string
		mutate  func(*CreatePaymentRequest)
		wantErr string
	}{
		{name: "zero amount", mutate: func(r *CreatePaymentRequest) { r.Amount = 0 }, wantErr: "amount"},
		{name: "negative amount", mutate: func(r *CreatePaymentRequest) { r.Amount = -5 }, wantErr: "amount"},
		{name: "unknown currency", mutate: func(r *CreatePaymentRequest) { r.Currency = "XYZ" }, wantErr: "currency"},
		{name: "missing card", mutate: func(r *CreatePaymentRequest) { r.Card.Number = "" }, wantErr: "card.number"},
		{
			name: "oversized metadata value",
			mutate: func(r *CreatePaymentRequest) {
				big := make([]byte, 501)
				for i := range big {
					big[i] = 'x'
				}
				r.Metadata = map[string]string{"k": string(big)}
			},
			wantErr: "metadata",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := valid
			tc.mutate(&req)
			_, err := req.ToInput(uuid.New())
			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrValidation)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestAmountRequestValidate(t *testing.T) {
	amount, err := AmountRequest{Amount: 0}.Validate()
	require.NoError(t, err)
	assert.Equal(t, money.Amount(0), amount, "zero means the full remaining amount")

	amount, err = AmountRequest{Amount: 500}.Validate()
	require.NoError(t, err)
	assert.Equal(t, money.Amount(500), amount)

	_, err = AmountRequest{Amount: -1}.Validate()
	assert.ErrorIs(t, err, domain.ErrValidation)
}

func TestCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 8, 27, 10, 30, 0, 123456789, time.UTC)
	id := uuid.New()

	cursor := encodeCursor(ts, id)
	assert.NotContains(t, cursor, id.String(), "the cursor is opaque")

	gotTS, gotID, err := decodeCursor(cursor)
	require.NoError(t, err)
	require.NotNil(t, gotTS)
	require.NotNil(t, gotID)
	assert.True(t, ts.Equal(*gotTS))
	assert.Equal(t, id, *gotID)
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	ts, id, err := decodeCursor("")
	require.NoError(t, err)
	assert.Nil(t, ts)
	assert.Nil(t, id)

	for _, bad := range []string{"!!!!", "bm90LWEtY3Vyc29y", "MjAyNi0wOC0yN3xub3QtYS11dWlk"} {
		_, _, err := decodeCursor(bad)
		assert.ErrorIsf(t, err, domain.ErrValidation, "cursor %q", bad)
	}
}

func TestLedgerCursorRoundTrip(t *testing.T) {
	cursor := encodeLedgerCursor(4242)

	id, err := decodeLedgerCursor(cursor)
	require.NoError(t, err)
	require.NotNil(t, id)
	assert.Equal(t, int64(4242), *id)

	id, err = decodeLedgerCursor("")
	require.NoError(t, err)
	assert.Nil(t, id)

	_, err = decodeLedgerCursor("!!!")
	assert.ErrorIs(t, err, domain.ErrValidation)
}

func TestQueryHelpers(t *testing.T) {
	ts, err := queryTime("2026-08-27T10:00:00Z", "from")
	require.NoError(t, err)
	require.NotNil(t, ts)
	assert.Equal(t, 2026, ts.Year())

	ts, err = queryTime("", "from")
	require.NoError(t, err)
	assert.Nil(t, ts)

	_, err = queryTime("27/08/2026", "from")
	assert.ErrorIs(t, err, domain.ErrValidation)

	limit, err := queryLimit("", 25, 100)
	require.NoError(t, err)
	assert.Equal(t, 25, limit)

	limit, err = queryLimit("500", 25, 100)
	require.NoError(t, err)
	assert.Equal(t, 100, limit, "the limit is clamped, not rejected")

	_, err = queryLimit("0", 25, 100)
	assert.ErrorIs(t, err, domain.ErrValidation)
	_, err = queryLimit("abc", 25, 100)
	assert.ErrorIs(t, err, domain.ErrValidation)
}

func TestPathUUID(t *testing.T) {
	id := uuid.New()
	got, err := pathUUID(id.String(), "id")
	require.NoError(t, err)
	assert.Equal(t, id, got)

	_, err = pathUUID("not-a-uuid", "id")
	assert.ErrorIs(t, err, domain.ErrValidation)
}
