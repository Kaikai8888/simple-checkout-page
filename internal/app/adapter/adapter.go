// Package adapter defines the input / output shapes of the app services.
package adapter

import (
	"checkout/internal/domain/def"
	"checkout/internal/domain/model"
)

// SubmitCheckoutInput has the same shape as the persisted draft payload.
type SubmitCheckoutInput model.CheckoutRequest

func (in SubmitCheckoutInput) ToModel() model.CheckoutRequest { return model.CheckoutRequest(in) }

type SubmitCheckoutOutput struct {
	DraftID string `json:"draft_id"`
	Status  string `json:"status"`
}

type DraftStatusOutput struct {
	DraftID string        `json:"draft_id"`
	Status  string        `json:"status"`
	OrderID string        `json:"order_id,omitempty"`
	Error   *def.AppError `json:"error,omitempty"`
}

type CheckoutOptionsOutput struct {
	ShippingRegions   []model.ShippingRegion `json:"shipping_regions"`
	PaymentMethods    []model.PaymentMethod  `json:"payment_methods"`
	PaymentTTLSeconds int                    `json:"payment_ttl_seconds"`
}

type CouponPreviewInput struct {
	Code     string `json:"code"`
	Subtotal int64  `json:"subtotal"`
}

type CouponPreviewOutput struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	Discount    int64  `json:"discount"`
}

type UpdateProductInput struct {
	Price *int64 `json:"price"`
	Stock *int   `json:"stock"`
}

type PaymentView struct {
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason,omitempty"`
}

type OrderOutput struct {
	model.Order
	LatestPayment *PaymentView `json:"latest_payment,omitempty"`
}

type StartPaymentOutput struct {
	PaymentUrl string `json:"payment_url"`
}
