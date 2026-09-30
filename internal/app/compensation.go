package app

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// 顾客补偿：重做触发时生成补偿方案（proposed）；顾客端可见延迟原因与方案；
// 平台客服确认补偿已兑现（fulfilled）；原杯在安全窗口交付时撤回（withdrawn）；
// 责任复核改判平台承担时只改承担方，原始方案保留；兑现时才写实际赔付流水。

type compInput struct {
	storeID     int64
	orderID     int64
	exceptionID *int64
	remakeJobID *int64
	reason      string
	planAmount  float64
	bearer      string
	message     string
	proposedBy  int64
}

func proposeCompensation(ctx context.Context, tx pgx.Tx, in compInput) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO compensations(store_id,order_id,exception_id,remake_job_id,reason,status,
			plan_amount,bearer,customer_message,proposed_by)
		VALUES($1,$2,$3,$4,$5,'proposed',$6,$7,$8,$9) RETURNING id`,
		in.storeID, in.orderID, in.exceptionID, in.remakeJobID, in.reason,
		in.planAmount, in.bearer, in.message, in.proposedBy).Scan(&id)
	return id, err
}

const compCols = `id,store_id,order_id,exception_id,remake_job_id,reason,status,plan_amount::float8,bearer,
	COALESCE(customer_message,''),proposed_by,proposed_at,fulfilled_by,fulfilled_at,COALESCE(fulfill_note,'')`

func scanComp(row pgx.Row) (*Compensation, error) {
	var c Compensation
	err := row.Scan(&c.ID, &c.StoreID, &c.OrderID, &c.ExceptionID, &c.RemakeJobID,
		&c.Reason, &c.Status, &c.PlanAmount, &c.Bearer, &c.CustomerMessage,
		&c.ProposedBy, &c.ProposedAt, &c.FulfilledBy, &c.FulfilledAt, &c.FulfillNote)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// queryComps 按 WHERE 片段查询补偿。
func queryComps(ctx context.Context, q rowQuerier, where string, args ...any) []Compensation {
	rows, err := q.Query(ctx, `SELECT `+compCols+` FROM compensations `+where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []Compensation{}
	for rows.Next() {
		c, err := scanComp(rows)
		if err != nil {
			return nil
		}
		out = append(out, *c)
	}
	return out
}

// GET /v1/orders/{id}/compensations：补偿方案与兑现状态（店员/客服/财务/顾客可见）。
func (a *App) listCompensations(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	writeJSON(w, 200, queryComps(r.Context(), a.Pool,
		`WHERE order_id=$1 ORDER BY id`, orderID))
}

// ---------- 顾客端：延迟原因 + 补偿方案 ----------

var delayReasonCN = map[string]string{
	"stale_drink":             "饮品放置过久，门店正在免费重做",
	"wrong_cup":               "出品与订单不符，门店正在重做",
	"note_missed":             "制作备注漏看，门店正在按备注重做",
	"equipment_fault":         "门店设备故障，出品延迟，门店正在重做",
	"platform_dispatch_delay": "平台派单/骑手调度延迟，导致饮品在店久置",
	"rider_early":             "骑手早于预计时间到店，饮品仍在制作",
}

// GET /v1/customers/orders/{id}/status：顾客端订单状态、延迟原因、新杯进度、补偿方案与兑现情况。
func (a *App) customerOrderStatus(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	ctx := r.Context()

	var storeID int64
	var status, platform string
	err := a.Pool.QueryRow(ctx,
		`SELECT store_id,status,COALESCE(platform,'') FROM orders WHERE id=$1`, orderID).
		Scan(&storeID, &status, &platform)
	if err != nil {
		writeErr(w, err)
		return
	}

	// 当前进行中的重做任务及新杯进度
	type jobView struct {
		RemakeJobID  int64  `json:"remake_job_id"`
		Reason       string `json:"reason"`
		Status       string `json:"status"`
		NewCupID     int64  `json:"new_cup_id"`
		NewCupStatus string `json:"new_cup_status"`
		NewSeq       int    `json:"new_seq"`
		Decision     string `json:"decision,omitempty"`
	}
	rows, err := a.Pool.Query(ctx, `
		SELECT j.id,j.reason,j.status,COALESCE(j.new_cup_id,0),
		       COALESCE(c.status,''),COALESCE(c.seq,0),COALESCE(j.decision,'')
		FROM remake_jobs j LEFT JOIN order_cups c ON c.id=j.new_cup_id
		WHERE j.order_id=$1 ORDER BY j.id DESC`, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	jobs := []jobView{}
	delayReasons := []string{}
	for rows.Next() {
		var jv jobView
		if err := rows.Scan(&jv.RemakeJobID, &jv.Reason, &jv.Status, &jv.NewCupID,
			&jv.NewCupStatus, &jv.NewSeq, &jv.Decision); err != nil {
			rows.Close()
			writeErr(w, err)
			return
		}
		jobs = append(jobs, jv)
		if jv.Status == "remaking" {
			if txt := delayReasonCN[jv.Reason]; txt != "" {
				delayReasons = appendUnique(delayReasons, txt)
			}
		}
	}
	rows.Close()

	// 已生效的责任复核改判（平台派单延迟等）
	rrRows, err := a.Pool.Query(ctx, `
		SELECT ground FROM responsibility_reviews WHERE order_id=$1 AND status='approved' AND approved=TRUE`, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	for rrRows.Next() {
		var g string
		_ = rrRows.Scan(&g)
		if txt := delayReasonCN[g]; txt != "" {
			delayReasons = appendUnique(delayReasons, txt)
		}
	}
	rrRows.Close()

	comps := queryComps(ctx, a.Pool, `WHERE order_id=$1 ORDER BY id`, orderID)
	type compView struct {
		ID         int64   `json:"id"`
		Reason     string  `json:"reason"`
		Status     string  `json:"status"`
		PlanAmount float64 `json:"plan_amount"`
		Bearer     string  `json:"bearer"`
		Message    string  `json:"message"`
		Fulfilled  bool    `json:"fulfilled"`
	}
	cv := []compView{}
	for _, c := range comps {
		if c.Status == "withdrawn" || c.Status == "rejected" {
			continue
		}
		cv = append(cv, compView{
			ID: c.ID, Reason: c.Reason, Status: c.Status, PlanAmount: c.PlanAmount,
			Bearer: c.Bearer, Message: c.CustomerMessage, Fulfilled: c.Status == "fulfilled",
		})
	}
	if len(delayReasons) == 0 && status == "received" {
		delayReasons = append(delayReasons, "订单正在制作中")
	}

	writeJSON(w, 200, map[string]any{
		"order_id": orderID, "store_id": storeID, "platform": platform,
		"order_status": status, "delay_reasons": delayReasons,
		"remakes": jobs, "compensations": cv,
	})
}

func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

// ---------- 平台客服确认补偿兑现 ----------

type fulfillReq struct {
	Note   string  `json:"note"`
	Amount float64 `json:"amount"` // 可选，实际兑现金额（默认方案金额）
}

// POST /v1/platform/compensations/{id}/fulfill
func (a *App) fulfillCompensation(w http.ResponseWriter, r *http.Request) {
	id := toInt64(chi.URLParam(r, "id"))
	var req fulfillReq
	_ = decode(w, r, &req)
	ac := actor(r)
	ctx := r.Context()

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var c Compensation
	var orderPlatform string
	err = tx.QueryRow(ctx, `
		SELECT c.id,c.store_id,c.order_id,c.exception_id,c.remake_job_id,c.reason,c.status,c.plan_amount::float8,
		       c.bearer,COALESCE(c.customer_message,''),c.proposed_by,c.proposed_at,
		       c.fulfilled_by,c.fulfilled_at,COALESCE(c.fulfill_note,''),COALESCE(o.platform,'')
		FROM compensations c JOIN orders o ON o.id=c.order_id WHERE c.id=$1 FOR UPDATE`, id).Scan(
		&c.ID, &c.StoreID, &c.OrderID, &c.ExceptionID, &c.RemakeJobID, &c.Reason, &c.Status,
		&c.PlanAmount, &c.Bearer, &c.CustomerMessage, &c.ProposedBy, &c.ProposedAt,
		&c.FulfilledBy, &c.FulfilledAt, &c.FulfillNote, &orderPlatform)
	if err != nil {
		writeErr(w, err)
		return
	}
	if ac.Role != "admin" && ac.Platform != "" && orderPlatform != "" && ac.Platform != orderPlatform {
		writeErr(w, errf(403, "cross_platform", "只能兑现本平台订单的补偿"))
		return
	}
	switch c.Status {
	case "proposed", "confirmed":
	default:
		writeErr(w, errf(409, "comp_not_payable", "补偿状态为 %s，不能确认兑现", c.Status))
		return
	}
	amount := req.Amount
	if amount <= 0 {
		amount = c.PlanAmount
	}
	if _, err := tx.Exec(ctx, `
		UPDATE compensations SET status='fulfilled',fulfilled_at=now(),fulfilled_by=$2,
		  fulfill_note=$3,plan_amount=$4 WHERE id=$1`, id, ac.ID, req.Note, amount); err != nil {
		writeErr(w, err)
		return
	}
	note := "平台客服确认补偿已兑现给顾客"
	if req.Note != "" {
		note = req.Note
	}
	// 实际赔付支出在兑现时入财务台账；承担方跟随最新责任（复核改判后可能是 platform）
	if _, err := tx.Exec(ctx, `
		INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,note,created_by)
		VALUES($1,$2,$3,'customer_comp',$4,$5,$6,$7)`,
		c.StoreID, c.OrderID, c.ExceptionID, -amount, c.Bearer, note, ac.ID); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, c.OrderID, "compensation_fulfilled", actorOf(ac), map[string]any{
		"compensation_id": id, "amount": amount, "bearer": c.Bearer, "note": req.Note,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "status": "fulfilled", "amount": amount, "bearer": c.Bearer,
	})
}

// ---------- 财务/店长确认补偿方案金额（proposed → confirmed） ----------

type confirmCompReq struct {
	Amount  float64 `json:"amount"`
	Message string  `json:"message"`
}

// POST /v1/finance/compensations/{id}/confirm
func (a *App) confirmCompensation(w http.ResponseWriter, r *http.Request) {
	id := toInt64(chi.URLParam(r, "id"))
	var req confirmCompReq
	_ = decode(w, r, &req)
	ac := actor(r)
	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var status string
	var orderID, storeID int64
	if err := tx.QueryRow(ctx, `SELECT status,order_id,store_id FROM compensations WHERE id=$1 FOR UPDATE`, id).
		Scan(&status, &orderID, &storeID); err != nil {
		writeErr(w, err)
		return
	}
	if ac.Role == "store_manager" && ac.StoreID != nil && *ac.StoreID != storeID {
		writeErr(w, errForbidden)
		return
	}
	if status != "proposed" {
		writeErr(w, errf(409, "comp_not_proposed", "补偿状态为 %s，不能确认", status))
		return
	}
	if _, err := tx.Exec(ctx,
		`UPDATE compensations SET status='confirmed',confirmed_at=now(),confirmed_by=$2,
		  plan_amount=COALESCE(NULLIF($3::float8,0),plan_amount),
		  customer_message=COALESCE(NULLIF($4::text,''),customer_message) WHERE id=$1`,
		id, ac.ID, req.Amount, req.Message); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, orderID, "compensation_confirmed", actorOf(ac), map[string]any{
		"compensation_id": id, "amount": req.Amount,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": "confirmed"})
}
