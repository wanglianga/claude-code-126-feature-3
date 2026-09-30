package app

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---------- 基础数据 ----------

func (a *App) listStores(w http.ResponseWriter, r *http.Request) {
	rows, err := a.Pool.Query(r.Context(),
		`SELECT id,code,name,region,capacity_per_5m,max_active_cups,stale_minutes,
		        surge_manual,COALESCE(surge_reason,''),surge_until FROM stores ORDER BY id`)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	var out []Store
	for rows.Next() {
		var s Store
		if err := rows.Scan(&s.ID, &s.Code, &s.Name, &s.Region, &s.CapacityPer5m,
			&s.MaxActiveCups, &s.StaleMinutes, &s.SurgeManual, &s.SurgeReason, &s.SurgeUntil); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, s)
	}
	writeJSON(w, 200, out)
}

func (a *App) getStore(w http.ResponseWriter, r *http.Request) {
	var s Store
	err := a.Pool.QueryRow(r.Context(),
		`SELECT id,code,name,region,capacity_per_5m,max_active_cups,stale_minutes,
		        surge_manual,COALESCE(surge_reason,''),surge_until
		 FROM stores WHERE id=$1`, toInt64(chi.URLParam(r, "id"))).Scan(
		&s.ID, &s.Code, &s.Name, &s.Region, &s.CapacityPer5m,
		&s.MaxActiveCups, &s.StaleMinutes, &s.SurgeManual, &s.SurgeReason, &s.SurgeUntil)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s)
}

func (a *App) listMenu(w http.ResponseWriter, r *http.Request) {
	rows, err := a.Pool.Query(r.Context(),
		`SELECT id,sku,name,category,is_drink,includes_drink,price::float8,make_seconds,on_sale
		 FROM menu_items ORDER BY id`)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	var out []MenuItem
	for rows.Next() {
		var m MenuItem
		if err := rows.Scan(&m.ID, &m.SKU, &m.Name, &m.Category, &m.IsDrink,
			&m.IncludesDrink, &m.Price, &m.MakeSeconds, &m.OnSale); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, m)
	}
	writeJSON(w, 200, out)
}

type invRow struct {
	IngredientID int64   `json:"ingredient_id"`
	SKU          string  `json:"sku"`
	Name         string  `json:"name"`
	Unit         string  `json:"unit"`
	Qty          float64 `json:"qty"`
	CostPerUnit  float64 `json:"cost_per_unit"`
}

func (a *App) getInventory(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	rows, err := a.Pool.Query(r.Context(), `
		SELECT i.id,g.sku,g.name,g.unit,v.qty,g.cost_per_unit::float8
		FROM inventory v JOIN ingredients g ON g.id=v.ingredient_id JOIN ingredients i ON i.id=g.id
		WHERE v.store_id=$1 ORDER BY g.sku`, storeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	var out []invRow
	for rows.Next() {
		var x invRow
		if err := rows.Scan(&x.IngredientID, &x.SKU, &x.Name, &x.Unit, &x.Qty, &x.CostPerUnit); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, x)
	}
	writeJSON(w, 200, out)
}

func (a *App) listDelistings(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	activeOnly := r.URL.Query().Get("active") != "0"
	q := `SELECT d.id,d.store_id,m.sku,m.name,d.reason,d.active,d.created_at,d.resolved_at
	      FROM delistings d JOIN menu_items m ON m.id=d.menu_item_id WHERE d.store_id=$1`
	if activeOnly {
		q += " AND d.active=TRUE"
	}
	q += " ORDER BY d.id DESC"
	rows, err := a.Pool.Query(r.Context(), q, storeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type dl struct {
		ID         int64      `json:"id"`
		StoreID    int64      `json:"store_id"`
		SKU        string     `json:"sku"`
		Name       string     `json:"name"`
		Reason     string     `json:"reason"`
		Active     bool       `json:"active"`
		CreatedAt  time.Time  `json:"created_at"`
		ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	}
	var out []dl
	for rows.Next() {
		var x dl
		if err := rows.Scan(&x.ID, &x.StoreID, &x.SKU, &x.Name, &x.Reason, &x.Active, &x.CreatedAt, &x.ResolvedAt); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, x)
	}
	writeJSON(w, 200, out)
}

type restockReq struct {
	StoreID          int64            `json:"store_id"`
	Items            []restockItemReq `json:"items"`
	ResolveDelisting bool             `json:"resolve_delistings"`
}

type restockItemReq struct {
	IngredientSKU string  `json:"ingredient_sku"`
	Qty           float64 `json:"qty"`
}

// restock 原料补货；可自动恢复因缺货下架的商品（原料重新齐备后上架）。
func (a *App) restock(w http.ResponseWriter, r *http.Request) {
	var req restockReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.StoreID == 0 || len(req.Items) == 0 {
		writeErr(w, errf(400, "bad_request", "store_id 与 items 必填"))
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

	for _, it := range req.Items {
		if it.Qty <= 0 {
			writeErr(w, errf(400, "bad_request", "补货数量必须为正: %s", it.IngredientSKU))
			return
		}
		var ingID int64
		if err := tx.QueryRow(ctx, `SELECT id FROM ingredients WHERE sku=$1`, it.IngredientSKU).Scan(&ingID); err != nil {
			writeErr(w, errf(400, "unknown_ingredient", "未知原料: %s", it.IngredientSKU))
			return
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO inventory(store_id,ingredient_id,qty) VALUES($1,$2,$3)
			 ON CONFLICT (store_id,ingredient_id) DO UPDATE SET qty=inventory.qty+$3`,
			req.StoreID, ingID, it.Qty); err != nil {
			writeErr(w, err)
			return
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO stock_movements(store_id,ingredient_id,delta,reason,staff_id)
			 VALUES($1,$2,$3,'restock',$4)`, req.StoreID, ingID, it.Qty, ac.ID); err != nil {
			writeErr(w, err)
			return
		}
	}

	resolved := []int64{}
	if req.ResolveDelisting {
		// 找到当前临时下架商品，若其配方每种原料现有库存都 >= 一份用量，则恢复上架
		rows, err := tx.Query(ctx, `
			SELECT d.id, d.menu_item_id FROM delistings d
			WHERE d.store_id=$1 AND d.active=TRUE`, req.StoreID)
		if err != nil {
			writeErr(w, err)
			return
		}
		type pair struct{ dlID, itemID int64 }
		var pairs []pair
		for rows.Next() {
			var p pair
			_ = rows.Scan(&p.dlID, &p.itemID)
			pairs = append(pairs, p)
		}
		rows.Close()
		for _, p := range pairs {
			var missing int
			err := tx.QueryRow(ctx, `
				SELECT count(*) FROM recipes rp
				JOIN ingredients g ON g.id=rp.ingredient_id
				LEFT JOIN inventory v ON v.ingredient_id=g.id AND v.store_id=$1
				WHERE rp.menu_item_id=$2
				  AND COALESCE(rp.applicable_sizes,'')=''
				  AND COALESCE(v.qty,0) < rp.qty_per_cup + GREATEST(rp.qty_if_ice,0)`,
				req.StoreID, p.itemID).Scan(&missing)
			if err != nil {
				writeErr(w, err)
				return
			}
			// 杯耗材是按杯型分行的（applicable_sizes 非空），缺货多为内容物；保守起见再检查全部原料
			var missingAll int
			_ = tx.QueryRow(ctx, `
				SELECT count(*) FROM (
				  SELECT rp.ingredient_id, MAX(rp.qty_per_cup + GREATEST(rp.qty_if_ice,0)) need
				  FROM recipes rp WHERE rp.menu_item_id=$2 GROUP BY rp.ingredient_id
				) need_t
				JOIN ingredients g ON g.id=need_t.ingredient_id
				LEFT JOIN inventory v ON v.ingredient_id=g.id AND v.store_id=$1
				WHERE COALESCE(v.qty,0) < need_t.need`, req.StoreID, p.itemID).Scan(&missingAll)
			if missing == 0 && missingAll == 0 {
				if _, err := tx.Exec(ctx,
					`UPDATE delistings SET active=FALSE,resolved_at=now() WHERE id=$1`, p.dlID); err != nil {
					writeErr(w, err)
					return
				}
				resolved = append(resolved, p.itemID)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"restocked": req.Items, "relisted_menu_item_ids": resolved})
}

// ---------- 手动下架 / 恢复 ----------

type delistReq struct {
	StoreID int64  `json:"store_id"`
	Reason  string `json:"reason"`
}

func (a *App) manualDelist(w http.ResponseWriter, r *http.Request) {
	sku := chi.URLParam(r, "sku")
	var req delistReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.StoreID == 0 {
		writeErr(w, errf(400, "bad_request", "store_id 必填"))
		return
	}
	if req.Reason == "" {
		req.Reason = "门店手动临时下架"
	}
	ac := actor(r)
	var itemID int64
	if err := a.Pool.QueryRow(r.Context(), `SELECT id FROM menu_items WHERE sku=$1`, sku).Scan(&itemID); err != nil {
		writeErr(w, errf(404, "unknown_sku", "未知商品: %s", sku))
		return
	}
	var dlID int64
	err := a.Pool.QueryRow(r.Context(), `
		INSERT INTO delistings(store_id,menu_item_id,reason,created_by)
		VALUES($1,$2,$3,$4) RETURNING id`, req.StoreID, itemID, req.Reason, ac.ID).Scan(&dlID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"delisting_id": dlID, "sku": sku, "active": true})
}

// ---------- 爆单保护 ----------

type surgeReq struct {
	Paused  bool   `json:"paused"`
	Reason  string `json:"reason"`
	Minutes int    `json:"minutes"` // 临时限制时长，0=直到手动恢复
}

func (a *App) setSurge(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	var req surgeReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	var until *time.Time
	if req.Paused && req.Minutes > 0 {
		t := time.Now().Add(time.Duration(req.Minutes) * time.Minute)
		until = &t
	}
	if _, err := a.Pool.Exec(r.Context(),
		`UPDATE stores SET surge_manual=$2,surge_reason=$3,surge_until=$4 WHERE id=$1`,
		storeID, req.Paused, req.Reason, until); err != nil {
		writeErr(w, err)
		return
	}
	// 门店看板缓存失效
	_ = a.Redis.Del(r.Context(), "board:"+strconv.FormatInt(storeID, 10)).Err()
	s, err := a.getStoreEntity(r.Context(), storeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s)
}

func (a *App) getStoreEntity(ctx context.Context, id int64) (*Store, error) {
	var s Store
	err := a.Pool.QueryRow(ctx,
		`SELECT id,code,name,region,capacity_per_5m,max_active_cups,stale_minutes,
		        surge_manual,COALESCE(surge_reason,''),surge_until
		 FROM stores WHERE id=$1`, id).Scan(
		&s.ID, &s.Code, &s.Name, &s.Region, &s.CapacityPer5m,
		&s.MaxActiveCups, &s.StaleMinutes, &s.SurgeManual, &s.SurgeReason, &s.SurgeUntil)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ---------- 多平台订单合并 ----------

type mergeReq struct {
	StoreID  int64   `json:"store_id"`
	OrderIDs []int64 `json:"order_ids"`
	Note     string  `json:"note"`
}

func (a *App) mergeOrders(w http.ResponseWriter, r *http.Request) {
	var req mergeReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if len(req.OrderIDs) < 2 {
		writeErr(w, errf(400, "bad_request", "至少选择 2 个订单合并"))
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

	if req.StoreID == 0 {
		if err := tx.QueryRow(ctx, `SELECT store_id FROM orders WHERE id=$1`, req.OrderIDs[0]).Scan(&req.StoreID); err != nil {
			writeErr(w, err)
			return
		}
	}
	var platforms []string
	for _, oid := range req.OrderIDs {
		var st int64
		var status, platform string
		if err := tx.QueryRow(ctx, `SELECT store_id,status,platform FROM orders WHERE id=$1 FOR UPDATE`, oid).
			Scan(&st, &status, &platform); err != nil {
			writeErr(w, errf(404, "order_not_found", "订单 %d 不存在", oid))
			return
		}
		if st != req.StoreID {
			writeErr(w, errf(409, "cross_store", "订单 %d 不属于同一门店，不能合并", oid))
			return
		}
		if status == "handed" || status == "canceled" {
			writeErr(w, errf(409, "bad_status", "订单 %d 状态 %s，不能合并", oid, status))
			return
		}
		platforms = append(platforms, platform)
	}

	var gid int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO merged_groups(store_id,created_by,note) VALUES($1,$2,$3) RETURNING id`,
		req.StoreID, ac.ID, req.Note).Scan(&gid); err != nil {
		writeErr(w, err)
		return
	}
	for _, oid := range req.OrderIDs {
		if _, err := tx.Exec(ctx, `UPDATE orders SET merged_group_id=$2 WHERE id=$1`, oid, gid); err != nil {
			writeErr(w, err)
			return
		}
		// 合并后同一物理骑手取走，订单级时间线留痕
		if err := a.logEvent(ctx, tx, oid, "orders_merged", actorOf(ac), map[string]any{
			"group_id": gid, "order_ids": req.OrderIDs, "note": req.Note,
		}); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"group_id": gid, "order_ids": req.OrderIDs, "platforms": platforms,
	})
}

func (a *App) getMergedGroup(w http.ResponseWriter, r *http.Request) {
	gid := toInt64(chi.URLParam(r, "id"))
	var g struct {
		ID        int64     `json:"id"`
		StoreID   int64     `json:"store_id"`
		Note      string    `json:"note"`
		CreatedAt time.Time `json:"created_at"`
	}
	err := a.Pool.QueryRow(r.Context(),
		`SELECT id,store_id,COALESCE(note,''),created_at FROM merged_groups WHERE id=$1`, gid).
		Scan(&g.ID, &g.StoreID, &g.Note, &g.CreatedAt)
	if err != nil {
		writeErr(w, err)
		return
	}
	rows, err := a.Pool.Query(r.Context(),
		"SELECT "+orderCols+" FROM orders WHERE merged_group_id=$1 ORDER BY id", gid)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	var orders []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			writeErr(w, err)
			return
		}
		orders = append(orders, *o)
	}
	writeJSON(w, 200, map[string]any{"group": g, "orders": orders})
}

// ---------- 杯贴 / 袋贴 ----------

// cupLabels 返回杯贴打印数据：订单号、平台、出杯序号、商品名、冰热/糖度/杯型、顾客备注、封签（封口后）。
func (a *App) cupLabels(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	rows, err := a.Pool.Query(r.Context(), `
		SELECT c.id,c.seq,m.sku,c.name_snapshot,c.temp,c.sugar,c.cup_size,
		       COALESCE(c.cup_note,''),c.status,COALESCE(c.seal_code,''),c.remake_no,
		       o.platform,o.platform_order_no,COALESCE(o.customer_note,'')
		FROM order_cups c JOIN orders o ON o.id=c.order_id JOIN menu_items m ON m.id=c.menu_item_id
		WHERE c.order_id=$1 ORDER BY c.seq,c.id`, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type label struct {
		CupID        int64  `json:"cup_id"`
		Seq          int    `json:"seq"`
		SKU          string `json:"sku"`
		Name         string `json:"name"`
		Temp         string `json:"temp"`
		TempLabel    string `json:"temp_label"`
		Sugar        string `json:"sugar"`
		CupSize      string `json:"cup_size"`
		CupNote      string `json:"cup_note"`
		Status       string `json:"status"`
		SealCode     string `json:"seal_code"`
		RemakeNo     int    `json:"remake_no"`
		Platform     string `json:"platform"`
		PlatformNo   string `json:"platform_order_no"`
		CustomerNote string `json:"customer_note"`
	}
	var out []label
	tempCN := map[string]string{"hot": "热", "ice": "冰", "none": "常温"}
	for rows.Next() {
		var l label
		if err := rows.Scan(&l.CupID, &l.Seq, &l.SKU, &l.Name, &l.Temp, &l.Sugar, &l.CupSize,
			&l.CupNote, &l.Status, &l.SealCode, &l.RemakeNo, &l.Platform, &l.PlatformNo, &l.CustomerNote); err != nil {
			writeErr(w, err)
			return
		}
		l.TempLabel = tempCN[l.Temp]
		out = append(out, l)
	}
	writeJSON(w, 200, map[string]any{"order_id": orderID, "labels": out})
}

func (a *App) bagLabels(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	rows, err := a.Pool.Query(r.Context(), `
		SELECT b.bag_code,b.kind,count(*),
		       (SELECT platform_order_no FROM orders WHERE id=$1),
		       (SELECT platform FROM orders WHERE id=$1)
		FROM order_bags b WHERE b.order_id=$1
		GROUP BY b.bag_code,b.kind ORDER BY b.bag_code`, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type bl struct {
		BagCode    string `json:"bag_code"`
		Kind       string `json:"kind"`
		KindLabel  string `json:"kind_label"`
		CupCount   int    `json:"cup_count"`
		Platform   string `json:"platform"`
		PlatformNo string `json:"platform_order_no"`
	}
	var out []bl
	kindCN := map[string]string{"hot": "热饮袋", "cold": "冷饮袋", "food": "餐食袋"}
	for rows.Next() {
		var x bl
		if err := rows.Scan(&x.BagCode, &x.Kind, &x.CupCount, &x.PlatformNo, &x.Platform); err != nil {
			writeErr(w, err)
			return
		}
		x.KindLabel = kindCN[x.Kind]
		out = append(out, x)
	}
	writeJSON(w, 200, map[string]any{"order_id": orderID, "labels": out})
}

// ---------- 报损记录 ----------

func (a *App) listLossReports(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	rows, err := a.Pool.Query(r.Context(), `
		SELECT id,COALESCE(order_id,0),COALESCE(cup_id,0),COALESCE(exception_id,0),
		       reason,qty_cups,cost_amount::float8,COALESCE(note,''),created_at
		FROM loss_reports WHERE store_id=$1 ORDER BY id DESC LIMIT 200`, storeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type lr struct {
		ID          int64     `json:"id"`
		OrderID     int64     `json:"order_id"`
		CupID       int64     `json:"cup_id"`
		ExceptionID int64     `json:"exception_id"`
		Reason      string    `json:"reason"`
		QtyCups     int       `json:"qty_cups"`
		CostAmount  float64   `json:"cost_amount"`
		Note        string    `json:"note"`
		CreatedAt   time.Time `json:"created_at"`
	}
	var out []lr
	for rows.Next() {
		var x lr
		if err := rows.Scan(&x.ID, &x.OrderID, &x.CupID, &x.ExceptionID, &x.Reason,
			&x.QtyCups, &x.CostAmount, &x.Note, &x.CreatedAt); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, x)
	}
	writeJSON(w, 200, out)
}

// ---------- 门店作业看板 ----------

func (a *App) storeBoard(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	ctx := r.Context()
	type board struct {
		StoreID          int64 `json:"store_id"`
		SurgeManual      bool  `json:"surge_manual"`
		ActiveCups       int   `json:"active_cups"`
		MaxActiveCups    int   `json:"max_active_cups"`
		QueuedCups       int   `json:"queued_cups"`
		MakingCups       int   `json:"making_cups"`
		ReadyOrders      int   `json:"ready_orders"`
		MakingOrders     int   `json:"making_orders"`
		WaitingRiders    int   `json:"waiting_riders"`
		StaleInPickup    int   `json:"stale_in_pickup"`
		RemakingJobs     int   `json:"remaking_jobs"`
		OpenExceptions   int   `json:"open_exceptions"`
		ActiveDelistings int   `json:"active_delistings"`
	}
	var b board
	if err := a.Pool.QueryRow(ctx, `
		SELECT s.id,s.surge_manual,s.max_active_cups,
		  (SELECT count(*) FROM order_cups c JOIN orders o ON o.id=c.order_id
		     WHERE o.store_id=s.id AND c.status IN ('queued','making') AND o.status IN ('received','active')),
		  (SELECT count(*) FROM order_cups c JOIN orders o ON o.id=c.order_id
		     WHERE o.store_id=s.id AND c.status='queued' AND o.status IN ('received','active')),
		  (SELECT count(*) FROM order_cups c JOIN orders o ON o.id=c.order_id
		     WHERE o.store_id=s.id AND c.status='making' AND o.status IN ('received','active')),
		  (SELECT count(*) FROM orders WHERE store_id=s.id AND status='active'),
		  (SELECT count(*) FROM orders WHERE store_id=s.id AND status='received'),
		  (SELECT count(*) FROM rider_arrivals ra JOIN orders o ON o.id=ra.order_id
		     WHERE ra.store_id=s.id AND ra.handed_at IS NULL AND o.status<>'canceled'),
		  (SELECT count(*) FROM orders WHERE store_id=s.id AND status='active'
		     AND all_sealed_at IS NOT NULL AND all_sealed_at < now() - make_interval(mins => s.stale_minutes)),
		  (SELECT count(*) FROM remake_jobs WHERE store_id=s.id AND status='remaking'),
		  (SELECT count(*) FROM exceptions WHERE store_id=s.id AND status='open'),
		  (SELECT count(*) FROM delistings WHERE store_id=s.id AND active=TRUE)
		FROM stores s WHERE s.id=$1`, storeID).Scan(
		&b.StoreID, &b.SurgeManual, &b.MaxActiveCups,
		&b.ActiveCups, &b.QueuedCups, &b.MakingCups, &b.ReadyOrders, &b.MakingOrders,
		&b.WaitingRiders, &b.StaleInPickup, &b.RemakingJobs, &b.OpenExceptions, &b.ActiveDelistings); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, b)
}
