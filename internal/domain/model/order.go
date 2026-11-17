package model

import "time"

type Order struct {
	ID            string       `json:"id"`
	OrderNo       string       `json:"order_no"`
	DraftID       string       `json:"-"`
	CustomerID    int64        `json:"-"`
	Status        string       `json:"status"`
	Lines         []OrderLine  `json:"lines"`
	Subtotal      int64        `json:"subtotal"`
	Discount      int64        `json:"discount"`
	ShippingFee   int64        `json:"shipping_fee"`
	Donation      int64        `json:"donation"`
	Total         int64        `json:"total"`
	CouponCode    string       `json:"coupon_code,omitempty"`
	PaymentMethod string       `json:"payment_method"`
	Shipping      ShippingInfo `json:"shipping"`
	Invoice       InvoiceInfo  `json:"invoice"`
	Customer      CustomerInfo `json:"customer"`
	Note          string       `json:"note,omitempty"`
	ExpiresAt     time.Time    `json:"expires_at"`
	PaidAt        *time.Time   `json:"paid_at,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
}

type Payment struct {
	ID                string    `json:"id"`
	OrderID           string    `json:"order_id"`
	Method            string    `json:"method"`
	Amount            int64     `json:"amount"`
	Status            string    `json:"status"`
	ProviderSessionID string    `json:"-"`
	ProviderTxnID     string    `json:"provider_txn_id,omitempty"`
	PaymentUrl        string    `json:"-"`
	FailureReason     string    `json:"failure_reason,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}
