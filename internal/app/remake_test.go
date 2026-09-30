package app_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// 特性 3：放置过久重做的并发交接与成本归属。
// 复用 integration_test.go 中的 do / intake / mustStatus / suite。

func sealCup(t *testing.T, oid int64) (cupID int64) {
	t.Helper()
	sc, b := do(t, "GET", fmt.Sprintf("/v1/orders/%d", oid), "0000", nil)
	mustStatus(t, sc, 200, b)
	cups := b["cups"].([]any)
	if len(cups) == 0 {
		t.Fatalf("订单 %d 无杯", oid)
	}
	// 取第一个未作废杯
	for _, c := range cups {
		cup := c.(map[string]any)
		if cup["status"] != "voided" {
			return int64(cup["id"].(float64))
		}
	}
	return int64(cups[0].(map[string]any)["id"].(float64))
}

func makeSeal(t *testing.T, oid int64, cid int64) {
	t.Helper()
	if sc, b := do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/start", cid), "3001", nil); sc != 200 {
		t.Fatalf("开始制作失败 %d %v", sc, b)
	}
	if sc, b := do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/seal", cid), "3001", nil); sc != 200 {
		t.Fatalf("封签失败 %d %v", sc, b)
	}
}

func setIdleMinutes(t *testing.T, cupID int64, mins int) {
	t.Helper()
	if _, err := suite.pool.Exec(context.Background(),
		fmt.Sprintf("UPDATE order_cups SET sealed_at=now()-interval '%d min' WHERE id=$1", mins), cupID); err != nil {
		t.Fatal(err)
	}
}

func TestRemakeEvaluationWindowFactors(t *testing.T) {
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("RM-EVAL", []intakeItem{
		{"sku": "LT", "qty": 1, "temp": "ice", "sugar": "三分糖", "cup_size": "L"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	cid := sealCup(t, oid)
	makeSeal(t, oid, cid)

	// 刚封口：monitor，未达品质窗口
	sc, b = do(t, "GET", fmt.Sprintf("/v1/staff/cups/%d/remake-evaluation", cid), "3001", nil)
	mustStatus(t, sc, 200, b)
	p := b["policy"].(map[string]any)
	if p["action"] != "monitor" {
		t.Fatalf("刚封口应 monitor: %v", p["action"])
	}
	if p["quality_window_seconds"].(float64) != 1200 { // 冰拿铁空闲 20 分钟
		t.Fatalf("冰拿铁品质窗口应为 1200 秒: %v", p["quality_window_seconds"])
	}
	if p["safety_window_seconds"].(float64) != 1800 { // 安全窗口 30 分钟
		t.Fatalf("冰拿铁安全窗口应为 1800 秒: %v", p["safety_window_seconds"])
	}

	// 超过品质窗口 → remake + 补偿预览
	setIdleMinutes(t, cid, 21)
	sc, b = do(t, "GET", fmt.Sprintf("/v1/staff/cups/%d/remake-evaluation", cid), "3001", nil)
	mustStatus(t, sc, 200, b)
	p = b["policy"].(map[string]any)
	if p["action"] != "remake" {
		t.Fatalf("超过品质窗口应 remake: %v", p)
	}
	comp := b["comp_preview"].(map[string]any)
	if comp["amount"].(float64) != 11 { // 拿铁 22 * 50%
		t.Fatalf("过久重做补偿预览应为 11 元: %v", comp["amount"])
	}
}

func TestAutoRemakeTriggersAndOldCodeInvalid(t *testing.T) {
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("RM-AUTO", []intakeItem{
		{"sku": "LT", "qty": 1, "temp": "ice", "sugar": "无糖", "cup_size": "L"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	cid := sealCup(t, oid)
	makeSeal(t, oid, cid)

	// 未到窗口：自动重做不触发
	sc, b = do(t, "POST", "/v1/staff/stores/1/remake-stale", "3001",
		map[string]any{"cup_ids": []int64{cid}})
	mustStatus(t, sc, 200, b)
	if b["triggered_count"].(float64) != 0 {
		t.Fatalf("窗口内不应触发重做: %v", b)
	}

	oldCode := sealCodeOf(t, cid)
	setIdleMinutes(t, cid, 31) // > 品质 20、> 安全 30
	sc, b = do(t, "POST", "/v1/staff/stores/1/remake-stale", "3001",
		map[string]any{"cup_ids": []int64{cid}})
	mustStatus(t, sc, 200, b)
	if b["triggered_count"].(float64) != 1 {
		t.Fatalf("应触发 1 个重做: %v", b)
	}
	trig := b["triggered"].([]any)[0].(map[string]any)
	newID := int64(trig["new_cup_id"].(float64))
	jobID := int64(trig["job"].(map[string]any)["id"].(float64))
	if trig["loss_cost"].(float64) <= 0 || trig["remake_cost"].(float64) <= 0 {
		t.Fatalf("报损与重做成本均应>0: %v", trig)
	}
	if trig["comp_amount"].(float64) != 11 {
		t.Fatalf("应生成 11 元补偿方案: %v", trig["comp_amount"])
	}

	// 旧杯 voided、新杯新制作序号 + remake_no=1
	var oldStatus, oldReason string
	var newSeq, newRemakeNo int
	err := suite.pool.QueryRow(context.Background(),
		`SELECT status,COALESCE(void_reason,'') FROM order_cups WHERE id=$1`, cid).Scan(&oldStatus, &oldReason)
	if err != nil || oldStatus != "voided" || oldReason != "stale_drink" {
		t.Fatalf("旧杯应 stale_drink 报损: %s %s %v", oldStatus, oldReason, err)
	}
	err = suite.pool.QueryRow(context.Background(),
		`SELECT seq,remake_no FROM order_cups WHERE id=$1`, newID).Scan(&newSeq, &newRemakeNo)
	if err != nil || newSeq != 2 || newRemakeNo != 1 {
		t.Fatalf("新杯应为 seq=2/remake_no=1: %d %d %v", newSeq, newRemakeNo, err)
	}

	// 打印新杯贴 → 只能 wait_new
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/remakes/%d/new-cup-label", jobID), "3001", nil)
	mustStatus(t, sc, 200, b)
	if sc, b = do(t, "POST", fmt.Sprintf("/v1/rider/remakes/%d/decision", jobID), "4001",
		map[string]any{"action": "take_old"}); sc != 409 || b["error"] != "new_cup_committed" {
		t.Fatalf("打印新杯贴后不得取原杯: %d %v", sc, b)
	}

	// 新杯封口 → 旧码立即失效，只有新码可交接
	makeSeal(t, oid, newID)
	newCode := sealCodeOf(t, newID)
	if newCode == oldCode {
		t.Fatal("新旧封签码不应相同")
	}
	sc, b = do(t, "POST", "/v1/rider/handoffs", "4001", map[string]any{
		"order_id": oid, "seal_codes": []string{oldCode}, "actual_bags": 1,
	})
	if sc != 409 || b["error"] != "old_seal_voided" {
		t.Fatalf("旧码应被判 old_seal_voided: %d %v", sc, b)
	}
	sc, b = do(t, "POST", "/v1/rider/handoffs", "4001", map[string]any{
		"order_id": oid, "seal_codes": []string{newCode},
		"actual_bags": 1, "actual_cold": 1, "has_straw": true,
	})
	mustStatus(t, sc, 200, b)

	// 任务 completed
	var status, decision string
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT status,decision FROM remake_jobs WHERE id=$1`, jobID).Scan(&status, &decision)
	if status != "completed" || decision != "wait_new" {
		t.Fatalf("任务应 completed/wait_new: %s/%s", status, decision)
	}
}

func TestRemakeDecisionTakeOldReversesCosts(t *testing.T) {
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("RM-OLD", []intakeItem{
		{"sku": "LT", "qty": 1, "temp": "ice", "sugar": "无糖", "cup_size": "L"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	cid := sealCup(t, oid)
	makeSeal(t, oid, cid)
	oldCode := sealCodeOf(t, cid)

	// 放置 10 分钟（仍在安全窗口 30 分内）手动触发重做
	setIdleMinutes(t, cid, 10)
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/remake", cid), "3001",
		map[string]any{"reason": "stale_drink"})
	mustStatus(t, sc, 201, b)
	jobID := int64(b["remake_job"].(map[string]any)["id"].(float64))
	newID := int64(b["new_cup"].(map[string]any)["id"].(float64))

	// 系统自动裁决 → take_old
	sc, b = do(t, "POST", fmt.Sprintf("/v1/rider/remakes/%d/decision", jobID), "4001",
		map[string]any{})
	mustStatus(t, sc, 200, b)
	if b["decision"] != "take_old" {
		t.Fatalf("安全窗口内应建议 take_old: %v", b["decision"])
	}

	// 旧杯恢复 sealed、新杯作废、报损冲回、补偿撤回
	var oldStatus, newStatus string
	_ = suite.pool.QueryRow(context.Background(), `SELECT status FROM order_cups WHERE id=$1`, cid).Scan(&oldStatus)
	_ = suite.pool.QueryRow(context.Background(), `SELECT status FROM order_cups WHERE id=$1`, newID).Scan(&newStatus)
	if oldStatus != "sealed" || newStatus != "voided" {
		t.Fatalf("take_old 后应旧杯 sealed/新杯 voided: %s/%s", oldStatus, newStatus)
	}
	var reversed bool
	if err := suite.pool.QueryRow(context.Background(),
		`SELECT reversed FROM loss_reports WHERE cup_id=$1 ORDER BY id DESC LIMIT 1`, cid).Scan(&reversed); err != nil || !reversed {
		t.Fatalf("原杯报损记录应标记冲回: %v %v", reversed, err)
	}
	var compStatus string
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT status FROM compensations WHERE remake_job_id=$1`, jobID).Scan(&compStatus)
	if compStatus != "withdrawn" {
		t.Fatalf("take_old 后补偿方案应撤回: %s", compStatus)
	}
	// 冲回流水存在
	var reversals int
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM finance_records WHERE order_id=$1 AND kind IN ('loss_reversal','remake_reversal')`,
		oid).Scan(&reversals)
	if reversals != 2 {
		t.Fatalf("应有报损冲回+重做成本冲回 2 条流水, got %d", reversals)
	}

	// 旧码恢复有效，可正常交接；任务 old_taken
	sc, b = do(t, "POST", "/v1/rider/handoffs", "4001", map[string]any{
		"order_id": oid, "seal_codes": []string{oldCode},
		"actual_bags": 1, "actual_cold": 1, "has_straw": true,
	})
	mustStatus(t, sc, 200, b)
	var jobStatus string
	_ = suite.pool.QueryRow(context.Background(), `SELECT status FROM remake_jobs WHERE id=$1`, jobID).Scan(&jobStatus)
	if jobStatus != "old_taken" {
		t.Fatalf("任务应 old_taken: %s", jobStatus)
	}
}

func TestCancelDuringRemakeAbortsJob(t *testing.T) {
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("RM-CXL", []intakeItem{
		{"sku": "LT", "qty": 1, "temp": "ice", "sugar": "无糖", "cup_size": "L"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	cid := sealCup(t, oid)
	makeSeal(t, oid, cid)
	setIdleMinutes(t, cid, 40) // 冰拿铁品质 20 分
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/remake", cid), "3001",
		map[string]any{"reason": "stale_drink"})
	mustStatus(t, sc, 201, b)
	jobID := int64(b["remake_job"].(map[string]any)["id"].(float64))

	sc, b = do(t, "POST", fmt.Sprintf("/v1/platform/orders/%d/cancel", oid), "5001",
		map[string]any{"reason": "顾客取消", "store_fault": false})
	mustStatus(t, sc, 200, b)

	var status, decision, compStatus string
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT status,decision FROM remake_jobs WHERE id=$1`, jobID).Scan(&status, &decision)
	if status != "canceled" || decision != "aborted" {
		t.Fatalf("取消后任务应 canceled/aborted: %s/%s", status, decision)
	}
	_ = suite.pool.QueryRow(context.Background(),
		`SELECT status FROM compensations WHERE remake_job_id=$1`, jobID).Scan(&compStatus)
	if compStatus != "withdrawn" {
		t.Fatalf("取消后补偿应撤回: %s", compStatus)
	}
	// 终结后不可再裁决
	sc, _ = do(t, "POST", fmt.Sprintf("/v1/rider/remakes/%d/decision", jobID), "4001", map[string]any{})
	if sc != 409 {
		t.Fatalf("终结任务裁决应 409, got %d", sc)
	}
}

func TestResponsibilityReviewReattribution(t *testing.T) {
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("RM-RV", []intakeItem{
		{"sku": "AM", "qty": 1, "temp": "hot", "sugar": "无糖", "cup_size": "M"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	cid := sealCup(t, oid)
	makeSeal(t, oid, cid)
	setIdleMinutes(t, cid, 26) // 热美式品质 15 / 安全 25 → wait_new
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/remake", cid), "3001",
		map[string]any{"reason": "stale_drink"})
	mustStatus(t, sc, 201, b)
	jobID := int64(b["remake_job"].(map[string]any)["id"].(float64))
	newID := int64(b["new_cup"].(map[string]any)["id"].(float64))
	makeSeal(t, oid, newID)
	newCode := sealCodeOf(t, newID)
	sc, b = do(t, "POST", "/v1/rider/handoffs", "4001", map[string]any{
		"order_id": oid, "seal_codes": []string{newCode}, "actual_bags": 1, "actual_hot": 1,
	})
	mustStatus(t, sc, 200, b)

	// 店长可申请复核但不能改判
	sc, b = do(t, "POST", fmt.Sprintf("/v1/management/orders/%d/responsibility-reviews", oid), "2001",
		map[string]any{"ground": "platform_dispatch_delay", "remake_job_id": jobID,
			"reason":   "平台派单延迟 26 分钟",
			"evidence": "骑手轨迹"})
	mustStatus(t, sc, 201, b)
	reviewID := int64(b["id"].(float64))
	if sc, b = do(t, "POST", fmt.Sprintf("/v1/management/responsibility-reviews/%d/adjudicate", reviewID),
		"2001", map[string]any{"approved": true, "new_responsibility": "platform"}); sc != 403 {
		t.Fatalf("店长不能改判，应 403: %d %v", sc, b)
	}

	// 区域经理改判平台承担
	sc, b = do(t, "POST", fmt.Sprintf("/v1/management/responsibility-reviews/%d/adjudicate", reviewID),
		"1001", map[string]any{"approved": true, "new_responsibility": "platform",
			"resolution": "派单延迟证据充分"})
	mustStatus(t, sc, 200, b)
	if b["reattributed_amount"].(float64) <= 0 {
		t.Fatalf("回算金额应>0: %v", b["reattributed_amount"])
	}

	// 原始流水保留，新增 platform_comp 且带 review_id；异常责任更新但原责任保留
	var kinds []string
	var linked int
	rows, err := suite.pool.Query(context.Background(),
		`SELECT kind, (review_id IS NOT NULL)::int FROM finance_records WHERE order_id=$1 ORDER BY id`, oid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var link int
		_ = rows.Scan(&k, &link)
		kinds = append(kinds, k)
		if k == "platform_comp" && link == 1 {
			linked++
		}
	}
	has := func(k string) bool {
		for _, x := range kinds {
			if x == k {
				return true
			}
		}
		return false
	}
	if !has("loss") || !has("remake_cost") || linked != 1 {
		t.Fatalf("原始 loss/remake_cost 应保留且恰有 1 条带复核的 platform_comp: %v", kinds)
	}
	var resp, orig string
	if err := suite.pool.QueryRow(context.Background(),
		`SELECT responsibility,COALESCE(original_responsibility,'') FROM exceptions
		 WHERE order_id=$1 AND type='stale_drink' ORDER BY id DESC LIMIT 1`, oid).Scan(&resp, &orig); err != nil {
		t.Fatal(err)
	}
	if resp != "platform" || orig != "store" {
		t.Fatalf("异常责任应 platform 且原始责任保留为 store: %s/%s", resp, orig)
	}

	// 不可重复改判
	if sc, _ = do(t, "POST", fmt.Sprintf("/v1/management/responsibility-reviews/%d/adjudicate", reviewID),
		"1001", map[string]any{"approved": false}); sc != 409 {
		t.Fatalf("重复改判应 409, got %d", sc)
	}
}

func TestCompensationFulfillCrossPlatformGuard(t *testing.T) {
	sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake("RM-COMP", []intakeItem{
		{"sku": "AM", "qty": 1, "temp": "hot", "sugar": "无糖", "cup_size": "M"},
	}, nil))
	mustStatus(t, sc, 201, b)
	oid := int64(b["id"].(float64))
	cid := sealCup(t, oid)
	makeSeal(t, oid, cid)
	setIdleMinutes(t, cid, 16)
	sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/remake", cid), "3001",
		map[string]any{"reason": "stale_drink"})
	mustStatus(t, sc, 201, b)

	var compID int64
	if err := suite.pool.QueryRow(context.Background(),
		`SELECT id FROM compensations WHERE order_id=$1`, oid).Scan(&compID); err != nil {
		t.Fatal(err)
	}
	// 美团客服不能兑现饿了么订单
	if sc, b = do(t, "POST", fmt.Sprintf("/v1/platform/compensations/%d/fulfill", compID), "5002",
		map[string]any{"note": "串平台"}); sc != 403 || b["error"] != "cross_platform" {
		t.Fatalf("跨平台兑现应 403 cross_platform: %d %v", sc, b)
	}
	// 顾客端可见延迟原因与方案
	sc, b = do(t, "GET", fmt.Sprintf("/v1/customers/orders/%d/status", oid), "7001", nil)
	mustStatus(t, sc, 200, b)
	reasons := b["delay_reasons"].([]any)
	if len(reasons) == 0 {
		t.Fatalf("重做中顾客端应展示延迟原因: %v", b)
	}
	comps := b["compensations"].([]any)
	if len(comps) != 1 || comps[0].(map[string]any)["fulfilled"] != false {
		t.Fatalf("应展示 1 个未兑现补偿方案: %v", comps)
	}
	// 饿了么客服兑现
	sc, b = do(t, "POST", fmt.Sprintf("/v1/platform/compensations/%d/fulfill", compID), "5001",
		map[string]any{"note": "优惠券已到账"})
	mustStatus(t, sc, 200, b)
	if b["status"] != "fulfilled" {
		t.Fatalf("应 fulfilled: %v", b)
	}
	// 已兑现不能重复
	if sc, _ = do(t, "POST", fmt.Sprintf("/v1/platform/compensations/%d/fulfill", compID), "5001",
		map[string]any{}); sc != 409 {
		t.Fatalf("重复兑现应 409, got %d", sc)
	}
}

func sealCodeOf(t *testing.T, cupID int64) string {
	t.Helper()
	var code string
	if err := suite.pool.QueryRow(context.Background(),
		`SELECT COALESCE(seal_code,'') FROM order_cups WHERE id=$1`, cupID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	return code
}

// TestRemakeConcurrentSealVsTakeOld 验证并发不变量：重做过程中，无论“新杯封签”与
// “take_old 裁决”如何竞争，原杯与新杯永远不会同时处于可交接状态（杜绝两杯同时被交接）。
func TestRemakeConcurrentSealVsTakeOld(t *testing.T) {
	for iter := 0; iter < 6; iter++ {
		no := fmt.Sprintf("RM-CONC-%d", iter)
		sc, b := do(t, "POST", "/v1/platform/orders", "9001", intake(no, []intakeItem{
			{"sku": "LT", "qty": 1, "temp": "ice", "sugar": "无糖", "cup_size": "L"},
		}, nil))
		mustStatus(t, sc, 201, b)
		oid := int64(b["id"].(float64))
		oldID := sealCup(t, oid)
		makeSeal(t, oid, oldID)
		setIdleMinutes(t, oldID, 10) // 冰拿铁安全窗口 30 分内 → take_old 合法（直到新杯封口）
		sc, b = do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/remake", oldID), "3001",
			map[string]any{"reason": "stale_drink"})
		mustStatus(t, sc, 201, b)
		jobID := int64(b["remake_job"].(map[string]any)["id"].(float64))
		newID := int64(b["new_cup"].(map[string]any)["id"].(float64))

		var wg sync.WaitGroup
		var sealCode int
		var decisionCode int
		wg.Add(2)
		go func() { // 店员抢做新杯并封签
			defer wg.Done()
			r1, _ := do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/start", newID), "3001", nil)
			if r1 != 200 {
				return
			}
			r2, _ := do(t, "POST", fmt.Sprintf("/v1/staff/cups/%d/seal", newID), "3001", nil)
			sealCode = r2
		}()
		go func() { // 骑手到店要求取原杯
			defer wg.Done()
			r, _ := do(t, "POST", fmt.Sprintf("/v1/rider/remakes/%d/decision", jobID), "4001",
				map[string]any{"action": "take_old"})
			decisionCode = r
		}()
		wg.Wait()

		// 不变量：订单中恰有一杯 sealed 可交接，绝不允许原杯+新杯同时 sealed
		var sealed, sealedIDs string
		err := suite.pool.QueryRow(context.Background(),
			`SELECT count(*)::text, COALESCE(string_agg(id::text,','),'') FROM order_cups
			 WHERE order_id=$1 AND status='sealed'`, oid).Scan(&sealed, &sealedIDs)
		if err != nil {
			t.Fatal(err)
		}
		if sealed != "1" {
			t.Fatalf("迭代 %d：可交接杯数必须恰为 1，实际 %s（id=%s）；seal=%d decision=%d",
				iter, sealed, sealedIDs, sealCode, decisionCode)
		}

		// 唯一有效杯必须能成功交接，且只能交接一次
		var code string
		if err := suite.pool.QueryRow(context.Background(),
			`SELECT seal_code FROM order_cups WHERE order_id=$1 AND status='sealed'`, oid).Scan(&code); err != nil {
			t.Fatal(err)
		}
		hsc, hb := do(t, "POST", "/v1/rider/handoffs", "4001", map[string]any{
			"order_id": oid, "seal_codes": []string{code},
			"actual_bags": 1, "actual_cold": 1, "has_straw": true,
		})
		if hsc != 200 || hb["status"] != "handed" {
			t.Fatalf("迭代 %d：唯一有效杯交接应成功: %d %v", iter, hsc, hb)
		}
		if hsc2, _ := do(t, "POST", "/v1/rider/handoffs", "4001", map[string]any{
			"order_id": oid, "seal_codes": []string{code}, "actual_bags": 1,
		}); hsc2 != 409 {
			t.Fatalf("迭代 %d：二次交接必须 409, got %d", iter, hsc2)
		}
	}
}

var _ = http.MethodPost
