# 關鍵設計決策

> 本文為 [README.md](../README.md) 中「Key design decisions」章節的中文翻譯。若內容有出入，以 README 為準。

## 1. 購物者載入頁面後，價格或庫存變動了

前端會送出畫面上顯示的單價，以及購物者確認過的 `expected_total`。伺服器**絕不會默默套用新價格**，而是以 `ITEMS_CHANGED`（每個品項附原因：`PRICE_CHANGED`、`INSUFFICIENT_STOCK`、`SOLD_OUT`、`NOT_FOUND`）或 `AMOUNT_MISMATCH` 拒絕。頁面會重新取得商品資料、更新畫面，並請購物者再確認一次。

## 2. 透過 Kafka 非同步建立訂單

`POST /api/checkouts` 只負責寫入 draft 並回傳 `202`。負擔重、競爭激烈的工作（row lock、扣庫存）交給 consumer group 處理，讓流量尖峰被平滑消化，而不是在 request handler 裡堆積大量 DB 連線。

- Topic `checkout.requested` 有 3 個 partition，以 `draft_id` 作為 key。
- Offset 在處理**完成後**才 commit（at-least-once delivery）。
- 重複的訊息不會造成影響：
  - worker 以 `UPDATE … WHERE status='pending'` 認領 draft；
  - `orders.draft_id` 設有 `UNIQUE`。
- 處理失敗的 draft 不會卡住 partition：遇到暫時性錯誤時，draft 會被放回 `pending`、標記為未發佈，並設定 `next_retry_at` = 現在 + backoff（2 秒、4 秒；嘗試 3 次後判定失敗）。舊訊息的 offset 照常 commit，等 backoff 時間到了，再由 reclaim job 發佈一則**新的**訊息。
- **Postgres 中的 draft 資料列才是真相來源（source of truth）**，並且記錄了它的訊息是否已在 Kafka 中（`published_at`）。Kafka 不會遺失已確認的訊息（`acks=all`），因此 draft 缺少訊息只會是因為發佈失敗，而不會是因為 queue 處理得慢：
  - `Submit` 會立即發佈並寫入 `published_at`。若發佈失敗，reclaim job 會在 5 秒的寬限期過後補發。
  - Reclaim job（每 5 秒）只發佈「`pending`、未發佈、且已到期」的 draft。**已發佈但仍是 pending 的 draft 只是在 queue 中排隊**，不論 consumer lag 多大都不會被重送。鎖定資料列、發佈、標記已發佈在同一個 transaction 內完成，因此發佈失敗時不會留下任何標記。
  - **Worker 掛掉：** Kafka 會重新投遞它的訊息，但因為 draft 仍是 `processing`，`Claim` 會拒絕。因此 `processing` 超過 60 秒（lease）的 draft 會被改回 `pending` 並重新發佈。每次處理最多 30 秒，處理慢但仍存活的 worker 不會超過 lease。被認領超過 3 次（持續讓 worker 掛掉）的 draft 會被判定失敗。

## 3. 庫存預留：所有商品都用 PostgreSQL，限時搶購商品再加一道 Valkey gate

Kafka 只保證同一個 partition 內的訊息順序，而且有多個 consumer 時，完成順序本來就不等於送出順序；若只用單一 partition 又會失去平行處理能力。因此「誰搶到最後一件」是在**送出當下同步決定**的，而不是交給 queue。

每個商品有兩個數字：

| 欄位 | 意義 | 何時變動 |
|---|---|---|
| `stock` | 在手、尚未賣給訂單的數量 | 建立訂單（−）、未付款訂單過期（+） |
| `available_stock` | 購物者還能買的數量 = `stock` − 進行中結帳所佔用的數量 | 送出結帳（−）、結帳失敗或訂單過期（+） |

**所有商品。** `POST /api/checkouts` 會在**同一個 transaction** 內寫入 draft，並對每個品項執行 `UPDATE products SET available_stock = available_stock - qty WHERE id = ? AND available_stock >= qty`，同時在 `inventory_reservations` 新增一筆紀錄（`held → committed | released`）。

- 只有一個真相來源，不會出現「draft 存在但沒佔到庫存」（或反過來）的空窗。
- 依 product id 順序鎖定資料列，購物車內容重疊的結帳不會互相 deadlock。
- 只要有一個品項不足，整個 transaction 就 rollback（all-or-nothing）。
- 釋放庫存與將 draft 標記為失敗，也是在同一個 transaction 內完成。
- `CHECK (available_stock >= 0 AND available_stock <= stock)` 保證不會超賣。

**限時搶購商品**（`products.flash_sale`）。成千上萬的購物者同時更新*同一列*資料，會在該列的 row lock 上排隊、佔住 DB 連線，拖慢整個資料庫。因此這類商品會先經過一道 **Valkey gate**（`gate:{id}`，以 Lua 原子性、all-or-nothing 地扣減）：

- 只有大約 `available_stock` 數量的請求會抵達 DB，其餘的立即收到「已售完」。
- Gate 是**流量削減器（load shedder），不是真相來源**。若它產生漂移（gate 與 DB 之間發生 crash、Valkey 重啟），最壞情況只是提早拒絕少數購物者，或多放幾個請求進到 DB 再被 DB 拒絕，絕不會超賣。
- **初始化 gate。** App 啟動時不會建立 gate，因此不會等待 gate sync，也不會因它而啟動失敗。缺少的 gate 會在第一次使用時建立：Take 腳本會一次回報購物車中**所有**缺少的 gate（遇到第一個缺少的之後，只會繼續找其他缺少的），接著伺服器用一次查詢讀取它們的 `available_stock`，以 `SETNX` 建立後重試一次。
- **重新對齊。** 由獨立的 cronjob（`cmd/gatesync`，即 compose 中的 `gate-sync` 服務，每 30 秒一次）把每個 gate 設回 `available_stock`。若 Valkey 掛掉，gate 會 fail open，交由 DB 判斷。
- **記憶體上限。** Valkey 以 `maxmemory 256mb` 搭配 `volatile-lru` 執行：只有設定了 TTL 的 key（商品快取、session）會被淘汰。Gate 沒有 TTL，因此永遠不會被淘汰；若記憶體仍然滿了，gate 寫入會失敗，結帳改由 DB 判斷。

為什麼不全部都用 Valkey？那代表每條路徑都要調和兩個真相來源（crash 空窗、重啟、漂移）。只有少數熱門商品的 row lock 競爭才是真正瓶頸，這個成本只值得花在它們身上。

## 4. 更新商品時的 DB + 快取寫入

`PUT /api/admin/products/{id}`：

1. 在 DB transaction 內更新該列。
2. 在 **commit 之前**寫入 Valkey。若快取寫入失敗，就 rollback。
3. 若快取已寫入但 commit 失敗，就刪除該快取 key。

因此結果是「DB 與快取都成功，或什麼都沒改變」。讀取採用 cache-aside。

## 5. 付款

- 金流 client 會對請求簽章；callback 以 HMAC-SHA256 驗證，並重新核對金額。
- Callback 具冪等性：付款資料列會被鎖定，已處於最終狀態的付款會直接回應確認、不產生副作用。金流方對任何非 2xx 回應都會重試。
- 訂單過期後才到的付款會變成 `refund_required`，而不會遺失。
- 付款失敗後訂單仍維持 `pending_payment`，購物者可在 `expires_at` 前重試。超過期限後，job 會將訂單設為過期，並退回庫存與折扣碼使用次數。

## 6. 冪等送出

瀏覽器會針對每份確認過的內容送出一個 `Idempotency-Key`。以相同 key 重送時，會回傳同一個 draft，且不會再次預留庫存。

伺服器接受結帳後，頁面會把 `draft_id` 寫進 URL。重新整理、從付款頁按上一頁，或 60 秒輪詢逾時，都會接續**同一個 draft**：當 draft 還在處理中時，頁面絕不提供第二次送出，因為那會產生第二筆訂單與第二次庫存預留。若接續的 draft 已經完成，頁面會導向訂單頁，而不是默默開啟新的付款流程。
