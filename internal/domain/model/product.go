package model

import "time"

type Product struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	Highlights    []string  `json:"highlights"`
	ImageUrl      string    `json:"image_url"`
	Price         int64     `json:"price"`
	OriginalPrice int64     `json:"original_price"`
	Stock         int       `json:"stock"`
	IsAddon       bool      `json:"is_addon"`
	Version       int64     `json:"version"`
	UpdatedAt     time.Time `json:"updated_at"`
}
