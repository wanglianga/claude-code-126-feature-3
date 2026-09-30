package app

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

func (a *App) listOrderExceptions(w http.ResponseWriter, r *http.Request) {
	id := toInt64(chi.URLParam(r, "id"))
	writeJSON(w, 200, a.queryExceptions(r, "WHERE order_id=$1 ORDER BY id", id))
}

func (a *App) queryExceptions(r *http.Request, where string, args ...any) []Exception {
	rows, err := a.Pool.Query(r.Context(), `
		SELECT id,order_id,store_id,type,status,responsibility,occurrences,detail,
		       comp_amount::float8,cost_amount::float8,created_at,resolved_at,COALESCE(resolution,'')
		FROM exceptions `+where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Exception
	for rows.Next() {
		var e Exception
		var raw []byte
		if err := rows.Scan(&e.ID, &e.OrderID, &e.StoreID, &e.Type, &e.Status,
			&e.Responsibility, &e.Occurrences, &raw, &e.CompAmount, &e.CostAmount,
			&e.CreatedAt, &e.ResolvedAt, &e.Resolution); err != nil {
			return nil
		}
		_ = json.Unmarshal(raw, &e.Detail)
		out = append(out, e)
	}
	return out
}

// ---------- 顾客投诉（漏液等） ----------

type complainReq struct {
	Type       string  `json:"type"` // leak_complaint / wrong_cup / note_missed / other
	Detail     string  `json:"detail"`
	CompAmount float64 `json:"comp_amount"` // 顾客诉求金额，可选；为空按政策=订单金额
}

func (a *App) complain(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	var req complainReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Type == "" {
		req.Type = "leak_complaint"
	}
	ac := actor(r)
	ctx := r.Context()

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	o, err := scanOrder(tx.QueryRow(ctx, "SELECT "+orderCols+" FROM orders WHERE id=$1 FOR UPDATE", orderID))
	if err != nil {
		writeErr(w, err)
		return
	}
	comp := req.CompAmount
	if comp <= 0 {
		comp = o.TotalAmount // 默认全额补偿，财务可改判
	}
	detail := map[string]any{"detail": req.Detail, "claimed_by": ac.Name, "via_platform": ac.Platform}
	var excID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO exceptions(order_id,store_id,type,status,responsibility,detail,comp_amount,created_by)
		VALUES($1,$2,$3,'open','pending',$4,$5,$6) RETURNING id`,
		orderID, o.StoreID, req.Type, detail, comp, ac.ID).Scan(&excID); err != nil {
		writeErr(w, err)
		return
	}
	// 补偿挂账，待财务定责后确定承担方
	if _, err := tx.Exec(ctx, `
		INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,note,created_by)
		VALUES($1,$2,$3,'customer_comp',$4,'pending',$5,$6)`,
		o.StoreID, orderID, excID, -comp, "顾客投诉补偿(待财务定责)", ac.ID); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.logEvent(ctx, tx, orderID, "customer_complaint", actorOf(ac), map[string]any{
		"type": req.Type, "detail": req.Detail, "claimed_comp": comp,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"exception_id": excID, "type": req.Type, "status": "open",
		"responsibility": "pending", "claimed_comp_amount": comp,
		"message": "投诉已受理并同步门店/平台客服/财务，等待定责",
	})
}

// ---------- 财务定责 ----------

type adjudicateReq struct {
	Responsibility string  `json:"responsibility"` // store / rider / platform / customer / shared
	CompAmount     float64 `json:"comp_amount"`    // 最终补偿金额
	PlatformShare  float64 `json:"platform_share"` // 平台承担金额（平台补贴给门店，正数）
	Resolution     string  `json:"resolution"`
}

func (a *App) adjudicate(w http.ResponseWriter, r *http.Request) {
	excID := toInt64(chi.URLParam(r, "id"))
	var req adjudicateReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	switch req.Responsibility {
	case "store", "rider", "platform", "customer", "shared":
	default:
		writeErr(w, errf(400, "bad_request", "responsibility 非法"))
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
	var oldComp float64
	var typ string
	if err := tx.QueryRow(ctx,
		`SELECT order_id,store_id,comp_amount,type FROM exceptions WHERE id=$1 FOR UPDATE`,
		excID).Scan(&orderID, &storeID, &oldComp, &typ); err != nil {
		writeErr(w, err)
		return
	}
	comp := req.CompAmount
	if comp == 0 {
		comp = oldComp
	}
	if _, err := tx.Exec(ctx, `
		UPDATE exceptions SET status='resolved',resolved_at=now(),resolved_by=$2,
		       responsibility=$3,comp_amount=$4,resolution=$5 WHERE id=$1`,
		excID, ac.ID, req.Responsibility, comp, req.Resolution); err != nil {
		writeErr(w, err)
		return
	}
	// 更新挂账补偿金额与承担方
	bearer := req.Responsibility
	if bearer == "shared" {
		bearer = "store"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE finance_records SET amount=$2,bearer=$3,note=COALESCE(NULLIF($4::text,''),note)
		WHERE exception_id=$1 AND kind='customer_comp' AND bearer='pending'`,
		excID, -comp, bearer, req.Resolution); err != nil {
		writeErr(w, err)
		return
	}
	// 平台分摊：平台补贴给门店
	if req.PlatformShare > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,note,created_by)
			VALUES($1,$2,$3,'platform_comp',$4,'platform',$5,$6)`,
			storeID, orderID, excID, req.PlatformShare, "平台分摊补偿", ac.ID); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := a.logEvent(ctx, tx, orderID, "exception_adjudicated", actorOf(ac), map[string]any{
		"exception_id": excID, "type": typ, "responsibility": req.Responsibility,
		"comp_amount": comp, "platform_share": req.PlatformShare,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"exception_id": excID, "status": "resolved",
		"responsibility": req.Responsibility, "comp_amount": comp,
		"platform_share": req.PlatformShare,
	})
}

// ---------- 门店财务台账 ----------

func (a *App) storeFinance(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	from, to := parseRange(r)
	rows, err := a.Pool.Query(r.Context(), `
		SELECT kind,bearer,count(*),COALESCE(sum(amount),0)::float8
		FROM finance_records
		WHERE store_id=$1 AND period_date BETWEEN $2 AND $3
		GROUP BY kind,bearer ORDER BY kind,bearer`, storeID, from, to)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type agg struct {
		Kind   string  `json:"kind"`
		Bearer string  `json:"bearer"`
		Count  int     `json:"count"`
		Amount float64 `json:"amount"`
	}
	var aggs []agg
	var net float64
	for rows.Next() {
		var x agg
		_ = rows.Scan(&x.Kind, &x.Bearer, &x.Count, &x.Amount)
		aggs = append(aggs, x)
		net += x.Amount
	}
	// 明细
	drows, err := a.Pool.Query(r.Context(), `
		SELECT id,COALESCE(order_id,0),COALESCE(exception_id,0),kind,amount::float8,bearer,
		       COALESCE(note,''),created_at
		FROM finance_records
		WHERE store_id=$1 AND period_date BETWEEN $2 AND $3
		ORDER BY id DESC LIMIT 300`, storeID, from, to)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer drows.Close()
	type detail struct {
		ID          int64     `json:"id"`
		OrderID     int64     `json:"order_id"`
		ExceptionID int64     `json:"exception_id"`
		Kind        string    `json:"kind"`
		Amount      float64   `json:"amount"`
		Bearer      string    `json:"bearer"`
		Note        string    `json:"note"`
		CreatedAt   time.Time `json:"created_at"`
	}
	var details []detail
	for drows.Next() {
		var d detail
		_ = drows.Scan(&d.ID, &d.OrderID, &d.ExceptionID, &d.Kind, &d.Amount, &d.Bearer, &d.Note, &d.CreatedAt)
		details = append(details, d)
	}
	writeJSON(w, 200, map[string]any{
		"store_id": storeID, "from": from, "to": to,
		"summary": aggs, "net_profit_impact": net, "records": details,
	})
}

func parseRange(r *http.Request) (string, string) {
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")
	if from == "" {
		from = time.Now().AddDate(0, -1, 0).Format("2006-01-02")
	}
	if to == "" {
		to = time.Now().Format("2006-01-02")
	}
	return from, to
}

// ---------- 门店时效/质量/利润汇总 ----------

func (a *App) storeSummary(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	from, to := parseRange(r)
	ctx := r.Context()

	var m struct {
		OrdersReceived    int     `json:"orders_received"`
		OrdersHanded      int     `json:"orders_handed"`
		OrdersCanceled    int     `json:"orders_canceled"`
		StoreFaultCancel  int     `json:"store_fault_cancels"`
		AvgMakeSeconds    float64 `json:"avg_make_seconds"`
		P90MakeSeconds    float64 `json:"p90_make_seconds"`
		SlowOrders        int     `json:"slow_orders"`
		RiderWaitStoreSec int     `json:"store_fault_wait_seconds"`
		WrongCups         int     `json:"wrong_cups"`
		MissingStraw      int     `json:"missing_straw"`
		MissingUtensil    int     `json:"missing_utensil"`
		WrongBags         int     `json:"wrong_bag_handoffs"`
		Remakes           int     `json:"remakes"`
		RemakeCost        float64 `json:"remake_cost"`
		RemakeWaitSec     int     `json:"remake_rider_wait_seconds"`
		RemakeTeamNetCost float64 `json:"remake_team_net_cost"`
		PlatformReattr    float64 `json:"platform_reattributed"`
		TakeOldReversals  int     `json:"remake_take_old"`
		LeakComplaints    int     `json:"leak_complaints"`
		FoodOOS           int     `json:"food_oos"`
		NoteMissed        int     `json:"note_missed"`
		Stale             int     `json:"stale_drinks"`
		Revenue           float64 `json:"revenue"`
		Refunds           float64 `json:"refunds"`
		CustomerComp      float64 `json:"customer_comp"`
		PlatformComp      float64 `json:"platform_comp"`
		LossCost          float64 `json:"loss_cost"`
		NetProfitImpact   float64 `json:"net_profit_impact"`
	}
	err := a.Pool.QueryRow(ctx, `
		WITH ord AS (
		  SELECT * FROM orders WHERE store_id=$1 AND ordered_at::date BETWEEN $2 AND $3
		), exc AS (
		  SELECT * FROM exceptions WHERE store_id=$1 AND created_at::date BETWEEN $2 AND $3
		), fin AS (
		  SELECT * FROM finance_records WHERE store_id=$1 AND period_date BETWEEN $2 AND $3
		)
		SELECT
		  (SELECT count(*) FROM ord),
		  (SELECT count(*) FROM ord WHERE status='handed'),
		  (SELECT count(*) FROM ord WHERE status='canceled'),
		  (SELECT count(*) FROM ord WHERE status='canceled' AND cancel_store_fault),
		  COALESCE((SELECT avg(EXTRACT(EPOCH FROM (all_sealed_at-ordered_at)))
		    FROM ord WHERE all_sealed_at IS NOT NULL),0),
		  COALESCE((SELECT percentile_cont(0.9) WITHIN GROUP (
		    ORDER BY EXTRACT(EPOCH FROM (all_sealed_at-ordered_at)))
		    FROM ord WHERE all_sealed_at IS NOT NULL),0),
		  (SELECT count(*) FROM ord WHERE all_sealed_at IS NOT NULL
		    AND all_sealed_at > ordered_at + make_interval(mins => promised_minutes)),
		  COALESCE((SELECT sum(store_fault_wait_seconds) FROM rider_arrivals
		    WHERE store_id=$1 AND arrived_at::date BETWEEN $2 AND $3),0),
		  (SELECT count(*) FROM exc WHERE type='wrong_cup'),
		  (SELECT count(*) FROM exc WHERE type='missing_straw'),
		  (SELECT count(*) FROM exc WHERE type='missing_utensil'),
		  (SELECT count(*) FROM exc WHERE type='wrong_bag'),
		  (SELECT count(*) FROM loss_reports WHERE store_id=$1 AND reason IN ('wrong_cup','note_missed','stale_drink')
		    AND created_at::date BETWEEN $2 AND $3),
		  COALESCE((SELECT sum(amount) FROM fin WHERE kind='remake_cost'),0),
		  COALESCE((SELECT sum(rider_wait_seconds) FROM remake_jobs
		    WHERE store_id=$1 AND created_at::date BETWEEN $2 AND $3),0),
		  COALESCE((SELECT COALESCE(sum(amount),0) FROM fin
		    WHERE kind IN ('remake_cost','loss','loss_reversal','remake_reversal')
		      OR (kind='customer_comp' AND bearer IN ('team','store'))
		      OR (kind='platform_comp' AND review_id IS NOT NULL)),0),
		  COALESCE((SELECT sum(amount) FROM fin WHERE kind='platform_comp' AND review_id IS NOT NULL),0),
		  (SELECT count(*) FROM remake_jobs WHERE store_id=$1 AND status='old_taken'
		    AND created_at::date BETWEEN $2 AND $3),
		  (SELECT count(*) FROM exc WHERE type='leak_complaint'),
		  (SELECT count(*) FROM exc WHERE type='food_oos'),
		  (SELECT count(*) FROM exc WHERE type='note_missed'),
		  (SELECT count(*) FROM exc WHERE type='stale_drink'),
		  COALESCE((SELECT sum(amount) FROM fin WHERE kind='revenue'),0),
		  COALESCE((SELECT sum(amount) FROM fin WHERE kind='refund'),0),
		  COALESCE((SELECT sum(amount) FROM fin WHERE kind='customer_comp'),0),
		  COALESCE((SELECT sum(amount) FROM fin WHERE kind='platform_comp'),0),
		  COALESCE((SELECT sum(amount) FROM fin WHERE kind='loss'),0),
		  COALESCE((SELECT sum(amount) FROM fin),0)
		`, storeID, from, to).Scan(
		&m.OrdersReceived, &m.OrdersHanded, &m.OrdersCanceled, &m.StoreFaultCancel,
		&m.AvgMakeSeconds, &m.P90MakeSeconds, &m.SlowOrders, &m.RiderWaitStoreSec,
		&m.WrongCups, &m.MissingStraw, &m.MissingUtensil, &m.WrongBags, &m.Remakes,
		&m.RemakeCost, &m.RemakeWaitSec, &m.RemakeTeamNetCost, &m.PlatformReattr, &m.TakeOldReversals,
		&m.LeakComplaints, &m.FoodOOS, &m.NoteMissed, &m.Stale,
		&m.Revenue, &m.Refunds, &m.CustomerComp, &m.PlatformComp, &m.LossCost, &m.NetProfitImpact)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 收入在交接时才实现；报表同时把 handed 订单金额按平台口径汇总为收入参考
	if m.Revenue == 0 {
		_ = a.Pool.QueryRow(ctx,
			`SELECT COALESCE(sum(total_amount),0)::float8 FROM orders
			 WHERE store_id=$1 AND status='handed' AND handed_at::date BETWEEN $2 AND $3`,
			storeID, from, to).Scan(&m.Revenue)
	}
	writeJSON(w, 200, map[string]any{"store_id": storeID, "from": from, "to": to, "metrics": m})
}

// ---------- 区域报表 ----------

func (a *App) regionReport(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	if region == "" {
		region = actor(r).Region
	}
	from, to := parseRange(r)
	rows, err := a.Pool.Query(r.Context(), `
		SELECT s.id,s.code,s.name,
		  (SELECT count(*) FROM orders o WHERE o.store_id=s.id AND o.ordered_at::date BETWEEN $1 AND $2),
		  (SELECT count(*) FROM orders o WHERE o.store_id=s.id AND o.status='handed' AND o.handed_at::date BETWEEN $1 AND $2),
		  (SELECT count(*) FROM orders o WHERE o.store_id=s.id AND o.status='canceled' AND o.canceled_at::date BETWEEN $1 AND $2),
		  COALESCE((SELECT avg(EXTRACT(EPOCH FROM (o.all_sealed_at-o.ordered_at))) FROM orders o
		    WHERE o.store_id=s.id AND o.all_sealed_at IS NOT NULL AND o.ordered_at::date BETWEEN $1 AND $2),0),
		  (SELECT count(*) FROM orders o WHERE o.store_id=s.id AND o.all_sealed_at IS NOT NULL
		    AND o.ordered_at::date BETWEEN $1 AND $2
		    AND o.all_sealed_at > o.ordered_at + make_interval(mins => o.promised_minutes)),
		  (SELECT count(*) FROM exceptions e WHERE e.store_id=s.id AND e.type='wrong_cup' AND e.created_at::date BETWEEN $1 AND $2),
		  (SELECT count(*) FROM exceptions e WHERE e.store_id=s.id AND e.type IN ('missing_straw','missing_utensil') AND e.created_at::date BETWEEN $1 AND $2),
		  (SELECT count(*) FROM loss_reports l WHERE l.store_id=s.id AND l.reason IN ('wrong_cup','note_missed','stale_drink') AND l.created_at::date BETWEEN $1 AND $2),
		  COALESCE((SELECT sum(amount) FROM finance_records f WHERE f.store_id=s.id AND f.period_date BETWEEN $1 AND $2),0)::float8
		FROM stores s WHERE ($3='' OR s.region=$3)
		ORDER BY s.id`, from, to, region)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type row struct {
		StoreID         int64   `json:"store_id"`
		Code            string  `json:"code"`
		Name            string  `json:"name"`
		Orders          int     `json:"orders"`
		Handed          int     `json:"handed"`
		Canceled        int     `json:"canceled"`
		AvgMakeSeconds  float64 `json:"avg_make_seconds"`
		SlowOrders      int     `json:"slow_orders"`
		WrongCups       int     `json:"wrong_cups"`
		MissingItems    int     `json:"missing_straw_utensil"`
		Remakes         int     `json:"remakes"`
		NetProfitImpact float64 `json:"net_profit_impact"`
	}
	var out []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.StoreID, &x.Code, &x.Name, &x.Orders, &x.Handed, &x.Canceled,
			&x.AvgMakeSeconds, &x.SlowOrders, &x.WrongCups, &x.MissingItems, &x.Remakes,
			&x.NetProfitImpact); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, x)
	}
	writeJSON(w, 200, map[string]any{"region": region, "from": from, "to": to, "stores": out})
}

// ---------- 班组评分 ----------

func computeScore(m map[string]int) (int, map[string]int) {
	score := 100
	penalty := map[string]int{}
	orderCount := m["orders"]
	if orderCount > 0 {
		slowPen := m["slow"] * 100 / orderCount // 慢单百分比
		if slowPen > 100 {
			slowPen = 100
		}
		score -= slowPen / 5 // 慢单率每 5% 扣 1 分
		penalty["slow_rate_penalty"] = slowPen / 5
	}
	score -= m["wrong_cup"] * 4
	score -= m["missing"] * 2
	score -= m["wrong_bag"] * 2
	score -= m["stale"] * 3
	score -= m["note_missed"] * 2
	score -= m["store_cancel"] * 5
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	penalty["wrong_cup_penalty"] = m["wrong_cup"] * 4
	penalty["missing_penalty"] = m["missing"] * 2
	penalty["wrong_bag_penalty"] = m["wrong_bag"] * 2
	penalty["stale_penalty"] = m["stale"] * 3
	penalty["note_missed_penalty"] = m["note_missed"] * 2
	penalty["store_cancel_penalty"] = m["store_cancel"] * 5
	return score, penalty
}

func (a *App) refreshScore(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	day := time.Now().Format("2006-01-02")
	ctx := r.Context()
	var counts map[string]int
	var remakeCost float64
	rows, err := a.Pool.Query(ctx, `
		SELECT 'orders', count(*) FROM orders WHERE store_id=$1 AND ordered_at::date=$2
		UNION ALL SELECT 'slow', count(*) FROM orders WHERE store_id=$1 AND ordered_at::date=$2
		  AND all_sealed_at IS NOT NULL AND all_sealed_at > ordered_at + make_interval(mins => promised_minutes)
		UNION ALL SELECT 'wrong_cup', count(*) FROM exceptions WHERE store_id=$1 AND type='wrong_cup' AND created_at::date=$2
		UNION ALL SELECT 'missing', count(*) FROM exceptions WHERE store_id=$1 AND type IN ('missing_straw','missing_utensil') AND created_at::date=$2
		UNION ALL SELECT 'wrong_bag', count(*) FROM exceptions WHERE store_id=$1 AND type='wrong_bag' AND created_at::date=$2
		UNION ALL SELECT 'stale', count(*) FROM exceptions WHERE store_id=$1 AND type='stale_drink' AND created_at::date=$2
		UNION ALL SELECT 'note_missed', count(*) FROM exceptions WHERE store_id=$1 AND type='note_missed' AND created_at::date=$2
		UNION ALL SELECT 'store_cancel', count(*) FROM orders WHERE store_id=$1 AND status='canceled' AND cancel_store_fault AND canceled_at::date=$2`,
		storeID, day)
	if err != nil {
		writeErr(w, err)
		return
	}
	counts = map[string]int{}
	for rows.Next() {
		var k string
		var v int
		_ = rows.Scan(&k, &v)
		counts[k] = v
	}
	rows.Close()
	_ = a.Pool.QueryRow(ctx,
		`SELECT COALESCE(sum(amount),0)::float8 FROM finance_records
		 WHERE store_id=$1 AND period_date=$2
		   AND (kind IN ('remake_cost','loss_reversal','remake_reversal')
		        OR (kind='platform_comp' AND review_id IS NOT NULL))`, storeID, day).Scan(&remakeCost)

	score, penalty := computeScore(counts)
	metrics := map[string]any{
		"counts": counts, "penalties": penalty, "remake_cost": remakeCost, "date": day,
	}
	var id int64
	err = a.Pool.QueryRow(ctx, `
		INSERT INTO team_score_snapshots(store_id,score_date,metrics,score)
		VALUES($1,$2,$3,$4)
		ON CONFLICT (store_id,score_date) DO UPDATE SET metrics=$3,score=$4,updated_at=now()
		RETURNING id`, storeID, day, metrics, score).Scan(&id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "store_id": storeID, "score_date": day, "score": score,
		"metrics": metrics, "remake_cost": remakeCost,
	})
}

func (a *App) getScore(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	var id int
	var day time.Time
	var raw []byte
	var score int
	err := a.Pool.QueryRow(r.Context(),
		`SELECT id,score_date,metrics,score FROM team_score_snapshots
		 WHERE store_id=$1 ORDER BY score_date DESC LIMIT 1`, storeID).Scan(&id, &day, &raw, &score)
	if err != nil {
		writeErr(w, err)
		return
	}
	var metrics map[string]any
	_ = json.Unmarshal(raw, &metrics)
	writeJSON(w, 200, map[string]any{"store_id": storeID, "score_date": day.Format("2006-01-02"), "score": score, "metrics": metrics})
}

func (a *App) listScores(w http.ResponseWriter, r *http.Request) {
	rows, err := a.Pool.Query(r.Context(), `
		SELECT t.store_id,s.name,t.score_date,t.score
		FROM team_score_snapshots t JOIN stores s ON s.id=t.store_id
		WHERE t.score_date=(SELECT max(score_date) FROM team_score_snapshots t2 WHERE t2.store_id=t.store_id)
		ORDER BY t.score DESC, t.store_id`)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type row struct {
		StoreID   int64  `json:"store_id"`
		StoreName string `json:"store_name"`
		Date      string `json:"score_date"`
		Score     int    `json:"score"`
	}
	var out []row
	for rows.Next() {
		var x row
		var d time.Time
		_ = rows.Scan(&x.StoreID, &x.StoreName, &d, &x.Score)
		x.Date = d.Format("2006-01-02")
		out = append(out, x)
	}
	writeJSON(w, 200, out)
}

// ---------- 督导复盘 ----------

type reviewReq struct {
	From    string   `json:"from"`
	To      string   `json:"to"`
	Summary string   `json:"summary"`
	Actions []string `json:"actions"`
}

func (a *App) createReview(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	var req reviewReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Summary == "" || req.From == "" || req.To == "" {
		writeErr(w, errf(400, "bad_request", "from/to/summary 必填"))
		return
	}
	ac := actor(r)
	ctx := r.Context()

	var remakeTotal float64
	if err := a.Pool.QueryRow(ctx, `
		SELECT COALESCE(sum(amount),0)::float8 FROM finance_records
		WHERE store_id=$1 AND period_date BETWEEN $2 AND $3
		  AND (kind IN ('remake_cost','loss','loss_reversal','remake_reversal')
		       OR (kind='platform_comp' AND review_id IS NOT NULL))`,
		storeID, req.From, req.To).Scan(&remakeTotal); err != nil {
		writeErr(w, err)
		return
	}
	score := 0
	_ = a.Pool.QueryRow(ctx,
		`SELECT score FROM team_score_snapshots WHERE store_id=$1 AND score_date<=$2 ORDER BY score_date DESC LIMIT 1`,
		storeID, req.To).Scan(&score)
	if score == 0 {
		score = 100
	}
	if req.Actions == nil {
		req.Actions = []string{}
	}
	var rid int64
	if err := a.Pool.QueryRow(ctx, `
		INSERT INTO store_reviews(store_id,from_date,to_date,supervisor_id,summary,actions,
		  score_snapshot,remake_cost_total)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		storeID, req.From, req.To, ac.ID, req.Summary, req.Actions, score, remakeTotal).Scan(&rid); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"review_id": rid, "store_id": storeID, "from": req.From, "to": req.To,
		"score_snapshot": score, "remake_cost_total": remakeTotal,
		"summary": req.Summary, "actions": req.Actions,
		"note": "重做成本已计入门店班组复盘",
	})
}

func (a *App) listReviews(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	rows, err := a.Pool.Query(r.Context(), `
		SELECT id,from_date,to_date,COALESCE(supervisor_id,0),summary,actions,
		       score_snapshot,remake_cost_total::float8,created_at
		FROM store_reviews WHERE store_id=$1 ORDER BY id DESC LIMIT 100`, storeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type rv struct {
		ID            int64     `json:"id"`
		From          string    `json:"from"`
		To            string    `json:"to"`
		SupervisorID  int64     `json:"supervisor_id"`
		Summary       string    `json:"summary"`
		Actions       []string  `json:"actions"`
		ScoreSnapshot int       `json:"score_snapshot"`
		RemakeCost    float64   `json:"remake_cost_total"`
		CreatedAt     time.Time `json:"created_at"`
	}
	var out []rv
	for rows.Next() {
		var x rv
		var actions []byte
		if err := rows.Scan(&x.ID, &x.From, &x.To, &x.SupervisorID, &x.Summary, &actions,
			&x.ScoreSnapshot, &x.RemakeCost, &x.CreatedAt); err != nil {
			writeErr(w, err)
			return
		}
		_ = json.Unmarshal(actions, &x.Actions)
		out = append(out, x)
	}
	writeJSON(w, 200, out)
}
