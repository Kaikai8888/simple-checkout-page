package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"checkout/internal/domain/model"
)

const productTTL = 24 * time.Hour

// ProductCache caches product info (name, price, ...). The cached stock fields are not used: ProductService
// overlays live stock from ProductRepo.GetStockLevels, because available_stock changes on every checkout.
type ProductCache struct {
	client *redis.Client
}

func NewProductCache(client *redis.Client) *ProductCache {
	return &ProductCache{client: client}
}

func productKey(id int64) string { return "product:" + strconv.FormatInt(id, 10) }

// GetMany returns cached products and the ids that were not in cache.
func (c *ProductCache) GetMany(ctx context.Context, ids []int64) (map[int64]model.Product, []int64, error) {
	if len(ids) == 0 {
		return map[int64]model.Product{}, nil, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = productKey(id)
	}
	values, err := c.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, ids, fmt.Errorf("mget products: %w", err)
	}
	found := make(map[int64]model.Product, len(ids))
	var missing []int64
	for i, v := range values {
		s, ok := v.(string)
		if !ok {
			missing = append(missing, ids[i])
			continue
		}
		var p model.Product
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			missing = append(missing, ids[i])
			continue
		}
		found[ids[i]] = p
	}
	return found, missing, nil
}

func (c *ProductCache) SetMany(ctx context.Context, products []model.Product) error {
	if len(products) == 0 {
		return nil
	}
	pipe := c.client.Pipeline()
	for _, p := range products {
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		pipe.Set(ctx, productKey(p.ID), b, productTTL)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (c *ProductCache) Delete(ctx context.Context, ids ...int64) error {
	if len(ids) == 0 {
		return nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = productKey(id)
	}
	return c.client.Del(ctx, keys...).Err()
}

func IsNil(err error) bool { return errors.Is(err, redis.Nil) }
