package app

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }

// 放置过久重做的并发交接与成本归属：
//  1. 规则引擎按 饮品类型 × 冰热 × 门店忙闲 给出口味安全窗口（建议重做阈值 / 硬安全窗口）
//  2. 重做两阶段：原杯进入 loss_hold（报损留存、旧封签立即失效），新杯生成新制作序号并入队
//  3. 骑手恰在重做中到店：按 原杯是否仍在硬安全窗口 / 新杯贴是否已打印 / 顾客是否已取消
//     决定 wait_new / take_original / abort，杜绝两杯同时被交接
//  4. 原杯报损(loss_reports)、骑手等待(rider_arrivals)、门店时效(订单时效)、
//     原料扣减(stock_movements reason=remake)、顾客补偿(compensations) 分别归因
//  5. 责任复核只追加冲销/重挂记录，不覆盖原始事件

const (
	remakeStatusInProgress = "in_progress"
	remakeStatusNewReady   = "new_ready"
	remakeStatusCompleted  = "completed"
	remakeStatusAborted    = "aborted"
)

type orderCupFull struct {
	id         int64
	orderID    int64
	storeID    int64
	menuItemID int64
	name       string
	price      float64
	temp       string
	sugar      string
	size       string
	note       string
	seq        int
	status     string
	remakeNo   int
}

// ---------- 规则引擎 ----------

// pickStaleRule 返回最匹配的过久阈值（精确 品类/冰热/忙闲 > any > default）。
func pickStaleRule(ctx context.Context, tx pgx.Tx, category, temp, busyness string) (safeMin, hardMin int, err error) {
	err = tx.QueryRow(ctx, `
		SELECT safe_minutes, hard_safe_minutes FROM stale_rules
		WHERE (category=$1 OR category='default')
		  AND (temp=$2 OR temp='any')
		  AND (busyness=$3 OR busyness='any')
		ORDER BY (category=$1) DESC, (temp=$2) DESC, (busyness=$3) DESC,
		         CASE WHEN category='default' THEN 1 ELSE 0 END
		LIMIT 1`, category, temp, busyness).Scan(&safeMin, &hardMin)
	return
}

// storeBusyness 按当前在制杯量/爆单阈值判断门店忙闲（>=80% 视为忙）。
func storeBusyness(ctx context.Context, tx pgx.Tx, storeID int64, forceBusy bool) (string, int, int, error) {
	var active, max int
	if err := tx.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM order_cups c JOIN orders o ON o.id=c.order_id
		   WHERE o.store_id=$1 AND c.status IN ('queued','making') AND o.status IN ('received','active')),
		  (SELECT max_active_cups FROM stores WHERE id=$1)`, storeID).Scan(&active, &max); err != nil {
		return "", 0, 0, err
	}
	busy := forceBusy || max > 0 && active*100 >= max*80
	if busy {
		return "busy", active, max, nil
	}
	return "normal", active, max, nil
}

// assessStaleCup 评估单杯是否“放置过久”，因子全部回填供留痕与决策使用。
func assessStaleCup(ctx context.Context, tx pgx.Tx, cupID int64, forceBusy bool) (map[string]any, error) {
	var (
		orderID, storeID, menuID int64
		category, temp, status   string
		sealedAt                 *time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT c.order_id,o.store_id,c.menu_item_id,m.category,c.temp,c.status,c.sealed_at
		FROM order_cups c JOIN orders o ON o.id=c.order_id JOIN menu_items m ON m.id=c.menu_item_id
		WHERE c.id=$1`, cupID).Scan(&orderID, &storeID, &menuID, &category, &temp, &status, &sealedAt)
	if err != nil {
		return nil, err
	}
	busyness, active, maxCups, err := storeBusyness(ctx, tx, storeID, forceBusy)
	if err != nil {
		return nil, err
	}
	safeMin, hardMin, err := pickStaleRule(ctx, tx, category, temp, busyness)
	if err != nil {
		return nil, err
	}
	elapsed := 0.0
	if sealedAt != nil {
		elapsed = time.Since(*sealedAt).Minutes()
	}
	var openRemakeID *int64
	_ = tx.QueryRow(ctx, `
		SELECT id FROM remakes WHERE old_cup_id=$1 AND status IN ('in_progress','new_ready') LIMIT 1`,
		cupID).Scan(&openRemakeID)
	out := map[string]any{
		"cup_id": cupID, "order_id": orderID, "store_id": storeID,
		"category": category, "temp": temp, "cup_status": status,
		"sealed_at": sealedAt, "elapsed_minutes": round1(elapsed),
		"busyness": busyness, "active_cups": active, "max_active_cups": maxCups,
		"safe_minutes": safeMin, "hard_safe_minutes": hardMin,
		"stale": sealedAt != nil && int(elapsed) >= safeMin,
	}
	if openRemakeID != nil {
		out["open_remake_id"] = *openRemakeID
	}
	return out, nil
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}

// GET /v1/staff/cups/{id}/stale-check —— 单杯评估（不产生副作用）。
func (a *App) cupStaleCheck(w http.ResponseWriter, r *http.Request) {
	cupID := toInt64(chi.URLParam(r, "id"))
	force := r.URL.Query().Get("force_busy") == "1"
	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	v, err := assessStaleCup(r.Context(), tx, cupID, force)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

// POST /v1/staff/stores/{id}/stale-check —— 扫描门店待取区，auto_remake=true 时自动重做。
type staleCheckReq struct {
	AutoRemake bool `json:"auto_remake"`
	ForceBusy  bool `json:"force_busy"`
}

func (a *App) storeStaleCheck(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	var req staleCheckReq
	if r.ContentLength != 0 {
		if err := decode(w, r, &req); err != nil {
			writeErr(w, err)
			return
		}
	}
	ac := actor(r)
	ctx := r.Context()

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT c.id FROM order_cups c JOIN orders o ON o.id=c.order_id
		WHERE o.store_id=$1 AND c.status='sealed' AND o.status IN ('received','active')
		ORDER BY c.sealed_at`, storeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	var cupIDs []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		cupIDs = append(cupIDs, id)
	}
	rows.Close()

	stale := []map[string]any{}
	triggered := []map[string]any{}
	for _, cid := range cupIDs {
		ev, err := assessStaleCup(ctx, tx, cid, req.ForceBusy)
		if err != nil {
			writeErr(w, err)
			return
		}
		if ev["stale"].(bool) {
			stale = append(stale, ev)
		}
		if req.AutoRemake && ev["stale"].(bool) {
			if _, busy := ev["open_remake_id"]; busy {
				continue
			}
			res, err := a.startRemakeTx(ctx, tx, cid, "stale_drink", "auto_stale",
				map[string]any{"auto": true}, &req.ForceBusy, ac)
			if err != nil {
				writeErr(w, err)
				return
			}
			triggered = append(triggered, map[string]any{
				"remake_id": res.RemakeID, "old_cup_id": res.OldCupID, "new_cup_id": res.NewCupID,
				"exception_id": res.ExceptionID, "loss_cost": res.LossCost, "factors": res.Factors,
			})
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"store_id": storeID, "checked": len(cupIDs),
		"stale_cups": stale, "triggered_remakes": triggered,
	})
}

// ---------- 重做（两阶段工作流） ----------

type remakeResult struct {
	RemakeID    int64
	OldCupID    int64
	NewCupID    int64
	ExceptionID int64
	LossCost    float64
	Factors     map[string]any
}

func (a *App) remakeCup(w http.ResponseWriter, r *http.Request) {
	cupID := toInt64(chi.URLParam(r, "id"))
	var req remakeReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	switch req.Reason {
	case "stale_drink", "wrong_cup", "note_missed":
	default:
		writeErr(w, errf(400, "bad_request", "reason 必须是 stale_drink/wrong_cup/note_missed"))
		return
	}
	ac := actor(r)
	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)
	res, err := a.startRemakeTx(ctx, tx, cupID, req.Reason, "manual", req.Detail, nil, ac)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	cup, _ := a.loadCup(ctx, res.NewCupID)
	writeJSON(w, 201, map[string]any{
		"new_cup": cup, "old_cup_id": res.OldCupID, "remake_id": res.RemakeID,
		"exception_id": res.ExceptionID, "loss_cost": res.LossCost, "factors": res.Factors,
	})
}

// startRemakeTx 在事务内发起重做：原杯 loss_hold 报损 → 扣新杯原料 → 新杯（新序号）入队 →
// 异常/报损/财务/补偿方案/事件分别落账。调用方负责事务提交。
func (a *App) startRemakeTx(ctx context.Context, tx pgx.Tx, cupID int64,
	reason, triggerType string, detail map[string]any, forceBusy *bool, ac *Account) (*remakeResult, error) {

	if detail == nil {
		detail = map[string]any{}
	}
	// 先取订单，再按“订单→杯”的固定顺序加锁，避免与取消/交接互锁
	var orderID, storeID int64
	if err := tx.QueryRow(ctx, `SELECT order_id FROM order_cups WHERE id=$1`, cupID).Scan(&orderID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(93002, $1)`, orderID); err != nil {
		return nil, err
	}
	var oStatus string
	if err := tx.QueryRow(ctx, `SELECT status,store_id FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&oStatus, &storeID); err != nil {
		return nil, err
	}
	if oStatus == "canceled" || oStatus == "handed" {
		return nil, errf(409, "conflict", "订单已 %s，不能重做", oStatus)
	}

	var old orderCupFull
	var oldSealCode, category string
	var sealedAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT c.id,c.order_id,o.store_id,c.menu_item_id,c.name_snapshot,c.unit_price,c.temp,c.sugar,
		       c.cup_size,COALESCE(c.cup_note,''),c.seq,c.status,c.remake_no,m.category,c.sealed_at,
		       COALESCE(c.seal_code,'')
		FROM order_cups c JOIN orders o ON o.id=c.order_id JOIN menu_items m ON m.id=c.menu_item_id
		WHERE c.id=$1 FOR UPDATE`, cupID).
		Scan(&old.id, &old.orderID, &old.storeID, &old.menuItemID, &old.name, &old.price,
			&old.temp, &old.sugar, &old.size, &old.note, &old.seq, &old.status, &old.remakeNo,
			&category, &sealedAt, &oldSealCode)
	if err != nil {
		return nil, err
	}
	switch old.status {
	case "voided", "loss_hold":
		return nil, errf(409, "conflict", "该杯已作废/报损留存，不能重做（请基于最新一杯重做）")
	}
	// 同一杯链只允许一个进行中的重做
	var busy int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM remakes
		WHERE (old_cup_id=$1 OR new_cup_id=$1) AND status IN ('in_progress','new_ready')`,
		cupID).Scan(&busy); err != nil {
		return nil, err
	}
	if busy > 0 {
		return nil, errf(409, "remake_in_progress", "该杯已有进行中的重做，不能重复触发")
	}

	// 判定因子：品类 × 冰热 × 放置时长 × 门店忙闲
	force := forceBusy != nil && *forceBusy
	busyness, activeCups, maxCups, err := storeBusyness(ctx, tx, old.storeID, force)
	if err != nil {
		return nil, err
	}
	safeMin, hardMin, err := pickStaleRule(ctx, tx, category, old.temp, busyness)
	if err != nil {
		return nil, err
	}
	elapsedMin := 0.0
	if sealedAt != nil {
		elapsedMin = time.Since(*sealedAt).Minutes()
	}
	factors := map[string]any{
		"trigger": triggerType, "reason": reason, "category": category, "temp": old.temp,
		"busyness": busyness, "active_cups": activeCups, "max_active_cups": maxCups,
		"elapsed_minutes": round1(elapsedMin), "safe_minutes": safeMin, "hard_safe_minutes": hardMin,
	}
	if sealedAt != nil {
		factors["sealed_at"] = sealedAt.Format(time.RFC3339)
	}
	if reason == "stale_drink" && sealedAt != nil && int(elapsedMin) < safeMin && triggerType == "manual" {
		factors["early_remake"] = true // 未达阈值仍由门店手动重做，留痕
	}

	recipes, err := loadRecipes(ctx, tx, []int64{old.menuItemID})
	if err != nil {
		return nil, err
	}
	cost := cupCost(recipes[old.menuItemID], old.temp, old.size, old.sugar)

	// 1) 扣减新杯原料（原料扣减独立归因：stock_movements.reason='remake'）
	var needs []stockNeed
	for _, rr := range recipes[old.menuItemID] {
		if q := cupRequirement(rr, old.temp, old.size, old.sugar); q > 0 {
			needs = append(needs, stockNeed{IngID: rr.IngredientID, Delta: -q, Reason: "remake", OrderID: &old.orderID, StaffID: &ac.ID})
		}
	}
	if err := applyStock(ctx, tx, old.storeID, needs); err != nil {
		return nil, err
	}

	// 2) 原杯进入报损留存：不可再交接；旧封签码保留仅用于识别“扫旧码”，但立即失效
	if _, err := tx.Exec(ctx,
		`UPDATE order_cups SET status='loss_hold',voided_at=now(),void_reason=$2 WHERE id=$1`,
		old.id, reason); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM order_bags WHERE cup_id=$1`, old.id); err != nil {
		return nil, err
	}

	// 3) 新杯：新的制作序号（不是沿用旧序号），封签在重新封口时生成新码
	var newSeq int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(max(seq),0)+1 FROM order_cups WHERE order_id=$1`, old.orderID).Scan(&newSeq); err != nil {
		return nil, err
	}
	var newID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO order_cups(order_id,menu_item_id,name_snapshot,unit_price,temp,sugar,cup_size,cup_note,
			seq,status,remake_of_cup_id,remake_no)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'queued',$10,$11) RETURNING id`,
		old.orderID, old.menuItemID, old.name, old.price, old.temp, old.sugar, old.size, old.note,
		newSeq, old.id, old.remakeNo+1).Scan(&newID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE orders SET status='received',all_sealed_at=NULL WHERE id=$1 AND status='active'`, old.orderID); err != nil {
		return nil, err
	}

	// 4) 异常单（初责门店：放置过久属门店时效；店长可就设备故障/平台派单延迟申请复核改判）
	excID, existed, err := openException(ctx, tx, old.orderID, reason)
	if err != nil {
		return nil, err
	}
	if existed {
		if _, err := tx.Exec(ctx,
			`UPDATE exceptions SET occurrences=occurrences+1,cost_amount=cost_amount+$2,
			        detail=detail||$3
			 WHERE id=$1`,
			excID, cost, map[string]any{"remake_history": detail}); err != nil {
			return nil, err
		}
	} else {
		edetail := map[string]any{"old_cup_id": old.id, "new_cup_id": newID, "factors": factors}
		for k, v := range detail {
			edetail[k] = v
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO exceptions(order_id,store_id,type,status,responsibility,detail,cost_amount,created_by)
			VALUES($1,$2,$3,'open','store',$4,$5,$6) RETURNING id`,
			old.orderID, old.storeID, reason, edetail, cost, ac.ID).Scan(&excID); err != nil {
			return nil, err
		}
	}

	// 5) 原杯报损台账
	if _, err := tx.Exec(ctx, `
		INSERT INTO loss_reports(store_id,order_id,cup_id,exception_id,reason,qty_cups,cost_amount,note,staff_id)
		VALUES($1,$2,$3,$4,$5,1,$6,$7,$8)`,
		old.storeID, old.orderID, old.id, excID, reason, cost,
		reason+" 重做报损（原杯留存不可交接）", ac.ID); err != nil {
		return nil, err
	}
	// 6) 重做成本先计入班组（临时归属；责任复核改判后以追加冲销/重挂回算）
	if _, err := tx.Exec(ctx, `
		INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,note,created_by)
		VALUES($1,$2,$3,'remake_cost',$4,'team',$5,$6)`,
		old.storeID, old.orderID, excID, -cost, reason+" 重做成本暂计门店班组（可责任复核）", ac.ID); err != nil {
		return nil, err
	}

	// 7) 重做工作流
	var safetyDeadline *time.Time
	if sealedAt != nil && reason == "stale_drink" {
		t := sealedAt.Add(time.Duration(hardMin) * time.Minute)
		safetyDeadline = &t
	}
	var remakeID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO remakes(store_id,order_id,old_cup_id,new_cup_id,exception_id,reason,trigger_type,
			status,factors,old_seal_code,safety_deadline,created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,'in_progress',$8,$9,$10,$11) RETURNING id`,
		old.storeID, old.orderID, old.id, newID, excID, reason, triggerType,
		factors, oldSealCode, safetyDeadline, ac.ID).Scan(&remakeID); err != nil {
		return nil, err
	}

	// 8) 顾客补偿方案（仅过久重做；wrong_cup/note_missed 走投诉/定责路径）
	if reason == "stale_drink" {
		planAmount := 0.3 * old.price
		if planAmount < 5 {
			planAmount = 5
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO compensations(store_id,order_id,remake_id,exception_id,reason,planned_amount,
				plan_detail,status,proposed_by)
			VALUES($1,$2,$3,$4,'stale_remake',$5,$6,'proposed',$7)`,
			old.storeID, old.orderID, remakeID, excID, planAmount,
			map[string]any{
				"type": "voucher", "basis": "stale_remake_wait",
				"elapsed_minutes": factors["elapsed_minutes"], "safe_minutes": safeMin,
				"message": "饮品放置过久已为您重做，补偿方案待平台客服确认兑现",
			}, ac.ID); err != nil {
			return nil, err
		}
	}

	if err := a.logEvent(ctx, tx, old.orderID, "cup_remade", actorOf(ac), map[string]any{
		"remake_id": remakeID, "old_cup_id": old.id, "new_cup_id": newID,
		"old_seal_code_invalidated": oldSealCode, "new_seq": newSeq,
		"reason": reason, "trigger": triggerType, "cost": cost, "factors": factors,
	}); err != nil {
		return nil, err
	}
	return &remakeResult{
		RemakeID: remakeID, OldCupID: old.id, NewCupID: newID,
		ExceptionID: excID, LossCost: cost, Factors: factors,
	}, nil
}

// ---------- 新杯就绪 / 杯贴打印标记 ----------

// markRemakeNewReady 在新杯封签后调用：新杯已成，等待骑手扫新码取走。
func (a *App) markRemakeNewReady(ctx context.Context, tx pgx.Tx, newCupID int64, sealCode string) error {
	var remakeID, orderID int64
	err := tx.QueryRow(ctx, `
		SELECT id,order_id FROM remakes
		WHERE new_cup_id=$1 AND status IN ('in_progress','new_ready') LIMIT 1`,
		newCupID).Scan(&remakeID, &orderID)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var printedAt *time.Time
	_ = tx.QueryRow(ctx, `SELECT label_printed_at FROM order_cups WHERE id=$1`, newCupID).Scan(&printedAt)
	if _, err := tx.Exec(ctx,
		`UPDATE remakes SET status='new_ready',new_label_printed_at=COALESCE(new_label_printed_at,$2)
		 WHERE id=$1`, remakeID, printedAt); err != nil {
		return err
	}
	return a.logEvent(ctx, tx, orderID, "remake_new_ready", nil, map[string]any{
		"remake_id": remakeID, "new_cup_id": newCupID, "new_seal_code": sealCode,
	})
}

// POST /v1/staff/cups/{id}/label-print —— 标记新杯贴已打印（交接决策因子之一）。
func (a *App) markCupLabelPrinted(w http.ResponseWriter, r *http.Request) {
	cupID := toInt64(chi.URLParam(r, "id"))
	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var orderID int64
	var status string
	if err := tx.QueryRow(ctx, `SELECT order_id,status FROM order_cups WHERE id=$1 FOR UPDATE`, cupID).
		Scan(&orderID, &status); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := tx.Exec(ctx,
		`UPDATE order_cups SET label_printed_at=COALESCE(label_printed_at,now()) WHERE id=$1`, cupID); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := tx.Exec(ctx,
		`UPDATE remakes SET new_label_printed_at=now() WHERE new_cup_id=$1 AND status='in_progress'`, cupID); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"cup_id": cupID, "label_printed": true})
}

// ---------- 重做中骑手到店的交接决策 ----------

type openRemake struct {
	ID              int64
	OrderID         int64
	OldCupID        int64
	NewCupID        *int64
	Reason          string
	Status          string
	OldSealCode     *string
	NewSealCode     *string
	NewCupStatus    string
	SafetyDeadline  *time.Time
	NewLabelPrinted bool
	Decision        *string
}

func loadOpenRemakes(ctx context.Context, tx pgx.Tx, orderID int64) ([]openRemake, error) {
	return loadRemakes(ctx, tx, orderID, false)
}

// loadRemakes includeAborted=true 时额外带出“顾客取消刚终止”的重做，供决策确认与查询展示。
func loadRemakes(ctx context.Context, tx pgx.Tx, orderID int64, includeAborted bool) ([]openRemake, error) {
	statuses := `r.status IN ('in_progress','new_ready')`
	if includeAborted {
		statuses = `r.status IN ('in_progress','new_ready','aborted')`
	}
	rows, err := tx.Query(ctx, `
		SELECT r.id,r.order_id,r.old_cup_id,r.new_cup_id,r.reason,r.status,
		       NULLIF(r.old_seal_code,''), nc.seal_code, COALESCE(nc.status,''),
		       r.safety_deadline, (r.new_label_printed_at IS NOT NULL), r.decision
		FROM remakes r LEFT JOIN order_cups nc ON nc.id=r.new_cup_id
		WHERE r.order_id=$1 AND `+statuses+`
		ORDER BY r.id FOR UPDATE OF r`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []openRemake
	for rows.Next() {
		var x openRemake
		if err := rows.Scan(&x.ID, &x.OrderID, &x.OldCupID, &x.NewCupID, &x.Reason, &x.Status,
			&x.OldSealCode, &x.NewSealCode, &x.NewCupStatus,
			&x.SafetyDeadline, &x.NewLabelPrinted, &x.Decision); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// recommendRemake 三因子决策：顾客取消 > 新杯已封口 > 原杯安全窗口且未打新杯贴。
func recommendRemake(r openRemake, customerCanceled bool, now time.Time) (string, string) {
	switch {
	case customerCanceled:
		return "abort", "customer_canceled"
	case r.Status == remakeStatusNewReady || r.NewCupStatus == "sealed":
		return "wait_new", "new_cup_ready"
	case r.SafetyDeadline != nil && !now.After(*r.SafetyDeadline) && !r.NewLabelPrinted:
		return "take_original", "original_within_safety_window"
	default:
		if r.SafetyDeadline == nil {
			return "wait_new", "original_not_servable" // 错杯/漏备注重做，原杯绝不能再给顾客
		}
		if now.After(*r.SafetyDeadline) {
			return "wait_new", "safety_window_expired"
		}
		return "wait_new", "new_label_printed"
	}
}

func remakeView(r openRemake, customerCanceled bool, now time.Time) map[string]any {
	rec, recReason := recommendRemake(r, customerCanceled, now)
	inWindow := r.SafetyDeadline != nil && !now.After(*r.SafetyDeadline)
	v := map[string]any{
		"remake_id": r.ID, "reason": r.Reason, "status": r.Status,
		"old_cup_id": r.OldCupID, "new_cup_id": r.NewCupID,
		"old_seal_code": r.OldSealCode, "new_seal_code": r.NewSealCode,
		"new_cup_status": r.NewCupStatus, "new_label_printed": r.NewLabelPrinted,
		"original_in_safety_window": inWindow, "safety_deadline": r.SafetyDeadline,
		"decision_recorded": r.Decision, "recommended": rec, "recommend_reason": recReason,
	}
	if r.SafetyDeadline != nil {
		v["remaining_safety_minutes"] = round1(r.SafetyDeadline.Sub(now).Minutes())
	}
	return v
}

// GET /v1/rider/orders/{id}/remake-decision
func (a *App) remakeDecisionGet(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, orderID).Scan(&status); err != nil {
		writeErr(w, err)
		return
	}
	list, err := loadRemakes(ctx, tx, orderID, true)
	if err != nil {
		writeErr(w, err)
		return
	}
	views := []map[string]any{}
	now := time.Now()
	for _, rm := range list {
		views = append(views, remakeView(rm, status == "canceled", now))
	}
	writeJSON(w, 200, map[string]any{
		"order_id": orderID, "order_status": status,
		"customer_canceled": status == "canceled", "remakes": views,
	})
}

type remakeDecisionReq struct {
	RemakeID int64  `json:"remake_id"`
	Decision string `json:"decision"` // wait_new / take_original / abort
}

// POST /v1/rider/orders/{id}/remake-decision
func (a *App) remakeDecisionPost(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	var req remakeDecisionReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	switch req.Decision {
	case "wait_new", "take_original", "abort":
	default:
		writeErr(w, errf(400, "bad_request", "decision 必须是 wait_new/take_original/abort"))
		return
	}
	ac := actor(r)
	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(93002, $1)`, orderID); err != nil {
		writeErr(w, err)
		return
	}
	var oStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1 FOR UPDATE`, orderID).Scan(&oStatus); err != nil {
		writeErr(w, err)
		return
	}
	list, err := loadRemakes(ctx, tx, orderID, true)
	if err != nil {
		writeErr(w, err)
		return
	}
	if len(list) == 0 {
		writeErr(w, errf(409, "no_open_remake", "该订单没有进行中的重做"))
		return
	}
	// abort 用于顾客取消后确认终止（取消事务已把重做为 aborted）
	if req.Decision == "abort" && oStatus == "canceled" {
		found := false
		for _, rm := range list {
			if (req.RemakeID == 0 || rm.ID == req.RemakeID) && rm.Status == remakeStatusAborted {
				found = true
				break
			}
		}
		if !found {
			writeErr(w, errf(409, "abort_pending_cancel", "取消处理中或无可终止的重做，请稍后重试"))
			return
		}
		if err := a.logEvent(ctx, tx, orderID, "remake_decision", actorOf(ac), map[string]any{
			"remake_id": req.RemakeID, "decision": "abort", "reason": "customer_canceled",
		}); err != nil {
			writeErr(w, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"status": remakeStatusAborted, "decision": "abort"})
		return
	}
	// wait_new / take_original 只能针对进行中的重做，默认取最早一个
	var active []openRemake
	for _, rm := range list {
		if rm.Status == remakeStatusInProgress || rm.Status == remakeStatusNewReady {
			active = append(active, rm)
		}
	}
	if len(active) == 0 {
		writeErr(w, errf(409, "no_open_remake", "该订单没有进行中的重做（可能已随取消终止）"))
		return
	}
	target := active[0]
	if req.RemakeID != 0 {
		found := false
		for _, rm := range active {
			if rm.ID == req.RemakeID {
				target, found = rm, true
				break
			}
		}
		if !found {
			writeErr(w, errf(404, "remake_not_found", "指定的重做流程不存在或已结束"))
			return
		}
	}
	now := time.Now()
	rec, recReason := recommendRemake(target, oStatus == "canceled", now)

	switch req.Decision {
	case "abort":
		writeErr(w, errf(409, "abort_requires_cancel", "顾客未取消，不能终止重做；取消由平台客服发起"))
		return

	case "take_original":
		if oStatus == "canceled" {
			writeErr(w, errf(409, "order_canceled", "订单已取消，不能取原杯"))
			return
		}
		if target.NewCupStatus == "sealed" || target.Status == remakeStatusNewReady {
			writeErr(w, errf(409, "new_cup_ready",
				"新杯已封签，只能等待新杯（扫新封签码），避免两杯同时交接"))
			return
		}
		if target.SafetyDeadline == nil {
			writeErr(w, errf(409, "original_not_servable", "该重做非过久重做（错杯/漏备注），原杯不得再交接"))
			return
		}
		if now.After(*target.SafetyDeadline) {
			writeErr(w, errf(409, "safety_window_expired",
				"原杯已超出硬安全窗口 %s，只能等待新杯", target.SafetyDeadline.Format(time.RFC3339)))
			return
		}
		if target.NewLabelPrinted {
			writeErr(w, errf(409, "new_label_printed", "新杯贴已打印，只能等待新杯"))
			return
		}
		if err := applyTakeOriginal(ctx, tx, a, target, ac); err != nil {
			writeErr(w, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{
			"remake_id": target.ID, "status": remakeStatusCompleted,
			"decision": "take_original", "old_cup_id": target.OldCupID,
			"old_seal_code": target.OldSealCode, "message": "原杯仍在安全窗口且新杯贴未打印，骑手可取原杯；新杯已作废并回库",
		})
		return

	case "wait_new":
		if oStatus == "canceled" {
			writeErr(w, errf(409, "order_canceled", "订单已取消，应终止重做(abort)"))
			return
		}
		if _, err := tx.Exec(ctx, `
			UPDATE remakes SET decision='wait_new',decided_by=$2,decided_at=now()
			WHERE id=$1 AND status IN ('in_progress','new_ready')`, target.ID, ac.ID); err != nil {
			writeErr(w, err)
			return
		}
		if err := a.logEvent(ctx, tx, orderID, "remake_decision", actorOf(ac), map[string]any{
			"remake_id": target.ID, "decision": "wait_new", "recommended": rec, "recommend_reason": recReason,
		}); err != nil {
			writeErr(w, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{
			"remake_id": target.ID, "status": target.Status,
			"decision": "wait_new", "recommended": rec,
			"message": "骑手继续等待新杯；新杯完成后只能扫描新封签码取走",
		})
	}
}

// applyTakeOriginal 执行“取原杯”：新杯作废回库、原杯恢复 sealed 并回袋（不重复扣袋耗材）、
// 补偿方案作废、重做流程完成。
func applyTakeOriginal(ctx context.Context, tx pgx.Tx, a *App, rm openRemake, ac *Account) error {
	// 原杯门店（回库需要）
	var storeID int64
	var menuItemID int64
	var ctemp string
	if err := tx.QueryRow(ctx, `
		SELECT o.store_id,c.menu_item_id,c.temp FROM order_cups c JOIN orders o ON o.id=c.order_id
		WHERE c.id=$1`, rm.OldCupID).Scan(&storeID, &menuItemID, &ctemp); err != nil {
		return err
	}
	// 1) 未封口的新杯作废并回库（封口的新杯按决策校验不可能走到这里）
	newCupID := int64(0)
	if rm.NewCupID != nil {
		newCupID = *rm.NewCupID
	}
	if newCupID != 0 {
		var itemID int64
		var temp, size, sugar, nstatus string
		if err := tx.QueryRow(ctx, `
			SELECT menu_item_id,temp,cup_size,sugar,status FROM order_cups WHERE id=$1 FOR UPDATE`,
			newCupID).Scan(&itemID, &temp, &size, &sugar, &nstatus); err != nil {
			return err
		}
		if nstatus != "sealed" {
			recipes, err := loadRecipes(ctx, tx, []int64{itemID})
			if err != nil {
				return err
			}
			var needs []stockNeed
			for _, rr := range recipes[itemID] {
				if q := cupRequirement(rr, temp, size, sugar); q > 0 {
					needs = append(needs, stockNeed{IngID: rr.IngredientID, Delta: q, Reason: "remake_cancel_restock", OrderID: &rm.OrderID})
				}
			}
			if err := applyStock(ctx, tx, storeID, needs); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE order_cups SET status='voided',voided_at=now(),void_reason='remake_take_original' WHERE id=$1`,
			newCupID); err != nil {
			return err
		}
	}
	// 2) 原杯恢复为 sealed 并重新挂回原物理袋（袋子仍在，不重复扣袋耗材）
	if _, err := tx.Exec(ctx, `UPDATE order_cups SET status='sealed' WHERE id=$1`, rm.OldCupID); err != nil {
		return err
	}
	var isDrink, includesDrink bool
	if err := tx.QueryRow(ctx, `SELECT is_drink,includes_drink FROM menu_items WHERE id=$1`, menuItemID).
		Scan(&isDrink, &includesDrink); err != nil {
		return err
	}
	kind := "food"
	if isDrink || includesDrink {
		if ctemp == "ice" {
			kind = "cold"
		} else {
			kind = "hot"
		}
	}
	bagCode := bagCodeOf(rm.OrderID, kind)
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_bags(store_id,order_id,group_id,bag_code,kind,cup_id)
		SELECT $1,o.id,o.merged_group_id,$2,$3,$4 FROM orders o WHERE o.id=$5
		ON CONFLICT DO NOTHING`,
		storeID, bagCode, kind, rm.OldCupID, rm.OrderID); err != nil {
		return err
	}
	// 3) 重做流程结案
	if _, err := tx.Exec(ctx, `
		UPDATE remakes SET status='completed',decision='take_original',decided_by=$2,
		       decided_at=now(),completed_at=now() WHERE id=$1`, rm.ID, ac.ID); err != nil {
		return err
	}
	// 4) 顾客未受重做影响 → 未兑现的补偿方案作废
	if _, err := tx.Exec(ctx,
		`UPDATE compensations SET status='voided' WHERE remake_id=$1 AND status='proposed'`, rm.ID); err != nil {
		return err
	}
	// 5) 原杯正常售出、新杯未做已回库 → 无实际物料损失，追加一条重做成本冲销（原暂计记录保留不覆盖）
	var temp2, size2, sugar2 string
	var menuID2 int64
	if err := tx.QueryRow(ctx, `
		SELECT menu_item_id,temp,cup_size,sugar FROM order_cups WHERE id=$1`, rm.OldCupID).
		Scan(&menuID2, &temp2, &size2, &sugar2); err != nil {
		return err
	}
	if recipes, err := loadRecipes(ctx, tx, []int64{menuID2}); err == nil {
		if oldCost := cupCost(recipes[menuID2], temp2, size2, sugar2); oldCost > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO finance_records(store_id,order_id,kind,amount,bearer,note,created_by)
				VALUES($1,$2,'remake_cost',$3,'team',$4,$5)`,
				storeID, rm.OrderID, oldCost,
				"骑手安全窗口内取原杯，重做未实际消耗物料，冲销暂计重做成本（重做#"+itoa(rm.ID)+"）", ac.ID); err != nil {
				return err
			}
		}
	}
	maybeReady(ctx, tx, rm.OrderID)
	return a.logEvent(ctx, tx, rm.OrderID, "remake_decision", actorOf(ac), map[string]any{
		"remake_id": rm.ID, "decision": "take_original", "old_cup_id": rm.OldCupID,
		"new_cup_voided": newCupID, "old_seal_code_reactivated": rm.OldSealCode,
	})
}

// abortRemakesOnCancel 顾客取消时终止进行中的重做：工作流结案 + 补偿方案作废。
// 新杯（queued/making/sealed）的回库或报损由 cancelOrder 的统一杯循环处理，不重复动库存；
// 原杯已在发起重做时报损(loss_hold)，取消时同样不重复记账。
func (a *App) abortRemakesOnCancel(ctx context.Context, tx pgx.Tx, orderID int64, ac *eventActor) error {
	rows, err := tx.Query(ctx, `
		SELECT id,new_cup_id FROM remakes
		WHERE order_id=$1 AND status IN ('in_progress','new_ready') FOR UPDATE`, orderID)
	if err != nil {
		return err
	}
	type pair struct {
		remakeID int64
		newCupID *int64
	}
	var list []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.remakeID, &p.newCupID); err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	rows.Close()
	for _, p := range list {
		if _, err := tx.Exec(ctx, `
			UPDATE remakes SET status='aborted',decision='abort',aborted_at=now(),
			       abort_reason='customer_canceled' WHERE id=$1`, p.remakeID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE compensations SET status='voided' WHERE remake_id=$1 AND status='proposed'`,
			p.remakeID); err != nil {
			return err
		}
		if err := a.logEvent(ctx, tx, orderID, "remake_aborted", ac, map[string]any{
			"remake_id": p.remakeID, "new_cup_id": p.newCupID, "reason": "customer_canceled",
		}); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 交接时的封签码核验 / 重做结案 ----------

// verifyRemakeHandoff 校验骑手扫描码并在交接通过时完成重做工作流。
// 规则：进行中/新杯就绪的重做，骑手必须扫描新封签码；扫到旧码（原杯 loss_hold）立即拒绝。
// 返回扫描到的封签码集合对应的错误；nil 表示通过。
func (a *App) verifyRemakeHandoff(ctx context.Context, tx pgx.Tx, orderID int64,
	scanned []string, ac *Account) error {
	openList, err := loadOpenRemakes(ctx, tx, orderID)
	if err != nil {
		return err
	}
	scanSet := map[string]bool{}
	for _, s := range scanned {
		scanSet[s] = true
	}
	for _, rm := range openList {
		// 扫到旧封签码 → 旧码已失效
		if rm.OldSealCode != nil && *rm.OldSealCode != "" && scanSet[*rm.OldSealCode] {
			return errf(409, "old_seal_invalid",
				"扫描到的封签码 %s 属于已报损原杯（重做#%d），旧码已失效；只能扫描新杯封签码取走",
				*rm.OldSealCode, rm.ID)
		}
		newCode := ""
		if rm.NewSealCode != nil {
			newCode = *rm.NewSealCode
		}
		switch {
		case newCode == "":
			// 新杯尚未封签 → 订单整体本就不会全部 sealed，drinks_not_ready 已拦截
			return errf(409, "remake_new_not_ready",
				"重做#%d 的新杯尚未完成，请等待新杯", rm.ID)
		case !scanSet[newCode]:
			// 重做后骑手只能扫描新封签码取走（店员代核验也必须扫新码）
			return errf(409, "must_scan_new_seal",
				"订单存在进行中的重做#%d，骑手必须扫描新封签码 %s 才能取走", rm.ID, newCode)
		}
	}
	return nil
}

// completeRemakesOnHandoff 交接成功后把 wait_new 的重做结案。
func (a *App) completeRemakesOnHandoff(ctx context.Context, tx pgx.Tx, orderID int64, ac *Account) error {
	rows, err := tx.Query(ctx, `
		SELECT id,new_cup_id FROM remakes
		WHERE order_id=$1 AND status IN ('in_progress','new_ready')`, orderID)
	if err != nil {
		return err
	}
	type pair struct {
		remakeID int64
		newCupID *int64
	}
	var list []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.remakeID, &p.newCupID); err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	rows.Close()
	for _, p := range list {
		if _, err := tx.Exec(ctx, `
			UPDATE remakes SET status='completed',decision=COALESCE(decision,'wait_new'),
			       decided_by=COALESCE(decided_by,$2),decided_at=COALESCE(decided_at,now()),
			       completed_at=now() WHERE id=$1`, p.remakeID, ac.ID); err != nil {
			return err
		}
		// 交接即补偿触发点：仍未兑现的重做补偿保持 proposed 等客服兑现（顾客端已可见方案）
		if err := a.logEvent(ctx, tx, orderID, "remake_completed", actorOf(ac), map[string]any{
			"remake_id": p.remakeID, "new_cup_id": p.newCupID, "decision": "wait_new",
		}); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 顾客端：延迟原因 + 补偿方案；客服确认兑现 ----------
func (a *App) customerDelay(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var status, platform string
	var eta *time.Time
	if err := tx.QueryRow(ctx, `SELECT status,COALESCE(platform,''),rider_eta FROM orders WHERE id=$1`,
		orderID).Scan(&status, &platform, &eta); err != nil {
		writeErr(w, err)
		return
	}
	// 骑手等待
	arrRows, err := tx.Query(ctx, `
		SELECT rider_name,arrived_at,COALESCE(wait_seconds,
		       EXTRACT(EPOCH FROM (now()-arrived_at))::int),store_fault_wait_seconds,handed_at
		FROM rider_arrivals WHERE order_id=$1 ORDER BY id`, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	type arrv struct {
		Rider     string     `json:"rider"`
		ArrivedAt time.Time  `json:"arrived_at"`
		WaitSec   int        `json:"wait_seconds"`
		StoreWait int        `json:"store_fault_wait_seconds"`
		HandedAt  *time.Time `json:"handed_at,omitempty"`
	}
	arrivals := []arrv{}
	for arrRows.Next() {
		var x arrv
		if err := arrRows.Scan(&x.Rider, &x.ArrivedAt, &x.WaitSec, &x.StoreWait, &x.HandedAt); err != nil {
			writeErr(w, err)
			return
		}
		arrivals = append(arrivals, x)
	}
	arrRows.Close()
	// 重做（延迟原因来自因子留痕）
	remRows, err := tx.Query(ctx, `
		SELECT id,reason,trigger_type,status,factors,COALESCE(decision,''),created_at,completed_at,aborted_at
		FROM remakes WHERE order_id=$1 ORDER BY id`, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	type remv struct {
		RemakeID    int64          `json:"remake_id"`
		Reason      string         `json:"reason"`
		Trigger     string         `json:"trigger_type"`
		Status      string         `json:"status"`
		Decision    string         `json:"decision"`
		Factors     map[string]any `json:"factors"`
		CreatedAt   time.Time      `json:"created_at"`
		CompletedAt *time.Time     `json:"completed_at,omitempty"`
		AbortedAt   *time.Time     `json:"aborted_at,omitempty"`
	}
	remakes := []remv{}
	for remRows.Next() {
		var x remv
		var raw []byte
		if err := remRows.Scan(&x.RemakeID, &x.Reason, &x.Trigger, &x.Status, &raw,
			&x.Decision, &x.CreatedAt, &x.CompletedAt, &x.AbortedAt); err != nil {
			writeErr(w, err)
			return
		}
		_ = jsonUnmarshal(raw, &x.Factors)
		remakes = append(remakes, x)
	}
	remRows.Close()
	// 补偿方案
	cRows, err := tx.Query(ctx, `
		SELECT id,reason,planned_amount::float8,status,plan_detail,fulfilled_at
		FROM compensations WHERE order_id=$1 ORDER BY id`, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	type compv struct {
		ID            int64          `json:"compensation_id"`
		Reason        string         `json:"reason"`
		PlannedAmount float64        `json:"planned_amount"`
		Status        string         `json:"status"`
		PlanDetail    map[string]any `json:"plan_detail"`
		FulfilledAt   *time.Time     `json:"fulfilled_at,omitempty"`
	}
	comps := []compv{}
	for cRows.Next() {
		var x compv
		var raw []byte
		if err := cRows.Scan(&x.ID, &x.Reason, &x.PlannedAmount, &x.Status, &raw, &x.FulfilledAt); err != nil {
			writeErr(w, err)
			return
		}
		_ = jsonUnmarshal(raw, &x.PlanDetail)
		comps = append(comps, x)
	}
	cRows.Close()

	reasons := []string{}
	for _, rm := range remakes {
		switch rm.Status {
		case remakeStatusInProgress:
			reasons = append(reasons, "饮品放置过久正在重做（按饮品类型、冰热属性、放置时长与门店忙闲判定），请等待新杯")
		case remakeStatusNewReady:
			reasons = append(reasons, "新杯已重做完成，骑手将扫描新封签码取走")
		case remakeStatusCompleted:
			reasons = append(reasons, "重做已完成（决策: "+rm.Decision+"）")
		case remakeStatusAborted:
			reasons = append(reasons, "订单取消，重做已终止")
		}
	}
	writeJSON(w, 200, map[string]any{
		"order_id": orderID, "platform": platform, "order_status": status, "rider_eta": eta,
		"delay_reasons": reasons, "arrivals": arrivals, "remakes": remakes, "compensations": comps,
	})
}

// GET /v1/compensations —— 客服/财务兑现队列（店长只看本门店）
func (a *App) listCompensations(w http.ResponseWriter, r *http.Request) {
	switch actor(r).Role {
	case "platform_cs", "finance", "store_manager", "admin", "area_manager", "supervisor":
	default:
		writeErr(w, errForbidden)
		return
	}
	q := r.URL.Query()
	sql := `
		SELECT c.id,c.store_id,c.order_id,COALESCE(c.remake_id,0),COALESCE(c.exception_id,0),
		       c.reason,c.planned_amount::float8,c.status,c.plan_detail,
		       COALESCE(c.fulfill_note,''),c.created_at,c.fulfilled_at
		FROM compensations c WHERE 1=1`
	args := []any{}
	if s := q.Get("status"); s != "" {
		args = append(args, s)
		sql += " AND c.status=$" + itoa(int64(len(args)))
	}
	if ac := actor(r); ac.Role == "store_manager" && ac.StoreID != nil {
		args = append(args, *ac.StoreID)
		sql += " AND c.store_id=$" + itoa(int64(len(args)))
	}
	sql += " ORDER BY c.id DESC LIMIT 200"
	rows, err := a.Pool.Query(r.Context(), sql, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type cv struct {
		ID            int64          `json:"compensation_id"`
		StoreID       int64          `json:"store_id"`
		OrderID       int64          `json:"order_id"`
		RemakeID      int64          `json:"remake_id"`
		ExceptionID   int64          `json:"exception_id"`
		Reason        string         `json:"reason"`
		PlannedAmount float64        `json:"planned_amount"`
		Status        string         `json:"status"`
		PlanDetail    map[string]any `json:"plan_detail"`
		FulfillNote   string         `json:"fulfill_note"`
		CreatedAt     time.Time      `json:"created_at"`
		FulfilledAt   *time.Time     `json:"fulfilled_at,omitempty"`
	}
	out := []cv{}
	for rows.Next() {
		var x cv
		var raw []byte
		if err := rows.Scan(&x.ID, &x.StoreID, &x.OrderID, &x.RemakeID, &x.ExceptionID,
			&x.Reason, &x.PlannedAmount, &x.Status, &raw, &x.FulfillNote, &x.CreatedAt, &x.FulfilledAt); err != nil {
			writeErr(w, err)
			return
		}
		_ = jsonUnmarshal(raw, &x.PlanDetail)
		out = append(out, x)
	}
	writeJSON(w, 200, out)
}

type compFulfillReq struct {
	Note   string  `json:"note"`
	Amount float64 `json:"amount"` // 为空取方案金额
}

// POST /v1/platform/compensations/{id}/fulfill —— 平台客服确认补偿已兑现
func (a *App) fulfillCompensation(w http.ResponseWriter, r *http.Request) {
	id := toInt64(chi.URLParam(r, "id"))
	var req compFulfillReq
	if r.ContentLength != 0 {
		if err := decode(w, r, &req); err != nil {
			writeErr(w, err)
			return
		}
	}
	ac := actor(r)
	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var orderID, storeID, excID int64
	var planned float64
	var status string
	if err := tx.QueryRow(ctx, `
		SELECT order_id,store_id,COALESCE(exception_id,0),planned_amount,status
		FROM compensations WHERE id=$1 FOR UPDATE`, id).Scan(&orderID, &storeID, &excID, &planned, &status); err != nil {
		writeErr(w, err)
		return
	}
	if status == "fulfilled" {
		writeErr(w, errf(409, "already_fulfilled", "补偿已确认兑现，不能重复操作"))
		return
	}
	if status == "voided" {
		writeErr(w, errf(409, "comp_voided", "补偿方案已作废（顾客取消或原杯取走）"))
		return
	}
	amount := req.Amount
	if amount <= 0 {
		amount = planned
	}
	// 承担方按异常当前定责；尚未定责的过久重做，平台客服兑现时先挂平台，可再由财务/复核调整
	bearer := "platform"
	var responsibility string
	if excID != 0 {
		if err := tx.QueryRow(ctx, `SELECT responsibility FROM exceptions WHERE id=$1`, excID).Scan(&responsibility); err != nil {
			writeErr(w, err)
			return
		}
		switch responsibility {
		case "store", "team", "shared":
			bearer = "store"
		case "rider":
			bearer = "rider"
		default:
			bearer = "platform"
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE compensations SET status='fulfilled',fulfilled_at=now(),fulfilled_by=$2,fulfill_note=$3 WHERE id=$1`,
		id, ac.ID, req.Note); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,note,created_by)
		VALUES($1,$2,$3,'customer_comp',$4,$5,$6,$7)`,
		storeID, orderID, excID, -amount, bearer,
		"重做等待顾客补偿-平台客服确认兑现: "+req.Note, ac.ID); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, orderID, "compensation_fulfilled", actorOf(ac), map[string]any{
		"compensation_id": id, "amount": amount, "bearer": bearer, "note": req.Note,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"compensation_id": id, "status": "fulfilled", "amount": amount, "bearer": bearer,
	})
}

// ---------- 责任复核（店长申请 / 区域经理改判，追加不覆盖） ----------

type reviewReqNew struct {
	RequestType string         `json:"request_type"` // equipment_fault / platform_dispatch_delay
	Reason      string         `json:"reason"`
	Evidence    map[string]any `json:"evidence"`
}

// POST /v1/management/exceptions/{id}/responsibility-review —— 店长发起责任复核
func (a *App) requestResponsibilityReview(w http.ResponseWriter, r *http.Request) {
	excID := toInt64(chi.URLParam(r, "id"))
	var req reviewReqNew
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	switch req.RequestType {
	case "equipment_fault", "platform_dispatch_delay":
	default:
		writeErr(w, errf(400, "bad_request", "request_type 必须是 equipment_fault/platform_dispatch_delay"))
		return
	}
	if req.Reason == "" {
		writeErr(w, errf(400, "bad_request", "reason 必填"))
		return
	}
	ac := actor(r)
	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var orderID, storeID int64
	var resp string
	if err := tx.QueryRow(ctx, `SELECT order_id,store_id,responsibility FROM exceptions WHERE id=$1 FOR UPDATE`,
		excID).Scan(&orderID, &storeID, &resp); err != nil {
		writeErr(w, err)
		return
	}
	if ac.Role == "store_manager" && ac.StoreID != nil && *ac.StoreID != storeID {
		writeErr(w, errForbidden)
		return
	}
	var open int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM responsibility_reviews WHERE exception_id=$1 AND status='requested'`,
		excID).Scan(&open); err != nil {
		writeErr(w, err)
		return
	}
	if open > 0 {
		writeErr(w, errf(409, "review_pending", "该异常已有待审批的责任复核"))
		return
	}
	var rid int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO responsibility_reviews(store_id,order_id,exception_id,request_type,reason,evidence,
			status,requested_by,original_responsibility)
		VALUES($1,$2,$3,$4,$5,$6,'requested',$7,$8) RETURNING id`,
		storeID, orderID, excID, req.RequestType, req.Reason, req.Evidence, ac.ID, resp).Scan(&rid); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, orderID, "responsibility_review_requested", actorOf(ac), map[string]any{
		"review_id": rid, "exception_id": excID, "request_type": req.RequestType,
		"original_responsibility": resp, "reason": req.Reason,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"review_id": rid, "exception_id": excID, "status": "requested",
		"original_responsibility": resp, "request_type": req.RequestType,
	})
}

type reviewDecisionReq struct {
	Approve           bool   `json:"approve"`
	NewResponsibility string `json:"new_responsibility"` // store/team/platform/shared
	Note              string `json:"note"`
}

// POST /v1/management/responsibility-reviews/{id}/decision —— 区域经理改判
func (a *App) decideResponsibilityReview(w http.ResponseWriter, r *http.Request) {
	id := toInt64(chi.URLParam(r, "id"))
	var req reviewDecisionReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Approve {
		switch req.NewResponsibility {
		case "store", "team", "platform", "shared":
		default:
			writeErr(w, errf(400, "bad_request", "批准改判时 new_responsibility 必须是 store/team/platform/shared"))
			return
		}
	}
	ac := actor(r)
	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var orderID, storeID, excID int64
	var status, origResp, reqType string
	if err := tx.QueryRow(ctx, `
		SELECT order_id,store_id,exception_id,status,original_responsibility,request_type
		FROM responsibility_reviews WHERE id=$1 FOR UPDATE`,
		id).Scan(&orderID, &storeID, &excID, &status, &origResp, &reqType); err != nil {
		writeErr(w, err)
		return
	}
	if status != "requested" {
		writeErr(w, errf(409, "review_closed", "该复核已审批：%s", status))
		return
	}

	if !req.Approve {
		if _, err := tx.Exec(ctx,
			`UPDATE responsibility_reviews SET status='rejected',reviewed_by=$2,reviewed_at=now(),decision_note=$3 WHERE id=$1`,
			id, ac.ID, req.Note); err != nil {
			writeErr(w, err)
			return
		}
		if err := a.logEvent(ctx, tx, orderID, "responsibility_review_decided", actorOf(ac), map[string]any{
			"review_id": id, "exception_id": excID, "approve": false, "note": req.Note,
		}); err != nil {
			writeErr(w, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"review_id": id, "status": "rejected"})
		return
	}

	// 批准：异常责任更新为最新判定（原始责任保留在 original_responsibility 与事件流中，不覆盖原始事件）
	if _, err := tx.Exec(ctx,
		`UPDATE exceptions SET responsibility=$2 WHERE id=$1`, excID, req.NewResponsibility); err != nil {
		writeErr(w, err)
		return
	}
	// 财务只追加：对该异常既有的重做成本/报损/顾客补偿，按原 period_date 冲销并按新责任重挂，
	// 重跑报表/班组利润即自动回算；原始财务行不更新、不删除。
	rows, err := tx.Query(ctx, `
		SELECT id,kind,amount,bearer,period_date::text FROM finance_records
		WHERE exception_id=$1 AND kind IN ('remake_cost','loss','customer_comp')
		ORDER BY id`, excID)
	if err != nil {
		writeErr(w, err)
		return
	}
	type fin struct {
		id     int64
		kind   string
		amount float64
		bearer string
		period string
	}
	var fins []fin
	for rows.Next() {
		var x fin
		if err := rows.Scan(&x.id, &x.kind, &x.amount, &x.bearer, &x.period); err != nil {
			rows.Close()
			writeErr(w, err)
			return
		}
		fins = append(fins, x)
	}
	rows.Close()
	for _, f := range fins {
		if _, err := tx.Exec(ctx, `
			INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,period_date,note,created_by)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			storeID, orderID, excID, f.kind, -f.amount, f.bearer, f.period,
			"责任复核#"+itoa(id)+" 冲销原始记录#"+itoa(f.id), ac.ID); err != nil {
			writeErr(w, err)
			return
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,period_date,note,created_by)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			storeID, orderID, excID, f.kind, f.amount, req.NewResponsibility, f.period,
			"责任复核#"+itoa(id)+" 改判 "+origResp+"→"+req.NewResponsibility+"（原记录#"+itoa(f.id)+"）", ac.ID); err != nil {
			writeErr(w, err)
			return
		}
	}
	// 已兑现补偿随改判换承担方；未兑现方案在兑现时直接读最新责任
	if _, err := tx.Exec(ctx, `
		UPDATE compensations SET plan_detail=jsonb_set(COALESCE(plan_detail,'{}'),
		       '{responsibility_after_review}',$2::jsonb,true)
		WHERE exception_id=$1`, excID, `"`+req.NewResponsibility+`"`); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE responsibility_reviews SET status='approved',new_responsibility=$2,reviewed_by=$3,
		       reviewed_at=now(),decision_note=$4 WHERE id=$1`,
		id, req.NewResponsibility, ac.ID, req.Note); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, orderID, "responsibility_review_decided", actorOf(ac), map[string]any{
		"review_id": id, "exception_id": excID, "approve": true,
		"original_responsibility": origResp, "new_responsibility": req.NewResponsibility,
		"request_type": reqType, "reallocated_records": len(fins), "note": req.Note,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"review_id": id, "status": "approved",
		"original_responsibility": origResp, "new_responsibility": req.NewResponsibility,
		"reallocated_finance_records": len(fins) * 2,
		"note":                        "原始事件与财务记录未覆盖；已按原日期追加冲销/重挂，重跑班组利润即回算",
	})
}

// GET /v1/management/responsibility-reviews
func (a *App) listResponsibilityReviews(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	sql := `
		SELECT id,store_id,order_id,exception_id,request_type,reason,evidence,status,
		       COALESCE(requested_by,0),original_responsibility,COALESCE(new_responsibility,''),
		       COALESCE(reviewed_by,0),reviewed_at,COALESCE(decision_note,''),created_at
		FROM responsibility_reviews WHERE 1=1`
	args := []any{}
	if status != "" {
		args = append(args, status)
		sql += " AND status=$" + itoa(int64(len(args)))
	}
	if ac := actor(r); ac.Role == "store_manager" && ac.StoreID != nil {
		args = append(args, *ac.StoreID)
		sql += " AND store_id=$" + itoa(int64(len(args)))
	}
	sql += " ORDER BY id DESC LIMIT 200"
	rows, err := a.Pool.Query(r.Context(), sql, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type rv struct {
		ID           int64          `json:"review_id"`
		StoreID      int64          `json:"store_id"`
		OrderID      int64          `json:"order_id"`
		ExceptionID  int64          `json:"exception_id"`
		RequestType  string         `json:"request_type"`
		Reason       string         `json:"reason"`
		Evidence     map[string]any `json:"evidence"`
		Status       string         `json:"status"`
		RequestedBy  int64          `json:"requested_by"`
		OrigResp     string         `json:"original_responsibility"`
		NewResp      string         `json:"new_responsibility"`
		ReviewedBy   int64          `json:"reviewed_by"`
		ReviewedAt   *time.Time     `json:"reviewed_at,omitempty"`
		DecisionNote string         `json:"decision_note"`
		CreatedAt    time.Time      `json:"created_at"`
	}
	out := []rv{}
	for rows.Next() {
		var x rv
		var raw []byte
		if err := rows.Scan(&x.ID, &x.StoreID, &x.OrderID, &x.ExceptionID, &x.RequestType,
			&x.Reason, &raw, &x.Status, &x.RequestedBy, &x.OrigResp, &x.NewResp,
			&x.ReviewedBy, &x.ReviewedAt, &x.DecisionNote, &x.CreatedAt); err != nil {
			writeErr(w, err)
			return
		}
		_ = jsonUnmarshal(raw, &x.Evidence)
		out = append(out, x)
	}
	writeJSON(w, 200, out)
}
