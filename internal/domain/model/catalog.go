package model

type ShippingRegion struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Fee      int64  `json:"fee"`
	Domestic bool   `json:"domestic"`
}

// ShippingRegions is the source of truth for shipping fees (frontend fetches it via /api/checkout/options).
var ShippingRegions = []ShippingRegion{
	{Code: "TW_MAIN", Name: "台灣（本島） Taiwan", Fee: 0, Domestic: true},
	{Code: "TW_OUTLYING", Name: "台灣（離島） Taiwan Outlying Islands", Fee: 150, Domestic: true},
	{Code: "JP", Name: "日本 Japan", Fee: 500},
	{Code: "US", Name: "美國 United States", Fee: 800},
	{Code: "AU", Name: "澳洲 Australia", Fee: 800},
}

func FindShippingRegion(code string) (ShippingRegion, bool) {
	for _, r := range ShippingRegions {
		if r.Code == code {
			return r, true
		}
	}
	return ShippingRegion{}, false
}

type PaymentMethod struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

var PaymentMethods = []PaymentMethod{
	{Code: "credit_card", Name: "信用卡付款", Description: "台新、玉山享 3 期 / 6 期零利率、可用銀聯卡、可用國外卡"},
	{Code: "aftee", Name: "AFTEE", Description: "安心便利，免註冊免綁卡（收到繳費通知 14 天內完成即可）"},
	{Code: "atm", Name: "ATM 轉帳或銀行臨櫃繳款", Description: "需於指定時間內完成付款，超過時限則會取消交易"},
}

func FindPaymentMethod(code string) (PaymentMethod, bool) {
	for _, m := range PaymentMethods {
		if m.Code == code {
			return m, true
		}
	}
	return PaymentMethod{}, false
}
