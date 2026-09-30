package app

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// 店长可对“设备故障 / 平台派单延迟”申请责任复核；区域经理改判后，
// 用调整流水回算门店班组利润与补偿承担方，原始事件、原始流水一律保留不覆盖。

type reviewCreateReq struct {
	ExceptionID *int64 `json:"exception_id"`
	RemakeJobID *int64 `json:"remake_job_id"`
	Ground      string `json:"ground"` // equipment_fault / platform_dispatch_delay
	Reason      string `json:"reason"`
	Evidence    string `json:"evidence"`
}

// POST /v1/management/orders/{id}/responsibility-reviews
func (a *App) createResponsibilityReview(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	var req reviewCreateReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Ground != "equipment_fault" && req.Ground != "platform_dispatch_delay" {
		writeErr(w, errf(400, "bad_request", "ground 必须是 equipment_fault/platform_dispatch_delay"))
		return
	}
	if req.Reason == "" {
		writeErr(w, errf(400, "bad_request", "reason 必填（说明故障/派单延迟情况）"))
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

	var storeID int64
	if err := tx.QueryRow(ctx, `SELECT store_id FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&storeID); err != nil {
		writeErr(w, err)
		return
	}
	if ac.Role == "store_manager" && ac.StoreID != nil && *ac.StoreID != storeID {
		writeErr(w, errForbidden)
		return
	}
	if req.RemakeJobID != nil {
		var jStore int64
		if err := tx.QueryRow(ctx, `SELECT store_id FROM remake_jobs WHERE id=$1`, *req.RemakeJobID).
			Scan(&jStore); err != nil {
			writeErr(w, errf(400, "bad_remake_job", "重做任务不存在"))
			return
		}
		if jStore != storeID {
			writeErr(w, errf(409, "cross_store", "重做任务不属于该订单门店"))
			return
		}
	}
	if req.ExceptionID != nil {
		var eStore int64
		if err := tx.QueryRow(ctx, `SELECT store_id FROM exceptions WHERE id=$1`, *req.ExceptionID).
			Scan(&eStore); err != nil {
			writeErr(w, errf(400, "bad_exception", "异常不存在"))
			return
		}
		if eStore != storeID {
			writeErr(w, errf(409, "cross_store", "异常不属于该订单门店"))
			return
		}
	}
	// 同一标的（订单+ground）已有待审申请则不重复提交
	var dup int
	_ = tx.QueryRow(ctx,
		`SELECT count(*) FROM responsibility_reviews WHERE order_id=$1 AND ground=$2 AND status='pending'`,
		orderID, req.Ground).Scan(&dup)
	if dup > 0 {
		writeErr(w, errf(409, "review_pending", "该事项已有待区域经理审核的复核申请"))
		return
	}

	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO responsibility_reviews(store_id,order_id,exception_id,remake_job_id,ground,reason,
			evidence,status,requested_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,'pending',$8) RETURNING id`,
		storeID, orderID, req.ExceptionID, req.RemakeJobID, req.Ground, req.Reason, req.Evidence,
		ac.ID).Scan(&id); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, orderID, "responsibility_review_requested", actorOf(ac), map[string]any{
		"review_id": id, "ground": req.Ground, "reason": req.Reason,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"id": id, "status": "pending", "ground": req.Ground,
		"message": "复核申请已提交，待区域经理改判",
	})
}

type reviewAdjudicateReq struct {
	Approved           bool    `json:"approved"`
	NewResponsibility  string  `json:"new_responsibility"`  // platform / shared / store
	ReattributedAmount float64 `json:"reattributed_amount"` // 为空则按重做相关流水自动回算
	Resolution         string  `json:"resolution"`
}

// POST /v1/management/responsibility-reviews/{id}/adjudicate（区域经理）
func (a *App) adjudicateReview(w http.ResponseWriter, r *http.Request) {
	id := toInt64(chi.URLParam(r, "id"))
	var req reviewAdjudicateReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Approved {
		switch req.NewResponsibility {
		case "platform", "shared", "store":
		default:
			writeErr(w, errf(400, "bad_request", "approved=true 时 new_responsibility 必须是 platform/shared/store"))
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

	var rv ResponsibilityReview
	err = tx.QueryRow(ctx, `
		SELECT id,store_id,order_id,exception_id,remake_job_id,COALESCE(ground,''),COALESCE(reason,''),
		       COALESCE(evidence,''),status,requested_by,requested_at
		FROM responsibility_reviews WHERE id=$1 FOR UPDATE`, id).Scan(
		&rv.ID, &rv.StoreID, &rv.OrderID, &rv.ExceptionID, &rv.RemakeJobID, &rv.Ground, &rv.Reason,
		&rv.Evidence, &rv.Status, &rv.RequestedBy, &rv.RequestedAt)
	if err != nil {
		writeErr(w, err)
		return
	}
	if ac.Role == "area_manager" && ac.Region != "" {
		var region string
		if err := tx.QueryRow(ctx, `SELECT region FROM stores WHERE id=$1`, rv.StoreID).Scan(&region); err != nil {
			writeErr(w, err)
			return
		}
		if region != ac.Region {
			writeErr(w, errForbidden)
			return
		}
	}
	if rv.Status != "pending" {
		writeErr(w, errf(409, "review_decided", "该复核已 %s，不能重复改判（原始判定保留）", rv.Status))
		return
	}

	if !req.Approved {
		if _, err := tx.Exec(ctx, `
			UPDATE responsibility_reviews SET status='rejected',approved=FALSE,reviewed_by=$2,
			  reviewed_at=now(),resolution=$3 WHERE id=$1`, id, ac.ID, req.Resolution); err != nil {
			writeErr(w, err)
			return
		}
		if err := a.logEvent(ctx, tx, rv.OrderID, "responsibility_review_rejected", actorOf(ac), map[string]any{
			"review_id": id, "ground": rv.Ground, "resolution": req.Resolution,
		}); err != nil {
			writeErr(w, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"id": id, "status": "rejected", "approved": false})
		return
	}

	// ---- 批准改判：回算（只追加调整流水，不覆盖原始事件/流水） ----
	excIDs := []int64{}
	if rv.ExceptionID != nil {
		excIDs = append(excIDs, *rv.ExceptionID)
	} else {
		rows, e := tx.Query(ctx,
			`SELECT id FROM exceptions WHERE order_id=$1
			 AND type IN ('stale_drink','wrong_cup','note_missed','equipment_fault')`, rv.OrderID)
		if e != nil {
			writeErr(w, e)
			return
		}
		for rows.Next() {
			var x int64
			_ = rows.Scan(&x)
			excIDs = append(excIDs, x)
		}
		rows.Close()
	}
	for _, eid := range excIDs {
		if _, err := tx.Exec(ctx, `
			UPDATE exceptions
			SET original_responsibility=COALESCE(original_responsibility,responsibility),
			    responsibility=$2
			WHERE id=$1`, eid, req.NewResponsibility); err != nil {
			writeErr(w, err)
			return
		}
	}

	// 自动回算金额：班组已承担的重做/报损净成本 + 已由班组兑付的补偿 - 既往改判已补回
	amount := req.ReattributedAmount
	if amount <= 0 {
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(-sum(amount),0)::float8 FROM finance_records
			WHERE store_id=$1 AND order_id=$2
			  AND (kind IN ('loss','remake_cost','loss_reversal','remake_reversal')
			       OR (kind='customer_comp' AND bearer IN ('team','store')))`,
			rv.StoreID, rv.OrderID).Scan(&amount); err != nil {
			writeErr(w, err)
			return
		}
		var already float64
		_ = tx.QueryRow(ctx, `
			SELECT COALESCE(sum(amount),0)::float8 FROM finance_records
			WHERE store_id=$1 AND order_id=$2 AND kind='platform_comp'`,
			rv.StoreID, rv.OrderID).Scan(&already)
		amount -= already
		if amount < 0 {
			amount = 0
		}
	}

	if amount > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,note,created_by,review_id)
			VALUES($1,$2,$3,'platform_comp',$4,'platform',$5,$6,$7)`,
			rv.StoreID, rv.OrderID, rv.ExceptionID, amount,
			"区域经理改判（"+rv.Ground+"）：平台承担重做/报损/补偿，回算班组利润，原始流水保留", ac.ID, id); err != nil {
			writeErr(w, err)
			return
		}
	}
	// 未兑付的补偿方案承担方改记平台（已兑付的由 platform_comp 一次性补回班组）
	if _, err := tx.Exec(ctx, `
		UPDATE compensations SET bearer='platform'
		WHERE order_id=$1 AND status IN ('proposed','confirmed')`, rv.OrderID); err != nil {
		writeErr(w, err)
		return
	}

	if _, err := tx.Exec(ctx, `
		UPDATE responsibility_reviews SET status='approved',approved=TRUE,reviewed_by=$2,
		  reviewed_at=now(),new_responsibility=$3,reattributed_amount=$4,resolution=$5
		WHERE id=$1`, id, ac.ID, req.NewResponsibility, amount, req.Resolution); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, rv.OrderID, "responsibility_reattributed", actorOf(ac), map[string]any{
		"review_id": id, "ground": rv.Ground,
		"new_responsibility": req.NewResponsibility, "reattributed_amount": amount,
		"note": "原始事件与流水保留，仅追加 platform_comp 调整流水",
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "status": "approved", "approved": true,
		"new_responsibility": req.NewResponsibility, "reattributed_amount": amount,
	})
}

const reviewCols = `id,store_id,order_id,exception_id,remake_job_id,ground,reason,evidence,status,
	requested_by,requested_at,reviewed_by,reviewed_at,approved,COALESCE(new_responsibility,''),
	reattributed_amount::float8,COALESCE(resolution,'')`

// GET /v1/management/responsibility-reviews?status=&store_id=
func (a *App) listResponsibilityReviews(w http.ResponseWriter, r *http.Request) {
	q := `SELECT ` + reviewCols + ` FROM responsibility_reviews WHERE 1=1`
	args := []any{}
	ac := actor(r)
	if ac.Role == "store_manager" && ac.StoreID != nil {
		q += ` AND store_id=$` + itoa(int64(len(args)+1))
		args = append(args, *ac.StoreID)
	}
	if st := r.URL.Query().Get("status"); st != "" {
		q += ` AND status=$` + itoa(int64(len(args)+1))
		args = append(args, st)
	}
	if sid := r.URL.Query().Get("store_id"); sid != "" {
		q += ` AND store_id=$` + itoa(int64(len(args)+1))
		args = append(args, toInt64(sid))
	}
	q += ` ORDER BY id DESC LIMIT 200`
	rows, err := a.Pool.Query(r.Context(), q, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []ResponsibilityReview{}
	for rows.Next() {
		var rv ResponsibilityReview
		if err := rows.Scan(&rv.ID, &rv.StoreID, &rv.OrderID, &rv.ExceptionID, &rv.RemakeJobID,
			&rv.Ground, &rv.Reason, &rv.Evidence, &rv.Status, &rv.RequestedBy, &rv.RequestedAt,
			&rv.ReviewedBy, &rv.ReviewedAt, &rv.Approved, &rv.NewResponsibility,
			&rv.ReattributedAmount, &rv.Resolution); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, rv)
	}
	writeJSON(w, 200, out)
}
