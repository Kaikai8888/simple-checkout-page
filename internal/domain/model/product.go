package model

import "time"

type Product struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Highlights     []string  `json:"highlights"`
	ImageUrl       string    `json:"image_url"`
	Price          int64     `json:"price"`
	OriginalPrice  int64     `json:"original_price"`
	Stock          int       `json:"stock"`           // on hand, not yet sold to an order
	AvailableStock int       `json:"available_stock"` // what shoppers can still buy (excludes in-flight reservations)
	FlashSale      bool      `json:"flash_sale"`      // hot product: reservations go through the Valkey gate first
	IsAddon        bool      `json:"is_addon"`
	Version        int64     `json:"version"`
	UpdatedAt      time.Time `json:"updated_at"`
}
