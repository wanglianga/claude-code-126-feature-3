package app

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type App struct {
	Pool  *pgxpool.Pool
	Redis *redis.Client
}

func New(pool *pgxpool.Pool, rdb *redis.Client) *App {
	return &App{Pool: pool, Redis: rdb}
}

// ---------- 错误 ----------

type apiError struct {
	Status  int    `json:"-"`
	Code    string `json:"error"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func errf(status int, code, format string, a ...any) *apiError {
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

var (
	errNotFound  = errf(http.StatusNotFound, "not_found", "资源不存在")
	errConflict  = errf(http.StatusConflict, "conflict", "状态冲突")
	errForbidden = errf(http.StatusForbidden, "forbidden", "无权限执行该操作")
	errBadAuth   = errf(http.StatusUnauthorized, "unauthorized", "请先通过 X-Staff-Code 或 Bearer Token 登录")
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeJSON(w, ae.Status, ae)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, 404, map[string]string{"error": "not_found", "message": "资源不存在"})
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeJSON(w, 409, map[string]string{"error": "conflict", "message": pgErr.ConstraintName + " 唯一约束冲突"})
		return
	}
	writeJSON(w, 500, map[string]string{"error": "internal", "message": err.Error()})
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return errf(http.StatusBadRequest, "bad_json", "请求体不是合法 JSON: %v", err)
	}
	return nil
}

// ---------- 认证 ----------

type ctxKey string

const actorKey ctxKey = "actor"

func (a *App) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var acc Account
		code := r.Header.Get("X-Staff-Code")
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case code != "":
			err := a.Pool.QueryRow(r.Context(),
				`SELECT id,login,login_code,name,role,store_id,COALESCE(region,''),COALESCE(platform,''),active
				 FROM accounts WHERE login_code=$1`, code).
				Scan(&acc.ID, &acc.Login, &acc.LoginCode, &acc.Name, &acc.Role,
					&acc.StoreID, &acc.Region, &acc.Platform, &acc.Active)
			if err != nil {
				writeErr(w, errBadAuth)
				return
			}
		case token != "" && token != r.Header.Get("Authorization"):
			raw, err := a.Redis.Get(r.Context(), "auth:"+token).Result()
			if err != nil || json.Unmarshal([]byte(raw), &acc) != nil || !acc.Active {
				writeErr(w, errBadAuth)
				return
			}
		default:
			writeErr(w, errBadAuth)
			return
		}
		if !acc.Active {
			writeErr(w, errForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey, &acc)))
	})
}

func actor(r *http.Request) *Account {
	return r.Context().Value(actorKey).(*Account)
}

func require(roles ...string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, x := range roles {
		allowed[x] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			acc := actor(r)
			if acc.Role != "admin" && !allowed[acc.Role] {
				writeErr(w, errForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------- 事件时间线 ----------

type eventActor struct {
	Role string
	ID   int64
	Name string
}

func (a *App) logEvent(ctx context.Context, tx pgx.Tx, orderID int64, etype string, ac *eventActor, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	var role *string
	var id *int64
	var name *string
	if ac != nil {
		s1, s2 := ac.Role, ac.Name
		role, name = &s1, &s2
		if ac.ID != 0 {
			id = &ac.ID
		}
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO order_events(order_id,event_type,actor_role,actor_id,actor_name,detail)
		 VALUES($1,$2,$3,$4,$5,$6)`, orderID, etype, role, id, name, detail)
	return err
}

func actorOf(acc *Account) *eventActor {
	if acc == nil {
		return nil
	}
	return &eventActor{Role: acc.Role, ID: acc.ID, Name: acc.Name}
}

// ---------- 路由 ----------

func (a *App) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		pg, rd := "ok", "ok"
		if err := a.Pool.Ping(ctx); err != nil {
			pg = "down"
		}
		if err := a.Redis.Ping(ctx).Err(); err != nil {
			rd = "down"
		}
		writeJSON(w, 200, map[string]string{"status": "ok", "postgres": pg, "redis": rd})
	})

	r.Route("/v1", func(r chi.Router) {
		r.Post("/auth/login", a.login)

		r.Group(func(r chi.Router) {
			r.Use(a.authenticate)
			r.Get("/auth/me", a.me)

			// 基础数据
			r.Get("/stores", a.listStores)
			r.Get("/stores/{id}", a.getStore)
			r.Get("/menu", a.listMenu)
			r.Get("/stores/{id}/inventory", a.getInventory)
			r.Get("/stores/{id}/delistings", a.listDelistings)
			r.Get("/stores/{id}/board", a.storeBoard)

			// 订单
			r.Get("/orders/{id}", a.getOrder)
			r.Get("/orders/{id}/events", a.listEvents)
			r.Get("/orders/{id}/exceptions", a.listOrderExceptions)
			r.Get("/stores/{id}/orders", a.listStoreOrders)

			// 平台 / 门店接单
			r.With(require("platform", "store_manager")).Post("/platform/orders", a.intakeOrder)
			r.With(require("platform", "platform_cs")).Post("/platform/orders/{id}/cancel", a.cancelOrder)

			// 门店作业
			r.With(require("store_manager", "barista")).Post("/staff/merge", a.mergeOrders)
			r.Get("/merged-groups/{id}", a.getMergedGroup)
			r.With(require("store_manager", "barista")).Get("/staff/stores/{id}/queue", a.makeQueue)
			r.With(require("store_manager", "barista")).Post("/staff/stores/{id}/cups/next", a.startNextCup)
			r.With(require("store_manager", "barista")).Post("/staff/cups/{id}/start", a.startCup)
			r.With(require("store_manager", "barista")).Post("/staff/cups/{id}/seal", a.sealCup)
			r.With(require("store_manager", "barista")).Post("/staff/cups/{id}/remake", a.remakeCup)
			r.With(require("store_manager", "barista")).Post("/staff/cups/{id}/label-print", a.markCupLabelPrinted)
			r.With(require("store_manager", "barista")).Get("/staff/cups/{id}/stale-check", a.cupStaleCheck)
			r.With(require("store_manager", "barista")).Post("/staff/stores/{id}/stale-check", a.storeStaleCheck)
			r.With(require("store_manager", "barista")).Post("/staff/orders/{id}/item-oos", a.itemOOS)
			r.With(require("store_manager", "barista")).Get("/staff/orders/{id}/cup-labels", a.cupLabels)
			r.With(require("store_manager", "barista")).Get("/staff/orders/{id}/bag-labels", a.bagLabels)
			r.With(require("store_manager", "barista")).Post("/staff/inventory/restock", a.restock)
			r.With(require("store_manager", "barista")).Post("/staff/menu/{sku}/delist", a.manualDelist)
			r.With(require("store_manager", "barista")).Post("/staff/stores/{id}/surge", a.setSurge)
			r.Get("/staff/stores/{id}/loss-reports", a.listLossReports)

			// 骑手
			r.With(require("rider")).Post("/rider/arrivals", a.riderArrive)
			r.Get("/rider/arrivals/{id}", a.getArrival)
			r.Get("/rider/screen", a.riderScreen)
			r.With(require("rider", "store_manager", "barista")).Post("/rider/handoffs", a.handoff)
			// 重做中骑手到店的交接决策（wait_new / take_original / abort）
			r.Get("/rider/orders/{id}/remake-decision", a.remakeDecisionGet)
			r.With(require("rider", "store_manager", "barista")).Post("/rider/orders/{id}/remake-decision", a.remakeDecisionPost)

			// 顾客 / 客服
			r.With(require("customer", "platform_cs", "finance")).Post("/customers/orders/{id}/complain", a.complain)
			r.Get("/customers/orders/{id}/delay", a.customerDelay)
			r.Get("/compensations", a.listCompensations)
			r.With(require("platform_cs", "finance")).Post("/platform/compensations/{id}/fulfill", a.fulfillCompensation)

			// 财务
			r.With(require("finance", "store_manager")).Patch("/finance/exceptions/{id}", a.adjudicate)
			r.Get("/stores/{id}/finance", a.storeFinance)

			// 责任复核：店长申请 / 区域经理改判（追加不覆盖，回算班组利润）
			r.With(require("store_manager", "area_manager", "admin")).Post("/management/exceptions/{id}/responsibility-review", a.requestResponsibilityReview)
			r.With(require("area_manager", "admin")).Post("/management/responsibility-reviews/{id}/decision", a.decideResponsibilityReview)
			r.Get("/management/responsibility-reviews", a.listResponsibilityReviews)

			// 管理 / 报表
			r.Get("/reports/stores/{id}/summary", a.storeSummary)
			r.With(require("area_manager", "supervisor")).Get("/reports/region", a.regionReport)
			r.With(require("store_manager", "area_manager", "supervisor")).Post("/management/stores/{id}/team-score/refresh", a.refreshScore)
			r.Get("/management/stores/{id}/team-score", a.getScore)
			r.With(require("area_manager", "supervisor")).Get("/management/team-scores", a.listScores)
			r.With(require("supervisor", "area_manager")).Post("/management/stores/{id}/reviews", a.createReview)
			r.Get("/stores/{id}/reviews", a.listReviews)
		})
	})

	r.Mount("/swagger", swaggerUI())
	r.Get("/openapi.yaml", a.openAPI)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger/", http.StatusFound)
	})
	return r
}

func (a *App) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, actor(r))
}

type loginReq struct {
	Code string `json:"code"`
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Code == "" {
		req.Code = r.Header.Get("X-Staff-Code")
	}
	var acc Account
	err := a.Pool.QueryRow(r.Context(),
		`SELECT id,login,login_code,name,role,store_id,COALESCE(region,''),COALESCE(platform,''),active
		 FROM accounts WHERE login_code=$1 AND active=TRUE`, req.Code).
		Scan(&acc.ID, &acc.Login, &acc.LoginCode, &acc.Name, &acc.Role,
			&acc.StoreID, &acc.Region, &acc.Platform, &acc.Active)
	if err != nil {
		writeErr(w, errf(401, "bad_code", "工号/验证码无效"))
		return
	}
	tok := make([]byte, 16)
	if _, err := crand.Read(tok); err != nil {
		writeErr(w, err)
		return
	}
	token := hex.EncodeToString(tok)
	raw, _ := json.Marshal(acc)
	if err := a.Redis.Set(r.Context(), "auth:"+token, raw, 12*time.Hour).Err(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"token": token, "account": acc})
}
