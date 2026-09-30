package app

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---------- 骑手到店 ----------

type arriveReq struct {
	OrderID         int64  `json:"order_id"`
	PlatformOrderNo string `json:"platform_order_no"`
	GroupID         *int64 `json:"group_id"`
	RiderName       string `json:"rider_name"`
}

type arriveItem struct {
	ArrivalID   int64  `json:"arrival_id"`
	OrderID     int64  `json:"order_id"`
	Ready       bool   `json:"ready"`
	OrderStatus string `json:"order_status"`
	SealedCups  int    `json:"sealed_cups"`
	TotalCups   int    `json:"total_cups"`
	Early       bool   `json:"early"`
	Bags        any    `json:"bags"`
	ExceptionID *int64 `json:"exception_id,omitempty"`
}

func (a *App) riderArrive(w http.ResponseWriter, r *http.Request) {
	var req arriveReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	ac := actor(r)
	if req.RiderName == "" {
		req.RiderName = ac.Name
	}
	ctx := r.Context()

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	// 确定订单（支持合并组：组内全部订单一起到店登记）
	var orderIDs []int64
	var storeID int64
	switch {
	case req.OrderID != 0:
		var st int64
		var grp *int64
		if err := tx.QueryRow(ctx, `SELECT store_id,merged_group_id FROM orders WHERE id=$1`,
			req.OrderID).Scan(&st, &grp); err != nil {
			writeErr(w, err)
			return
		}
		storeID, orderIDs = st, []int64{req.OrderID}
		if req.GroupID != nil {
			_ = tx.QueryRow(ctx, `SELECT store_id FROM merged_groups WHERE id=$1`, *req.GroupID).Scan(&storeID)
			rows, err := tx.Query(ctx, `SELECT id FROM orders WHERE merged_group_id=$1 ORDER BY id`, *req.GroupID)
			if err != nil {
				writeErr(w, err)
				return
			}
			defer rows.Close()
			orderIDs = orderIDs[:0]
			for rows.Next() {
				var id int64
				_ = rows.Scan(&id)
				orderIDs = append(orderIDs, id)
			}
		}
	case req.PlatformOrderNo != "":
		if err := tx.QueryRow(ctx, `SELECT id,store_id FROM orders WHERE platform=$1 AND platform_order_no=$2`,
			ac.Platform, req.PlatformOrderNo).Scan(&req.OrderID, &storeID); err != nil {
			writeErr(w, errf(404, "order_not_found", "平台单号未找到"))
			return
		}
		orderIDs = []int64{req.OrderID}
	default:
		writeErr(w, errf(400, "bad_request", "order_id 或 platform_order_no 必填"))
		return
	}

	var results []arriveItem
	for _, oid := range orderIDs {
		var status string
		var sealedN, totalN int
		if err := tx.QueryRow(ctx,
			`SELECT status FROM orders WHERE id=$1 FOR UPDATE`, oid).Scan(&status); err != nil {
			writeErr(w, err)
			return
		}
		if status == "canceled" {
			results = append(results, arriveItem{OrderID: oid, Ready: false, OrderStatus: status})
			continue
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE status='sealed'),count(*) FILTER (WHERE status<>'voided')
			 FROM order_cups WHERE order_id=$1`, oid).Scan(&sealedN, &totalN); err != nil {
			writeErr(w, err)
			return
		}
		ready := status == "active" || (totalN > 0 && sealedN == totalN)

		// 已存在未完成到店记录则复用
		var arrivalID int64
		err := tx.QueryRow(ctx,
			`SELECT id FROM rider_arrivals WHERE order_id=$1 AND handed_at IS NULL ORDER BY id DESC LIMIT 1`,
			oid).Scan(&arrivalID)
		if err != nil {
			var aid int64
			if err := tx.QueryRow(ctx, `
				INSERT INTO rider_arrivals(store_id,order_id,group_id,rider_account_id,rider_name,platform,ready_at)
				VALUES($1,$2,$3,$4,$5,$6,CASE WHEN $7 THEN now() ELSE NULL END) RETURNING id`,
				storeID, oid, req.GroupID, ac.ID, req.RiderName, ac.Platform, ready).Scan(&aid); err != nil {
				writeErr(w, err)
				return
			}
			arrivalID = aid
			etype := "rider_arrived"
			detail := map[string]any{"rider": req.RiderName, "ready": ready}
			if !ready {
				etype = "rider_arrived_early"
				var excID *int64
				eid, existed, err := openException(ctx, tx, oid, "rider_early")
				if err != nil {
					writeErr(w, err)
					return
				}
				if !existed {
					var newID int64
					if err := tx.QueryRow(ctx, `
						INSERT INTO exceptions(order_id,store_id,type,status,responsibility,detail,created_by)
						VALUES($1,$2,'rider_early','open','pending',$3,$4) RETURNING id`,
						oid, storeID, map[string]any{"rider": req.RiderName, "sealed": sealedN, "total": totalN},
						ac.ID).Scan(&newID); err != nil {
						writeErr(w, err)
						return
					}
					eid = newID
				}
				excID = &eid
				detail["exception_id"] = eid
				results = append(results, arriveItem{
					ArrivalID: arrivalID, OrderID: oid, Ready: false, OrderStatus: status,
					SealedCups: sealedN, TotalCups: totalN, Early: true,
					Bags: bagSummary(ctx, tx, oid), ExceptionID: excID,
				})
			}
			if err := a.logEvent(ctx, tx, oid, etype, actorOf(ac), detail); err != nil {
				writeErr(w, err)
				return
			}
			if ready {
				results = append(results, arriveItem{
					ArrivalID: arrivalID, OrderID: oid, Ready: true, OrderStatus: status,
					SealedCups: sealedN, TotalCups: totalN, Bags: bagSummary(ctx, tx, oid),
				})
			}
			continue
		}
		results = append(results, arriveItem{
			ArrivalID: arrivalID, OrderID: oid, Ready: ready, OrderStatus: status,
			SealedCups: sealedN, TotalCups: totalN, Early: !ready, Bags: bagSummary(ctx, tx, oid),
		})
	}

	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"rider": req.RiderName, "orders": results})
}

func (a *App) getArrival(w http.ResponseWriter, r *http.Request) {
	id := toInt64(chi.URLParam(r, "id"))
	var v struct {
		ID        int64      `json:"id"`
		StoreID   int64      `json:"store_id"`
		OrderID   int64      `json:"order_id"`
		RiderName string     `json:"rider_name"`
		Platform  string     `json:"platform"`
		ArrivedAt time.Time  `json:"arrived_at"`
		ReadyAt   *time.Time `json:"ready_at,omitempty"`
		WaitSec   *int       `json:"wait_seconds,omitempty"`
		StoreWait int        `json:"store_fault_wait_seconds"`
		HandedAt  *time.Time `json:"handed_at,omitempty"`
	}
	err := a.Pool.QueryRow(r.Context(), `
		SELECT id,store_id,order_id,rider_name,COALESCE(platform,''),arrived_at,ready_at,
		       wait_seconds,store_fault_wait_seconds,handed_at
		FROM rider_arrivals WHERE id=$1`, id).Scan(
		&v.ID, &v.StoreID, &v.OrderID, &v.RiderName, &v.Platform, &v.ArrivedAt,
		&v.ReadyAt, &v.WaitSec, &v.StoreWait, &v.HandedAt)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

// riderScreen 骑手到店屏：在店等待中的骑手与订单状态（可按门店筛选）。
func (a *App) riderScreen(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sql := `
		SELECT ra.id,ra.store_id,s.name,ra.order_id,o.platform,o.platform_order_no,ra.rider_name,
		       COALESCE(ra.platform,''),ra.arrived_at,
		       o.status,
		       (SELECT count(*) FROM order_cups c WHERE c.order_id=o.id AND c.status='sealed'),
		       (SELECT count(*) FROM order_cups c WHERE c.order_id=o.id AND c.status<>'voided'),
		       EXTRACT(EPOCH FROM (now()-ra.arrived_at))::int
		FROM rider_arrivals ra
		JOIN stores s ON s.id=ra.store_id
		JOIN orders o ON o.id=ra.order_id
		WHERE ra.handed_at IS NULL AND o.status<>'canceled'`
	args := []any{}
	if sid := q.Get("store_id"); sid != "" {
		sql += " AND ra.store_id=$1"
		args = append(args, toInt64(sid))
	}
	if actor(r).Role == "rider" {
		sql += " AND ra.rider_account_id=$" + itoa(int64(len(args)+1))
		args = append(args, actor(r).ID)
	}
	sql += " ORDER BY ra.arrived_at LIMIT 100"
	rows, err := a.Pool.Query(r.Context(), sql, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type row struct {
		ArrivalID   int64     `json:"arrival_id"`
		StoreID     int64     `json:"store_id"`
		StoreName   string    `json:"store_name"`
		OrderID     int64     `json:"order_id"`
		Platform    string    `json:"platform"`
		PlatformNo  string    `json:"platform_order_no"`
		RiderName   string    `json:"rider_name"`
		ArrivedAt   time.Time `json:"arrived_at"`
		OrderStatus string    `json:"order_status"`
		SealedCups  int       `json:"sealed_cups"`
		TotalCups   int       `json:"total_cups"`
		WaitingSec  int       `json:"waiting_seconds"`
	}
	var out []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.ArrivalID, &x.StoreID, &x.StoreName, &x.OrderID, &x.Platform,
			&x.PlatformNo, &x.RiderName, &x.Platform, &x.ArrivedAt, &x.OrderStatus,
			&x.SealedCups, &x.TotalCups, &x.WaitingSec); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, x)
	}
	writeJSON(w, 200, out)
}

// ---------- 核验交接 ----------

type handoffReq struct {
	OrderID         int64    `json:"order_id"`
	PlatformOrderNo string   `json:"platform_order_no"`
	Platform        string   `json:"platform"` // 店员代核验时指定平台
	RiderName       string   `json:"rider_name"`
	SealCodes       []string `json:"seal_codes"` // 骑手逐杯扫描的封签码；重做后只有新码可通过
	ActualBags      *int     `json:"actual_bags"`
	ActualHot       *int     `json:"actual_hot"`
	ActualCold      *int     `json:"actual_cold"`
	ActualFood      *int     `json:"actual_food"`
	HasStraw        *bool    `json:"has_straw"`
	HasUtensil      *bool    `json:"has_utensil"`
}

func (a *App) handoff(w http.ResponseWriter, r *http.Request) {
	var req handoffReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	ac := actor(r)
	ctx := r.Context()

	// 支持店员代骑手做核验；骑手也可只提供平台单号
	riderName := req.RiderName
	if riderName == "" {
		riderName = ac.Name
	}
	if req.OrderID == 0 && req.PlatformOrderNo != "" {
		platform := ac.Platform
		if req.Platform != "" {
			platform = req.Platform
		}
		if err := a.Pool.QueryRow(ctx,
			`SELECT id FROM orders WHERE platform=$1 AND platform_order_no=$2`,
			platform, req.PlatformOrderNo).Scan(&req.OrderID); err != nil {
			writeErr(w, errf(404, "order_not_found", "平台单号未找到"))
			return
		}
	}
	if req.OrderID == 0 {
		writeErr(w, errf(400, "bad_request", "order_id 或 platform_order_no 必填"))
		return
	}

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	o, err := scanOrder(tx.QueryRow(ctx, "SELECT "+orderCols+" FROM orders WHERE id=$1 FOR UPDATE", req.OrderID))
	if err != nil {
		writeErr(w, err)
		return
	}
	// 订单级串行锁：与重做/取消/重做裁决互斥，杜绝两杯同时被交接
	if err := lockOrderTx(ctx, tx, req.OrderID); err != nil {
		writeErr(w, err)
		return
	}
	if req.PlatformOrderNo != "" && req.PlatformOrderNo != o.PlatformOrderNo {
		writeErr(w, errf(409, "order_no_mismatch", "核验失败：平台单号与订单不一致"))
		return
	}
	if o.Status == "canceled" {
		writeErr(w, errf(409, "order_canceled", "订单已被平台取消，禁止交接"))
		return
	}
	if o.Status == "handed" {
		writeErr(w, errf(409, "already_handed", "订单已交接"))
		return
	}

	// 未出杯完成 → 不允许取走
	var sealedN, totalN int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status='sealed'),count(*) FILTER (WHERE status<>'voided')
		 FROM order_cups WHERE order_id=$1`, req.OrderID).Scan(&sealedN, &totalN); err != nil {
		writeErr(w, err)
		return
	}
	if sealedN < totalN {
		writeErr(w, errf(409, "drinks_not_ready",
			"饮品尚未全部出杯（%d/%d），骑手请等待，门店赶制中", sealedN, totalN))
		return
	}

	// 封签码扫描核验：骑手只能扫当前有效（sealed）杯的封签码；
	// 重做后旧杯 voided，旧码立即失效；码集合必须与本单有效封签杯完全一致，
	// 任何旧码/重复码/外单码都会拒绝，杜绝两杯同时被交接。
	if len(req.SealCodes) > 0 {
		validCodes := map[string]int64{}
		scRows, scErr := tx.Query(ctx,
			`SELECT seal_code,id FROM order_cups WHERE order_id=$1 AND status='sealed' AND seal_code IS NOT NULL`,
			req.OrderID)
		if scErr != nil {
			writeErr(w, scErr)
			return
		}
		for scRows.Next() {
			var code string
			var cid int64
			_ = scRows.Scan(&code, &cid)
			validCodes[code] = cid
		}
		scRows.Close()
		seen := map[string]bool{}
		for _, code := range req.SealCodes {
			if _, ok := validCodes[code]; !ok {
				// 该码不属于本单当前有效杯：判定是否为已作废旧码
				var oldCup int64
				var oldStatus string
				var remade bool
				errf1 := tx.QueryRow(ctx, `
					SELECT c.id,c.status,EXISTS(SELECT 1 FROM remake_jobs j
					  WHERE j.old_cup_id=c.id AND j.status IN ('remaking','completed'))
					FROM order_cups c WHERE c.seal_code=$1`, code).Scan(&oldCup, &oldStatus, &remade)
				if errf1 == nil && (oldStatus == "voided" || remade) {
					writeErr(w, errf(409, "old_seal_voided",
						"封签码 %s 已随原杯重做作废，请扫描新杯封签码取走新杯", code))
					return
				}
				writeErr(w, errf(409, "seal_code_invalid",
					"封签码 %s 不属于本单当前待交接饮品，请核对", code))
				return
			}
			if seen[code] {
				writeErr(w, errf(409, "seal_code_duplicate", "封签码 %s 被重复扫描", code))
				return
			}
			seen[code] = true
		}
		if len(seen) != len(validCodes) {
			writeErr(w, errf(409, "seal_codes_incomplete",
				"封签码数量与待交接杯数不符（扫描 %d，应 %d），请逐杯扫描后取餐", len(seen), len(validCodes)))
			return
		}
	}

	// 期望袋数 / 冷热分袋
	expHot, expCold, expFood := 0, 0, 0
	type b struct{ code, kind string }
	bagSet := map[string]string{}
	brows, err := tx.Query(ctx, `SELECT DISTINCT bag_code,kind FROM order_bags WHERE order_id=$1`, req.OrderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	for brows.Next() {
		var code, kind string
		_ = brows.Scan(&code, &kind)
		bagSet[code] = kind
		switch kind {
		case "hot":
			expHot++
		case "cold":
			expCold++
		case "food":
			expFood++
		}
	}
	brows.Close()
	expBags := len(bagSet)

	// 是否需要吸管（含冰饮）/餐具（含轻食、套餐）
	needStraw, needUtensil := false, false
	mrows, err := tx.Query(ctx,
		`SELECT DISTINCT m.is_drink,m.includes_drink,c.temp
		 FROM order_cups c JOIN menu_items m ON m.id=c.menu_item_id
		 WHERE c.order_id=$1 AND c.status<>'voided'`, req.OrderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	for mrows.Next() {
		var isDrink, includesDrink bool
		var temp string
		_ = mrows.Scan(&isDrink, &includesDrink, &temp)
		if (isDrink || includesDrink) && temp == "ice" {
			needStraw = true
		}
		if !isDrink {
			needUtensil = true
		}
	}
	mrows.Close()

	issues := []string{}
	actualBags := expBags
	if req.ActualBags != nil {
		actualBags = *req.ActualBags
		if actualBags != expBags {
			issues = append(issues, "wrong_bag_count")
		}
	}
	actualHot, actualCold, actualFood := expHot, expCold, expFood
	if req.ActualHot != nil {
		actualHot = *req.ActualHot
		if actualHot != expHot {
			issues = append(issues, "hot_bag_mismatch")
		}
	}
	if req.ActualCold != nil {
		actualCold = *req.ActualCold
		if actualCold != expCold {
			issues = append(issues, "cold_bag_mismatch")
		}
	}
	if req.ActualFood != nil {
		actualFood = *req.ActualFood
		if actualFood != expFood {
			issues = append(issues, "food_bag_mismatch")
		}
	}
	hasStraw, hasUtensil := true, true
	if req.HasStraw != nil {
		hasStraw = *req.HasStraw
	}
	if req.HasUtensil != nil {
		hasUtensil = *req.HasUtensil
	}
	if needStraw && !hasStraw {
		issues = append(issues, "missing_straw")
	}
	if needUtensil && !hasUtensil {
		issues = append(issues, "missing_utensil")
	}

	var staffID *int64
	if ac.Role == "store_manager" || ac.Role == "barista" {
		staffID = &ac.ID
	}

	if len(issues) > 0 {
		// 核验不通过：登记 handoff，异常按类型归责（取错袋=骑手；漏放吸管餐具=门店）
		if _, err := tx.Exec(ctx, `
			INSERT INTO handoffs(store_id,order_id,rider_name,staff_id,passed,
				expect_bags,expect_hot,expect_cold,expect_food,
				actual_bags,actual_hot,actual_cold,has_straw,has_utensil,issues)
			VALUES($1,$2,$3,$4,FALSE,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			o.StoreID, req.OrderID, ac.Name, staffID,
			expBags, expHot, expCold, expFood,
			actualBags, actualHot, actualCold, hasStraw, hasUtensil, issues); err != nil {
			writeErr(w, err)
			return
		}
		for _, iss := range issues {
			typ, resp := iss, "store"
			switch iss {
			case "wrong_bag_count", "hot_bag_mismatch", "cold_bag_mismatch", "food_bag_mismatch":
				typ, resp = "wrong_bag", "rider"
			case "missing_straw":
				typ = "missing_straw"
			case "missing_utensil":
				typ = "missing_utensil"
			}
			var excID int64
			err := tx.QueryRow(ctx, `
				INSERT INTO exceptions(order_id,store_id,type,status,responsibility,detail,created_by)
				VALUES($1,$2,$3,'open',$4,$5,$6) RETURNING id`,
				req.OrderID, o.StoreID, typ, resp,
				map[string]any{"issues": issues, "rider": riderName}, ac.ID).Scan(&excID)
			if err != nil {
				writeErr(w, err)
				return
			}
		}
		if err := a.logEvent(ctx, tx, req.OrderID, "handoff_check_failed", actorOf(ac), map[string]any{
			"issues": issues, "expect_bags": expBags, "actual_bags": actualBags,
			"expect_hot": expHot, "expect_cold": expCold, "expect_food": expFood,
		}); err != nil {
			writeErr(w, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 409, map[string]any{
			"passed": false, "issues": issues,
			"expect": map[string]any{"bags": expBags, "hot": expHot, "cold": expCold, "food": expFood,
				"straw": needStraw, "utensil": needUtensil},
			"actual": map[string]any{"bags": actualBags, "hot": actualHot, "cold": actualCold, "food": actualFood,
				"straw": hasStraw, "utensil": hasUtensil},
		})
		return
	}

	// 核验通过 → 交接
	if _, err := tx.Exec(ctx, `
		INSERT INTO handoffs(store_id,order_id,rider_name,staff_id,passed,
			expect_bags,expect_hot,expect_cold,expect_food,
			actual_bags,actual_hot,actual_cold,has_straw,has_utensil,issues)
		VALUES($1,$2,$3,$4,TRUE,$5,$6,$7,$8,$9,$10,$11,$12,$13,'[]')`,
		o.StoreID, req.OrderID, riderName, staffID,
		expBags, expHot, expCold, expFood,
		actualBags, actualHot, actualCold, hasStraw, hasUtensil); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := tx.Exec(ctx,
		`UPDATE orders SET status='handed',handed_at=now() WHERE id=$1`, req.OrderID); err != nil {
		writeErr(w, err)
		return
	}
	// 交接完成确认营业收入（以订单实际金额为准；此前的单品退款/取消已调整 total_amount）
	if o.TotalAmount > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO finance_records(store_id,order_id,kind,amount,bearer,note,created_by)
			VALUES($1,$2,'revenue',$3,'store','骑手核验交接确认收入',$4)`,
			o.StoreID, req.OrderID, o.TotalAmount, ac.ID); err != nil {
			writeErr(w, err)
			return
		}
	}

	// 关闭该单的 open 异常（rider_early 等到交接时由财务/客服定责的不自动关？
	// rider_early 在交接时按等待时长归责）
	var waitSec, storeWaitSec int
	arrRows, err := tx.Query(ctx,
		`SELECT id,arrived_at,ready_at FROM rider_arrivals WHERE order_id=$1 AND handed_at IS NULL ORDER BY id FOR UPDATE`,
		req.OrderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	type arr struct {
		id        int64
		arrivedAt time.Time
	}
	var arrs []arr
	now := time.Now()
	for arrRows.Next() {
		var x arr
		var readyAt *time.Time
		_ = arrRows.Scan(&x.id, &x.arrivedAt, &readyAt)
		arrs = append(arrs, x)
	}
	arrRows.Close()
	// 门店责任等待：骑手按预计时间到店（或迟到）却等不到杯的部分；
	// 骑手早于自身 ETA 到店的提前量不计门店责任。
	storeFaultStart := now
	if o.RiderETA != nil {
		storeFaultStart = *o.RiderETA
	}
	for _, x := range arrs {
		wait := int(now.Sub(x.arrivedAt).Seconds())
		if wait < 0 {
			wait = 0
		}
		waitSec += wait
		var wasEarly bool
		_ = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM exceptions WHERE order_id=$1 AND type='rider_early' AND status='open')`,
			req.OrderID).Scan(&wasEarly)
		sw := 0
		if wasEarly {
			sw = int(now.Sub(maxTime(x.arrivedAt, storeFaultStart)).Seconds())
			if sw < 0 {
				sw = 0
			}
			if sw > wait {
				sw = wait
			}
			storeWaitSec += sw
		}
		readyAt := o.AllSealedAt
		if readyAt == nil {
			t := now
			readyAt = &t
		}
		if _, err := tx.Exec(ctx,
			`UPDATE rider_arrivals SET handed_at=now(),wait_seconds=$2,store_fault_wait_seconds=$3,ready_at=$4 WHERE id=$1`,
			x.id, wait, sw, readyAt); err != nil {
			writeErr(w, err)
			return
		}
		if wasEarly {
			resp := "rider"
			resolution := fmt.Sprintf("骑手提前到店等待 %d 秒，未超过预计到店时间，等待责任归骑手", wait)
			if sw > 0 {
				resp = "store"
				resolution = fmt.Sprintf("骑手等待 %d 秒，其中超过预计到店时间 %d 秒为门店出杯慢，责任归门店", wait, sw)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE exceptions SET status='resolved',resolved_at=now(),resolved_by=$2,
				  responsibility=$3,resolution=$4
				WHERE order_id=$1 AND type='rider_early' AND status='open'`,
				req.OrderID, ac.ID, resp, resolution); err != nil {
				writeErr(w, err)
				return
			}
		}
	}

	if err := a.logEvent(ctx, tx, req.OrderID, "order_handed", actorOf(ac), map[string]any{
		"rider": riderName, "bags": expBags, "hot": expHot, "cold": expCold, "food": expFood,
		"straw": hasStraw, "utensil": hasUtensil, "wait_seconds": waitSec,
		"store_fault_wait_seconds": storeWaitSec,
	}); err != nil {
		writeErr(w, err)
		return
	}

	// 关闭本单重做任务：新杯交接 → completed，并把重做额外等待计入门店时效；
	// old_taken / canceled 已在裁决时终结，不在此处理。
	var remakeWait int
	jrows, jerr := tx.Query(ctx,
		`SELECT id,created_at FROM remake_jobs WHERE order_id=$1 AND status='remaking' FOR UPDATE`,
		req.OrderID)
	if jerr != nil {
		writeErr(w, jerr)
		return
	}
	type jb struct {
		id        int64
		createdAt time.Time
	}
	var jobs []jb
	for jrows.Next() {
		var x jb
		_ = jrows.Scan(&x.id, &x.createdAt)
		jobs = append(jobs, x)
	}
	jrows.Close()
	for _, j := range jobs {
		rw := int(now.Sub(j.createdAt).Seconds())
		if rw < 0 {
			rw = 0
		}
		remakeWait += rw
		if _, err := tx.Exec(ctx, `
			UPDATE remake_jobs SET status='completed',completed_at=now(),
			  rider_wait_seconds=$2,
			  decision=COALESCE(NULLIF(decision,''),'wait_new')
			WHERE id=$1`, j.id, rw); err != nil {
			writeErr(w, err)
			return
		}
		if err := a.logEvent(ctx, tx, req.OrderID, "remake_completed", actorOf(ac), map[string]any{
			"remake_job_id": j.id, "rider_wait_seconds": rw,
			"seal_codes_scanned": req.SealCodes,
		}); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"passed": true, "order_id": req.OrderID, "status": "handed",
		"bags": expBags, "hot": expHot, "cold": expCold, "food": expFood,
		"rider_wait_seconds": waitSec, "store_fault_wait_seconds": storeWaitSec,
		"remake_wait_seconds": remakeWait,
	})
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
