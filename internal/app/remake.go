package app

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

func actorByID(ctx context.Context, tx pgx.Tx, id int64) *eventActor {
	var name, role string
	if err := tx.QueryRow(ctx, `SELECT name,role FROM accounts WHERE id=$1`, id).Scan(&name, &role); err != nil {
		return nil
	}
	return &eventActor{ID: id, Name: name, Role: role}
}

// lockOrderTx 订单级串行锁：重做 / 交接 / 取消 / 裁决互斥，杜绝两杯同时被交接。
func lockOrderTx(ctx context.Context, tx pgx.Tx, orderID int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(93001, $1)`, orderID)
	return err
}

// lockStoreTx 门店级串行锁：自动重做扫描与另一轮自动重做互斥。
func lockStoreTx(ctx context.Context, tx pgx.Tx, storeID int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(94001, $1)`, storeID)
	return err
}

// ---------- 判定策略（品质窗口 / 安全窗口） ----------

// 品质窗口 qualityWindow：封口后放置超过该时长，品质不可接受，必须重做。
// 安全窗口 safetyWindow：重做过程中原杯仍处于该窗口内时，骑手到店可放行原杯。
// 冰饮比热饮耐放；冷萃最稳，果咖因鲜果切最不稳；热饮放凉最快。
var baseWindows = map[string]struct{ iceQ, iceS, hotQ, hotS int }{
	"americano":    {25, 40, 15, 25},
	"latte":        {20, 30, 15, 25},
	"cold_brew":    {30, 45, 20, 30},
	"fruit_coffee": {20, 30, 15, 25},
	"light_food":   {40, 60, 40, 60},
	"combo":        {25, 40, 15, 25},
}

const (
	busyRatio = 0.6 // 在制杯量达到爆单阈值 60% 视为门店忙
)

type remakePolicy struct {
	Category         string `json:"category"`
	IsDrink          bool   `json:"is_drink"`
	Temp             string `json:"temp"`
	IdleSeconds      int    `json:"idle_seconds"`
	ActiveMaking     int    `json:"active_making_cups"`
	MaxActiveCups    int    `json:"max_active_cups"`
	StoreBusy        bool   `json:"store_busy"`
	QualityWindowSec int    `json:"quality_window_seconds"`
	SafetyWindowSec  int    `json:"safety_window_seconds"`
	Action           string `json:"action"` // remake / monitor / keep
	FactorText       string `json:"factor_text"`
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// storeLoad 返回门店在制杯量与爆单阈值、忙闲标志。
func storeLoad(ctx context.Context, q rowQuerier, storeID int64) (active, maxCups int, busy bool, err error) {
	if err = q.QueryRow(ctx, `SELECT max_active_cups FROM stores WHERE id=$1`, storeID).Scan(&maxCups); err != nil {
		return 0, 0, false, err
	}
	if err = q.QueryRow(ctx,
		`SELECT count(*) FROM order_cups c JOIN orders o ON o.id=c.order_id
		 WHERE o.store_id=$1 AND c.status IN ('queued','making') AND o.status IN ('received','active')`,
		storeID).Scan(&active); err != nil {
		return 0, 0, false, err
	}
	busy = maxCups > 0 && float64(active) >= float64(maxCups)*busyRatio
	return active, maxCups, busy, nil
}

// windowsFor 返回基准（品质窗口, 安全窗口）分钟数。
func windowsFor(category, temp string, isDrink, includesDrink bool) (qualityMin, safetyMin int) {
	w, ok := baseWindows[category]
	if !ok {
		if isDrink || includesDrink {
			w = baseWindows["americano"]
		} else {
			w = baseWindows["light_food"]
		}
	}
	if temp == "ice" {
		return w.iceQ, w.iceS
	}
	return w.hotQ, w.hotS
}

// evaluatePolicy 汇总一杯当前的重做判定因子。sealedAt 为 nil（尚未封口）时 action=keep。
func evaluatePolicy(category, temp string, isDrink, includesDrink bool, sealedAt *time.Time,
	active, maxCups int, busy bool, now time.Time) remakePolicy {
	qMin, sMin := windowsFor(category, temp, isDrink, includesDrink)
	if busy {
		// 门店忙：产能紧张、骑手集中到店，窗口适度放宽 50%
		qMin = qMin * 3 / 2
		sMin = sMin * 3 / 2
	}
	idle := 0
	action := "keep"
	if sealedAt != nil {
		idle = int(now.Sub(*sealedAt).Seconds())
		if idle < 0 {
			idle = 0
		}
		action = "monitor"
		if idle >= qMin*60 {
			action = "remake"
		}
	}
	tempText := map[string]string{"ice": "冰饮", "hot": "热饮", "none": "常温餐食"}[temp]
	if tempText == "" {
		tempText = temp
	}
	factor := "品质窗口 " + itoa(int64(qMin)) + " 分钟、安全窗口 " + itoa(int64(sMin)) + " 分钟"
	if busy {
		factor += "；门店忙（在制 " + itoa(int64(active)) + "/" + itoa(int64(maxCups)) + "），窗口放宽 50%"
	} else {
		factor += "；门店空闲（在制 " + itoa(int64(active)) + "/" + itoa(int64(maxCups)) + "）"
	}
	factor += "；已放置 " + itoa(int64(idle/60)) + " 分" + itoa(int64(idle%60)) + " 秒"
	return remakePolicy{
		Category: category, IsDrink: isDrink || includesDrink, Temp: temp, IdleSeconds: idle,
		ActiveMaking: active, MaxActiveCups: maxCups, StoreBusy: busy,
		QualityWindowSec: qMin * 60, SafetyWindowSec: sMin * 60,
		Action: action, FactorText: tempText + factor,
	}
}

// staleCompPlan 重做补偿方案（顾客补偿单独归因，初始由门店班组承担，可经责任复核改判）。
func staleCompPlan(reason string, unitPrice float64) (float64, string) {
	switch reason {
	case "equipment_fault":
		return round2(unitPrice), "门店设备故障导致出品异常，已免费重做并补偿全额无门槛券"
	case "stale_drink":
		return round2(unitPrice * 0.5), "骑手到店延迟导致饮品放置过久，门店已免费重做并补偿 50% 无门槛券"
	case "wrong_cup":
		return 0, "出品与订单不符，已免费重做"
	case "note_missed":
		return 0, "顾客备注漏看，已按备注免费重做"
	default:
		return 0, "已免费重做"
	}
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// ---------- 评估查询 ----------

type cupPolicyRow struct {
	cupID         int64
	orderID       int64
	storeID       int64
	menuItemID    int64
	sku           string
	category      string
	isDrink       bool
	includesDrink bool
	name          string
	temp          string
	size          string
	sugar         string
	seq           int
	status        string
	remakeNo      int
	sealedAt      *time.Time
	unitPrice     float64
}

const cupPolicyCols = `c.id,c.order_id,o.store_id,c.menu_item_id,COALESCE(m.sku,''),COALESCE(m.category,''),
	m.is_drink,m.includes_drink,c.name_snapshot,c.temp,c.cup_size,c.sugar,c.seq,c.status,c.remake_no,c.sealed_at,c.unit_price::float8`

func loadCupPolicy(ctx context.Context, tx pgx.Tx, cupID int64, lock bool) (*cupPolicyRow, error) {
	sql := `SELECT ` + cupPolicyCols + `
		FROM order_cups c JOIN orders o ON o.id=c.order_id JOIN menu_items m ON m.id=c.menu_item_id
		WHERE c.id=$1`
	if lock {
		sql += " FOR UPDATE"
	}
	var r cupPolicyRow
	err := tx.QueryRow(ctx, sql, cupID).Scan(
		&r.cupID, &r.orderID, &r.storeID, &r.menuItemID, &r.sku, &r.category,
		&r.isDrink, &r.includesDrink, &r.name, &r.temp, &r.size, &r.sugar, &r.seq,
		&r.status, &r.remakeNo, &r.sealedAt, &r.unitPrice)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// activeRemakeJob 返回某杯当前仍在重做中的任务（该杯为旧杯或新杯）。
func activeRemakeJob(ctx context.Context, tx pgx.Tx, cupID int64) (int64, string, error) {
	var id int64
	var status string
	err := tx.QueryRow(ctx,
		`SELECT id,status FROM remake_jobs WHERE (old_cup_id=$1 OR new_cup_id=$1) AND status='remaking'
		 ORDER BY id DESC LIMIT 1`, cupID).Scan(&id, &status)
	if err == pgx.ErrNoRows {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	return id, status, nil
}

// GET /v1/staff/cups/{id}/remake-evaluation：单杯重做因子评估。
func (a *App) remakeEvaluation(w http.ResponseWriter, r *http.Request) {
	cupID := toInt64(chi.URLParam(r, "id"))
	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)
	cp, err := loadCupPolicy(ctx, tx, cupID, false)
	if err != nil {
		writeErr(w, err)
		return
	}
	active, maxCups, busy, err := storeLoad(ctx, tx, cp.storeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	var orderStatus string
	_ = tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, cp.orderID).Scan(&orderStatus)
	jobID, _, _ := activeRemakeJob(ctx, tx, cupID)
	p := evaluatePolicy(cp.category, cp.temp, cp.isDrink, cp.includesDrink, cp.sealedAt,
		active, maxCups, busy, time.Now())
	comp, msg := 0.0, ""
	if p.Action == "remake" {
		comp, msg = staleCompPlan("stale_drink", cp.unitPrice)
	}
	writeJSON(w, 200, map[string]any{
		"cup_id": cupID, "order_id": cp.orderID, "store_id": cp.storeID,
		"cup_status": cp.status, "order_status": orderStatus,
		"active_remake_job_id": jobID,
		"policy":               p,
		"comp_preview": map[string]any{
			"amount": comp, "reason": "stale_drink", "customer_message": msg,
		},
	})
}

// GET /v1/staff/stores/{id}/stale-cups：门店待取区全部已封杯的放置监控（含建议动作）。
func (a *App) listStaleCups(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	ctx := r.Context()
	rows, err := a.Pool.Query(ctx, `
		SELECT `+cupPolicyCols+`,EXTRACT(EPOCH FROM (now()-c.sealed_at))::int,
		       EXISTS(SELECT 1 FROM remake_jobs j WHERE (j.old_cup_id=c.id OR j.new_cup_id=c.id) AND j.status='remaking')
		FROM order_cups c JOIN orders o ON o.id=c.order_id JOIN menu_items m ON m.id=c.menu_item_id
		WHERE o.store_id=$1 AND c.status='sealed' AND o.status IN ('received','active')
		ORDER BY c.sealed_at LIMIT 200`, storeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	active, maxCups, busy, err := storeLoad(r.Context(), a.Pool, storeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	type item struct {
		CupID     int64        `json:"cup_id"`
		OrderID   int64        `json:"order_id"`
		SKU       string       `json:"sku"`
		Name      string       `json:"name"`
		Temp      string       `json:"temp"`
		Seq       int          `json:"seq"`
		IdleSec   int          `json:"idle_seconds"`
		ActiveJob bool         `json:"remaking"`
		Policy    remakePolicy `json:"policy"`
	}
	var out []item
	for rows.Next() {
		var cp cupPolicyRow
		var idle int
		var inJob bool
		if err := rows.Scan(&cp.cupID, &cp.orderID, &cp.storeID, &cp.menuItemID, &cp.sku, &cp.category,
			&cp.isDrink, &cp.includesDrink, &cp.name, &cp.temp, &cp.size, &cp.sugar, &cp.seq,
			&cp.status, &cp.remakeNo, &cp.sealedAt, &cp.unitPrice, &idle, &inJob); err != nil {
			writeErr(w, err)
			return
		}
		p := evaluatePolicy(cp.category, cp.temp, cp.isDrink, cp.includesDrink, cp.sealedAt,
			active, maxCups, busy, time.Now())
		p.IdleSeconds = idle // 用数据库 now() 口径
		out = append(out, item{
			CupID: cp.cupID, OrderID: cp.orderID, SKU: cp.sku, Name: cp.name, Temp: cp.temp,
			Seq: cp.seq, IdleSec: idle, ActiveJob: inJob, Policy: p,
		})
	}
	writeJSON(w, 200, map[string]any{
		"store_id": storeID, "store_busy": busy,
		"active_making_cups": active, "max_active_cups": maxCups, "cups": out,
	})
}

// ---------- 重做核心（事务内） ----------

type remakeInput struct {
	cupID       int64
	reason      string
	triggerType string // manual / auto_policy
	detail      map[string]any
	staffID     int64
}

type remakeOutcome struct {
	Job         RemakeJob `json:"job"`
	OldCupID    int64     `json:"old_cup_id"`
	NewCupID    int64     `json:"new_cup_id"`
	ExceptionID int64     `json:"exception_id"`
	LossCost    float64   `json:"loss_cost"`
	RemakeCost  float64   `json:"remake_cost"`
	CompAmount  float64   `json:"comp_amount"`
}

// createRemake 在调用方事务内执行重做全流程（调用方需先持有订单级 advisory lock）。
func (a *App) createRemake(ctx context.Context, tx pgx.Tx, in remakeInput) (*remakeOutcome, error) {
	old, err := loadCupPolicy(ctx, tx, in.cupID, true)
	if err != nil {
		return nil, err
	}
	if old.status == "voided" {
		return nil, errf(409, "cup_voided", "原杯已报损失效，不能再重做（请基于最新一杯操作）")
	}
	var oStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, old.orderID).Scan(&oStatus); err != nil {
		return nil, err
	}
	if oStatus == "canceled" || oStatus == "handed" {
		return nil, errf(409, "conflict", "订单已 %s，不能重做", oStatus)
	}
	if jid, _, _ := activeRemakeJob(ctx, tx, old.cupID); jid != 0 {
		return nil, errf(409, "remake_in_progress", "该杯已在重做流程中（remake_job_id=%d），不能重复触发", jid)
	}

	now := time.Now()
	active, maxCups, busy, err := storeLoad(ctx, tx, old.storeID)
	if err != nil {
		return nil, err
	}
	policy := evaluatePolicy(old.category, old.temp, old.isDrink, old.includesDrink, old.sealedAt,
		active, maxCups, busy, now)
	if in.reason == "stale_drink" && policy.Action != "remake" {
		// 手动触发也允许（店员判断），但自动策略只会在达到品质窗口时触发
		if in.triggerType == "auto_policy" {
			return nil, errf(409, "within_quality_window",
				"放置 %d 秒未达品质窗口 %d 秒，暂不需重做", policy.IdleSeconds, policy.QualityWindowSec)
		}
	}

	recipes, err := loadRecipes(ctx, tx, []int64{old.menuItemID})
	if err != nil {
		return nil, err
	}
	oldCost := cupCost(recipes[old.menuItemID], old.temp, old.size, old.sugar)

	// 1) 扣减新杯原料（重做成本，单独归因 reason=remake）
	var needs []stockNeed
	for _, rr := range recipes[old.menuItemID] {
		if q := cupRequirement(rr, old.temp, old.size, old.sugar); q > 0 {
			needs = append(needs, stockNeed{IngID: rr.IngredientID, Delta: -q, Reason: "remake",
				OrderID: &old.orderID, StaffID: &in.staffID})
		}
	}
	if err := applyStock(ctx, tx, old.storeID, needs); err != nil {
		return nil, err
	}

	// 2) 原杯报损：voided + 旧封签立即失效（随状态失效，扫描旧码拒绝交接）
	if _, err := tx.Exec(ctx,
		`UPDATE order_cups SET status='voided',voided_at=now(),void_reason=$2 WHERE id=$1`,
		old.cupID, in.reason); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM order_bags WHERE cup_id=$1`, old.cupID); err != nil {
		return nil, err
	}

	// 3) 新杯入队：新的制作序号（接队尾）+ remake_no+1；封口时生成新封签码
	var newSeq int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(max(seq),0)+1 FROM order_cups WHERE order_id=$1`, old.orderID).Scan(&newSeq); err != nil {
		return nil, err
	}
	var newID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO order_cups(order_id,menu_item_id,name_snapshot,unit_price,temp,sugar,cup_size,cup_note,
			seq,status,remake_of_cup_id,remake_no)
		SELECT order_id,menu_item_id,name_snapshot,unit_price,temp,sugar,cup_size,cup_note,
		       $1,'queued',id,remake_no+1 FROM order_cups WHERE id=$2 RETURNING id`,
		newSeq, old.cupID).Scan(&newID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE orders SET status='received',all_sealed_at=NULL WHERE id=$1 AND status='active'`, old.orderID); err != nil {
		return nil, err
	}

	// 4) 异常单（复用 open 异常），记录初始责任=门店
	excID, existed, err := openException(ctx, tx, old.orderID, in.reason)
	if err != nil {
		return nil, err
	}
	detail := map[string]any{
		"old_cup_id": old.cupID, "new_cup_id": newID,
		"factors": map[string]any{
			"category": policy.Category, "temp": policy.Temp, "idle_seconds": policy.IdleSeconds,
			"store_busy": policy.StoreBusy, "active_making_cups": policy.ActiveMaking,
			"max_active_cups":        policy.MaxActiveCups,
			"quality_window_seconds": policy.QualityWindowSec,
			"safety_window_seconds":  policy.SafetyWindowSec,
		},
		"trigger_type": in.triggerType,
	}
	for k, v := range in.detail {
		detail[k] = v
	}
	if existed {
		if _, err := tx.Exec(ctx,
			`UPDATE exceptions SET occurrences=occurrences+1,cost_amount=cost_amount+$2,detail=detail||$3
			 WHERE id=$1`, excID, oldCost, detail); err != nil {
			return nil, err
		}
	} else {
		if err := tx.QueryRow(ctx, `
			INSERT INTO exceptions(order_id,store_id,type,status,responsibility,original_responsibility,
				detail,cost_amount,created_by)
			VALUES($1,$2,$3,'open','store','store',$4,$5,$6) RETURNING id`,
			old.orderID, old.storeID, in.reason, detail, oldCost, in.staffID).Scan(&excID); err != nil {
			return nil, err
		}
	}

	// 5) 成本分别归因：
	//    原杯报损 loss_reports + finance(loss, bearer=team)
	//    新杯原料 finance(remake_cost, bearer=team)
	if _, err := tx.Exec(ctx, `
		INSERT INTO loss_reports(store_id,order_id,cup_id,exception_id,reason,qty_cups,cost_amount,note,staff_id)
		VALUES($1,$2,$3,$4,$5,1,$6,$7,$8)`,
		old.storeID, old.orderID, old.cupID, excID, in.reason, oldCost,
		in.reason+" 重做报损（原杯不可再交接）", in.staffID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,note,created_by)
		VALUES($1,$2,$3,'loss',$4,'team',$5,$6)`,
		old.storeID, old.orderID, excID, -oldCost, in.reason+" 原杯报损成本", in.staffID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,note,created_by)
		VALUES($1,$2,$3,'remake_cost',$4,'team',$5,$6)`,
		old.storeID, old.orderID, excID, -oldCost, in.reason+" 新杯原料重做成本", in.staffID); err != nil {
		return nil, err
	}

	// 6) 顾客补偿方案（单独归因；take_old 撤回，复核可改判平台承担）
	compAmount, compMsg := staleCompPlan(in.reason, old.unitPrice)
	var compID *int64
	if compAmount > 0 {
		cid, err := proposeCompensation(ctx, tx, compInput{
			storeID: old.storeID, orderID: old.orderID, exceptionID: &excID,
			reason: "stale_remake", planAmount: compAmount, bearer: "team",
			message: compMsg, proposedBy: in.staffID,
		})
		if err != nil {
			return nil, err
		}
		compID = &cid
	}

	// 7) 重做任务状态机
	var jobID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO remake_jobs(store_id,order_id,old_cup_id,new_cup_id,exception_id,reason,trigger_type,
			category,is_drink,temp,idle_seconds,store_busy,quality_window_seconds,safety_window_seconds,
			loss_cost,remake_cost,comp_amount,created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18) RETURNING id`,
		old.storeID, old.orderID, old.cupID, newID, excID, in.reason, in.triggerType,
		policy.Category, policy.IsDrink, policy.Temp, policy.IdleSeconds, policy.StoreBusy,
		policy.QualityWindowSec, policy.SafetyWindowSec, oldCost, oldCost, compAmount, in.staffID).Scan(&jobID); err != nil {
		return nil, err
	}
	if compID != nil {
		if _, err := tx.Exec(ctx, `UPDATE compensations SET remake_job_id=$2 WHERE id=$1`, *compID, jobID); err != nil {
			return nil, err
		}
	}

	if err := a.logEvent(ctx, tx, old.orderID, "cup_remade", actorByID(ctx, tx, in.staffID), map[string]any{
		"old_cup_id": old.cupID, "new_cup_id": newID, "remake_job_id": jobID,
		"reason": in.reason, "trigger_type": in.triggerType,
		"loss_cost": oldCost, "remake_cost": oldCost, "comp_amount": compAmount,
		"old_seal_code_invalid": true,
		"factors":               detail["factors"],
	}); err != nil {
		return nil, err
	}

	job, err := loadRemakeJob(ctx, tx, jobID)
	if err != nil {
		return nil, err
	}
	return &remakeOutcome{
		Job: *job, OldCupID: old.cupID, NewCupID: newID, ExceptionID: excID,
		LossCost: oldCost, RemakeCost: oldCost, CompAmount: compAmount,
	}, nil
}

// ---------- HTTP：手动重做 / 自动重做 ----------

type remakeReq struct {
	Reason string         `json:"reason"` // stale_drink / wrong_cup / note_missed / equipment_fault
	Detail map[string]any `json:"detail"`
}

func validRemakeReason(s string) bool {
	switch s {
	case "stale_drink", "wrong_cup", "note_missed", "equipment_fault":
		return true
	}
	return false
}

// POST /v1/staff/cups/{id}/remake
func (a *App) remakeCup(w http.ResponseWriter, r *http.Request) {
	cupID := toInt64(chi.URLParam(r, "id"))
	var req remakeReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if !validRemakeReason(req.Reason) {
		writeErr(w, errf(400, "bad_request",
			"reason 必须是 stale_drink/wrong_cup/note_missed/equipment_fault"))
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

	var orderID int64
	if err := tx.QueryRow(ctx, `SELECT order_id FROM order_cups WHERE id=$1`, cupID).Scan(&orderID); err != nil {
		writeErr(w, err)
		return
	}
	if err := lockOrderTx(ctx, tx, orderID); err != nil {
		writeErr(w, err)
		return
	}
	out, err := a.createRemake(ctx, tx, remakeInput{
		cupID: cupID, reason: req.Reason, triggerType: "manual",
		detail: req.Detail, staffID: ac.ID,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	cup, _ := a.loadCup(ctx, out.NewCupID)
	writeJSON(w, 201, map[string]any{
		"new_cup": cup, "old_cup_id": out.OldCupID, "exception_id": out.ExceptionID,
		"loss_cost": out.LossCost, "remake_cost": out.RemakeCost,
		"comp_amount": out.CompAmount, "remake_job": out.Job,
	})
}

type autoRemakeReq struct {
	CupIDs []int64 `json:"cup_ids"` // 可选：只处理指定杯；为空则扫描全门店到期杯
}

// POST /v1/staff/stores/{id}/remake-stale：按策略自动重做所有过品质窗口的已封杯。
func (a *App) autoRemakeStale(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	var req autoRemakeReq
	_ = decode(w, r, &req)
	ac := actor(r)
	ctx := r.Context()

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)
	// 门店串行锁（94001）→ 逐单加订单锁（93001）；与交接/取消/裁决的加锁次序一致，避免死锁
	if err := lockStoreTx(ctx, tx, storeID); err != nil {
		writeErr(w, err)
		return
	}

	// 选出候选杯：已封口、订单未终结、放置达品质窗口、无进行中任务
	q := `
		SELECT c.id,c.order_id,COALESCE(m.category,''),m.is_drink,m.includes_drink,c.temp,c.sealed_at
		FROM order_cups c JOIN orders o ON o.id=c.order_id JOIN menu_items m ON m.id=c.menu_item_id
		WHERE o.store_id=$1 AND c.status='sealed' AND o.status IN ('received','active')
		  AND NOT EXISTS(SELECT 1 FROM remake_jobs j WHERE (j.old_cup_id=c.id OR j.new_cup_id=c.id) AND j.status='remaking')`
	args := []any{storeID}
	if len(req.CupIDs) > 0 {
		q += " AND c.id = ANY($2)"
		args = append(args, req.CupIDs)
	}
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	type cand struct {
		cupID, orderID int64
		category, temp string
		isDrink, incl  bool
		sealedAt       *time.Time
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.cupID, &c.orderID, &c.category, &c.isDrink, &c.incl, &c.temp, &c.sealedAt); err != nil {
			rows.Close()
			writeErr(w, err)
			return
		}
		cands = append(cands, c)
	}
	rows.Close()

	activeN, maxCups, busy, err := storeLoad(ctx, tx, storeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	now := time.Now()
	triggered := []remakeOutcome{}
	skipped := []map[string]any{}
	for _, c := range cands {
		p := evaluatePolicy(c.category, c.temp, c.isDrink, c.incl, c.sealedAt, activeN, maxCups, busy, now)
		if p.Action != "remake" {
			skipped = append(skipped, map[string]any{
				"cup_id": c.cupID, "action": p.Action, "idle_seconds": p.IdleSeconds,
				"quality_window_seconds": p.QualityWindowSec,
			})
			continue
		}
		// 每个订单单独加顺序锁，避免与交接/裁决互踩；门店锁 94001 已保证扫描串行
		if err := lockOrderTx(ctx, tx, c.orderID); err != nil {
			writeErr(w, err)
			return
		}
		out, err := a.createRemake(ctx, tx, remakeInput{
			cupID: c.cupID, reason: "stale_drink", triggerType: "auto_policy",
			detail: map[string]any{"auto": true}, staffID: ac.ID,
		})
		if err != nil {
			var ae *apiError
			if asErr(err, &ae) && (ae.Code == "within_quality_window" || ae.Code == "remake_in_progress" ||
				ae.Code == "cup_voided" || ae.Code == "conflict") {
				skipped = append(skipped, map[string]any{"cup_id": c.cupID, "skip_reason": ae.Code})
				continue
			}
			writeErr(w, err)
			return
		}
		triggered = append(triggered, *out)
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"store_id": storeID, "triggered_count": len(triggered), "triggered": triggered,
		"skipped": skipped,
	})
}

// ---------- 查询 ----------

const remakeJobCols = `id,store_id,order_id,old_cup_id,new_cup_id,exception_id,reason,trigger_type,status,
	category,is_drink,temp,idle_seconds,store_busy,quality_window_seconds,safety_window_seconds,
	loss_cost::float8,remake_cost::float8,comp_amount,new_label_printed,new_label_printed_at,
	COALESCE(decision,''),decided_by,decided_at,decision_detail,rider_wait_seconds,completed_at,created_by,created_at`

func scanRemakeJob(row pgx.Row) (*RemakeJob, error) {
	var j RemakeJob
	var raw []byte
	var decision string
	err := row.Scan(&j.ID, &j.StoreID, &j.OrderID, &j.OldCupID, &j.NewCupID, &j.ExceptionID,
		&j.Reason, &j.TriggerType, &j.Status, &j.Category, &j.IsDrink, &j.Temp,
		&j.IdleSeconds, &j.StoreBusy, &j.QualityWindowSec, &j.SafetyWindowSec,
		&j.LossCost, &j.RemakeCost, &j.CompAmount, &j.NewLabelPrinted, &j.NewLabelPrintedAt,
		&decision, &j.DecidedBy, &j.DecidedAt, &raw, &j.RiderWaitSeconds, &j.CompletedAt,
		&j.CreatedBy, &j.CreatedAt)
	if err != nil {
		return nil, err
	}
	j.Decision = decision
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &j.DecisionDetail)
	}
	return &j, nil
}

func loadRemakeJob(ctx context.Context, tx pgx.Tx, id int64) (*RemakeJob, error) {
	return scanRemakeJob(tx.QueryRow(ctx, `SELECT `+remakeJobCols+` FROM remake_jobs WHERE id=$1`, id))
}

func (a *App) getRemakeJob(w http.ResponseWriter, r *http.Request) {
	id := toInt64(chi.URLParam(r, "id"))
	job, err := scanRemakeJob(a.Pool.QueryRow(r.Context(),
		`SELECT `+remakeJobCols+` FROM remake_jobs WHERE id=$1`, id))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, job)
}

func (a *App) listOrderRemakeJobs(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	rows, err := a.Pool.Query(r.Context(),
		`SELECT `+remakeJobCols+` FROM remake_jobs WHERE order_id=$1 ORDER BY id`, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	var out []RemakeJob
	for rows.Next() {
		j, err := scanRemakeJob(rows)
		if err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, *j)
	}
	writeJSON(w, 200, out)
}
