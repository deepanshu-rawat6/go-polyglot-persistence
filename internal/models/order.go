package models

import "time"

type Order struct {
	ID          string    `json:"id"`
	ProductName string    `json:"product_name"`
	Amount      float64   `json:"amount"`
	CreatedAt   time.Time `json:"created_at"`
}
