INSERT INTO products (id, name, description, highlights, image_url, price, original_price, stock, is_addon) VALUES
(1, '特典同捆', '10/21 截止！輸入折扣碼【RETURN500】再折 500 元',
    ARRAY['本島免運', '《台北大空襲 10 週年紀念版》含〈天火基隆〉擴充，集資結束即絕版', '《高雄大空襲》完全版含本體與〈戰爭保險〉擴充', '人物皆為高妍全新繪製', '二戰新書《白人俘虜》', '2027 月曆《戰時紀事曆》', '集資限定版《一九四五》電影海報'],
    '', 5500, 6960, 10, FALSE),
(2, '台北大空襲 10 週年紀念版', '單入桌遊本體，含〈天火基隆〉擴充',
    ARRAY['本島免運', '可寄送至美國、澳洲、日本等地區'],
    '', 1980, 2480, 100, FALSE),
(3, '限量收藏卡（已售完示範）', '用來示範售完情境',
    ARRAY['數量限定'],
    '', 990, 1200, 0, FALSE),
(101, '加購：《一九四五》電影海報', '集資限定版', ARRAY[]::TEXT[], '', 300, 400, 50, TRUE),
(102, '加購：空襲標記金屬幣', '正反面設計，數量有限', ARRAY[]::TEXT[], '', 150, 200, 3, TRUE);

SELECT setval('products_id_seq', 1000);

INSERT INTO coupons (code, description, discount_type, value, min_subtotal, usage_limit) VALUES
('RETURN500', '返鄉投票再折 500 元（滿 3,000）', 'fixed', 500, 3000, NULL),
('SAVE10', '全館 9 折', 'percent', 10, 0, NULL),
('ONCE100', '限量 1 次的 100 元折扣（示範使用上限）', 'fixed', 100, 0, 1);
