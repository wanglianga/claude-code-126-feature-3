package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"coffee-chain/internal/app"
	"coffee-chain/internal/db"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// 集成测试：默认连接本地 5433/6380（见 README）。
// 可用 TEST_DATABASE_URL / TEST_REDIS_ADDR 覆盖；数据库不存在会自动 drop/create。
var (
	testDSN = envOr("TEST_DATABASE_URL", "postgres://coffee@127.0.0.1:5433/coffee_test?sslmode=disable")
	testRDS = envOr("TEST_REDIS_ADDR", "localhost:6380")
	suite   *testEnv
)

type testEnv struct {
	pool *pgxpool.Pool
	rdb  *redis.Client
	srv  *httptest.Server
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestMain(m *testing.M) {
	ctx := context.Background()

	// 管理连接：drop/create 测试库
	adminDSN := strings.Replace(testDSN, "/coffee_test?", "/postgres?", 1)
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "跳过集成测试：无法连接 PostgreSQL: %v\n", err)
		os.Exit(0)
	}
	_, _ = admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='coffee_test' AND pid<>pg_backend_pid()`)
	_, err = admin.Exec(ctx, `DROP DATABASE IF EXISTS coffee_test`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "跳过集成测试：drop 测试库失败: %v\n", err)
		os.Exit(0)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE coffee_test`); err != nil {
		fmt.Fprintf(os.Stderr, "跳过集成测试：create 测试库失败: %v\n", err)
		os.Exit(0)
	}
	admin.Close()

	pool, err := db.Connect(ctx, testDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "跳过集成测试：%v\n", err)
		os.Exit(0)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "迁移失败: %v\n", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(&redis.Options{Addr: testRDS, DB: 15})
	if err := rdb.Ping(ctx).Err(); err != nil {
		fmt.Fprintf(os.Stderr, "跳过集成测试：无法连接 Redis: %v\n", err)
		os.Exit(0)
	}
	_ = rdb.FlushDB(ctx)

	a := app.New(pool, rdb)
	suite = &testEnv{pool: pool, rdb: rdb, srv: httptest.NewServer(a.Router())}
	code := m.Run()
	suite.srv.Close()
	_ = rdb.FlushDB(ctx)
	os.Exit(code)
}

func do(t *testing.T, method, path, code string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, suite.srv.URL+path, rdr)
	req.Header.Set("X-Staff-Code", code)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败 %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func mustStatus(t *testing.T, got, want int, body map[string]any) {
	t.Helper()
	if got != want {
		t.Fatalf("HTTP %d, 期望 %d, body=%v", got, want, body)
	}
}

type intakeItem map[string]any

func intake(platformNo string, items []intakeItem, extra map[string]any) map[string]any {
	body := map[string]any{
		"platform_order_no": platformNo,
		"store_id":          1,
		"items":             items,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func TestHealthAndAuth(t *testing.T) {
	resp, err := http.Get(suite.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
	// 错误工号
	if sc, b := do(t, "GET", "/v1/auth/me", "9999", nil); sc != 401 {
		t.Fatalf("错误工号应 401, got %d %v", sc, b)
	}
	// 正确工号
	if sc, b := do(t, "GET", "/v1/auth/me", "3001", nil); sc != 200 || b["role"] != "barista" {
		t.Fatalf("登录失败 %d %v", sc, b)
	}
}

func TestIntakeDeductsStock(t *testing.T) {
	var before float64
	err := suite.pool.QueryRow(context.Background(),
		`SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='milk')`).
		Scan(&before)
	if err != nil {
		t.Fatal(err)
	}
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("T-LT-1", []intakeItem{
		{"sku": "LT", "qty": 1, "temp": "ice", "sugar": "三分糖", "cup_size": "L"},
	}, nil))
	mustStatus(t, sc, 201, b)
	cups := b["cups"].([]any)
	if len(cups) != 1 {
		t.Fatalf("应生成 1 杯, got %v", b)
	}
	var after float64
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='milk')`).
		Scan(&after)
	if before-after != 180 {
		t.Fatalf("牛奶应扣 180ml, before=%v after=%v", before, after)
	}
}

func TestMakeSealColdBagAndHandoff(t *testing.T) {
	// 冰美式一杯
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("T-AM-ICE", []intakeItem{
		{"sku": "AM", "qty": 1, "temp": "ice", "sugar": "无糖", "cup_size": "L"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))

	// 骑手提前到店
	sc, b = do(t, "POST", "/v1/rider/arrivals", "4001", map[string]any{"order_id": oid})
	mustStatus(t, sc, 200, b)
	orders := b["orders"].([]any)
	if !orders[0].(map[string]any)["early"].(bool) {
		t.Fatalf("未出杯应判定提前到店: %v", b)
	}
	// 未出杯禁止交接
	sc, b = do(t, "POST", "/v1/rider/handoffs", "4001", map[string]any{"order_id": oid, "actual_bags": 1})
	if sc != 409 || b["error"] != "drinks_not_ready" {
		t.Fatalf("应拒绝未出杯交接: %d %v", sc, b)
	}
	// 叫号（显式指定本单的杯，避免与其他用例的队列互相干扰）
	sc, b = do(t, "GET", fmt.Sprintf("/v1/orders/%d", oid), "0000", nil)
	mustStatus(t, sc, 200, b)
	cid := int64(b["cups"].([]any)[0].(map[string]any)["id"].(float64))
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/start", cid), "3001", nil)
	mustStatus(t, sc, 200, b)
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/seal", cid), "3001", nil)
	mustStatus(t, sc, 200, b)
	if b["bag_kind"] != "cold" {
		t.Fatalf("冰饮应入冷饮袋 cold, got %v", b["bag_kind"])
	}
	// 取错袋
	sc, b = do(t, "POST", "/v1/rider/handoffs", "4001", map[string]any{"order_id": oid, "actual_bags": 5})
	if sc != 409 {
		t.Fatalf("袋数不符应 409, got %d", sc)
	}
	issues := b["issues"].([]any)
	if issues[0] != "wrong_bag_count" {
		t.Fatalf("应识别 wrong_bag_count: %v", issues)
	}
	// 正确交接（冰饮需吸管）
	sc, b = do(t, "POST", "/v1/rider/handoffs", "4001",
		map[string]any{"order_id": oid, "actual_bags": 1, "actual_cold": 1, "has_straw": true})
	mustStatus(t, sc, 200, b)
	if b["status"] != "handed" {
		t.Fatalf("交接后状态应为 handed: %v", b)
	}
}

func TestSurgeProtection(t *testing.T) {
	sc, b := do(t, "POST", "/v1/staff/stores/2/surge", "2001",
		map[string]any{"paused": true, "reason": "测试爆单", "minutes": 10})
	mustStatus(t, sc, 200, b)
	sc, b = do(t, "POST", "/v1/platform/orders", "9002", intake("T-SURGE", []intakeItem{
		{"sku": "AM", "qty": 1, "temp": "hot", "sugar": "无糖", "cup_size": "M"},
	}, map[string]any{"store_id": 2}))
	if sc != 429 || b["error"] != "surge_paused" {
		t.Fatalf("爆单应拒单: %d %v", sc, b)
	}
	sc, _ = do(t, "POST", "/v1/staff/stores/2/surge", "2001", map[string]any{"paused": false})
	if sc != 200 {
		t.Fatalf("解除爆单失败 %d", sc)
	}
}

func TestShortageTriggersDelisting(t *testing.T) {
	ctx := context.Background()
	if _, err := suite.pool.Exec(ctx,
		`UPDATE inventory SET qty=100 WHERE store_id=1
		 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='coldbase')`); err != nil {
		t.Fatal(err)
	}
	body := intake("T-OOS", []intakeItem{
		{"sku": "CB", "qty": 1, "temp": "ice", "sugar": "无糖", "cup_size": "L"},
	}, nil)
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", body)
	if sc != 409 || b["error"] != "stock_shortage" {
		t.Fatalf("库存不足应 stock_shortage: %d %v", sc, b)
	}
	// 事务回滚后下架已独立持久化 → 再次推单得到 item_delisted
	sc, b = do(t, "POST", "/v1/platform/orders", "9001", body)
	if sc != 409 || b["error"] != "item_delisted" {
		t.Fatalf("缺货商品应被临时下架: %d %v", sc, b)
	}
	var n int
	if err := suite.pool.QueryRow(ctx,
		`SELECT count(*) FROM delistings d JOIN menu_items m ON m.id=d.menu_item_id
		 WHERE d.store_id=1 AND m.sku='CB' AND d.active=TRUE`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("应存在 1 条 CB 下架记录, n=%d err=%v", n, err)
	}
}

func TestCancelRestocksUnmadeCups(t *testing.T) {
	ctx := context.Background()
	var before float64
	if err := suite.pool.QueryRow(ctx,
		`SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='bean')`).
		Scan(&before); err != nil {
		t.Fatal(err)
	}
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("T-CXL", []intakeItem{
		{"sku": "AM", "qty": 2, "temp": "hot", "sugar": "无糖", "cup_size": "M"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))

	sc, b = do(t, "POST", fmt.Sprintf("/v1/platform/orders/%d/cancel", oid), "5001",
		map[string]any{"reason": "顾客取消", "store_fault": false})
	mustStatus(t, sc, 200, b)
	if b["bearer"] != "platform" {
		t.Fatalf("非门店责任取消承担方应为 platform: %v", b)
	}
	var after float64
	if err := suite.pool.QueryRow(ctx,
		`SELECT qty FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='bean')`).
		Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("未制作原料应回库: before=%v after=%v", before, after)
	}
}

func TestRemakeAddsLossCost(t *testing.T) {
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("T-RMK", []intakeItem{
		{"sku": "LT", "qty": 1, "temp": "hot", "sugar": "全糖", "cup_size": "M"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))

	sc, b = do(t, "GET", fmt.Sprintf("/v1/orders/%d", oid), "0000", nil)
	mustStatus(t, sc, 200, b)
	cid := int64(b["cups"].([]any)[0].(map[string]any)["id"].(float64))
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/start", cid), "3001", nil)
	mustStatus(t, sc, 200, b)
	sc, _ = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/seal", cid), "3001", nil)
	if sc != 200 {
		t.Fatalf("封签失败 %d", sc)
	}
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/remake", cid), "3001",
		map[string]any{"reason": "stale_drink", "detail": map[string]any{"waited": 20}})
	mustStatus(t, sc, 201, b)
	if b["loss_cost"].(float64) <= 0 {
		t.Fatalf("重做应产生报损成本: %v", b)
	}
	// 旧杯进入报损留存 loss_hold（不可交接）、新杯入队
	sc, b = do(t, "GET", fmt.Sprintf("/v1/orders/%d", oid), "0000", nil)
	mustStatus(t, sc, 200, b)
	var queued, voided, lossHold int
	for _, c := range b["cups"].([]any) {
		switch c.(map[string]any)["status"] {
		case "queued":
			queued++
		case "voided":
			voided++
		case "loss_hold":
			lossHold++
		}
	}
	if queued != 1 || lossHold != 1 || voided != 0 {
		t.Fatalf("重做后应 1 新杯入队 1 旧杯报损留存, queued=%d voided=%d loss_hold=%d", queued, voided, lossHold)
	}
}

func TestReportAndTeamScore(t *testing.T) {
	// 权限：骑手不能看区域报表
	if sc, _ := do(t, "GET", "/v1/reports/region", "4001", nil); sc != 403 {
		t.Fatalf("骑手访问区域报表应 403, got %d", sc)
	}
	sc, b := do(t, "GET", "/v1/reports/stores/1/summary", "1001", nil)
	mustStatus(t, sc, 200, b)
	metrics := b["metrics"].(map[string]any)
	if metrics["orders_received"].(float64) < 3 {
		t.Fatalf("报表订单数异常: %v", metrics["orders_received"])
	}
	sc, b = do(t, "POST", "/v1/management/stores/1/team-score/refresh", "1002", nil)
	mustStatus(t, sc, 200, b)
	if b["score"].(float64) > 100 || b["score"].(float64) < 0 {
		t.Fatalf("班组评分越界: %v", b["score"])
	}
	// 督导复盘必须带时间范围与结论
	sc, b = do(t, "POST", "/v1/management/stores/1/reviews", "1002", map[string]any{
		"from":    time.Now().AddDate(0, 0, -1).Format("2006-01-02"),
		"to":      time.Now().Format("2006-01-02"),
		"summary": "集成测试复盘",
		"actions": []string{"整改1"},
	})
	mustStatus(t, sc, 201, b)
}

var _ = strings.TrimSpace

// TestStaleRuleEngine 验证规则按 饮品类型×冰热×放置时长×门店忙闲 判定。
func TestStaleRuleEngine(t *testing.T) {
	ctx := context.Background()
	// 前序缺货用例会把 CB 临时下架，先补齐冷萃液并恢复上架
	if sc, b := do(t, "POST", "/v1/staff/inventory/restock", "3001", map[string]any{
		"store_id": 1, "resolve_delistings": true,
		"items": []map[string]any{{"ingredient_sku": "coldbase", "qty": 5000}},
	}); sc != 200 {
		t.Fatalf("补货恢复上架失败 %d %v", sc, b)
	}
	// 热美式：阈值 15/硬窗 20
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("T-STALE-H", []intakeItem{
		{"sku": "AM", "qty": 1, "temp": "hot", "sugar": "无糖", "cup_size": "M"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	cid := int64(b["cups"].([]any)[0].(map[string]any)["id"].(float64))
	mustStartSeal(t, cid)
	if _, err := suite.pool.Exec(ctx,
		`UPDATE order_cups SET sealed_at=now()-interval '16 min' WHERE id=$1`, cid); err != nil {
		t.Fatal(err)
	}
	sc, b = do(t, "GET", fmt.Sprintf("/v1/staff/cups/%d/stale-check", cid), "3001", nil)
	mustStatus(t, sc, 200, b)
	if b["stale"] != true || b["safe_minutes"].(float64) != 15 || b["hard_safe_minutes"].(float64) != 20 {
		t.Fatalf("热饮16分钟应过久: %v", b)
	}
	// 冰冷萃：阈值 40，30 分钟未过久
	sc, b = do(t, "POST", "/v1/platform/orders", "9001", intake("T-STALE-CB", []intakeItem{
		{"sku": "CB", "qty": 1, "temp": "ice", "sugar": "无糖", "cup_size": "L"},
	}, nil))
	mustStatus(t, sc, 201, b)
	cbID := int64(b["cups"].([]any)[0].(map[string]any)["id"].(float64))
	mustStartSeal(t, cbID)
	if _, err := suite.pool.Exec(ctx,
		`UPDATE order_cups SET sealed_at=now()-interval '30 min' WHERE id=$1`, cbID); err != nil {
		t.Fatal(err)
	}
	sc, b = do(t, "GET", fmt.Sprintf("/v1/staff/cups/%d/stale-check", cbID), "3001", nil)
	mustStatus(t, sc, 200, b)
	if b["stale"] != false || b["safe_minutes"].(float64) != 40 {
		t.Fatalf("冰冷萃30分钟不应过久: %v", b)
	}
	_ = oid
}

func mustStartSeal(t *testing.T, cid int64) {
	t.Helper()
	if sc, b := do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/start", cid), "3001", nil); sc != 200 {
		t.Fatalf("start 失败 %d %v", sc, b)
	}
	if sc, b := do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/seal", cid), "3001", nil); sc != 200 {
		t.Fatalf("seal 失败 %d %v", sc, b)
	}
}

// TestRemakeConcurrencyNewSealOnly 过久重做 → wait_new → 旧码失效只能扫新码 → 责任复核改判 → 补偿兑现。
func TestRemakeConcurrencyNewSealOnly(t *testing.T) {
	ctx := context.Background()
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("T-RMK-CONC", []intakeItem{
		{"sku": "AM", "qty": 1, "temp": "hot", "sugar": "无糖", "cup_size": "M"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	oldID := int64(b["cups"].([]any)[0].(map[string]any)["id"].(float64))
	mustStartSeal(t, oldID)
	if _, err := suite.pool.Exec(ctx,
		`UPDATE order_cups SET sealed_at=now()-interval '16 min' WHERE id=$1`, oldID); err != nil {
		t.Fatal(err)
	}
	// 自动重做
	sc, b = do(t, "POST", "/v1/staff/stores/1/stale-check", "3001",
		map[string]any{"auto_remake": true})
	mustStatus(t, sc, 200, b)
	var remakeID, newID int64
	for _, tr := range b["triggered_remakes"].([]any) {
		m := tr.(map[string]any)
		if int64(m["old_cup_id"].(float64)) == oldID {
			remakeID = int64(m["remake_id"].(float64))
			newID = int64(m["new_cup_id"].(float64))
		}
	}
	if remakeID == 0 {
		t.Fatalf("未触发本杯重做: %v", b)
	}
	if sc, b := do(t, "GET", fmt.Sprintf("/v1/orders/%d", oid), "0000", nil); sc != 200 {
		t.Fatalf("查单失败 %d %v", sc, b)
	}
	// 决策查询：热饮 16 分钟在硬窗 20 内且未打新杯贴 → 推荐取原杯；骑手选择等待新杯
	sc, b = do(t, "GET", fmt.Sprintf("/v1/rider/orders/%d/remake-decision", oid), "4001", nil)
	mustStatus(t, sc, 200, b)
	if b["remakes"].([]any)[0].(map[string]any)["recommended"] != "take_original" {
		t.Fatalf("应推荐 take_original: %v", b)
	}
	sc, b = do(t, "POST", fmt.Sprintf("/v1/rider/orders/%d/remake-decision", oid), "4001",
		map[string]any{"remake_id": remakeID, "decision": "wait_new"})
	mustStatus(t, sc, 200, b)
	// 新杯未封不能交接
	if sc, b = do(t, "POST", "/v1/rider/handoffs", "4001",
		map[string]any{"order_id": oid, "actual_bags": 1}); sc != 409 || b["error"] != "drinks_not_ready" {
		t.Fatalf("新杯未封应 drinks_not_ready: %d %v", sc, b)
	}
	// 封新杯
	mustStartSeal(t, newID)
	sc, b = do(t, "GET", fmt.Sprintf("/v1/orders/%d", oid), "0000", nil)
	mustStatus(t, sc, 200, b)
	var newSeal string
	for _, c := range b["cups"].([]any) {
		cm := c.(map[string]any)
		if int64(cm["id"].(float64)) == newID {
			newSeal = cm["seal_code"].(string)
		}
	}
	var oldSeal string
	_ = suite.pool.QueryRow(ctx, `SELECT seal_code FROM order_cups WHERE id=$1`, oldID).Scan(&oldSeal)
	if newSeal == "" || newSeal == oldSeal {
		t.Fatalf("新封签码必须与旧码不同: new=%q old=%q", newSeal, oldSeal)
	}
	// 扫旧码被拒
	if sc, b = do(t, "POST", "/v1/rider/handoffs", "4001",
		map[string]any{"order_id": oid, "actual_bags": 1, "actual_hot": 1, "seal_codes": []string{oldSeal}}); sc != 409 || b["error"] != "old_seal_invalid" {
		t.Fatalf("旧封签码应失效: %d %v", sc, b)
	}
	// 不扫码（有进行中重做且未带新码）也拒
	if sc, b = do(t, "POST", "/v1/rider/handoffs", "4001",
		map[string]any{"order_id": oid, "actual_bags": 1, "actual_hot": 1}); sc != 409 || b["error"] != "must_scan_new_seal" {
		t.Fatalf("应要求扫新码: %d %v", sc, b)
	}
	// 扫新码交接
	sc, b = do(t, "POST", "/v1/rider/handoffs", "4001",
		map[string]any{"order_id": oid, "actual_bags": 1, "actual_hot": 1, "seal_codes": []string{newSeal}})
	mustStatus(t, sc, 200, b)
	var rstatus, rdecision string
	if err := suite.pool.QueryRow(ctx, `SELECT status,decision FROM remakes WHERE id=$1`, remakeID).
		Scan(&rstatus, &rdecision); err != nil || rstatus != "completed" || rdecision != "wait_new" {
		t.Fatalf("重做应 completed/wait_new: %s/%s err=%v", rstatus, rdecision, err)
	}
	// 顾客端可见补偿方案
	sc, b = do(t, "GET", fmt.Sprintf("/v1/customers/orders/%d/delay", oid), "7001", nil)
	mustStatus(t, sc, 200, b)
	comps := b["compensations"].([]any)
	if len(comps) != 1 || comps[0].(map[string]any)["status"] != "proposed" {
		t.Fatalf("应展示 1 个 proposed 补偿方案: %v", b["compensations"])
	}
	var excID int64
	_ = suite.pool.QueryRow(ctx, `SELECT exception_id FROM remakes WHERE id=$1`, remakeID).Scan(&excID)

	// 店长就平台派单延迟申请复核；区域经理改判 platform；财务追加冲销/重挂
	sc, b = do(t, "POST", fmt.Sprintf("/v1/management/exceptions/%d/responsibility-review", excID), "2001",
		map[string]any{"request_type": "platform_dispatch_delay", "reason": "骑手被平台延迟派单40分钟",
			"evidence": map[string]any{"delay_min": 40}})
	mustStatus(t, sc, 201, b)
	reviewID := int64(b["review_id"].(float64))
	if sc, _ := do(t, "POST", fmt.Sprintf("/v1/management/responsibility-reviews/%d/decision", reviewID), "2001",
		map[string]any{"approve": true, "new_responsibility": "platform"}); sc != 403 {
		t.Fatalf("店长无权审批复核, got %d", sc)
	}
	sc, b = do(t, "POST", fmt.Sprintf("/v1/management/responsibility-reviews/%d/decision", reviewID), "1001",
		map[string]any{"approve": true, "new_responsibility": "platform", "note": "派单延迟成立"})
	mustStatus(t, sc, 200, b)
	if b["reallocated_finance_records"].(float64) < 2 {
		t.Fatalf("应至少追加 2 条冲销/重挂: %v", b)
	}
	var teamNet float64
	if err := suite.pool.QueryRow(ctx,
		`SELECT COALESCE(sum(amount),0)::float8 FROM finance_records
		 WHERE exception_id=$1 AND kind='remake_cost' AND bearer='team'`, excID).Scan(&teamNet); err != nil {
		t.Fatal(err)
	}
	if teamNet != 0 {
		t.Fatalf("改判后班组净额应回算为 0（原始负值+冲销）, got %v", teamNet)
	}
	// 原始记录未被覆盖：仍能查到备注为“暂计门店班组”的原始行
	var orig int
	if err := suite.pool.QueryRow(ctx,
		`SELECT count(*) FROM finance_records WHERE exception_id=$1 AND note LIKE '%暂计门店班组%'`,
		excID).Scan(&orig); err != nil || orig != 1 {
		t.Fatalf("原始财务行必须保留, n=%d err=%v", orig, err)
	}
	// 客服确认补偿兑现：责任已改判平台 → bearer=platform
	var compID int64
	if err := suite.pool.QueryRow(ctx, `SELECT id FROM compensations WHERE remake_id=$1`, remakeID).Scan(&compID); err != nil {
		t.Fatal(err)
	}
	sc, b = do(t, "POST", fmt.Sprintf("/v1/platform/compensations/%d/fulfill", compID), "5001",
		map[string]any{"note": "优惠券到账"})
	mustStatus(t, sc, 200, b)
	if b["bearer"] != "platform" || b["status"] != "fulfilled" {
		t.Fatalf("补偿兑现承担方应随改判为 platform: %v", b)
	}
}

// TestRemakeTakeOriginalAndAbort 覆盖取原杯回库与顾客取消终止两条决策分支。
func TestRemakeTakeOriginalAndAbort(t *testing.T) {
	ctx := context.Background()
	beanBefore := beanQty(t)

	// --- 取原杯：冰美式 26 分钟（硬窗 35 内，新杯贴未打印）---
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("T-RMK-ORIG", []intakeItem{
		{"sku": "AM", "qty": 1, "temp": "ice", "sugar": "无糖", "cup_size": "L"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	oldID := int64(b["cups"].([]any)[0].(map[string]any)["id"].(float64))
	mustStartSeal(t, oldID)
	if _, err := suite.pool.Exec(ctx,
		`UPDATE order_cups SET sealed_at=now()-interval '26 min' WHERE id=$1`, oldID); err != nil {
		t.Fatal(err)
	}
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/remake", oldID), "3001",
		map[string]any{"reason": "stale_drink"})
	mustStatus(t, sc, 201, b)
	remakeID := int64(b["remake_id"].(float64))
	newID := int64(b["new_cup"].(map[string]any)["id"].(float64))
	sc, b = do(t, "POST", fmt.Sprintf("/v1/rider/orders/%d/remake-decision", oid), "4001",
		map[string]any{"remake_id": remakeID, "decision": "take_original"})
	mustStatus(t, sc, 200, b)
	var oldStatus, newStatus string
	_ = suite.pool.QueryRow(ctx, `SELECT status FROM order_cups WHERE id=$1`, oldID).Scan(&oldStatus)
	_ = suite.pool.QueryRow(ctx, `SELECT status FROM order_cups WHERE id=$1`, newID).Scan(&newStatus)
	if oldStatus != "sealed" || newStatus != "voided" {
		t.Fatalf("取原杯后旧杯 sealed/新杯 voided: %s/%s", oldStatus, newStatus)
	}
	// 净只消耗一杯豆（重做扣减后回库）
	if d := beanBefore - beanQty(t); d != 18 {
		t.Fatalf("取原杯净应只消耗 18g 豆, got %v", d)
	}
	// 原杯可正常交接（袋映射已恢复）
	sc, b = do(t, "POST", "/v1/rider/handoffs", "4001",
		map[string]any{"order_id": oid, "actual_bags": 1, "actual_cold": 1, "has_straw": true})
	mustStatus(t, sc, 200, b)

	// --- 顾客取消终止重做 ---
	before2 := beanQty(t)
	sc, b = do(t, "POST", "/v1/platform/orders", "9001", intake("T-RMK-ABORT", []intakeItem{
		{"sku": "AM", "qty": 1, "temp": "ice", "sugar": "无糖", "cup_size": "L"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid2 := int64(b["id"].(float64))
	cup2 := int64(b["cups"].([]any)[0].(map[string]any)["id"].(float64))
	mustStartSeal(t, cup2)
	if _, err := suite.pool.Exec(ctx,
		`UPDATE order_cups SET sealed_at=now()-interval '26 min' WHERE id=$1`, cup2); err != nil {
		t.Fatal(err)
	}
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/remake", cup2), "3001",
		map[string]any{"reason": "stale_drink"})
	mustStatus(t, sc, 201, b)
	rid2 := int64(b["remake_id"].(float64))
	sc, b = do(t, "POST", fmt.Sprintf("/v1/platform/orders/%d/cancel", oid2), "5001",
		map[string]any{"reason": "顾客取消", "store_fault": false})
	mustStatus(t, sc, 200, b)
	var rst string
	_ = suite.pool.QueryRow(ctx, `SELECT status FROM remakes WHERE id=$1`, rid2).Scan(&rst)
	if rst != "aborted" {
		t.Fatalf("重做应随取消 aborted, got %s", rst)
	}
	var vcomp int
	if err := suite.pool.QueryRow(ctx,
		`SELECT count(*) FROM compensations WHERE remake_id=$1 AND status='voided'`, rid2).Scan(&vcomp); err != nil || vcomp != 1 {
		t.Fatalf("补偿方案应作废, n=%d", vcomp)
	}
	// 旧杯消耗 18g，新杯未做回库 → 净 18g
	if d := before2 - beanQty(t); d != 18 {
		t.Fatalf("取消重做单净应消耗 18g 豆（旧杯报损）, got %v", d)
	}
	// 取消后禁止交接
	if sc, b = do(t, "POST", "/v1/rider/handoffs", "4001", map[string]any{"order_id": oid2}); sc != 409 || b["error"] != "order_canceled" {
		t.Fatalf("取消单禁止交接: %d %v", sc, b)
	}
}

func beanQty(t *testing.T) float64 {
	t.Helper()
	var q float64
	if err := suite.pool.QueryRow(context.Background(),
		`SELECT qty::float8 FROM inventory WHERE store_id=1 AND ingredient_id=(SELECT id FROM ingredients WHERE sku='bean')`).
		Scan(&q); err != nil {
		t.Fatal(err)
	}
	return q
}

var _ = strings.TrimSpace
