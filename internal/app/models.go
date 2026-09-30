package app

import "time"

// 领域对象的 JSON 投影（金额统一 float64，SQL 中以 ::float8 输出）

type Store struct {
	ID            int64      `json:"id"`
	Code          string     `json:"code"`
	Name          string     `json:"name"`
	Region        string     `json:"region"`
	CapacityPer5m int        `json:"capacity_per_5m"`
	MaxActiveCups int        `json:"max_active_cups"`
	StaleMinutes  int        `json:"stale_minutes"`
	SurgeManual   bool       `json:"surge_manual"`
	SurgeReason   string     `json:"surge_reason,omitempty"`
	SurgeUntil    *time.Time `json:"surge_until,omitempty"`
}

type Account struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	LoginCode string `json:"login_code,omitempty"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	StoreID   *int64 `json:"store_id,omitempty"`
	Region    string `json:"region,omitempty"`
	Platform  string `json:"platform,omitempty"`
	Active    bool   `json:"active"`
}

type MenuItem struct {
	ID            int64   `json:"id"`
	SKU           string  `json:"sku"`
	Name          string  `json:"name"`
	Category      string  `json:"category"`
	IsDrink       bool    `json:"is_drink"`
	IncludesDrink bool    `json:"includes_drink"`
	Price         float64 `json:"price"`
	MakeSeconds   int     `json:"make_seconds"`
	OnSale        bool    `json:"on_sale"`
}

type Cup struct {
	ID             int64      `json:"id"`
	OrderID        int64      `json:"order_id"`
	MenuItemID     int64      `json:"menu_item_id"`
	SKU            string     `json:"sku,omitempty"`
	NameSnapshot   string     `json:"name_snapshot"`
	UnitPrice      float64    `json:"unit_price"`
	Temp           string     `json:"temp"`
	Sugar          string     `json:"sugar"`
	CupSize        string     `json:"cup_size"`
	CupNote        string     `json:"cup_note,omitempty"`
	Seq            int        `json:"seq"`
	SealCode       string     `json:"seal_code,omitempty"`
	Status         string     `json:"status"`
	RemakeNo       int        `json:"remake_no"`
	RemakeOfCupID  *int64     `json:"remake_of_cup_id,omitempty"`
	QueuedAt       time.Time  `json:"queued_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	SealedAt       *time.Time `json:"sealed_at,omitempty"`
	VoidedAt       *time.Time `json:"voided_at,omitempty"`
	VoidReason     string     `json:"void_reason,omitempty"`
	LabelPrintedAt *time.Time `json:"label_printed_at,omitempty"`
	StaffStartID   *int64     `json:"staff_start_id,omitempty"`
	StaffSealID    *int64     `json:"staff_seal_id,omitempty"`
}

type Bag struct {
	ID      int64  `json:"id"`
	OrderID int64  `json:"order_id"`
	BagCode string `json:"bag_code"`
	Kind    string `json:"kind"` // hot / cold / food
	CupID   int64  `json:"cup_id"`
}

type Order struct {
	ID              int64      `json:"id"`
	Platform        string     `json:"platform"`
	PlatformOrderNo string     `json:"platform_order_no"`
	StoreID         int64      `json:"store_id"`
	Status          string     `json:"status"`
	CustomerID      string     `json:"customer_id,omitempty"`
	CustomerNote    string     `json:"customer_note,omitempty"`
	TotalAmount     float64    `json:"total_amount"`
	PromisedMinutes int        `json:"promised_minutes"`
	OrderedAt       time.Time  `json:"ordered_at"`
	RiderETA        *time.Time `json:"rider_eta,omitempty"`
	AllSealedAt     *time.Time `json:"all_sealed_at,omitempty"`
	HandedAt        *time.Time `json:"handed_at,omitempty"`
	CanceledAt      *time.Time `json:"canceled_at,omitempty"`
	CancelReason    string     `json:"cancel_reason,omitempty"`
	MergedGroupID   *int64     `json:"merged_group_id,omitempty"`
	Cups            []Cup      `json:"cups,omitempty"`
	Bags            []Bag      `json:"bags,omitempty"`
}

type OrderEvent struct {
	ID        int64          `json:"id"`
	OrderID   int64          `json:"order_id"`
	EventType string         `json:"event_type"`
	ActorRole string         `json:"actor_role,omitempty"`
	ActorName string         `json:"actor_name,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

type Exception struct {
	ID             int64          `json:"id"`
	OrderID        int64          `json:"order_id"`
	StoreID        int64          `json:"store_id"`
	Type           string         `json:"type"`
	Status         string         `json:"status"`
	Responsibility string         `json:"responsibility"`
	Occurrences    int            `json:"occurrences"`
	Detail         map[string]any `json:"detail,omitempty"`
	CompAmount     float64        `json:"comp_amount"`
	CostAmount     float64        `json:"cost_amount"`
	CreatedAt      time.Time      `json:"created_at"`
	ResolvedAt     *time.Time     `json:"resolved_at,omitempty"`
	Resolution     string         `json:"resolution,omitempty"`
}
