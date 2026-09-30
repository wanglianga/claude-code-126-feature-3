package app

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

func toInt64(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// ---------- 出杯队列 ----------

// makeQueue 返回门店出杯队列（骑手预计到店早的订单优先，制作中的置顶）。
func (a *App) makeQueue(w http.ResponseWriter, r *http.Request) {
	storeID := toInt64(chi.URLParam(r, "id"))
	rows, err := a.Pool.Query(r.Context(), `
		SELECT c.id,c.order_id,m.sku,c.name_snapshot,c.temp,c.sugar,c.cup_size,
		       COALESCE(c.cup_note,''),c.seq,c.status,c.remake_no,
		       COALESCE(o.customer_note,''),o.rider_eta
		FROM order_cups c
		JOIN orders o ON o.id=c.order_id
		JOIN menu_items m ON m.id=c.menu_item_id
		WHERE o.store_id=$1 AND c.status IN ('queued','making') AND o.status IN ('received','active')
		ORDER BY CASE c.status WHEN 'making' THEN 0 ELSE 1 END,
		         COALESCE(o.rider_eta, o.ordered_at + interval '30 min'), o.ordered_at, c.seq, c.id
		LIMIT 100`, storeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	type qItem struct {
		CupID        int64      `json:"cup_id"`
		OrderID      int64      `json:"order_id"`
		SKU          string     `json:"sku"`
		Name         string     `json:"name"`
		Temp         string     `json:"temp"`
		Sugar        string     `json:"sugar"`
		CupSize      string     `json:"cup_size"`
		CupNote      string     `json:"cup_note"`
		CustomerNote string     `json:"customer_note"`
		Seq          int        `json:"seq"`
		Status       string     `json:"status"`
		RemakeNo     int        `json:"remake_no"`
		RiderETA     *time.Time `json:"rider_eta,omitempty"`
	}
	var out []qItem
	for rows.Next() {
		var q qItem
		if err := rows.Scan(&q.CupID, &q.OrderID, &q.SKU, &q.Name,
			&q.Temp, &q.Sugar, &q.CupSize, &q.CupNote, &q.Seq, &q.Status,
			&q.RemakeNo, &q.CustomerNote, &q.RiderETA); err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, q)
	}
	writeJSON(w, 200, out)
}

type nextCupReq struct {
	StoreID int64 `json:"store_id"`
}

// startNextCup 店员叫号取下一杯开始制作。
func (a *App) startNextCup(w http.ResponseWriter, r *http.Request) {
	var req nextCupReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.StoreID == 0 {
		writeErr(w, errf(400, "bad_request", "store_id 必填"))
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

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(92001, $1)`, req.StoreID); err != nil {
		writeErr(w, err)
		return
	}
	var cupID int64
	err = tx.QueryRow(ctx, `
		SELECT c.id FROM order_cups c JOIN orders o ON o.id=c.order_id
		WHERE o.store_id=$1 AND c.status='queued' AND o.status IN ('received','active')
		ORDER BY COALESCE(o.rider_eta, o.ordered_at + interval '30 min'), o.ordered_at, c.seq, c.id
		LIMIT 1 FOR UPDATE SKIP LOCKED`, req.StoreID).Scan(&cupID)
	if err != nil {
		writeErr(w, errf(409, "queue_empty", "当前没有待制作的杯"))
		return
	}
	if err := startCupTx(ctx, tx, cupID, ac.ID); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	c, err := a.loadCup(ctx, cupID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, c)
}

func (a *App) loadCup(ctx context.Context, id int64) (*Cup, error) {
	var c Cup
	err := a.Pool.QueryRow(ctx,
		`SELECT id,order_id,menu_item_id,COALESCE((SELECT sku FROM menu_items WHERE id=order_cups.menu_item_id),''),
		name_snapshot,unit_price::float8,temp,sugar,cup_size,COALESCE(cup_note,''),seq,
		COALESCE(seal_code,''),status,remake_no,remake_of_cup_id,queued_at,started_at,sealed_at,
		voided_at,COALESCE(void_reason,''),staff_start_id,staff_seal_id
		FROM order_cups WHERE id=$1`, id).Scan(
		&c.ID, &c.OrderID, &c.MenuItemID, &c.SKU, &c.NameSnapshot,
		&c.UnitPrice, &c.Temp, &c.Sugar, &c.CupSize, &c.CupNote, &c.Seq,
		&c.SealCode, &c.Status, &c.RemakeNo, &c.RemakeOfCupID, &c.QueuedAt,
		&c.StartedAt, &c.SealedAt, &c.VoidedAt, &c.VoidReason, &c.StaffStartID, &c.StaffSealID)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func startCupTx(ctx context.Context, tx pgx.Tx, cupID, staffID int64) error {
	ct, err := tx.Exec(ctx,
		`UPDATE order_cups SET status='making',started_at=now(),staff_start_id=$2 WHERE id=$1 AND status='queued'`,
		cupID, staffID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return errConflict
	}
	return nil
}

func (a *App) startCup(w http.ResponseWriter, r *http.Request) {
	cupID := toInt64(chi.URLParam(r, "id"))
	ac := actor(r)
	ctx := r.Context()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var storeID int64
	if err := tx.QueryRow(ctx, `SELECT o.store_id FROM order_cups c JOIN orders o ON o.id=c.order_id WHERE c.id=$1`, cupID).
		Scan(&storeID); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(92001, $1)`, storeID); err != nil {
		writeErr(w, err)
		return
	}
	if err := startCupTx(ctx, tx, cupID, ac.ID); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	c, _ := a.loadCup(ctx, cupID)
	writeJSON(w, 200, c)
}

// ---------- 封签 ----------

func genSealCode(ctx context.Context, tx pgx.Tx) (string, error) {
	for i := 0; i < 5; i++ {
		b := make([]byte, 4)
		if _, err := crand.Read(b); err != nil {
			return "", err
		}
		code := "FC" + hex.EncodeToString(b)
		var exists int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM order_cups WHERE seal_code=$1`, code).Scan(&exists); err != nil {
			return "", err
		}
		if exists == 0 {
			return code, nil
		}
	}
	return "", errf(500, "seal_code", "封签码生成冲突，请重试")
}

// ensureBag 确保订单存在该温层的物理袋，并在首次创建时扣减对应袋子耗材。
func ensureBag(ctx context.Context, tx pgx.Tx, storeID, orderID, cupID int64, kind string, staffID int64) (string, error) {
	var code string
	err := tx.QueryRow(ctx, `SELECT bag_code FROM order_bags WHERE order_id=$1 AND kind=$2 LIMIT 1`,
		orderID, kind).Scan(&code)
	if err == nil {
		// 杯-袋映射
		if _, err := tx.Exec(ctx,
			`INSERT INTO order_bags(store_id,order_id,bag_code,kind,cup_id) VALUES($1,$2,$3,$4,$5)
			 ON CONFLICT DO NOTHING`, storeID, orderID, code, kind, cupID); err != nil {
			return "", err
		}
		return code, nil
	}
	if err != pgx.ErrNoRows {
		return "", err
	}
	var groupID *int64
	if err := tx.QueryRow(ctx, `SELECT merged_group_id FROM orders WHERE id=$1`, orderID).Scan(&groupID); err != nil {
		return "", err
	}
	code = bagCodeOf(orderID, kind)
	if _, err := tx.Exec(ctx,
		`INSERT INTO order_bags(store_id,order_id,group_id,bag_code,kind,cup_id) VALUES($1,$2,$3,$4,$5,$6)`,
		storeID, orderID, groupID, code, kind, cupID); err != nil {
		return "", err
	}
	ingSKU := map[string]string{"hot": "baghot", "cold": "bagcold", "food": "bagfood"}[kind]
	var ingID int64
	if err := tx.QueryRow(ctx, `SELECT id FROM ingredients WHERE sku=$1`, ingSKU).Scan(&ingID); err != nil {
		return "", err
	}
	if err := applyStock(ctx, tx, storeID, []stockNeed{
		{IngID: ingID, Delta: -1, Reason: "intake", OrderID: &orderID, CupID: &cupID, StaffID: &staffID},
	}); err != nil {
		return "", err
	}
	return code, nil
}

func bagCodeOf(orderID int64, kind string) string {
	suffix := map[string]string{"hot": "H", "cold": "C", "food": "F"}[kind]
	return bagCodeParts(orderID, suffix)
}

func bagCodeParts(orderID int64, suffix string) string {
	return "O" + itoa(orderID) + "-" + suffix
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

type sealResp struct {
	Cup            Cup    `json:"cup"`
	BagCode        string `json:"bag_code"`
	BagKind        string `json:"bag_kind"`
	OrderAllSealed bool   `json:"order_all_sealed"`
	SealedCups     int    `json:"sealed_cups"`
	TotalCups      int    `json:"total_cups"`
}

func (a *App) sealCup(w http.ResponseWriter, r *http.Request) {
	cupID := toInt64(chi.URLParam(r, "id"))
	ac := actor(r)
	ctx := r.Context()

	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var orderID, storeID, menuItemID int64
	var temp, name string
	err = tx.QueryRow(ctx,
		`SELECT c.order_id,o.store_id,c.menu_item_id,c.temp,m.name
		 FROM order_cups c JOIN orders o ON o.id=c.order_id JOIN menu_items m ON m.id=c.menu_item_id
		 WHERE c.id=$1 FOR UPDATE`, cupID).Scan(&orderID, &storeID, &menuItemID, &temp, &name)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 订单级串行锁：封签是重做裁决（wait_new/take_old）的分叉状态，必须与裁决/交接/重做互斥
	if err := lockOrderTx(ctx, tx, orderID); err != nil {
		writeErr(w, err)
		return
	}
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM order_cups WHERE id=$1`, cupID).Scan(&status); err != nil {
		writeErr(w, err)
		return
	}
	if status != "making" {
		writeErr(w, errf(409, "conflict", "杯状态为 %s，需先开始制作才能扫描封签", status))
		return
	}

	code, err := genSealCode(ctx, tx)
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, err := tx.Exec(ctx,
		`UPDATE order_cups SET status='sealed',sealed_at=now(),seal_code=$2,staff_seal_id=$3 WHERE id=$1`,
		cupID, code, ac.ID); err != nil {
		writeErr(w, err)
		return
	}

	// 分袋：热饮/冷饮/轻食分袋
	var isDrink, includesDrink bool
	if err := tx.QueryRow(ctx, `SELECT is_drink,includes_drink FROM menu_items WHERE id=$1`, menuItemID).
		Scan(&isDrink, &includesDrink); err != nil {
		writeErr(w, err)
		return
	}
	kind := "food"
	if isDrink || includesDrink {
		if temp == "ice" {
			kind = "cold"
		} else {
			kind = "hot"
		}
	}
	bag, err := ensureBag(ctx, tx, storeID, orderID, cupID, kind, ac.ID)
	if err != nil {
		writeErr(w, err)
		return
	}

	if err := a.logEvent(ctx, tx, orderID, "cup_sealed", actorOf(ac), map[string]any{
		"cup_id": cupID, "seal_code": code, "bag": bag, "bag_kind": kind,
	}); err != nil {
		writeErr(w, err)
		return
	}

	// 全部有效杯已封口 → 进入待取区
	var sealedN, totalN int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status='sealed'),
		        count(*) FILTER (WHERE status<>'voided')
		 FROM order_cups WHERE order_id=$1`, orderID).Scan(&sealedN, &totalN); err != nil {
		writeErr(w, err)
		return
	}
	allSealed := sealedN == totalN && totalN > 0
	if allSealed {
		if _, err := tx.Exec(ctx,
			`UPDATE orders SET all_sealed_at=now(),status='active' WHERE id=$1 AND status='received'`, orderID); err != nil {
			writeErr(w, err)
			return
		}
		if err := a.logEvent(ctx, tx, orderID, "order_ready", actorOf(ac), map[string]any{
			"bags": bagSummary(ctx, tx, orderID),
		}); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	cup, _ := a.loadCup(ctx, cupID)
	writeJSON(w, 200, sealResp{Cup: *cup, BagCode: bag, BagKind: kind,
		OrderAllSealed: allSealed, SealedCups: sealedN, TotalCups: totalN})
}

func bagSummary(ctx context.Context, tx pgx.Tx, orderID int64) map[string]any {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT bag_code,kind FROM order_bags WHERE order_id=$1`, orderID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	bags := []map[string]string{}
	hot, cold, food := 0, 0, 0
	for rows.Next() {
		var code, kind string
		_ = rows.Scan(&code, &kind)
		bags = append(bags, map[string]string{"bag_code": code, "kind": kind})
		switch kind {
		case "hot":
			hot++
		case "cold":
			cold++
		case "food":
			food++
		}
	}
	return map[string]any{"bags": bags, "hot": hot, "cold": cold, "food": food, "total": hot + cold + food}
}

// ---------- 重做 / 报损 ----------
// 重做的完整状态机（因子评估、自动重做、报损/重做/补偿分别归因）见 remake.go；
// 并发裁决（骑手在重做中到店）见 remake_decision.go。

func openException(ctx context.Context, tx pgx.Tx, orderID int64, typ string) (int64, bool, error) {
	var id int64
	err := tx.QueryRow(ctx,
		`SELECT id FROM exceptions WHERE order_id=$1 AND type=$2 AND status='open' ORDER BY id LIMIT 1`,
		orderID, typ).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if err != pgx.ErrNoRows {
		return 0, false, err
	}
	return 0, false, nil
}

// ---------- 轻食缺货 ----------

type itemOOSReq struct {
	CupID *int64 `json:"cup_id"`
	SKU   string `json:"sku"`
	Note  string `json:"note"`
}

// itemOOS 接单后发现轻食（或任意商品）缺货：临时下架、作废旧杯回库、退该商品款项。
func (a *App) itemOOS(w http.ResponseWriter, r *http.Request) {
	orderID := toInt64(chi.URLParam(r, "id"))
	var req itemOOSReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
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
	var status string
	if err := tx.QueryRow(ctx, `SELECT store_id,status FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&storeID, &status); err != nil {
		writeErr(w, err)
		return
	}
	if status == "canceled" || status == "handed" {
		writeErr(w, errf(409, "conflict", "订单已 %s", status))
		return
	}

	var cupID, menuItemID int64
	var temp, size, sugar string
	var price float64
	if req.CupID != nil {
		err = tx.QueryRow(ctx, `
			SELECT id,menu_item_id,temp,cup_size,sugar,unit_price FROM order_cups WHERE id=$1 AND order_id=$2 FOR UPDATE`,
			*req.CupID, orderID).Scan(&cupID, &menuItemID, &temp, &size, &sugar, &price)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT c.id,c.menu_item_id,c.temp,c.cup_size,c.sugar,c.unit_price
			FROM order_cups c JOIN menu_items m ON m.id=c.menu_item_id
			WHERE c.order_id=$1 AND c.status<>'voided' AND m.sku=$2
			ORDER BY c.id LIMIT 1 FOR UPDATE`, orderID, req.SKU).
			Scan(&cupID, &menuItemID, &temp, &size, &sugar, &price)
	}
	if err != nil {
		writeErr(w, errf(400, "no_cup", "找不到对应杯/商品: %v", err))
		return
	}

	// 临时下架
	if err := delistByIngredientOrItem(ctx, tx, storeID, menuItemID, "门店报缺："+req.Note, ac.ID); err != nil {
		writeErr(w, err)
		return
	}

	// 旧杯作废 + 未消耗原料回库（若已封口则报损）
	var cupStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM order_cups WHERE id=$1`, cupID).Scan(&cupStatus); err != nil {
		writeErr(w, err)
		return
	}
	recipes, _ := loadRecipes(ctx, tx, []int64{menuItemID})
	cost := cupCost(recipes[menuItemID], temp, size, sugar)
	if _, err := tx.Exec(ctx,
		`UPDATE order_cups SET status='voided',voided_at=now(),void_reason='food_oos' WHERE id=$1`, cupID); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM order_bags WHERE cup_id=$1`, cupID); err != nil {
		writeErr(w, err)
		return
	}
	if cupStatus != "sealed" {
		var needs []stockNeed
		for _, rr := range recipes[menuItemID] {
			if q := cupRequirement(rr, temp, size, sugar); q > 0 {
				needs = append(needs, stockNeed{IngID: rr.IngredientID, Delta: q, Reason: "cancel_restock", OrderID: &orderID})
			}
		}
		if err := applyStock(ctx, tx, storeID, needs); err != nil {
			writeErr(w, err)
			return
		}
		cost = 0
	}

	// 退该商品款项
	var excID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO exceptions(order_id,store_id,type,status,responsibility,detail,comp_amount,cost_amount,created_by)
		VALUES($1,$2,'food_oos','resolved','store',$3,$4,$5,$6) RETURNING id`,
		orderID, storeID, map[string]any{"cup_id": cupID, "note": req.Note},
		price, cost, ac.ID).Scan(&excID); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO finance_records(store_id,order_id,exception_id,kind,amount,bearer,note,created_by)
		VALUES($1,$2,$3,'refund',$4,'store','轻食缺货单品退款',$5)`,
		storeID, orderID, excID, -price, ac.ID); err != nil {
		writeErr(w, err)
		return
	}
	if cost > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO loss_reports(store_id,order_id,cup_id,exception_id,reason,qty_cups,cost_amount,staff_id)
			VALUES($1,$2,$3,$4,'canceled',1,$5,$6)`, storeID, orderID, cupID, excID, cost, ac.ID); err != nil {
			writeErr(w, err)
			return
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET total_amount=total_amount-$2 WHERE id=$1`, orderID, price); err != nil {
		writeErr(w, err)
		return
	}
	// 剩余杯全部封口则置 ready
	maybeReady(ctx, tx, orderID)
	if err := a.logEvent(ctx, tx, orderID, "item_oos", actorOf(ac), map[string]any{
		"cup_id": cupID, "refund": price, "note": req.Note,
	}); err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	o, _ := a.loadOrder(ctx, orderID)
	writeJSON(w, 200, map[string]any{"order": o, "exception_id": excID, "refund_amount": price})
}

func delistByIngredientOrItem(ctx context.Context, tx pgx.Tx, storeID, menuItemID int64, reason string, by int64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO delistings(store_id,menu_item_id,reason,created_by)
		SELECT $1,$2,$3,$4 WHERE NOT EXISTS
		(SELECT 1 FROM delistings WHERE store_id=$1 AND menu_item_id=$2 AND active=TRUE)`,
		storeID, menuItemID, reason, by)
	return err
}

// maybeReady 若订单剩余有效杯均已封口则进入待取区；若无有效杯则直接 ready。
func maybeReady(ctx context.Context, tx pgx.Tx, orderID int64) {
	var sealedN, totalN int
	_ = tx.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status='sealed'),count(*) FILTER (WHERE status<>'voided')
		 FROM order_cups WHERE order_id=$1`, orderID).Scan(&sealedN, &totalN)
	if totalN == 0 {
		_, _ = tx.Exec(ctx, `UPDATE orders SET all_sealed_at=now(),status='active' WHERE id=$1 AND status='received'`, orderID)
		return
	}
	if sealedN == totalN {
		_, _ = tx.Exec(ctx, `UPDATE orders SET all_sealed_at=now(),status='active' WHERE id=$1 AND status='received'`, orderID)
	} else {
		// 仍有未完成杯（如被 OOS 作废但其余已封），保留 active 让骑手可取剩余；不回退已 ready 的订单
		_, _ = tx.Exec(ctx, `UPDATE orders SET status='active' WHERE id=$1 AND all_sealed_at IS NOT NULL`, orderID)
	}
}
