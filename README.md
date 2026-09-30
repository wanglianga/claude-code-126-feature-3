# 咖啡连锁外卖出杯时效与骑手交接服务

面向咖啡连锁外卖场景的后端服务（Go + PostgreSQL + Redis）。把**门店、店员、骑手、顾客、外卖平台客服、财务赔付**串到同一张订单时间线上，覆盖：

- 多外卖平台（饿了么 / 美团）接单：美式、拿铁、冷萃、果咖、轻食、组合套餐；录入订单时间、门店产能、制作配方、冰热属性、糖度、杯型、封签码、骑手预计到店、顾客备注
- 接单即按配方扣减原料库存（杯型决定杯耗材、冰饮加冰与吸管、轻食/套餐扣餐具、每杯一封签）
- **门店爆单保护**：在制杯量超阈值自动拒单（`surge_full`），也可手动临时限制接单（`surge_paused`，可设时长）
- 门店按出杯顺序制作（队列按骑手 ETA 优先级排序，`FOR UPDATE SKIP LOCKED` 并发叫号）
- 店员扫描封签 → 生成封签码 → **热饮 / 冷饮 / 餐食自动分袋** → 全单封齐进入待取区
- 骑手到店核验：**订单号、袋数、冷热分袋、吸管、餐具**；未出杯禁止取走
- 全场景异常：骑手提前到店、饮品放置过久重做、顾客备注漏看、错杯、轻食缺货、骑手取错袋、平台取消订单、顾客漏液投诉
- **重做报损**：旧杯作废+报损成本，新杯重新入队，重做成本计入门店班组复盘与财务
- **放置过久重做的并发交接与成本归属**：
  - 按**饮品类型、冰热属性、放置时长、门店忙闲**计算品质窗口/安全窗口，支持单杯因子评估与一键自动重做
  - 触发重做即原杯**报损失效（旧封签码立即作废、不可再交接）**，新杯生成**新封签码与新制作序号**
  - 骑手恰在重做中到店：按**原杯是否在安全窗口 / 新杯贴是否已打印 / 顾客是否已取消**，系统裁决
    `wait_new`（等新杯）/ `take_old`（放行原杯，回库新杯并冲回成本）/ `aborted`（取消终止），
    订单级事务锁杜绝两杯同时被交接
  - **原杯报损、骑手等待、门店时效、原料扣减、顾客补偿分别归因**；责任复核改判后只追加调整流水回算班组利润，不覆盖原始事件
  - 店长可对**设备故障 / 平台派单延迟**申请责任复核，区域经理改判平台/共担承担
  - 顾客端展示**延迟原因与补偿方案**，平台客服确认补偿**是否已兑现**（跨平台越权拒绝）
- **缺货自动临时下架**（按原料反查商品），补货且原料齐备后自动恢复上架
- 平台取消：未制作杯原料回库、已封杯报损、按门店/平台责任退款
- 多平台订单**合并**（同一骑手顺路单一次到店登记、分别核验）
- 杯贴 / 袋贴打印数据、骑手到店屏、异常报损台账
- 财务赔付定责（门店 / 骑手 / 平台 / 顾客 / 共担），平台分摊补贴
- 区域经理按门店看**出杯慢、错杯、漏餐具、重做、平台取消对利润的影响**；区域督导复盘同步；门店班组评分更新
- 完整 **OpenAPI 3.x** 规范与离线内嵌 **Swagger UI**

---

## 1. 一键启动（Docker Compose）

需要 Docker（含 Compose 插件）。

```bash
docker compose up -d --build
```

首次启动会自动：
1. 拉起 PostgreSQL 15（端口 5432）与 Redis 7（端口 6379），健康检查通过后再启动应用；
2. 应用自动执行数据库迁移并写入种子数据（2 家门店、14 个账号、6 类商品、配方与库存）。

验证：

```bash
curl http://localhost:8080/healthz
# {"status":"ok","postgres":"ok","redis":"ok"}
```

- API 根地址：`http://localhost:8080`
- Swagger UI（可直接点选接口调试）：<http://localhost:8080/swagger/>
- OpenAPI 3.x 规范：<http://localhost:8080/openapi.yaml>

停止并清理数据卷：

```bash
docker compose down -v
```

---

## 2. 测试账号（X-Staff-Code 工号即密钥）

所有 `/v1` 接口（登录接口除外）都用请求头鉴权，二选一：

- 简单方式：每个请求带 `X-Staff-Code: <工号>`
- 或先 `POST /v1/auth/login`（body `{"code":"3001"}`）拿 Bearer Token，再带 `Authorization: Bearer <token>`

| 角色 | 登录名 | 工号 code | 说明 |
| --- | --- | --- | --- |
| 平台管理员 | admin | `0000` | 拥有所有接口权限 |
| 区域经理 | mgr.huadong | `1001` | 区域对比报表、班组评分 |
| 区域督导 | sup.li | `1002` | 门店复盘 |
| 店长 SH001 | sm.001 | `2001` | 接单、爆单保护、补货、下架 |
| 店员（门店1） | barista.001 | `3001` | 叫号、封签、重做、缺货 |
| 店员（门店1） | barista.002 | `3002` | |
| 饿了么骑手 | rider.eleme | `4001` | 到店登记、核验交接 |
| 美团骑手 | rider.meituan | `4002` | |
| 饿了么客服 | cs.eleme | `5001` | 取消订单 |
| 美团客服 | cs.meituan | `5002` | |
| 财务 | finance | `6001` | 赔付定责、财务台账 |
| 顾客 | customer.001 | `7001` | 投诉 |
| 饿了么平台 | platform.eleme | `9001` | 平台推单（自动带 platform=eleme） |
| 美团平台 | platform.meituan | `9002` | 平台推单（自动带 platform=meituan） |

种子门店：`SH001 上海人民广场店`（id=1）、`SH002 上海陆家嘴店`（id=2），均属「华东」。
种子商品 SKU：`AM 美式`、`LT 拿铁`、`CB 冷萃`、`FC 果咖`、`LF 鸡肉轻食卷`、`SET 元气套餐`。

> 服务对角色做了接口级权限控制（如骑手不能看区域报表、平台不能封签），越权返回 `403`。

---

## 3. 快速体验（核心链路）

```bash
# 1) 平台推单（冰拿铁 + 热美式 + 套餐 + 轻食）
curl -s -X POST http://localhost:8080/v1/platform/orders \
  -H "X-Staff-Code: 9001" -H "Content-Type: application/json" -d '{
    "platform_order_no":"EM20260929001","store_id":1,
    "customer_note":"杯盖贴紧不要摇","rider_eta":"2026-09-29T12:30:00+08:00",
    "items":[
      {"sku":"LT","qty":1,"temp":"ice","sugar":"三分糖","cup_size":"L","note":"少冰"},
      {"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"},
      {"sku":"SET","qty":1,"temp":"ice","sugar":"五分糖","cup_size":"M"},
      {"sku":"LF","qty":1}
    ]}'

# 2) 店员看出杯顺序（含顾客备注/杯备注/骑手ETA）
curl -s http://localhost:8080/v1/staff/stores/1/queue -H "X-Staff-Code: 3001"

# 3) 叫号取下一杯 → 扫描封签（重复到所有杯封齐；返回袋号与冷热分袋）
curl -s -X POST http://localhost:8080/v1/staff/stores/1/cups/next \
  -H "X-Staff-Code: 3001" -H "Content-Type: application/json" -d '{"store_id":1}'
curl -s -X POST http://localhost:8080/v1/staff/cups/1/seal -H "X-Staff-Code: 3001"

# 4) 骑手提前到店（饮品未出杯 → rider_early 异常 + 到店屏可见）
curl -s -X POST http://localhost:8080/v1/rider/arrivals \
  -H "X-Staff-Code: 4001" -H "Content-Type: application/json" -d '{"order_id":1}'

# 5) 取餐核验（订单号/袋数/冷热/吸管餐具；先故意取错袋）
curl -s -X POST http://localhost:8080/v1/rider/handoffs \
  -H "X-Staff-Code: 4001" -H "Content-Type: application/json" \
  -d '{"order_id":1,"actual_bags":2}'          # 409 + wrong_bag_count
curl -s -X POST http://localhost:8080/v1/rider/handoffs \
  -H "X-Staff-Code: 4001" -H "Content-Type: application/json" \
  -d '{"order_id":1,"actual_bags":3,"actual_hot":1,"actual_cold":1,"actual_food":1,
       "has_straw":true,"has_utensil":true}'   # 200 交接完成

# 6) 顾客漏液投诉 → 财务定责
curl -s -X POST http://localhost:8080/v1/customers/orders/1/complain \
  -H "X-Staff-Code: 7001" -H "Content-Type: application/json" \
  -d '{"type":"leak_complaint","detail":"拿铁漏液半杯"}'
curl -s -X PATCH http://localhost:8080/v1/finance/exceptions/1 \
  -H "X-Staff-Code: 6001" -H "Content-Type: application/json" \
  -d '{"responsibility":"shared","comp_amount":22,"platform_share":10,"resolution":"门店封盖不当与平台共担"}'

# 7) 一张订单的完整时间线（六类角色串联）
curl -s http://localhost:8080/v1/orders/1/events -H "X-Staff-Code: 0000"
```

更多场景（重做、缺货下架、爆单、取消、合并、报表、班组评分、复盘）均在
`scripts/e2e.sh` 中有可运行的完整演示，建议配合阅读。

### 放置过久重做的并发交接（特性 3）

```bash
# 0) 接单、制作、封口（热美式）
curl -s -X POST http://localhost:8080/v1/platform/orders -H "X-Staff-Code: 9001" \
  -H "Content-Type: application/json" \
  -d '{"platform_order_no":"EM-FEAT3-1","store_id":1,"items":[{"sku":"AM","qty":1,"temp":"hot","sugar":"无糖","cup_size":"M"}]}'
# ……店员 start/seal（见上文），假设骑手迟迟未到、放置 26 分钟（品质 15 / 安全 25 分）

# 1) 因子评估（饮品类型/冰热/放置时长/门店忙闲 → 动作与窗口）
curl -s http://localhost:8080/v1/staff/cups/<cup_id>/remake-evaluation -H "X-Staff-Code: 3001"
# 2) 一键自动重做（或 POST /v1/staff/cups/{id}/remake 手动）
curl -s -X POST http://localhost:8080/v1/staff/stores/1/remake-stale -H "X-Staff-Code: 3001" \
  -H "Content-Type: application/json" -d '{}'
# 3) 骑手恰在重做中到店：系统按安全窗口/杯贴/取消裁决 wait_new|take_old|aborted
curl -s -X POST http://localhost:8080/v1/rider/remakes/<job_id>/decision \
  -H "X-Staff-Code: 4001" -H "Content-Type: application/json" -d '{}'
# 4) 只能扫描新封签码交接（旧码立即报 old_seal_voided）
curl -s -X POST http://localhost:8080/v1/rider/handoffs -H "X-Staff-Code: 4001" \
  -H "Content-Type: application/json" \
  -d '{"order_id":1,"seal_codes":["<new_seal>"],"actual_bags":1,"actual_hot":1}'
# 5) 顾客端看延迟原因与补偿；财务确认；平台客服确认兑现
curl -s http://localhost:8080/v1/customers/orders/1/status -H "X-Staff-Code: 7001"
curl -s -X POST http://localhost:8080/v1/finance/compensations/<id>/confirm -H "X-Staff-Code: 6001" \
  -H "Content-Type: application/json" -d '{"amount":7.5}'
curl -s -X POST http://localhost:8080/v1/platform/compensations/<id>/fulfill -H "X-Staff-Code: 5001" \
  -H "Content-Type: application/json" -d '{"note":"7.5元券已到账"}'
# 6) 店长申请责任复核（设备故障/平台派单延迟），区域经理改判回算
curl -s -X POST http://localhost:8080/v1/management/orders/1/responsibility-reviews -H "X-Staff-Code: 2001" \
  -H "Content-Type: application/json" \
  -d '{"ground":"platform_dispatch_delay","reason":"派单延迟26分钟","evidence":"骑手轨迹"}'
curl -s -X POST http://localhost:8080/v1/management/responsibility-reviews/<id>/adjudicate \
  -H "X-Staff-Code: 1001" -H "Content-Type: application/json" \
  -d '{"approved":true,"new_responsibility":"platform","resolution":"平台承担"}'
```

---

## 4. 本地开发（不使用 Docker）

依赖：Go 1.19+、PostgreSQL 13+、Redis 6+。

```bash
# 1) 准备数据库与 Redis（示例：本机 PG 5432 / Redis 6379）
createdb coffee   # 账号需有建库权限；迁移会自动建表+种子

# 2) 运行（启动时自动执行内嵌迁移 migrations）
export DATABASE_URL="postgres://用户:密码@127.0.0.1:5432/coffee?sslmode=disable"
export REDIS_ADDR="127.0.0.1:6379"
export PORT=8080
go run ./cmd/server
```

数据库迁移 SQL 内嵌在二进制里（`internal/db/sql/*.sql`），按文件名顺序、每文件仅执行一次（记录在 `schema_migrations` 表）。修改种子后重置开发库：

```bash
psql -d postgres -c "DROP DATABASE coffee;" -c "CREATE DATABASE coffee;"
# 重启服务即自动重新迁移
```

### 运行测试

- **Go 集成测试**：`go test ./...`
  会自动创建并使用独立的 `coffee_test` 数据库（默认连 `127.0.0.1:5433`、Redis `localhost:6380`，
  可用 `TEST_DATABASE_URL` / `TEST_REDIS_ADDR` 覆盖；连不上数据库时测试自动跳过）。
  覆盖：接单扣库存、冷热分袋、提前到店、取错袋核验、爆单拒单、缺货自动下架、取消回库、重做报损、报表与班组评分、OpenAPI 合法性，
  以及特性 3 的**品质/安全窗口因子评估、自动重做、旧封签码立即失效、wait_new/take_old/aborted 三分支裁决、
  报损冲回、取消终止、补偿跨平台兑现守卫、责任复核改判不覆盖原始事件**（共 16 个测试）。
- **端到端冒烟脚本（136 项断言，23 个场景段）**：`scripts/e2e.sh`
  面向一个已运行在 `localhost:8099` 的服务（可用 `BASE=http://...` 改地址）。
  脚本默认会自动重置本地 `coffee` 库（`psql -h /tmp/cc126 -p 5433`）与 6380 Redis 并重启
  `/tmp/cc126/server`，如对接已运行环境请设 `SKIP_RESET=1`。

```bash
go build -o /tmp/cc126/server ./cmd/server
./scripts/e2e.sh
```

---

## 5. 主要 API 一览（完整定义见 Swagger UI / openapi.yaml）

| 模块 | 接口 |
| --- | --- |
| 鉴权 | `POST /v1/auth/login`、`GET /v1/auth/me` |
| 基础数据 | `GET /v1/stores`、`GET /v1/menu`、`GET /v1/stores/{id}/inventory`、`GET /v1/stores/{id}/board` |
| 平台接单 | `POST /v1/platform/orders`（支持 `Idempotency-Key`）、`POST /v1/platform/orders/{id}/cancel` |
| 门店作业 | `GET /v1/staff/stores/{id}/queue`、`POST /v1/staff/stores/{id}/cups/next`、`POST /v1/staff/cups/{id}/start`、`POST /v1/staff/cups/{id}/seal`、`POST /v1/staff/cups/{id}/remake` |
| 放置过久重做 | `GET /v1/staff/cups/{id}/remake-evaluation`、`GET /v1/staff/stores/{id}/stale-cups`、`POST /v1/staff/stores/{id}/remake-stale`、`GET /v1/staff/stores/{id}/remakes`、`GET /v1/remakes/{id}`、`GET /v1/orders/{id}/remakes`、`POST /v1/staff/remakes/{id}/new-cup-label` |
| 重做并发交接 | `POST /v1/rider/remakes/{id}/decision`（wait_new/take_old/aborted）、交接时传 `seal_codes` 扫描新封签码 |
| 顾客补偿 | `GET /v1/customers/orders/{id}/status`、`GET /v1/orders/{id}/compensations`、`POST /v1/finance/compensations/{id}/confirm`、`POST /v1/platform/compensations/{id}/fulfill` |
| 责任复核 | `POST /v1/management/orders/{id}/responsibility-reviews`、`GET /v1/management/responsibility-reviews`、`POST /v1/management/responsibility-reviews/{id}/adjudicate` |
| 缺货/下架 | `POST /v1/staff/orders/{id}/item-oos`、`POST /v1/staff/menu/{sku}/delist`、`GET /v1/stores/{id}/delistings`、`POST /v1/staff/inventory/restock` |
| 打印 | `GET /v1/staff/orders/{id}/cup-labels`、`GET /v1/staff/orders/{id}/bag-labels` |
| 爆单保护 | `POST /v1/staff/stores/{id}/surge` |
| 订单合并 | `POST /v1/staff/merge`、`GET /v1/merged-groups/{id}` |
| 骑手 | `POST /v1/rider/arrivals`、`GET /v1/rider/screen`、`GET /v1/rider/arrivals/{id}`、`POST /v1/rider/handoffs` |
| 顾客/客服 | `POST /v1/customers/orders/{id}/complain` |
| 财务 | `PATCH /v1/finance/exceptions/{id}`、`GET /v1/stores/{id}/finance` |
| 订单查询 | `GET /v1/orders/{id}`、`GET /v1/orders/{id}/events`、`GET /v1/orders/{id}/exceptions` |
| 管理报表 | `GET /v1/reports/stores/{id}/summary`、`GET /v1/reports/region` |
| 班组/复盘 | `POST /v1/management/stores/{id}/team-score/refresh`、`GET /v1/management/team-scores`、`POST /v1/management/stores/{id}/reviews` |

### 关键业务规则

- **库存扣减**：接单事务内对门店加 `pg_advisory_xact_lock` 串行化，锁行 `SELECT ... FOR UPDATE`
  后汇总扣减，不允许为负；任一原料不足整单回滚，并把相关商品**独立持久化临时下架**（不会因回滚丢失）。
- **出杯顺序**：队列先取制作中的杯，再按骑手 ETA（缺省按接单+30 分钟）、接单时间、杯序号排序。
- **分袋**：热饮→热袋、冰饮→冷袋、轻食→餐食袋；含饮品的组合套餐按其饮品冰热入袋。同温层首杯封签时扣减对应袋耗材。
- **重做（放置过久）**：
  - 因子判定：按饮品类型（冷萃最稳/果咖最不稳…）、冰热（冰饮耐放）、放置时长、门店忙闲（忙时窗口放宽 50%）
    得出**品质窗口**（超过必须重做）与**安全窗口**（窗口内骑手到店仍可放行原杯）。
  - 触发即**原杯报损**（`voided`、旧封签立即失效），新杯以**新制作序号**入队、封口生成新封签码，`remake_no+1`；
    原杯报损（`loss`）、新杯原料（`remake_cost`）、顾客补偿（`customer_comp`）三类成本分开记账。
  - 重做中骑手到店裁决 `POST /v1/rider/remakes/{id}/decision`：
    新杯已封/新杯贴已打印 → 只能 `wait_new`；安全窗口内且未打杯贴 → `take_old`
    （恢复旧杯、回库新杯、写 `loss_reversal`/`remake_reversal` 冲回、撤回补偿）；顾客已取消 → `aborted`。
  - 骑手交接可传 `seal_codes` 逐杯扫码：旧码报 `old_seal_voided`、外单/重复/缺码拒绝，杜绝两杯同时交接。
  - 责任复核：店长对设备故障/平台派单延迟申请，区域经理改判后写一条带 `review_id` 的 `platform_comp`
    调整流水回算班组利润，异常原始责任存 `original_responsibility`，**原始事件与流水不覆盖**。
- **顾客补偿**：重做时按原因生成方案（proposed）→ 财务确认（confirmed）→ 平台客服确认兑现（fulfilled，跨平台拒绝）；
  `take_old` 或取消会撤回未兑现方案。顾客端 `GET /v1/customers/orders/{id}/status` 展示延迟原因、新杯进度与补偿。
- **骑手等待责任**：交接时计算等待时长；骑手早于自身 ETA 到店的提前量不计门店责任，
  超过 ETA 仍等不到杯的部分记为门店出杯慢（`store_fault_wait_seconds`）。
- **取消**：未做/制作中杯原料回库（`cancel_restock`），已封杯按报损处理；退款承担方由 `store_fault` 决定。
- **班组评分**：100 分起，慢单率（每 5% 扣 1）、错杯(-4)、漏吸管餐具(-2)、取错袋(-2)、饮品过久(-3)、漏备注(-2)、门店责任取消(-5)。

---

## 6. 已执行的验证结论

以下验证在交付环境中实际执行通过（日期 2026-09-30）：

1. `go vet ./...` 无告警；`go build ./cmd/server` 通过。
2. `go test ./...` 通过：**15 个测试**（自动建 `coffee_test` 库），含原 9 个集成/OpenAPI 测试与
   特性 3 的 6 个新测试——窗口因子评估、自动重做与旧码失效、take_old 报损冲回、取消终止、
   责任复核改判（原始事件保留）、补偿兑现跨平台守卫。
3. `scripts/e2e.sh` 端到端 **136/136 项断言全部通过**，覆盖 23 个场景段：
   健康检查、接单四杯生成、库存扣减（牛奶/封签/吸管/餐具数量逐一核对）、幂等重推、
   出杯队列与备注、四杯封签与 3 个冷热餐食分袋、杯贴袋贴、骑手提前到店与到店屏、
   未出杯拒交接、取错袋 409 与正确交接 200、漏吸管漏餐具识别、投诉受理与财务定责（含平台分摊）、
   过久重做报损与新杯回队、冷萃液缺货自动下架（`stock_shortage`→`item_delisted`）、
   补货后自动恢复上架、轻食接单后缺货单品退款下架、爆单手动暂停（`surge_paused`）与
   容量阈值（`surge_full`）、平台取消原料回库与退款归责、饿了么+美团多平台合并一次到店、
   门店/区域报表、班组评分扣分、督导复盘含重做成本、
   **放置过久因子评估与一键自动重做、wait_new/旧码立即失效/新码交接、新杯贴不可逆分界、
   顾客端延迟原因与补偿方案、财务确认与平台客服兑现（跨平台 403）、店长复核与区域经理改判回算（原始流水保留）、
   安全窗口内 take_old 报损/重做双冲回与补偿撤回、重做中取消 aborted、报表与看板新口径**、
   订单时间线含 platform/barista/rider/customer/finance 五类角色。
4. Swagger UI（`/swagger/`）、`/openapi.yaml`、内嵌静态资源均返回 200；`/healthz` 返回 PG/Redis 均 ok。
5. Dockerfile / docker-compose.yml 已提供，可 `docker compose up -d --build` 一键启动
   （交付沙箱无 Docker 守护进程，未在沙箱内实际构建；镜像构建仅依赖标准 `golang:1.22-alpine` 与 `alpine:3.19`，应用为 CGO_ENABLED=0 静态二进制）。

---

## 7. 工程结构

```
.
├── cmd/server/main.go          # 入口：读环境变量、连库、迁移、起 HTTP
├── internal/
│   ├── db/                     # pgxpool 连接 + 内嵌 SQL 迁移执行器
│   │   └── sql/                # 0001 建表 / 0002 种子 / 0003 重做并发交接（随二进制 embed）
│   └── app/
│       ├── server.go           # 路由、角色鉴权、登录
│       ├── orders.go           # 接单、配方库存、幂等、缺货下架、平台取消（联动重做终止）
│       ├── production.go       # 出杯队列、封签分袋、轻食缺货、叫号
│       ├── remake.go           # 重做因子评估/自动重做、品质与安全窗口、报损+重做+补偿分别归因
│       ├── remake_decision.go  # 重做中骑手到店 wait_new/take_old/aborted 裁决、新杯贴、回库冲回
│       ├── compensation.go     # 顾客补偿方案、顾客端延迟视图、财务确认、平台客服兑现
│       ├── review.go           # 店长责任复核、区域经理改判回算（不覆盖原始事件）
│       ├── rider.go            # 骑手到店、到店屏、核验交接、封签码扫描（旧码立即失效）与等待归责
│       ├── management.go       # 门店/库存/爆单/合并/杯贴袋贴/看板
│       ├── reports.go          # 投诉、财务定责、台账、门店/区域报表、评分、复盘
│       ├── models.go           # JSON 模型
│       ├── swagger.go          # 内嵌 Swagger UI + OpenAPI
│       └── web/                # openapi.yaml + swagger-ui-dist 静态资源
├── scripts/e2e.sh              # 端到端冒烟脚本（136 断言、23 场景段）
├── Dockerfile
└── docker-compose.yml
```
