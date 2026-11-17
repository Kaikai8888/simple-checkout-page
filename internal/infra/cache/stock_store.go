package cache

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// StockStore keeps an "available to reserve" counter per product in Valkey:
//
//	stock:{id}               = DB stock - units reserved by in-flight checkouts
//	reservation:{draft_id}   = hash {product_id: qty} held by one checkout
//
// Reserving happens synchronously when the shopper submits, so "who gets the last unit" is decided by the order
// in which requests hit Valkey (single-threaded, atomic Lua), independent of Kafka partition / worker ordering.
// The DB conditional update (stock >= qty) stays as the final guard against overselling.
//
// Note: scripts touch several keys; on a Valkey cluster they would need hash tags to live in one slot.
type StockStore struct {
	client *redis.Client
}

func NewStockStore(client *redis.Client) *StockStore {
	return &StockStore{client: client}
}

const stockKeyPrefix = "stock:"

func stockKey(id int64) string             { return stockKeyPrefix + strconv.FormatInt(id, 10) }
func reservationKey(draftID string) string { return "reservation:" + draftID }

// KEYS[1] = reservation key, KEYS[2..] = stock keys
// ARGV[2i], ARGV[2i+1] = product id, qty for KEYS[i+1]
// returns {1} ok | {-1, idx} stock key missing | {0, idx, available} not enough stock
var reserveScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return {1} end
for i = 2, #KEYS do
  local v = redis.call('GET', KEYS[i])
  if not v then return {-1, i - 1} end
  if tonumber(v) < tonumber(ARGV[2 * (i - 1) + 1]) then return {0, i - 1, tonumber(v)} end
end
for i = 2, #KEYS do
  local qty = tonumber(ARGV[2 * (i - 1) + 1])
  redis.call('DECRBY', KEYS[i], qty)
  redis.call('HSET', KEYS[1], ARGV[2 * (i - 1)], qty)
end
return {1}
`)

// KEYS[1] = reservation key, ARGV[1] = stock key prefix. Idempotent: a second release is a no-op.
var releaseScript = redis.NewScript(`
local items = redis.call('HGETALL', KEYS[1])
if #items == 0 then return 0 end
for i = 1, #items, 2 do
  redis.call('INCRBY', ARGV[1] .. items[i], tonumber(items[i + 1]))
end
redis.call('DEL', KEYS[1])
return 1
`)

type ReserveItem struct {
	ProductID int64
	Quantity  int
}

type ReserveResult struct {
	OK                bool
	MissingProductID  int64 // stock counter not initialised; caller should init and retry
	ShortProductID    int64 // not enough stock for this product
	AvailableForShort int
}

func (s *StockStore) Reserve(ctx context.Context, draftID string, items []ReserveItem) (ReserveResult, error) {
	keys := []string{reservationKey(draftID)}
	args := []any{"unused"} // ARGV[1] is a placeholder so pairs start at ARGV[2]
	for _, it := range items {
		keys = append(keys, stockKey(it.ProductID))
		args = append(args, it.ProductID, it.Quantity)
	}
	res, err := reserveScript.Run(ctx, s.client, keys, args...).Int64Slice()
	if err != nil {
		return ReserveResult{}, fmt.Errorf("reserve stock: %w", err)
	}
	switch res[0] {
	case 1:
		return ReserveResult{OK: true}, nil
	case -1:
		return ReserveResult{MissingProductID: items[res[1]-1].ProductID}, nil
	default:
		return ReserveResult{ShortProductID: items[res[1]-1].ProductID, AvailableForShort: int(res[2])}, nil
	}
}

// Release gives reserved units back (checkout failed). Returns false if there was nothing to release.
func (s *StockStore) Release(ctx context.Context, draftID string) (bool, error) {
	n, err := releaseScript.Run(ctx, s.client, []string{reservationKey(draftID)}, stockKeyPrefix).Int()
	if err != nil {
		return false, fmt.Errorf("release stock: %w", err)
	}
	return n == 1, nil
}

// Commit drops the reservation record without giving units back: the order now owns them (DB stock was deducted).
func (s *StockStore) Commit(ctx context.Context, draftID string) error {
	return s.client.Del(ctx, reservationKey(draftID)).Err()
}

// Add returns units to the counter (e.g. unpaid order expired and DB stock was restored).
func (s *StockStore) Add(ctx context.Context, productID int64, qty int) error {
	return s.client.IncrBy(ctx, stockKey(productID), int64(qty)).Err()
}

func (s *StockStore) InitIfAbsent(ctx context.Context, productID int64, available int) error {
	return s.client.SetNX(ctx, stockKey(productID), available, 0).Err()
}

func (s *StockStore) Set(ctx context.Context, productID int64, available int) error {
	return s.client.Set(ctx, stockKey(productID), available, 0).Err()
}

// GetMany returns available counters and the ids whose counter is not initialised.
func (s *StockStore) GetMany(ctx context.Context, ids []int64) (map[int64]int, []int64, error) {
	if len(ids) == 0 {
		return map[int64]int{}, nil, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = stockKey(id)
	}
	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, nil, fmt.Errorf("mget stock: %w", err)
	}
	found := make(map[int64]int, len(ids))
	var missing []int64
	for i, v := range values {
		str, ok := v.(string)
		if !ok {
			missing = append(missing, ids[i])
			continue
		}
		n, err := strconv.Atoi(str)
		if err != nil {
			missing = append(missing, ids[i])
			continue
		}
		found[ids[i]] = n
	}
	return found, missing, nil
}
