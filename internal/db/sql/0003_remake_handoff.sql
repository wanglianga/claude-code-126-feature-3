-- 特性 3：放置过久重做的并发交接与成本归属
-- 可重复执行（IF NOT EXISTS / ADD COLUMN IF NOT EXISTS）

-- ---------- 重做任务（一次重做 = 一个状态机） ----------
-- 生命周期：
--   remaking  重做已触发：旧杯 voided 报损、旧封签立即失效，新杯 queued 重新制作
--   old_taken 骑手恰在重做中到店且原杯仍在安全窗口、新杯贴未打印 → 允许取原杯（终态）
--   completed 新杯封齐并交接给骑手（终态）
--   canceled  顾客/平台取消，或责任判定终止重做（终态）
CREATE TABLE IF NOT EXISTS remake_jobs (
    id              BIGSERIAL PRIMARY KEY,
    store_id        BIGINT NOT NULL REFERENCES stores(id),
    order_id        BIGINT NOT NULL REFERENCES orders(id),
    old_cup_id      BIGINT NOT NULL REFERENCES order_cups(id),
    new_cup_id      BIGINT REFERENCES order_cups(id),
    exception_id    BIGINT REFERENCES exceptions(id),
    reason          TEXT NOT NULL,                  -- stale_drink / wrong_cup / note_missed / equipment_fault
    trigger_type    TEXT NOT NULL DEFAULT 'manual', -- manual / auto_policy
    status          TEXT NOT NULL DEFAULT 'remaking',

    -- 触发时的判定因子快照（饮品类型 / 冰热 / 放置时长 / 门店忙闲）
    category        TEXT NOT NULL DEFAULT '',
    is_drink        BOOLEAN NOT NULL DEFAULT TRUE,
    temp            TEXT NOT NULL DEFAULT '',
    idle_seconds    INTEGER NOT NULL DEFAULT 0,
    store_busy      BOOLEAN NOT NULL DEFAULT FALSE,
    quality_window_seconds INTEGER NOT NULL DEFAULT 0, -- 品质窗口：超过即判定需要重做
    safety_window_seconds  INTEGER NOT NULL DEFAULT 0, -- 安全窗口：窗口内骑手到店仍可放行原杯
    policy_action   TEXT NOT NULL DEFAULT 'remake',

    -- 重做报损与补偿（金额快照；流水在 finance_records，分别归因）
    loss_cost       NUMERIC(10,2) NOT NULL DEFAULT 0, -- 原杯报损成本
    remake_cost     NUMERIC(10,2) NOT NULL DEFAULT 0, -- 新杯原料成本
    comp_amount     NUMERIC(10,2) NOT NULL DEFAULT 0, -- 顾客补偿方案金额

    -- 新杯贴打印状态：打印后不再允许回退取原杯
    new_label_printed   BOOLEAN NOT NULL DEFAULT FALSE,
    new_label_printed_at TIMESTAMPTZ,

    -- 骑手到店裁决
    decision        TEXT,                           -- wait_new / take_old / aborted
    decided_by      BIGINT,
    decided_at      TIMESTAMPTZ,
    decision_detail JSONB NOT NULL DEFAULT '{}',

    -- 重做导致骑手额外等待（完成交接时回填，归门店时效）
    rider_wait_seconds INTEGER NOT NULL DEFAULT 0,
    completed_at    TIMESTAMPTZ,

    created_by      BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_remake_jobs_order ON remake_jobs(order_id);
CREATE INDEX IF NOT EXISTS idx_remake_jobs_oldcup ON remake_jobs(old_cup_id);
CREATE INDEX IF NOT EXISTS idx_remake_jobs_newcup ON remake_jobs(new_cup_id);
CREATE INDEX IF NOT EXISTS idx_remake_jobs_store_status ON remake_jobs(store_id, status);

-- ---------- 顾客补偿方案与兑现 ----------
-- proposed  重做触发时按策略生成（顾客端可见“延迟原因+补偿方案”）
-- confirmed 财务/门店确认金额
-- fulfilled 平台客服确认补偿已兑现给顾客
-- withdrawn 原杯在安全窗口内交付（take_old），撤回重做补偿
-- rejected  财务/复核判定无需补偿
CREATE TABLE IF NOT EXISTS compensations (
    id            BIGSERIAL PRIMARY KEY,
    store_id      BIGINT NOT NULL REFERENCES stores(id),
    order_id      BIGINT NOT NULL REFERENCES orders(id),
    exception_id  BIGINT REFERENCES exceptions(id),
    remake_job_id BIGINT REFERENCES remake_jobs(id),
    reason        TEXT NOT NULL,               -- stale_remake / equipment_fault / platform_delay / complaint
    status        TEXT NOT NULL DEFAULT 'proposed',
    plan_amount   NUMERIC(10,2) NOT NULL DEFAULT 0,
    bearer        TEXT NOT NULL DEFAULT 'team', -- team / store / platform（初始归门店班组）
    customer_message TEXT,
    proposed_by   BIGINT,
    proposed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_by  BIGINT,
    confirmed_at  TIMESTAMPTZ,
    fulfilled_by  BIGINT,
    fulfilled_at  TIMESTAMPTZ,
    fulfill_note  TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_comp_order ON compensations(order_id);
CREATE INDEX IF NOT EXISTS idx_comp_status ON compensations(status);

-- ---------- 店长责任复核申请 / 区域经理改判 ----------
-- pending 待区域经理裁决；approved 改判（回算，不覆盖原始事件）；rejected 维持原判
CREATE TABLE IF NOT EXISTS responsibility_reviews (
    id              BIGSERIAL PRIMARY KEY,
    store_id        BIGINT NOT NULL REFERENCES stores(id),
    order_id        BIGINT NOT NULL REFERENCES orders(id),
    exception_id    BIGINT REFERENCES exceptions(id),
    remake_job_id   BIGINT REFERENCES remake_jobs(id),
    -- equipment_fault 设备故障 / platform_dispatch_delay 平台派单延迟
    ground          TEXT NOT NULL,
    reason          TEXT NOT NULL,
    evidence        TEXT,
    status          TEXT NOT NULL DEFAULT 'pending',
    requested_by    BIGINT NOT NULL,
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    reviewed_by     BIGINT,
    reviewed_at     TIMESTAMPTZ,
    approved        BOOLEAN,
    new_responsibility TEXT,                    -- platform / store / shared ...
    -- 改判回算：平台承担金额（原始流水保留，另写 platform_comp 调整流水）
    reattributed_amount NUMERIC(10,2) NOT NULL DEFAULT 0,
    resolution      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_rreview_status ON responsibility_reviews(status);
CREATE INDEX IF NOT EXISTS idx_rreview_store ON responsibility_reviews(store_id);

-- ---------- 既有表补列（不覆盖原始数据） ----------
ALTER TABLE exceptions ADD COLUMN IF NOT EXISTS original_responsibility TEXT;
ALTER TABLE finance_records ADD COLUMN IF NOT EXISTS review_id BIGINT REFERENCES responsibility_reviews(id);
ALTER TABLE loss_reports ADD COLUMN IF NOT EXISTS reversed BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE loss_reports ADD COLUMN IF NOT EXISTS reversed_at TIMESTAMPTZ;
ALTER TABLE loss_reports ADD COLUMN IF NOT EXISTS reversal_note TEXT;

-- 改判/撤回时的调整流水类型（finance_records.kind）：
--   loss_reversal(+)   原杯报损冲回（取原杯）
--   remake_reversal(+) 新杯未制作原料回库对应的重做成本冲回
--   comp_reversal(+)   补偿撤回
--   platform_comp(+)   区域经理改判后平台承担（回算门店/班组利润，原始流水保留不动）
