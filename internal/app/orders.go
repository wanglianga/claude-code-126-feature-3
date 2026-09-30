package app

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------- 请求结构 ----------

type intakeItem struct {
	SKU     string `json:"sku"`
	Qty     int    `json:"qty"`
	Temp    string `json:"temp"`     // hot / ice（含饮品的轻食套餐必填）
	Sugar   string `json:"sugar"`    // 无糖/三分糖/五分糖/七分糖/全糖
	CupSize string `json:"cup_size"` // M / L / XL
	Note    string `json:"note"`     // 杯贴备注
}

type intakeReq struct {
	Platform        string       `json:"platform"`
	PlatformOrderNo string       `json:"platform_order_no"`
	StoreID         *int64       `json:"store_id"`
	StoreCode       string       `json:"store_code"`
	CustomerID      string       `json:"customer_id"`
	CustomerNote    string       `json:"customer_note"`
	PromisedMinutes int          `json:"promised_minutes"`
	RiderETA        *time.Time   `json:"rider_eta"`
	TotalAmount     *float64     `json:"total_amount"`
	Items           []intakeItem `json:"items"`
}

var validSugar = map[string]float64{
	"无糖": 0, "三分糖": 0.3, "五分糖": 0.5, "七分糖": 0.7, "全糖": 1,
}

func sugarFactor(sugar string) float64 {
	if f, ok := validSugar[sugar]; ok {
		return f
	}
	return -1
}

// ---------- 配方/库存辅助 ----------

type recipeRow struct {
	MenuItemID   int64
	IngredientID int64
	IngSKU       string
	Cost         float64
	Qty          float64
	QtyIfIce     float64
	Sizes        *string
}

func sizeMatches(sizes *string, size string) bool {
	if sizes == nil || *sizes == "" {
		return true
	}
	for _, s := range splitCSV(*sizes) {
		if s == size {
			return true
		}
	}
	return false
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == ',' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(c)
		}
	}
	return append(out, cur)
}

type stockNeed struct {
	IngID   int64
	Delta   float64 // 正=增加（补货/回库），负=扣减
	Reason  string
	OrderID *int64
	CupID   *int64
	StaffID *int64
}

// applyStock 在事务内锁行并批量扣减/回补库存，同时写流水。扣减时不允许为负。
func applyStock(ctx context.Context, tx pgx.Tx, storeID int64, needs []stockNeed) error {
	ids := map[int64]bool{}
	for _, n := range needs {
		ids[n.IngID] = true
	}
	idList := make([]int64, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	sort.Slice(idList, func(i, j int) bool { return idList[i] < idList[j] })

	qty := map[int64]float64{}
	for _, id := range idList {
		var q float64
		err := tx.QueryRow(ctx,
			`SELECT qty FROM inventory WHERE store_id=$1 AND ingredient_id=$2 FOR UPDATE`,
			storeID, id).Scan(&q)
		if err != nil {
			return err
		}
		qty[id] = q
	}
	for _, n := range needs {
		qty[n.IngID] += n.Delta
		if qty[n.IngID] < -1e-9 {
			return errf(http.StatusConflict, "stock_shortage",
				"原料不足: ingredient_id=%d 剩余不足", n.IngID)
		}
	}
	for _, id := range idList {
		if _, err := tx.Exec(ctx,
			`UPDATE inventory SET qty=$1 WHERE store_id=$2 AND ingredient_id=$3`,
			qty[id], storeID, id); err != nil {
			return err
		}
	}
	for _, n := range needs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO stock_movements(store_id,ingredient_id,delta,reason,order_id,cup_id,staff_id)
			 VALUES($1,$2,$3,$4,$5,$6,$7)`,
			storeID, n.IngID, n.Delta, n.Reason, n.OrderID, n.CupID, n.StaffID); err != nil {
			return err
		}
	}
	return nil
}

func loadRecipes(ctx context.Context, tx pgx.Tx, itemIDs []int64) (map[int64][]recipeRow, error) {
	out := map[int64][]recipeRow{}
	rows, err := tx.Query(ctx,
		`SELECT r.menu_item_id,r.ingredient_id,i.sku,i.cost_per_unit,r.qty_per_cup,r.qty_if_ice,r.applicable_sizes
		 FROM recipes r JOIN ingredients i ON i.id=r.ingredient_id
		 WHERE r.menu_item_id = ANY($1)`, itemIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rr recipeRow
		if err := rows.Scan(&rr.MenuItemID, &rr.IngredientID, &rr.IngSKU, &rr.Cost,
			&rr.Qty, &rr.QtyIfIce, &rr.Sizes); err != nil {
			return nil, err
		}
		out[rr.MenuItemID] = append(out[rr.MenuItemID], rr)
	}
	return out, rows.Err()
}

// cupRequirement 计算一杯（或一份轻食）对某配方行的实际用量。
func cupRequirement(rr recipeRow, temp, size, sugar string) float64 {
	if !sizeMatches(rr.Sizes, size) {
		return 0
	}
	q := rr.Qty
	if temp == "ice" {
		q += rr.QtyIfIce
	}
	if rr.IngSKU == "sugar" {
		q *= sugarFactor(sugar)
	}
	return q
}

func cupCost(recipes []recipeRow, temp, size, sugar string) float64 {
	var c float64
	for _, rr := range recipes {
		c += cupRequirement(rr, temp, size, sugar) * rr.Cost
	}
	return c
}

// ---------- 接单 ----------

func (a *App) intakeOrder(w http.ResponseWriter, r *http.Request) {
	var req intakeReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	ac := actor(r)
	if ac.Role == "platform" && ac.Platform != "" {
		req.Platform = ac.Platform
	}
	if req.Platform == "" || req.PlatformOrderNo == "" {
		writeErr(w, errf(400, "bad_request", "platform 与 platform_order_no 必填"))
		return
	}
	if len(req.Items) == 0 {
		writeErr(w, errf(400, "bad_request", "items 不能为空"))
		return
	}
	if req.PromisedMinutes == 0 {
		req.PromisedMinutes = 15
	}

	// 幂等：平台可能重推同一单据
	idem := r.Header.Get("Idempotency-Key")
	if idem != "" {
		if cached, err := a.Redis.Get(r.Context(), "idem:intake:"+idem).Result(); err == nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Idempotent-Replay", "true")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(cached))
			return
		}
	}

	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	// 解析门店
	var store Store
	if req.StoreID != nil {
		err = tx.QueryRow(ctx, `SELECT id,code,name,region,capacity_per_5m,max_active_cups,stale_minutes,
			surge_manual,COALESCE(surge_reason,''),surge_until FROM stores WHERE id=$1`, *req.StoreID).Scan(
			&store.ID, &store.Code, &store.Name, &store.Region, &store.CapacityPer5m,
			&store.MaxActiveCups, &store.StaleMinutes, &store.SurgeManual, &store.SurgeReason, &store.SurgeUntil)
	} else {
		err = tx.QueryRow(ctx, `SELECT id,code,name,region,capacity_per_5m,max_active_cups,stale_minutes,
			surge_manual,COALESCE(surge_reason,''),surge_until FROM stores WHERE code=$1`, req.StoreCode).Scan(
			&store.ID, &store.Code, &store.Name, &store.Region, &store.CapacityPer5m,
			&store.MaxActiveCups, &store.StaleMinutes, &store.SurgeManual, &store.SurgeReason, &store.SurgeUntil)
	}
	if err != nil {
		writeErr(w, err)
		return
	}

	// 同门店接单串行化，避免并发超卖与爆单阈值失真
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(91001, $1)`, store.ID); err != nil {
		writeErr(w, err)
		return
	}

	// 爆单保护：手动暂停 或 在制杯量达上限
	if store.SurgeManual && (store.SurgeUntil == nil || store.SurgeUntil.After(time.Now())) {
		reason := store.SurgeReason
		if reason == "" {
			reason = "产能紧张"
		}
		writeErr(w, errf(429, "surge_paused", "门店已开启爆单保护，临时限制接单：%s", reason))
		return
	}
	var activeCups int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM order_cups c JOIN orders o ON o.id=c.order_id
		 WHERE o.store_id=$1 AND c.status IN ('queued','making') AND o.status IN ('received','active')`,
		store.ID).Scan(&activeCups); err != nil {
		writeErr(w, err)
		return
	}
	newCups := 0
	for _, it := range req.Items {
		newCups += it.Qty
	}
	if activeCups+newCups > store.MaxActiveCups {
		writeErr(w, errf(429, "surge_full",
			"门店在制杯量 %d，本单 %d 杯，超过爆单阈值 %d，请稍后再推单", activeCups, newCups, store.MaxActiveCups))
		return
	}

	// 解析商品并校验
	type line struct {
		item  MenuItem
		input intakeItem
	}
	var lines []line
	itemIDs := []int64{}
	for _, it := range req.Items {
		if it.Qty <= 0 {
			writeErr(w, errf(400, "bad_request", "qty 必须大于 0"))
			return
		}
		var mi MenuItem
		if err := tx.QueryRow(ctx,
			`SELECT id,sku,name,category,is_drink,includes_drink,price,make_seconds,on_sale
			 FROM menu_items WHERE sku=$1`, it.SKU).Scan(
			&mi.ID, &mi.SKU, &mi.Name, &mi.Category, &mi.IsDrink, &mi.IncludesDrink,
			&mi.Price, &mi.MakeSeconds, &mi.OnSale); err != nil {
			writeErr(w, errf(400, "unknown_sku", "未知商品: %s", it.SKU))
			return
		}
		if !mi.OnSale {
			writeErr(w, errf(409, "item_off_sale", "%s 已下架", mi.Name))
			return
		}
		var delisted int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM delistings WHERE store_id=$1 AND menu_item_id=$2 AND active=TRUE`,
			store.ID, mi.ID).Scan(&delisted); err != nil {
			writeErr(w, err)
			return
		}
		if delisted > 0 {
			writeErr(w, errf(409, "item_delisted", "%s 在该门店临时下架（缺货）", mi.Name))
			return
		}
		if mi.IsDrink || mi.IncludesDrink {
			if it.Temp != "hot" && it.Temp != "ice" {
				writeErr(w, errf(400, "bad_request", "%s 必须指定 temp=hot/ice", mi.Name))
				return
			}
			if sugarFactor(it.Sugar) < 0 {
				writeErr(w, errf(400, "bad_request", "%s 糖度非法（无糖/三分糖/五分糖/七分糖/全糖）", mi.Name))
				return
			}
			if it.CupSize != "M" && it.CupSize != "L" && it.CupSize != "XL" {
				writeErr(w, errf(400, "bad_request", "%s 杯型必须是 M/L/XL", mi.Name))
				return
			}
		} else {
			if it.Temp == "" {
				it.Temp = "none"
			}
			if it.CupSize == "" {
				it.CupSize = "-"
			}
			if it.Sugar == "" {
				it.Sugar = "无糖"
			}
		}
		lines = append(lines, line{item: mi, input: it})
		itemIDs = append(itemIDs, mi.ID)
	}

	recipes, err := loadRecipes(ctx, tx, itemIDs)
	if err != nil {
		writeErr(w, err)
		return
	}

	// 汇总原料需求（订单一次性扣减，含杯/封签/冰块/糖浆/吸管/餐具）
	needsByIng := map[int64]float64{}
	var total float64
	for _, ln := range lines {
		for k := 0; k < ln.input.Qty; k++ {
			total += ln.item.Price
			for _, rr := range recipes[ln.item.ID] {
				if q := cupRequirement(rr, ln.input.Temp, ln.input.CupSize, ln.input.Sugar); q > 0 {
					needsByIng[rr.IngredientID] += q
				}
			}
		}
	}
	if req.TotalAmount != nil {
		total = *req.TotalAmount
	}

	// 建单
	var orderID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO orders(platform,platform_order_no,store_id,customer_id,customer_note,
			total_amount,promised_minutes,rider_eta,created_by)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		req.Platform, req.PlatformOrderNo, store.ID, req.CustomerID, req.CustomerNote,
		total, req.PromisedMinutes, req.RiderETA, ac.ID).Scan(&orderID); err != nil {
		writeErr(w, err)
		return
	}

	// 扣库存（先锁行后判断，不足自动触发缺货下架）
	needs := make([]stockNeed, 0, len(needsByIng))
	for ingID, q := range needsByIng {
		needs = append(needs, stockNeed{IngID: ingID, Delta: -q, Reason: "intake", OrderID: &orderID, StaffID: &ac.ID})
	}
	if err := applyStock(ctx, tx, store.ID, needs); err != nil {
		var ae *apiError
		if asErr(err, &ae) && ae.Code == "stock_shortage" {
			// 接单事务将整体回滚；缺货下架必须独立提交，否则下次推单仍会超卖
			shortIng := shortIngredient(ctx, tx, store.ID, needsByIng)
			if rberr := tx.Rollback(ctx); rberr != nil {
				writeErr(w, rberr)
				return
			}
			delisted := 0
			if shortIng != "" {
				if derr := delistByIngredientPool(ctx, a.Pool, store.ID, shortIng,
					"接单时原料缺货自动临时下架: "+shortIng, ac.ID); derr == nil {
					delisted = 1
				}
			}
			ae.Details = map[string]any{
				"short_ingredient": shortIng,
				"delisted":         delisted == 1,
				"message":          "相关商品已在该门店临时下架，补货后可恢复",
			}
		}
		writeErr(w, err)
		return
	}

	// 建杯（出杯顺序从 1 开始）
	seq := 0
	type cupInfo struct {
		id     int64
		itemID int64
		temp   string
		size   string
		sugar  string
	}
	var cups []cupInfo
	for _, ln := range lines {
		for k := 0; k < ln.input.Qty; k++ {
			seq++
			var cid int64
			if err := tx.QueryRow(ctx,
				`INSERT INTO order_cups(order_id,menu_item_id,name_snapshot,unit_price,temp,sugar,cup_size,cup_note,seq)
				 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
				orderID, ln.item.ID, ln.item.Name, ln.item.Price,
				ln.input.Temp, ln.input.Sugar, ln.input.CupSize, ln.input.Note, seq).Scan(&cid); err != nil {
				writeErr(w, err)
				return
			}
			cups = append(cups, cupInfo{cid, ln.item.ID, ln.input.Temp, ln.input.CupSize, ln.input.Sugar})
		}
	}

	if err := a.logEvent(ctx, tx, orderID, "order_received", actorOf(ac), map[string]any{
		"platform": req.Platform, "platform_order_no": req.PlatformOrderNo,
		"cups": newCups, "total_amount": total, "customer_note": req.CustomerNote,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}

	ord, err := a.loadOrder(ctx, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if idem != "" {
		raw, _ := json.Marshal(ord)
		_ = a.Redis.Set(r.Context(), "idem:intake:"+idem, raw, 24*time.Hour).Err()
	}
	writeJSON(w, 201, ord)
}

func asErr(err error, target **apiError) bool {
	for err != nil {
		if ae, ok := err.(*apiError); ok {
			*target = ae
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// shortIngredient 返回第一个库存不足的原料 SKU。
func shortIngredient(ctx context.Context, tx pgx.Tx, storeID int64, needs map[int64]float64) string {
	for ingID, need := range needs {
		var have float64
		var sku string
		err := tx.QueryRow(ctx,
			`SELECT i.qty, g.sku FROM inventory i JOIN ingredients g ON g.id=i.ingredient_id
			 WHERE i.store_id=$1 AND i.ingredient_id=$2`, storeID, ingID).Scan(&have, &sku)
		if err == nil && have < need {
			return sku
		}
	}
	return ""
}

// delistByIngredient 把用到该原料且仍在售的商品在该门店临时下架（事务内）。
func delistByIngredient(ctx context.Context, tx pgx.Tx, storeID int64, ingSKU string, reason string, by int64) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO delistings(store_id,menu_item_id,reason,created_by)
		 SELECT $1, m.id, $2, $3 FROM menu_items m
		 JOIN recipes r ON r.menu_item_id=m.id
		 JOIN ingredients i ON i.id=r.ingredient_id
		 WHERE i.sku=$4 AND m.on_sale=TRUE
		 AND NOT EXISTS (SELECT 1 FROM delistings d WHERE d.store_id=$1 AND d.menu_item_id=m.id AND d.active=TRUE)`,
		storeID, reason, by, ingSKU)
	return err
}

// delistByIngredientPool 独立连接上提交缺货下架（用于接单事务回滚后持久化下架记录）。
func delistByIngredientPool(ctx context.Context, pool *pgxpool.Pool, storeID int64, ingSKU, reason string, by int64) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO delistings(store_id,menu_item_id,reason,created_by)
		 SELECT $1, m.id, $2, $3 FROM menu_items m
		 JOIN recipes r ON r.menu_item_id=m.id
		 JOIN ingredients i ON i.id=r.ingredient_id
		 WHERE i.sku=$4 AND m.on_sale=TRUE
		 AND NOT EXISTS (SELECT 1 FROM delistings d WHERE d.store_id=$1 AND d.menu_item_id=m.id AND d.active=TRUE)`,
		storeID, reason, by, ingSKU)
	return err
}

// ---------- 查询 ----------

func scanOrder(row pgx.Row) (*Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.Platform, &o.PlatformOrderNo, &o.StoreID, &o.Status,
		&o.CustomerID, &o.CustomerNote, &o.TotalAmount, &o.PromisedMinutes, &o.OrderedAt,
		&o.RiderETA, &o.AllSealedAt, &o.HandedAt, &o.CanceledAt, &o.CancelReason, &o.MergedGroupID)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

const orderCols = `id,platform,platform_order_no,store_id,status,COALESCE(customer_id,''),COALESCE(customer_note,''),
 total_amount,promised_minutes,ordered_at,rider_eta,all_sealed_at,handed_at,canceled_at,COALESCE(cancel_reason,''),merged_group_id`

func (a *App) loadOrder(ctx context.Context, id int64) (*Order, error) {
	o, err := scanOrder(a.Pool.QueryRow(ctx, "SELECT "+orderCols+" FROM orders WHERE id=$1", id))
	if err != nil {
		return nil, err
	}
	rows, err := a.Pool.Query(ctx,
		`SELECT id,order_id,menu_item_id,COALESCE((SELECT sku FROM menu_items WHERE id=order_cups.menu_item_id),''),
		name_snapshot,unit_price::float8,temp,sugar,cup_size,COALESCE(cup_note,''),seq,
		COALESCE(seal_code,''),status,remake_no,remake_of_cup_id,queued_at,started_at,sealed_at,
		voided_at,COALESCE(void_reason,''),label_printed_at,staff_start_id,staff_seal_id
		FROM order_cups WHERE order_id=$1 ORDER BY seq,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Cup
		if err := rows.Scan(&c.ID, &c.OrderID, &c.MenuItemID, &c.SKU, &c.NameSnapshot,
			&c.UnitPrice, &c.Temp, &c.Sugar, &c.CupSize, &c.CupNote, &c.Seq,
			&c.SealCode, &c.Status, &c.RemakeNo, &c.RemakeOfCupID, &c.QueuedAt,
			&c.StartedAt, &c.SealedAt, &c.VoidedAt, &c.VoidReason, &c.LabelPrintedAt, &c.StaffStartID, &c.StaffSealID); err != nil {
			return nil, err
		}
		o.Cups = append(o.Cups, c)
	}
	brows, err := a.Pool.Query(ctx,
		`SELECT id,order_id,bag_code,kind,cup_id FROM order_bags WHERE order_id=$1 ORDER BY bag_code`, id)
	if err != nil {
		return nil, err
	}
	defer brows.Close()
	for brows.Next() {
		var b Bag
		if err := brows.Scan(&b.ID, &b.OrderID, &b.BagCode, &b.Kind, &b.CupID); err != nil {
			return nil, err
		}
		o.Bags = append(o.Bags, b)
	}
	return o, rows.Err()
}

func (a *App) getOrder(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	o, err := a.loadOrder(r.Context(), toInt64(id))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, o)
}

func (a *App) listEvents(w http.ResponseWriter, r *http.Request) {
	id := toInt64(chi.URLParam(r, "id"))
	rows, err := a.Pool.Query(r.Context(),
		`SELECT id,order_id,event_type,COALESCE(actor_role,''),COALESCE(actor_name,''),detail,created_at
		 FROM order_events WHERE order_id=$1 ORDER BY id`, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	var out []OrderEvent
	for rows.Next() {
		var e OrderEvent
		var raw []byte
		if err := rows.Scan(&e.ID, &e.OrderID, &e.EventType, &e.ActorRole, &e.ActorName, &raw, &e.CreatedAt); err != nil {
			writeErr(w, err)
			return
		}
		_ = json.Unmarshal(raw, &e.Detail)
		out = append(out, e)
	}
	writeJSON(w, 200, out)
}

func (a *App) listStoreOrders(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	status := r.URL.Query().Get("status")
	q := "SELECT " + orderCols + " FROM orders WHERE store_id=$1"
	args := []any{storeID}
	if status != "" {
		q += " AND status=$2"
		args = append(args, status)
	}
	q += " ORDER BY id DESC LIMIT 200"
	rows, err := a.Pool.Query(r.Context(), q, args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, *o)
	}
	writeJSON(w, 200, out)
}

// ---------- 取消 ----------

type cancelReq struct {
	Reason     string `json:"reason"`
	StoreFault bool   `json:"store_fault"`
}

func (a *App) cancelOrder(w http.ResponseWriter, r *http.Request) {
	id := toInt64(chi.URLParam(r, "id"))
	var req cancelReq
	_ = decode(w, r, &req)
	if req.Reason == "" {
		req.Reason = "平台取消订单"
	}
	ac := actor(r)
	ctx := r.Context()

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	o, err := scanOrder(tx.QueryRow(ctx, "SELECT "+orderCols+" FROM orders WHERE id=$1 FOR UPDATE", id))
	if err != nil {
		writeErr(w, err)
		return
	}
	if o.Status == "handed" || o.Status == "canceled" {
		writeErr(w, errf(409, "conflict", "订单状态 %s 不可取消", o.Status))
		return
	}

	if _, err := tx.Exec(ctx,
		`UPDATE orders SET status='canceled',canceled_at=now(),cancel_reason=$2,cancel_store_fault=$3 WHERE id=$1`,
		id, req.Reason, req.StoreFault); err != nil {
		writeErr(w, err)
		return
	}

	// 未制作/制作中的杯：原料回库；已封口的杯：报损
	cupRows, err := tx.Query(ctx,
		`SELECT c.id,c.menu_item_id,c.temp,c.cup_size,c.sugar,c.status FROM order_cups c WHERE c.order_id=$1 FOR UPDATE`, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	type ck struct {
		id                        int64
		itemID                    int64
		temp, size, sugar, status string
	}
	var cups []ck
	for cupRows.Next() {
		var c ck
		if err := cupRows.Scan(&c.id, &c.itemID, &c.temp, &c.size, &c.sugar, &c.status); err != nil {
			cupRows.Close()
			writeErr(w, err)
			return
		}
		cups = append(cups, c)
	}
	cupRows.Close()
	idSet := map[int64]bool{}
	var distinctItemIDs []int64
	for _, c := range cups {
		if !idSet[c.itemID] {
			idSet[c.itemID] = true
			distinctItemIDs = append(distinctItemIDs, c.itemID)
		}
	}
	recipes, err := loadRecipes(ctx, tx, distinctItemIDs)
	if err != nil {
		writeErr(w, err)
		return
	}
	var needs []stockNeed
	var lossCost float64
	sealedCount := 0
	for _, c := range cups {
		if c.status == "voided" || c.status == "loss_hold" {
			// voided=已作废；loss_hold=重做原杯，报损与成本已在发起重做时入账，取消时不重复处理
			continue
		}
		if _, err := tx.Exec(ctx,
			`UPDATE order_cups SET status='voided',voided_at=now(),void_reason='order_canceled' WHERE id=$1`, c.id); err != nil {
			writeErr(w, err)
			return
		}
		if c.status == "sealed" {
			sealedCount++
			lossCost += cupCost(recipes[c.itemID], c.temp, c.size, c.sugar)
			continue
		}
		for _, rr := range recipes[c.itemID] {
			if q := cupRequirement(rr, c.temp, c.size, c.sugar); q > 0 {
				needs = append(needs, stockNeed{IngID: rr.IngredientID, Delta: q, Reason: "cancel_restock", OrderID: &id})
			}
		}
	}
	if err := applyStock(ctx, tx, o.StoreID, needs); err != nil {
		writeErr(w, err)
		return
	}

	// 终止进行中的重做工作流（新杯已由上面的统一循环回库/报损），作废未兑现补偿方案
	if err := a.abortRemakesOnCancel(ctx, tx, id, actorOf(ac)); err != nil {
		writeErr(w, err)
		return
	}

	var excID int64
	bearer := "platform"
	resp := "platform"
	if req.StoreFault {
		bearer, resp = "store", "store"
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO exceptions(order_id,store_id,type,status,responsibility,detail,comp_amount,cost_amount,created_by)
		 VALUES($1,$2,'platform_cancel','resolved',$3,$4,$5,$6,$7) RETURNING id`,
		id, o.StoreID, resp, map[string]any{"reason": req.Reason, "sealed_lost": sealedCount},
		o.TotalAmount, lossCost, ac.ID).Scan(&excID)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 退款
	if _, err := tx.Exec(ctx,
		`INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,note,created_by)
		 VALUES($1,$2,$3,'refund',$4,$5,$6,$7)`,
		o.StoreID, id, excID, -o.TotalAmount, bearer, "平台取消退款", ac.ID); err != nil {
		writeErr(w, err)
		return
	}
	// 已封口饮品报损
	if lossCost > 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO loss_reports(store_id,order_id,exception_id,reason,qty_cups,cost_amount,note,staff_id)
			 VALUES($1,$2,$3,'canceled',$4,$5,$6,$7)`,
			o.StoreID, id, excID, sealedCount, lossCost, "取消时已封口未交接", ac.ID); err != nil {
			writeErr(w, err)
			return
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,note,created_by)
			 VALUES($1,$2,$3,'loss',$4,'store',$5,$6)`,
			o.StoreID, id, excID, -lossCost, "取消已制作饮品报损", ac.ID); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := a.logEvent(ctx, tx, id, "order_canceled", actorOf(ac), map[string]any{
		"reason": req.Reason, "store_fault": req.StoreFault, "refund": o.TotalAmount,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"order_id": id, "status": "canceled", "exception_id": excID,
		"refund_amount": o.TotalAmount, "bearer": bearer, "loss_cost": lossCost,
	})
}
