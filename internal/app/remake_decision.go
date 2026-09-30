package app

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// 骑手恰在重做过程中到店的并发裁决：
// 根据 ①原杯是否仍在安全窗口 ②是否已打印新杯贴 ③顾客是否已取消，
// 决定 wait_new（继续等待新杯）/ take_old（允许取原杯）/ aborted（终止重做），
// 避免原杯与新杯同时被交接。订单级 advisory lock 与交接/重做/取消互斥。

type jobCupState struct {
	orderID       int64
	storeID       int64
	oldID         int64
	newID         int64
	exceptionID   int64
	reason        string
	oldStatus     string
	newStatus     string
	temp          string
	category      string
	isDrink       bool
	includesDrink bool
	labelPrinted  bool
	safetySec     int
	sealedAt      *time.Time
}

// loadJobState 在已持有订单锁时读取裁决所需的全部状态。
func loadJobState(ctx context.Context, tx pgx.Tx, jobID int64, forUpdate bool) (*RemakeJob, *jobCupState, error) {
	job, err := loadRemakeJob(ctx, tx, jobID)
	if err != nil {
		return nil, nil, err
	}
	// 调用方已持有订单级 advisory lock（93001），与重做/封签/交接/取消串行；这里仅锁任务行。
	sql := `
		SELECT j.order_id,j.store_id,j.old_cup_id,COALESCE(j.new_cup_id,0),COALESCE(j.exception_id,0),j.reason,
		       co.status,cn.status,cn.temp,COALESCE(m.category,''),m.is_drink,m.includes_drink,
		       j.new_label_printed,j.safety_window_seconds,co.sealed_at
		FROM remake_jobs j
		JOIN order_cups co ON co.id=j.old_cup_id
		LEFT JOIN order_cups cn ON cn.id=j.new_cup_id
		LEFT JOIN menu_items m ON m.id=cn.menu_item_id
		WHERE j.id=$1`
	if forUpdate {
		sql += " FOR UPDATE OF j"
	}
	var st jobCupState
	if err := tx.QueryRow(ctx, sql, jobID).Scan(
		&st.orderID, &st.storeID, &st.oldID, &st.newID, &st.exceptionID, &st.reason,
		&st.oldStatus, &st.newStatus, &st.temp, &st.category, &st.isDrink, &st.includesDrink,
		&st.labelPrinted, &st.safetySec, &st.sealedAt); err != nil {
		return job, nil, err
	}
	return job, &st, nil
}

// autoDecide 计算系统建议裁决（不做任何写操作）。
func autoDecide(st *jobCupState, orderStatus string, now time.Time) (string, string) {
	switch {
	case orderStatus == "canceled":
		return "aborted", "顾客/平台已取消订单，终止重做，按取消流程结算"
	case st.newStatus == "sealed":
		return "wait_new", "新杯已封口并生成新封签码，旧码已失效，请扫描新封签码取走新杯"
	case st.labelPrinted:
		return "wait_new", "新杯贴已打印，新杯正在制作中，请等待新杯（旧杯不得交接）"
	// 旧杯在重做触发时已报损（voided），但其实物仍在；安全窗口内放行 = 冲回报损并恢复旧杯。
	case st.sealedAt != nil && int(now.Sub(*st.sealedAt).Seconds()) < st.safetySec:
		return "take_old", "原杯仍在安全窗口内且新杯贴未打印，可放行原杯；新杯即刻停做，报损将冲回"
	default:
		return "wait_new", "原杯已超出安全窗口，必须等待新杯制作完成"
	}
}

type remakeDecisionReq struct {
	Action string `json:"action"` // wait_new / take_old；为空=系统自动裁决
	Note   string `json:"note"`
}

// POST /v1/rider/remakes/{id}/decision
func (a *App) remakeDecision(w http.ResponseWriter, r *http.Request) {
	jobID := toInt64(chi.URLParam(r, "id"))
	var req remakeDecisionReq
	_ = decode(w, r, &req)
	if req.Action != "" && req.Action != "wait_new" && req.Action != "take_old" {
		writeErr(w, errf(400, "bad_request", "action 只能是 wait_new/take_old（或留空自动裁决）"))
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
	if err := tx.QueryRow(ctx, `SELECT order_id FROM remake_jobs WHERE id=$1`, jobID).Scan(&orderID); err != nil {
		writeErr(w, err)
		return
	}
	if err := lockOrderTx(ctx, tx, orderID); err != nil {
		writeErr(w, err)
		return
	}
	job, st, err := loadJobState(ctx, tx, jobID, true)
	if err != nil {
		writeErr(w, err)
		return
	}
	if job.Status != "remaking" {
		writeErr(w, errf(409, "job_not_open", "重做任务已终结（%s），裁决不可更改", job.Status))
		return
	}
	var orderStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, st.orderID).Scan(&orderStatus); err != nil {
		writeErr(w, err)
		return
	}

	suggestion, instruction := autoDecide(st, orderStatus, time.Now())
	action := suggestion
	if req.Action != "" {
		action = req.Action
	}
	// 安全约束不可被请求覆盖
	if action == "take_old" {
		if orderStatus == "canceled" {
			writeErr(w, errf(409, "order_canceled", "订单已取消，不能取原杯"))
			return
		}
		if st.newStatus == "sealed" || st.labelPrinted {
			writeErr(w, errf(409, "new_cup_committed",
				"新杯贴已打印/新杯已封口，不能回退取原杯，请等待新杯"))
			return
		}
		idle := 0
		if st.sealedAt != nil {
			idle = int(time.Now().Sub(*st.sealedAt).Seconds())
		}
		if idle >= st.safetySec {
			writeErr(w, errf(409, "outside_safety_window",
				"原杯已超出安全窗口（放置 %d 秒/窗口 %d 秒），不得交接", idle, st.safetySec))
			return
		}
	}

	switch action {
	case "aborted":
		if err := abortRemakeJob(ctx, tx, a, job.ID, st, "order_canceled", ac.ID, instruction); err != nil {
			writeErr(w, err)
			return
		}
	case "take_old":
		if err := reverseRemakeTakeOld(ctx, tx, a, job, st, ac.ID, req.Note); err != nil {
			writeErr(w, err)
			return
		}
	case "wait_new":
		detail := map[string]any{
			"suggestion": suggestion, "requested_action": req.Action,
			"new_cup_status": st.newStatus, "new_label_printed": st.labelPrinted,
			"note": req.Note,
		}
		if _, err := tx.Exec(ctx, `
			UPDATE remake_jobs SET decision='wait_new',decided_by=$2,decided_at=now(),decision_detail=$3
			WHERE id=$1`, job.ID, ac.ID, detail); err != nil {
			writeErr(w, err)
			return
		}
		// 记录骑手到店等待事件（不改变任务状态；新杯交接后任务 completed）
		if err := a.logEvent(ctx, tx, st.orderID, "remake_wait_new", actorOf(ac), map[string]any{
			"remake_job_id": job.ID, "old_cup_id": st.oldID, "new_cup_id": st.newID,
			"rider": ac.Name, "note": req.Note,
		}); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	fresh, _ := scanRemakeJob(a.Pool.QueryRow(ctx,
		`SELECT `+remakeJobCols+` FROM remake_jobs WHERE id=$1`, job.ID))
	writeJSON(w, 200, map[string]any{
		"decision": action, "system_suggestion": suggestion,
		"rider_instruction": instruction, "job": fresh,
	})
}

// reverseRemakeTakeOld 允许取原杯：恢复旧杯与原封签、回库新杯原料并作废旧新杯、
// 冲回报损/重做成本、撤回补偿、任务置 old_taken。原始事件与流水全部保留，只写冲回流水。
func reverseRemakeTakeOld(ctx context.Context, tx pgx.Tx, a *App, job *RemakeJob, st *jobCupState,
	staffID int64, note string) error {
	// 恢复旧杯为 sealed，原封签码重新有效
	if _, err := tx.Exec(ctx,
		`UPDATE order_cups SET status='sealed',voided_at=NULL,void_reason=NULL WHERE id=$1 AND status='voided'`,
		st.oldID); err != nil {
		return err
	}
	// 重建旧杯的分袋映射（复用原物理袋码，不重复扣袋子耗材——袋子实物未作废）
	kind := "food"
	if st.isDrink || st.includesDrink {
		if st.temp == "ice" {
			kind = "cold"
		} else {
			kind = "hot"
		}
	}
	code := bagCodeOf(st.orderID, kind)
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_bags(store_id,order_id,bag_code,kind,cup_id)
		SELECT $1,$2,$3,$4,$5
		WHERE NOT EXISTS(SELECT 1 FROM order_bags WHERE cup_id=$5)`,
		st.storeID, st.orderID, code, kind, st.oldID); err != nil {
		return err
	}

	// 新杯未封口：作废并按配方回库（重做成本冲回）
	if st.newID != 0 && st.newStatus != "sealed" {
		if _, err := tx.Exec(ctx,
			`UPDATE order_cups SET status='voided',voided_at=now(),void_reason='remake_reversed'
			 WHERE id=$1 AND status<>'voided'`, st.newID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM order_bags WHERE cup_id=$1`, st.newID); err != nil {
			return err
		}
		var menuID int64
		var temp, size, sugar string
		if err := tx.QueryRow(ctx,
			`SELECT menu_item_id,temp,cup_size,sugar FROM order_cups WHERE id=$1`, st.newID).
			Scan(&menuID, &temp, &size, &sugar); err != nil {
			return err
		}
		recipes, err := loadRecipes(ctx, tx, []int64{menuID})
		if err != nil {
			return err
		}
		cost := cupCost(recipes[menuID], temp, size, sugar)
		var needs []stockNeed
		for _, rr := range recipes[menuID] {
			if q := cupRequirement(rr, temp, size, sugar); q > 0 {
				needs = append(needs, stockNeed{IngID: rr.IngredientID, Delta: q,
					Reason: "remake_reversal", OrderID: &st.orderID})
			}
		}
		if err := applyStock(ctx, tx, st.storeID, needs); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,note,created_by)
			VALUES($1,$2,$3,'remake_reversal',$4,'team',$5,$6)`,
			st.storeID, st.orderID, st.exceptionID, cost,
			"骑手在安全窗口到店，取原杯；新杯未制作原料回库，冲回重做成本", staffID); err != nil {
			return err
		}
	}

	// 报损冲回（旧杯恢复可交接）
	if _, err := tx.Exec(ctx,
		`UPDATE loss_reports SET reversed=TRUE,reversed_at=now(),
		 reversal_note='安全窗口内骑手到店，原杯放行，报损冲回'
		 WHERE cup_id=$1 AND reversed=FALSE`, st.oldID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,note,created_by)
		VALUES($1,$2,$3,'loss_reversal',$4,'team',$5,$6)`,
		st.storeID, st.orderID, st.exceptionID, job.LossCost,
		"原杯在安全窗口交付，报损成本冲回（原始报损流水保留）", staffID); err != nil {
		return err
	}

	// 撤回尚未兑现的重做补偿方案（已兑现的保留）
	if _, err := tx.Exec(ctx, `
		UPDATE compensations SET status='withdrawn'
		WHERE remake_job_id=$1 AND status IN ('proposed','confirmed')`, job.ID); err != nil {
		return err
	}

	// 订单恢复待取状态
	maybeReady(ctx, tx, st.orderID)

	detail := map[string]any{
		"old_cup_id": st.oldID, "new_cup_id": st.newID, "note": note,
		"old_seal_code_reactivated": true,
	}
	if _, err := tx.Exec(ctx, `
		UPDATE remake_jobs SET status='old_taken',decision='take_old',decided_by=$2,
		  decided_at=now(),decision_detail=$3,completed_at=now() WHERE id=$1`,
		job.ID, staffID, detail); err != nil {
		return err
	}
	return a.logEvent(ctx, tx, st.orderID, "remake_take_old", actorByID(ctx, tx, staffID), map[string]any{
		"remake_job_id": job.ID, "old_cup_id": st.oldID, "new_cup_id": st.newID,
		"rider": actorName(ctx, tx, staffID), "note": note,
	})
}

// abortRemakeJob 终止重做（顾客/平台取消触发）：新杯原料回库、报损保留、补偿撤回。
func abortRemakeJob(ctx context.Context, tx pgx.Tx, a *App, jobID int64, st *jobCupState,
	reason string, by int64, note string) error {
	// 未封口的新杯回库（已封口的新杯随取消流程按报损处理，此处不动）
	if st.newID != 0 && st.newStatus != "sealed" && st.newStatus != "voided" {
		if _, err := tx.Exec(ctx,
			`UPDATE order_cups SET status='voided',voided_at=now(),void_reason='order_canceled'
			 WHERE id=$1`, st.newID); err != nil {
			return err
		}
		var menuID int64
		var temp, size, sugar2 string
		if err := tx.QueryRow(ctx,
			`SELECT menu_item_id,temp,cup_size,sugar FROM order_cups WHERE id=$1`, st.newID).
			Scan(&menuID, &temp, &size, &sugar2); err != nil {
			return err
		}
		recipes, err := loadRecipes(ctx, tx, []int64{menuID})
		if err != nil {
			return err
		}
		var needs []stockNeed
		for _, rr := range recipes[menuID] {
			if q := cupRequirement(rr, temp, size, sugar2); q > 0 {
				needs = append(needs, stockNeed{IngID: rr.IngredientID, Delta: q,
					Reason: "cancel_restock", OrderID: &st.orderID})
			}
		}
		if err := applyStock(ctx, tx, st.storeID, needs); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE compensations SET status='withdrawn'
		WHERE remake_job_id=$1 AND status IN ('proposed','confirmed')`, jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE remake_jobs SET status='canceled',decision='aborted',decided_by=$2,
		  decided_at=now(),decision_detail=$3 WHERE id=$1`,
		jobID, by, map[string]any{"reason": reason, "note": note}); err != nil {
		return err
	}
	return a.logEvent(ctx, tx, st.orderID, "remake_aborted", actorByID(ctx, tx, by), map[string]any{
		"remake_job_id": jobID, "reason": reason, "note": note,
	})
}

func actorName(ctx context.Context, tx pgx.Tx, id int64) string {
	var name string
	if err := tx.QueryRow(ctx, `SELECT name FROM accounts WHERE id=$1`, id).Scan(&name); err != nil {
		return ""
	}
	return name
}

// ---------- 新杯贴打印（打印后即不可回退取原杯） ----------

// POST /v1/staff/remakes/{id}/new-cup-label
func (a *App) printNewCupLabel(w http.ResponseWriter, r *http.Request) {
	jobID := toInt64(chi.URLParam(r, "id"))
	ac := actor(r)
	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var orderID int64
	if err := tx.QueryRow(ctx, `SELECT order_id FROM remake_jobs WHERE id=$1`, jobID).Scan(&orderID); err != nil {
		writeErr(w, err)
		return
	}
	if err := lockOrderTx(ctx, tx, orderID); err != nil {
		writeErr(w, err)
		return
	}
	var status string
	var newCupID *int64
	if err := tx.QueryRow(ctx,
		`SELECT status,new_cup_id FROM remake_jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&status, &newCupID); err != nil {
		writeErr(w, err)
		return
	}
	if status != "remaking" {
		writeErr(w, errf(409, "job_not_open", "重做任务已 %s，不能打印新杯贴", status))
		return
	}
	if _, err := tx.Exec(ctx,
		`UPDATE remake_jobs SET new_label_printed=TRUE,new_label_printed_at=now()
		 WHERE id=$1 AND new_label_printed=FALSE`, jobID); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, orderID, "remake_new_label_printed", actorOf(ac), map[string]any{
		"remake_job_id": jobID, "new_cup_id": newCupID,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	job, _ := scanRemakeJob(a.Pool.QueryRow(ctx,
		`SELECT `+remakeJobCols+` FROM remake_jobs WHERE id=$1`, jobID))
	writeJSON(w, 200, map[string]any{
		"printed": true, "job": job,
		"message": "新杯贴已打印；此后骑手到店只能等待新杯，旧码持续失效",
	})
}

// ---------- 门店重做任务看板 ----------

// GET /v1/staff/stores/{id}/remakes
func (a *App) listStoreRemakes(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	status := r.URL.Query().Get("status")
	q := `SELECT ` + remakeJobCols + ` FROM remake_jobs WHERE store_id=$1`
	args := []any{storeID}
	if status != "" {
		q += ` AND status=$2`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT 200`
	rows, err := a.Pool.Query(r.Context(), q, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []RemakeJob{}
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
