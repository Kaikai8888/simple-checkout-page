package model

import (
	"time"

	"checkout/internal/domain/def"
)

const (
	ItemIssuePriceChanged      = "PRICE_CHANGED"
	ItemIssueInsufficientStock = "INSUFFICIENT_STOCK"
	ItemIssueSoldOut           = "SOLD_OUT"
	ItemIssueNotFound          = "NOT_FOUND"
)

// ItemIssue tells the frontend exactly what changed so it can refresh and ask the shopper to confirm again.
type ItemIssue struct {
	ProductID      int64  `json:"product_id"`
	Name           string `json:"name,omitempty"`
	Reason         string `json:"reason"`
	ExpectedPrice  int64  `json:"expected_price"`
	CurrentPrice   int64  `json:"current_price"`
	RequestedQty   int    `json:"requested_qty"`
	AvailableStock int    `json:"available_stock"`
}

// ValidateItems compares what the shopper saw with the current product data.
// Policy: never silently apply a new price; reject and let the shopper re-confirm.
func ValidateItems(items []CheckoutItem, products map[int64]Product) *def.AppError {
	var issues []ItemIssue
	for _, it := range items {
		p, ok := products[it.ProductID]
		if !ok {
			issues = append(issues, ItemIssue{ProductID: it.ProductID, Reason: ItemIssueNotFound, RequestedQty: it.Quantity})
			continue
		}
		issue := ItemIssue{
			ProductID: p.ID, Name: p.Name,
			ExpectedPrice: it.UnitPrice, CurrentPrice: p.Price,
			RequestedQty: it.Quantity, AvailableStock: p.Stock,
		}
		switch {
		case p.Price != it.UnitPrice:
			issue.Reason = ItemIssuePriceChanged
		case p.Stock <= 0:
			issue.Reason = ItemIssueSoldOut
		case p.Stock < it.Quantity:
			issue.Reason = ItemIssueInsufficientStock
		default:
			continue
		}
		issues = append(issues, issue)
	}
	if len(issues) > 0 {
		return def.NewError(def.ErrItemsChanged, "商品資訊已更新，請確認後重新送出").WithDetails(map[string]any{"items": issues})
	}
	return nil
}

type OrderLine struct {
	ProductID int64  `json:"product_id"`
	Name      string `json:"name"`
	UnitPrice int64  `json:"unit_price"`
	Quantity  int    `json:"quantity"`
	Amount    int64  `json:"amount"`
}

type PriceBreakdown struct {
	Lines       []OrderLine `json:"lines"`
	Subtotal    int64       `json:"subtotal"`
	Discount    int64       `json:"discount"`
	ShippingFee int64       `json:"shipping_fee"`
	Donation    int64       `json:"donation"`
	Total       int64       `json:"total"`
}

// Quote is the single place that computes the order amount.
// It validates items, coupon and the amount the shopper confirmed.
// coupon must be non-nil when req.CouponCode is set (nil means "coupon not found").
func Quote(req CheckoutRequest, products map[int64]Product, coupon *Coupon, now time.Time) (PriceBreakdown, *def.AppError) {
	if err := ValidateItems(req.Items, products); err != nil {
		return PriceBreakdown{}, err
	}
	region, ok := FindShippingRegion(req.Shipping.Region)
	if !ok {
		return PriceBreakdown{}, def.NewError(def.ErrInvalidRequest, "shipping.region is invalid")
	}

	var b PriceBreakdown
	for _, it := range req.Items {
		p := products[it.ProductID]
		amount := p.Price * int64(it.Quantity)
		b.Lines = append(b.Lines, OrderLine{ProductID: p.ID, Name: p.Name, UnitPrice: p.Price, Quantity: it.Quantity, Amount: amount})
		b.Subtotal += amount
	}

	if req.CouponCode != "" {
		if coupon == nil {
			return PriceBreakdown{}, def.NewError(def.ErrCouponInvalid, "折扣碼不存在").WithDetails(map[string]string{"coupon_code": req.CouponCode})
		}
		discount, err := coupon.Discount(b.Subtotal, now)
		if err != nil {
			return PriceBreakdown{}, err
		}
		b.Discount = discount
	}

	b.ShippingFee = region.Fee
	b.Donation = req.Donation
	b.Total = b.Subtotal - b.Discount + b.ShippingFee + b.Donation

	if b.Total != req.ExpectedTotal {
		return PriceBreakdown{}, def.NewError(def.ErrAmountMismatch, "訂單金額已變動，請確認後重新送出").
			WithDetails(map[string]any{"expected_total": req.ExpectedTotal, "actual": b})
	}
	return b, nil
}
