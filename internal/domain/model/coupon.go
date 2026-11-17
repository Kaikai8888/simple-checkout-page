package model

import (
	"time"

	"checkout/internal/domain/def"
)

type Coupon struct {
	Code         string     `json:"code"`
	Description  string     `json:"description"`
	DiscountType string     `json:"discount_type"`
	Value        int64      `json:"value"`
	MinSubtotal  int64      `json:"min_subtotal"`
	UsageLimit   *int       `json:"usage_limit,omitempty"`
	UsedCount    int        `json:"used_count"`
	Active       bool       `json:"active"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

// Discount returns the discount amount for the given product subtotal.
func (c Coupon) Discount(subtotal int64, now time.Time) (int64, *def.AppError) {
	invalid := func(msg string) (int64, *def.AppError) {
		return 0, def.NewError(def.ErrCouponInvalid, msg).WithDetails(map[string]string{"coupon_code": c.Code})
	}
	if !c.Active {
		return invalid("折扣碼已停用")
	}
	if c.ExpiresAt != nil && now.After(*c.ExpiresAt) {
		return invalid("折扣碼已過期")
	}
	if c.UsageLimit != nil && c.UsedCount >= *c.UsageLimit {
		return invalid("折扣碼已達使用上限")
	}
	if subtotal < c.MinSubtotal {
		return invalid("未達折扣碼最低消費金額")
	}

	var discount int64
	switch c.DiscountType {
	case def.DiscountTypeFixed:
		discount = c.Value
	case def.DiscountTypePercent:
		discount = subtotal * c.Value / 100
	default:
		return invalid("折扣碼設定錯誤")
	}
	if discount > subtotal {
		discount = subtotal
	}
	return discount, nil
}
