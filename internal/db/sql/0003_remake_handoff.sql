-- 放置过久重做的并发交接与成本归属：规则、重做工作流、补偿、责任复核
-- 可重复执行（IF NOT EXISTS / ON CONFLICT）

-- ---------- 杯：重做交接所需状态 ----------
-- 新增 status 取值 loss_hold = 原杯报损留存（不可再交接、旧封签码立即失效），
-- 与 queued/making/sealed/voided 并列（status 为 TEXT，无需枚举迁移）。
ALTER TABLE order_cups ADD COLUMN IF NOT EXISTS label_printed_at TIMESTAMPTZ;
COMMENT ON COLUMN order_cups.status IS 'queued / making / sealed / loss_hold / voided';

-- ---------- 过久判定规则：饮品类型 × 冰热 × 门店忙闲 ----------
-- safe_minutes：达到即判定“放置过久”建议/触发重做
-- hard_safe_minutes：硬安全窗口；重做进行中骑手到店，未超过该窗口且新杯贴未打印时允许取原杯
CREATE TABLE IF NOT EXISTS stale_rules (
    id               BIGSERIAL PRIMARY KEY,
    category         TEXT NOT NULL,           -- americano/latte/cold_brew/fruit_coffee/light_food/combo/default
    temp             TEXT NOT NULL,           -- hot / ice / any
    busyness         TEXT NOT NULL,           -- normal / busy / any
    safe_minutes     INTEGER NOT NULL,
    hard_safe_minutes INTEGER NOT NULL,
    UNIQUE (category, temp, busyness)
);

INSERT INTO stale_rules(category,temp,busyness,safe_minutes,hard_safe_minutes) VALUES
 ('default','hot','normal',15,20),
 ('default','hot','busy',20,25),
 ('default','ice','normal',25,35),
 ('default','ice','busy',30,40),
 ('americano','ice','any',25,35),
 ('latte','ice','any',25,35),
 ('cold_brew','ice','any',40,55),
 ('fruit_coffee','ice','normal',20,30),
 ('fruit_coffee','ice','busy',25,35),
 ('light_food','any','any',30,45),
 ('combo','hot','normal',15,20),
 ('combo','hot','busy',20,25),
 ('combo','ice','normal',25,35),
 ('combo','ice','busy',30,40)
ON CONFLICT (category,temp,busyness) DO NOTHING;

-- ---------- 重做工作流（并发交接的唯一事实源） ----------
CREATE TABLE IF NOT EXISTS remakes (
    id                    BIGSERIAL PRIMARY KEY,
    store_id              BIGINT NOT NULL REFERENCES stores(id),
    order_id              BIGINT NOT NULL REFERENCES orders(id),
    old_cup_id            BIGINT NOT NULL REFERENCES order_cups(id),
    new_cup_id            BIGINT REFERENCES order_cups(id),
    exception_id          BIGINT REFERENCES exceptions(id),
    reason                TEXT NOT NULL,        -- stale_drink / wrong_cup / note_missed
    trigger_type          TEXT NOT NULL,        -- manual / auto_stale
    -- in_progress：重做中（原杯 loss_hold）；new_ready：新杯已封待交接；
    -- completed：流程结束（wait_new 交接完成 / take_original 取原杯）；aborted：顾客取消终止
    status                TEXT NOT NULL DEFAULT 'in_progress',
    factors               JSONB NOT NULL DEFAULT '{}', -- 饮品类型/冰热/放置时长/门店忙闲/阈值等判定因子
    old_seal_code         TEXT,
    safety_deadline       TIMESTAMPTZ,          -- 原杯硬安全窗口结束时间
    new_label_printed_at  TIMESTAMPTZ,          -- 新杯贴是否已打印（交接决策因子）
    decision              TEXT,                 -- wait_new / take_original / abort
    decided_by            BIGINT,
    decided_at            TIMESTAMPTZ,
    completed_at          TIMESTAMPTZ,
    aborted_at            TIMESTAMPTZ,
    abort_reason          TEXT,
    created_by            BIGINT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_remakes_order ON remakes(order_id);
CREATE INDEX IF NOT EXISTS idx_remakes_status ON remakes(status) WHERE status IN ('in_progress','new_ready');
CREATE INDEX IF NOT EXISTS idx_remakes_oldcup ON remakes(old_cup_id);

-- ---------- 顾客补偿方案（重做等待自动生成；客服确认兑现） ----------
CREATE TABLE IF NOT EXISTS compensations (
    id              BIGSERIAL PRIMARY KEY,
    store_id        BIGINT NOT NULL REFERENCES stores(id),
    order_id        BIGINT NOT NULL REFERENCES orders(id),
    remake_id       BIGINT REFERENCES remakes(id),
    exception_id    BIGINT REFERENCES exceptions(id),
    reason          TEXT NOT NULL,             -- stale_remake / complaint
    planned_amount  NUMERIC(10,2) NOT NULL,
    plan_detail     JSONB NOT NULL DEFAULT '{}',
    -- proposed：方案已生成顾客可见；confirmed：已复核；fulfilled：客服确认已兑现；voided：订单取消等作废
    status          TEXT NOT NULL DEFAULT 'proposed',
    proposed_by     BIGINT,
    confirmed_by    BIGINT,
    fulfilled_by    BIGINT,
    fulfilled_at    TIMESTAMPTZ,
    fulfill_note    TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_comps_order ON compensations(order_id);
CREATE INDEX IF NOT EXISTS idx_comps_status ON compensations(status);

-- ---------- 责任复核（店长申请 / 区域经理改判；只追加，不覆盖原始事件） ----------
CREATE TABLE IF NOT EXISTS responsibility_reviews (
    id                      BIGSERIAL PRIMARY KEY,
    store_id                BIGINT NOT NULL REFERENCES stores(id),
    order_id                BIGINT NOT NULL REFERENCES orders(id),
    exception_id            BIGINT NOT NULL REFERENCES exceptions(id),
    request_type            TEXT NOT NULL,     -- equipment_fault / platform_dispatch_delay
    reason                  TEXT NOT NULL,
    evidence                JSONB NOT NULL DEFAULT '{}',
    status                  TEXT NOT NULL DEFAULT 'requested', -- requested / approved / rejected
    requested_by            BIGINT,
    original_responsibility TEXT NOT NULL,
    new_responsibility      TEXT,              -- 改判后 store/team/platform/rider/shared/customer
    reviewed_by             BIGINT,
    reviewed_at             TIMESTAMPTZ,
    decision_note           TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_reviews_exc ON responsibility_reviews(exception_id);
CREATE INDEX IF NOT EXISTS idx_reviews_status ON responsibility_reviews(status);

-- 责任改判产生的财务追加记录（冲销+重挂）都通过 finance_records 追加：
-- 备注中保留原记录 id，period_date 沿用原记录日期，以回算当日班组利润；原始行不更新、不删除。
