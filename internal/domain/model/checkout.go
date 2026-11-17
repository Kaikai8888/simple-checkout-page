package model

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"

	"checkout/internal/domain/def"
)

const maxQuantityPerItem = 99

var (
	mobileCarrierPattern = regexp.MustCompile(`^/[0-9A-Z.+\-]{7}$`)
	taxIdPattern         = regexp.MustCompile(`^[0-9]{8}$`)
	twPhonePattern       = regexp.MustCompile(`^09[0-9]{8}$`)
)

// CheckoutItem is what the shopper saw on the checkout page.
// UnitPrice is the price shown to the shopper; the backend re-checks it.
type CheckoutItem struct {
	ProductID int64 `json:"product_id"`
	Quantity  int   `json:"quantity"`
	UnitPrice int64 `json:"unit_price"`
}

type ShippingInfo struct {
	Region         string `json:"region"`
	County         string `json:"county"`
	District       string `json:"district"`
	Address        string `json:"address"`
	Zip            string `json:"zip"`
	RecipientName  string `json:"recipient_name"`
	RecipientPhone string `json:"recipient_phone"`
}

type CustomerInfo struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Phone string `json:"phone"`
}

type InvoiceInfo struct {
	Type    string `json:"type"`
	Carrier string `json:"carrier,omitempty"`
	TaxID   string `json:"tax_id,omitempty"`
	Title   string `json:"title,omitempty"`
}

// CheckoutRequest is the payload persisted in a checkout draft.
type CheckoutRequest struct {
	Items         []CheckoutItem `json:"items"`
	CouponCode    string         `json:"coupon_code,omitempty"`
	Donation      int64          `json:"donation"`
	PaymentMethod string         `json:"payment_method"`
	Shipping      ShippingInfo   `json:"shipping"`
	Customer      CustomerInfo   `json:"customer"`
	Invoice       InvoiceInfo    `json:"invoice"`
	Note          string         `json:"note,omitempty"`
	ExpectedTotal int64          `json:"expected_total"`
}

type CheckoutDraft struct {
	ID             string
	IdempotencyKey string
	Request        CheckoutRequest
	Status         string
	Attempts       int
	ErrorCode      string
	ErrorMessage   string
	ErrorDetails   json.RawMessage
	OrderID        string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Validate checks the request format only (synchronous validation).
// Business validation (price / stock / coupon) runs asynchronously in the worker.
func (r CheckoutRequest) Validate() *def.AppError {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if len(r.Items) == 0 {
		add("items is required")
	}
	seen := map[int64]bool{}
	for _, it := range r.Items {
		if it.ProductID <= 0 {
			add("items.product_id is invalid")
		}
		if it.Quantity <= 0 || it.Quantity > maxQuantityPerItem {
			add("items.quantity must be between 1 and %d", maxQuantityPerItem)
		}
		if it.UnitPrice < 0 {
			add("items.unit_price is invalid")
		}
		if seen[it.ProductID] {
			add("duplicated product_id %d", it.ProductID)
		}
		seen[it.ProductID] = true
	}
	if r.Donation < 0 {
		add("donation must be >= 0")
	}
	if _, ok := FindPaymentMethod(r.PaymentMethod); !ok {
		add("payment_method is invalid")
	}

	region, ok := FindShippingRegion(r.Shipping.Region)
	if !ok {
		add("shipping.region is invalid")
	}
	if region.Domestic && (strings.TrimSpace(r.Shipping.County) == "" || strings.TrimSpace(r.Shipping.District) == "") {
		add("shipping.county and shipping.district are required")
	}
	if strings.TrimSpace(r.Shipping.Address) == "" {
		add("shipping.address is required")
	}
	if strings.TrimSpace(r.Shipping.RecipientName) == "" {
		add("shipping.recipient_name is required")
	}
	if region.Domestic && !twPhonePattern.MatchString(r.Shipping.RecipientPhone) {
		add("shipping.recipient_phone must be a Taiwan mobile number (09xxxxxxxx)")
	} else if strings.TrimSpace(r.Shipping.RecipientPhone) == "" {
		add("shipping.recipient_phone is required")
	}

	if _, err := mail.ParseAddress(r.Customer.Email); err != nil {
		add("customer.email is invalid")
	}

	switch r.Invoice.Type {
	case def.InvoiceTypePersonal:
		if r.Invoice.Carrier != "" && !mobileCarrierPattern.MatchString(r.Invoice.Carrier) {
			add("invoice.carrier format is invalid (e.g. /AB1CD23)")
		}
	case def.InvoiceTypeCompany:
		if !taxIdPattern.MatchString(r.Invoice.TaxID) {
			add("invoice.tax_id must be 8 digits")
		}
		if strings.TrimSpace(r.Invoice.Title) == "" {
			add("invoice.title is required for company invoice")
		}
	default:
		add("invoice.type is invalid")
	}

	if len(problems) > 0 {
		return def.NewError(def.ErrInvalidRequest, "checkout request is invalid").WithDetails(problems)
	}
	return nil
}

// ProductIDs returns the unique product ids in ascending order
// (stable order also avoids deadlocks when locking rows).
func (r CheckoutRequest) ProductIDs() []int64 {
	ids := make([]int64, 0, len(r.Items))
	seen := map[int64]bool{}
	for _, it := range r.Items {
		if !seen[it.ProductID] {
			seen[it.ProductID] = true
			ids = append(ids, it.ProductID)
		}
	}
	slices.Sort(ids)
	return ids
}
