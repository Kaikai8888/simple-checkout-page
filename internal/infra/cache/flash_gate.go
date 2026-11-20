package cache

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// FlashSaleGate is a Valkey counter per flash-sale product ("gate:{id}") that sits IN FRONT of the database.
//
// During a flash sale thousands of shoppers hit the same product row. Without the gate every one of them would
// queue on that row's lock in Postgres. The gate takes units atomically in memory (single-threaded Lua), so only
// roughly <stock> requests ever reach the DB and the rest are rejected immediately.
//
// The gate is NOT the source of truth - products.available_stock is. If the gate drifts (crash between gate and
// DB, Valkey restart, ...) the worst case is rejecting a few shoppers too early or letting a few extra requests
// through to the DB, where the conditional UPDATE rejects them. It can never cause overselling, and
// the standalone gate-sync job (cmd/gatesync) periodically realigns it with the DB.
//
// Note: scripts touch several keys; on a Valkey cluster they would need hash tags to live in one slot.
type FlashSaleGate struct {
	client *redis.Client
}

func NewFlashSaleGate(client *redis.Client) *FlashSaleGate {
	return &FlashSaleGate{client: client}
}

func gateKey(id int64) string { return "gate:" + strconv.FormatInt(id, 10) }

// KEYS[i] = gate key, ARGV[i] = qty. All-or-nothing.
// returns {1} ok | {-1, i, j, ...} gates not initialised | {0, i, available} not enough
//
// Once a missing gate is found, the remaining keys are only scanned for other missing gates, so the caller can
// initialise all of them in one round instead of one per retry. A shortage seen before any missing gate is final.
var takeScript = redis.NewScript(`
local missing = {-1}
for i = 1, #KEYS do
  local v = redis.call('GET', KEYS[i])
  if not v then
    missing[#missing + 1] = i
  elseif #missing == 1 and tonumber(v) < tonumber(ARGV[i]) then
    return {0, i, tonumber(v)}
  end
end
if #missing > 1 then return missing end
for i = 1, #KEYS do
  redis.call('DECRBY', KEYS[i], ARGV[i])
end
return {1}
`)

// Only refill gates that exist; a missing gate is rebuilt from the DB on next use.
var giveScript = redis.NewScript(`
for i = 1, #KEYS do
  if redis.call('EXISTS', KEYS[i]) == 1 then redis.call('INCRBY', KEYS[i], ARGV[i]) end
end
return 1
`)

type GateItem struct {
	ProductID int64
	Quantity  int
}

type GateResult struct {
	OK                bool
	MissingProductIDs []int64 // gates not initialised; caller should init all of them and retry
	ShortProductID    int64
	Available         int
}

func (g *FlashSaleGate) Take(ctx context.Context, items []GateItem) (GateResult, error) {
	keys, args := gateArgs(items)
	res, err := takeScript.Run(ctx, g.client, keys, args...).Int64Slice()
	if err != nil {
		return GateResult{}, fmt.Errorf("flash gate take: %w", err)
	}
	switch res[0] {
	case 1:
		return GateResult{OK: true}, nil
	case -1:
		missing := make([]int64, 0, len(res)-1)
		for _, idx := range res[1:] {
			missing = append(missing, items[idx-1].ProductID)
		}
		return GateResult{MissingProductIDs: missing}, nil
	default:
		return GateResult{ShortProductID: items[res[1]-1].ProductID, Available: int(res[2])}, nil
	}
}

func (g *FlashSaleGate) Give(ctx context.Context, items []GateItem) error {
	if len(items) == 0 {
		return nil
	}
	keys, args := gateArgs(items)
	return giveScript.Run(ctx, g.client, keys, args...).Err()
}

// InitManyIfAbsent creates the missing gates in one round trip. SETNX keeps a gate another request created meanwhile.
func (g *FlashSaleGate) InitManyIfAbsent(ctx context.Context, available map[int64]int) error {
	if len(available) == 0 {
		return nil
	}
	pipe := g.client.Pipeline()
	for id, n := range available {
		pipe.SetNX(ctx, gateKey(id), n, 0)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// Set realigns the gate with the DB's available_stock.
func (g *FlashSaleGate) Set(ctx context.Context, productID int64, available int) error {
	return g.client.Set(ctx, gateKey(productID), available, 0).Err()
}

func (g *FlashSaleGate) Delete(ctx context.Context, productID int64) error {
	return g.client.Del(ctx, gateKey(productID)).Err()
}

func gateArgs(items []GateItem) ([]string, []any) {
	keys := make([]string, len(items))
	args := make([]any, len(items))
	for i, it := range items {
		keys[i] = gateKey(it.ProductID)
		args[i] = it.Quantity
	}
	return keys, args
}
