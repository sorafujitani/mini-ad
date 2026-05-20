-- mini-ad: 共通スキーマ
-- Step 6 以降で参照されます。
-- nix run .#db-create で投入されます。

-- ---------------------------------------------------------------
-- Advertiser: 広告主。請求単位。
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS advertisers (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------
-- Campaign: キャンペーン。広告主が「夏キャンペーン」のように切る単位。
--   - 期間と総予算を持つ
--   - 配信中フラグ (status) を持つ
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS campaigns (
    id            BIGSERIAL PRIMARY KEY,
    advertiser_id BIGINT NOT NULL REFERENCES advertisers(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'active',   -- 'active' | 'paused' | 'archived'
    starts_at     TIMESTAMPTZ NOT NULL,
    ends_at       TIMESTAMPTZ NOT NULL,
    daily_budget_cents  BIGINT NOT NULL DEFAULT 0,  -- 1 日の予算上限
    total_budget_cents  BIGINT NOT NULL DEFAULT 0,  -- 期間中の総予算
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------
-- LineItem (= ad group): 配信戦術の単位。ターゲティングと入札額を持つ。
--   - 同じキャンペーンの中に複数置ける（例: 男性向け / 女性向け）
--   - bid_cpm_cents: 1000 imp 単位の入札額
--   - targeting: ジオ・デバイス・キーワード等を JSON で保持（学習用に簡略化）
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS line_items (
    id             BIGSERIAL PRIMARY KEY,
    campaign_id    BIGINT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'active',
    bid_cpm_cents  BIGINT NOT NULL,
    targeting      JSONB NOT NULL DEFAULT '{}'::jsonb,
    frequency_cap_per_day INT NOT NULL DEFAULT 0,    -- 0 = no cap
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_line_items_campaign ON line_items(campaign_id);

-- ---------------------------------------------------------------
-- Creative: 実際に表示される素材 (画像 URL / リンク先 / サイズ)
-- 1 LineItem に複数 Creative を紐付けて A/B テストする想定。
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS creatives (
    id           BIGSERIAL PRIMARY KEY,
    line_item_id BIGINT NOT NULL REFERENCES line_items(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    image_url    TEXT NOT NULL,
    click_url    TEXT NOT NULL,
    width        INT NOT NULL,
    height       INT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_creatives_line_item ON creatives(line_item_id);

-- ---------------------------------------------------------------
-- Events: トラッキングログ。impression / click をここに書く。
-- Step 8 のレポーティングで集計対象になる。
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS events (
    id           BIGSERIAL PRIMARY KEY,
    event_type   TEXT NOT NULL,                     -- 'impression' | 'click'
    creative_id  BIGINT NOT NULL,
    line_item_id BIGINT NOT NULL,
    campaign_id  BIGINT NOT NULL,
    user_id      TEXT,
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_events_occurred ON events(occurred_at);
CREATE INDEX IF NOT EXISTS idx_events_campaign ON events(campaign_id);

-- ---------------------------------------------------------------
-- 初期データ（学習用サンプル）
-- ---------------------------------------------------------------
INSERT INTO advertisers (id, name) VALUES
    (1, 'Acme Shoes'),
    (2, 'Globex Coffee')
ON CONFLICT (id) DO NOTHING;

INSERT INTO campaigns (id, advertiser_id, name, status, starts_at, ends_at, daily_budget_cents, total_budget_cents) VALUES
    (1, 1, 'Acme Summer 2026', 'active', now() - interval '1 day', now() + interval '30 days', 100000, 3000000),
    (2, 2, 'Globex Morning Brew', 'active', now() - interval '1 day', now() + interval '30 days',  50000, 1500000)
ON CONFLICT (id) DO NOTHING;

INSERT INTO line_items (id, campaign_id, name, bid_cpm_cents, targeting, frequency_cap_per_day) VALUES
    (1, 1, 'Acme JP mobile',   300, '{"countries":["JP"], "devices":["mobile"]}'::jsonb, 5),
    (2, 1, 'Acme global desktop', 200, '{"devices":["desktop"]}'::jsonb, 0),
    (3, 2, 'Globex JP all',    250, '{"countries":["JP"]}'::jsonb, 3)
ON CONFLICT (id) DO NOTHING;

INSERT INTO creatives (id, line_item_id, name, image_url, click_url, width, height) VALUES
    (1, 1, 'Acme banner A', 'https://placehold.co/300x250/orange/white?text=Acme+Shoes', 'https://example.com/acme/lp1', 300, 250),
    (2, 1, 'Acme banner B', 'https://placehold.co/300x250/red/white?text=Acme+Sale',    'https://example.com/acme/lp2', 300, 250),
    (3, 2, 'Acme desktop',  'https://placehold.co/728x90/blue/white?text=Acme+Big',     'https://example.com/acme/lp3', 728,  90),
    (4, 3, 'Globex coffee', 'https://placehold.co/300x250/brown/white?text=Globex',     'https://example.com/globex',   300, 250)
ON CONFLICT (id) DO NOTHING;

-- BIGSERIAL のシーケンスを進めておく
SELECT setval('advertisers_id_seq', GREATEST((SELECT MAX(id) FROM advertisers), 1));
SELECT setval('campaigns_id_seq',   GREATEST((SELECT MAX(id) FROM campaigns), 1));
SELECT setval('line_items_id_seq',  GREATEST((SELECT MAX(id) FROM line_items), 1));
SELECT setval('creatives_id_seq',   GREATEST((SELECT MAX(id) FROM creatives), 1));
