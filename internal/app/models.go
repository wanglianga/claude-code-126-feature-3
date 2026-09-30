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
	ID            int64      `json:"id"`
	OrderID       int64      `json:"order_id"`
	MenuItemID    int64      `json:"menu_item_id"`
	SKU           string     `json:"sku,omitempty"`
	NameSnapshot  string     `json:"name_snapshot"`
	UnitPrice     float64    `json:"unit_price"`
	Temp          string     `json:"temp"`
	Sugar         string     `json:"sugar"`
	CupSize       string     `json:"cup_size"`
	CupNote       string     `json:"cup_note,omitempty"`
	Seq           int        `json:"seq"`
	SealCode      string     `json:"seal_code,omitempty"`
	Status        string     `json:"status"`
	RemakeNo      int        `json:"remake_no"`
	RemakeOfCupID *int64     `json:"remake_of_cup_id,omitempty"`
	QueuedAt      time.Time  `json:"queued_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	SealedAt      *time.Time `json:"sealed_at,omitempty"`
	VoidedAt      *time.Time `json:"voided_at,omitempty"`
	VoidReason    string     `json:"void_reason,omitempty"`
	StaffStartID  *int64     `json:"staff_start_id,omitempty"`
	StaffSealID   *int64     `json:"staff_seal_id,omitempty"`
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

// RemakeJob 一次“放置过久重做”的并发交接状态机。
type RemakeJob struct {
	ID                int64          `json:"id"`
	StoreID           int64          `json:"store_id"`
	OrderID           int64          `json:"order_id"`
	OldCupID          int64          `json:"old_cup_id"`
	NewCupID          *int64         `json:"new_cup_id,omitempty"`
	ExceptionID       *int64         `json:"exception_id,omitempty"`
	Reason            string         `json:"reason"`
	TriggerType       string         `json:"trigger_type"`
	Status            string         `json:"status"` // remaking / old_taken / completed / canceled
	Category          string         `json:"category"`
	IsDrink           bool           `json:"is_drink"`
	Temp              string         `json:"temp"`
	IdleSeconds       int            `json:"idle_seconds"`
	StoreBusy         bool           `json:"store_busy"`
	QualityWindowSec  int            `json:"quality_window_seconds"`
	SafetyWindowSec   int            `json:"safety_window_seconds"`
	PolicyAction      string         `json:"policy_action"`
	LossCost          float64        `json:"loss_cost"`
	RemakeCost        float64        `json:"remake_cost"`
	CompAmount        float64        `json:"comp_amount"`
	NewLabelPrinted   bool           `json:"new_label_printed"`
	NewLabelPrintedAt *time.Time     `json:"new_label_printed_at,omitempty"`
	Decision          string         `json:"decision,omitempty"`
	DecidedBy         *int64         `json:"decided_by,omitempty"`
	DecidedAt         *time.Time     `json:"decided_at,omitempty"`
	DecisionDetail    map[string]any `json:"decision_detail,omitempty"`
	RiderWaitSeconds  int            `json:"rider_wait_seconds"`
	CompletedAt       *time.Time     `json:"completed_at,omitempty"`
	CreatedBy         *int64         `json:"created_by,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
}

// Compensation 顾客补偿方案与兑现（重做/设备故障/派单延迟）。
type Compensation struct {
	ID              int64      `json:"id"`
	StoreID         int64      `json:"store_id"`
	OrderID         int64      `json:"order_id"`
	ExceptionID     *int64     `json:"exception_id,omitempty"`
	RemakeJobID     *int64     `json:"remake_job_id,omitempty"`
	Reason          string     `json:"reason"`
	Status          string     `json:"status"` // proposed / confirmed / fulfilled / withdrawn / rejected
	PlanAmount      float64    `json:"plan_amount"`
	Bearer          string     `json:"bearer"`
	CustomerMessage string     `json:"customer_message,omitempty"`
	ProposedBy      *int64     `json:"proposed_by,omitempty"`
	ProposedAt      time.Time  `json:"proposed_at"`
	FulfilledBy     *int64     `json:"fulfilled_by,omitempty"`
	FulfilledAt     *time.Time `json:"fulfilled_at,omitempty"`
	FulfillNote     string     `json:"fulfill_note,omitempty"`
}

// ResponsibilityReview 店长对设备故障/平台派单延迟的责任复核与区域经理改判。
type ResponsibilityReview struct {
	ID                 int64      `json:"id"`
	StoreID            int64      `json:"store_id"`
	OrderID            int64      `json:"order_id"`
	ExceptionID        *int64     `json:"exception_id,omitempty"`
	RemakeJobID        *int64     `json:"remake_job_id,omitempty"`
	Ground             string     `json:"ground"` // equipment_fault / platform_dispatch_delay
	Reason             string     `json:"reason"`
	Evidence           string     `json:"evidence,omitempty"`
	Status             string     `json:"status"` // pending / approved / rejected
	RequestedBy        int64      `json:"requested_by"`
	RequestedAt        time.Time  `json:"requested_at"`
	ReviewedBy         *int64     `json:"reviewed_by,omitempty"`
	ReviewedAt         *time.Time `json:"reviewed_at,omitempty"`
	Approved           *bool      `json:"approved,omitempty"`
	NewResponsibility  string     `json:"new_responsibility,omitempty"`
	ReattributedAmount float64    `json:"reattributed_amount"`
	Resolution         string     `json:"resolution,omitempty"`
}
