package http

import (
	"net/http"
	"time"

	"github.com/thaivanlam/mini-payment-gateway/internal/domain"
	"github.com/thaivanlam/mini-payment-gateway/internal/service"
)

// MerchantHandler serves the merchant endpoints.
type MerchantHandler struct {
	merchants *service.MerchantService
}

// NewMerchantHandler builds a MerchantHandler.
func NewMerchantHandler(merchants *service.MerchantService) *MerchantHandler {
	return &MerchantHandler{merchants: merchants}
}

// MerchantResponse is the public view of a merchant. Note what is absent:
// neither secret appears here, or in any other response.
type MerchantResponse struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Email      string    `json:"email"`
	APIKey     string    `json:"api_key"`
	WebhookURL string    `json:"webhook_url,omitempty"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

// CreateMerchantResponse additionally carries the plaintext secrets, which are
// shown exactly once and cannot be retrieved again.
type CreateMerchantResponse struct {
	MerchantResponse
	APISecret     string `json:"api_secret"`
	WebhookSecret string `json:"webhook_secret"`
	Notice        string `json:"notice"`
}

func toMerchantResponse(m *domain.Merchant) MerchantResponse {
	return MerchantResponse{
		ID:         m.ID.String(),
		Name:       m.Name,
		Email:      m.Email,
		APIKey:     m.APIKey,
		WebhookURL: m.WebhookURL,
		Status:     string(m.Status),
		CreatedAt:  m.CreatedAt,
	}
}

// Create handles POST /api/v1/merchants (admin only).
func (h *MerchantHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateMerchantRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(ctx, w, err)
		return
	}

	created, err := h.merchants.Create(ctx, service.CreateMerchantInput{
		Name:       req.Name,
		Email:      req.Email,
		WebhookURL: req.WebhookURL,
	})
	if err != nil {
		writeError(ctx, w, err)
		return
	}

	writeJSON(w, http.StatusCreated, CreateMerchantResponse{
		MerchantResponse: toMerchantResponse(created.Merchant),
		APISecret:        created.APISecret,
		WebhookSecret:    created.WebhookSecret,
		Notice:           "Store api_secret and webhook_secret now. They are shown once and cannot be retrieved again.",
	})
}

// Me handles GET /api/v1/merchants/me.
func (h *MerchantHandler) Me(w http.ResponseWriter, r *http.Request) {
	merchant := MerchantFrom(r.Context())
	writeJSON(w, http.StatusOK, toMerchantResponse(merchant))
}
