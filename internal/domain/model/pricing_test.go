package model

import (
	"testing"
	"time"

	"checkout/internal/domain/def"
)

func TestQuote(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	products := map[int64]Product{
		1: {ID: 1, Name: "特典同捆", Price: 5500, Stock: 10},
		2: {ID: 2, Name: "典藏海報", Price: 300, Stock: 1},
	}
	fixed500 := &Coupon{Code: "RETURN500", DiscountType: def.DiscountTypeFixed, Value: 500, MinSubtotal: 3000, Active: true}
	limit := 1
	usedUp := &Coupon{Code: "LIMIT1", DiscountType: def.DiscountTypeFixed, Value: 100, UsageLimit: &limit, UsedCount: 1, Active: true}

	baseReq := func() CheckoutRequest {
		return CheckoutRequest{
			Items:         []CheckoutItem{{ProductID: 1, Quantity: 1, UnitPrice: 5500}, {ProductID: 2, Quantity: 1, UnitPrice: 300}},
			Donation:      100,
			Shipping:      ShippingInfo{Region: "TW_OUTLYING"},
			ExpectedTotal: 5500 + 300 + 150 + 100,
		}
	}

	tests := []struct {
		name      string
		mutate    func(r *CheckoutRequest)
		coupon    *Coupon
		wantCode  def.ErrorCode
		wantTotal int64
	}{
		{name: "正常計算：商品小計 + 離島運費 + 加碼贊助", mutate: func(r *CheckoutRequest) {}, wantTotal: 6050},
		{name: "套用固定金額折扣碼", mutate: func(r *CheckoutRequest) { r.CouponCode = "RETURN500"; r.ExpectedTotal -= 500 }, coupon: fixed500, wantTotal: 5550},
		{name: "前端看到的價格與目前價格不同，應回 ITEMS_CHANGED", mutate: func(r *CheckoutRequest) { r.Items[0].UnitPrice = 5000 }, wantCode: def.ErrItemsChanged},
		{name: "庫存不足，應回 ITEMS_CHANGED", mutate: func(r *CheckoutRequest) { r.Items[1].Quantity = 2; r.ExpectedTotal += 300 }, wantCode: def.ErrItemsChanged},
		{name: "商品不存在，應回 ITEMS_CHANGED", mutate: func(r *CheckoutRequest) { r.Items[1].ProductID = 99 }, wantCode: def.ErrItemsChanged},
		{name: "折扣碼不存在", mutate: func(r *CheckoutRequest) { r.CouponCode = "NOPE" }, wantCode: def.ErrCouponInvalid},
		{name: "折扣碼已用完", mutate: func(r *CheckoutRequest) { r.CouponCode = "LIMIT1" }, coupon: usedUp, wantCode: def.ErrCouponInvalid},
		{name: "前端確認的總額與後端計算不同", mutate: func(r *CheckoutRequest) { r.ExpectedTotal = 1 }, wantCode: def.ErrAmountMismatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := baseReq()
			tt.mutate(&req)
			got, err := Quote(req, products, tt.coupon, now)
			if tt.wantCode != "" {
				if err == nil || err.Code != tt.wantCode {
					t.Fatalf("want error %s, got %v", tt.wantCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Total != tt.wantTotal {
				t.Fatalf("want total %d, got %d", tt.wantTotal, got.Total)
			}
		})
	}
}

func TestCouponPercentDiscount(t *testing.T) {
	c := Coupon{Code: "SAVE10", DiscountType: def.DiscountTypePercent, Value: 10, Active: true}
	got, err := c.Discount(5555, time.Now())
	if err != nil || got != 555 {
		t.Fatalf("want 555, got %d (%v)", got, err)
	}
}
