package registry

import "time"

// Account 是账户登记信息。停用后仍可查询其原有藏品与历史。
type Account struct {
	ID       string // 唯一编号
	Metadata string // 文字元数据，可为空
	Active   bool   // 是否可用；停用后不能发行、发起或接收新的转让
}

// Series 是系列信息。仅创建账户可以发行藏品或封存系列。
type Series struct {
	ID        string // 系列编号
	CreatorID string // 创建账户编号
	Metadata  string // 文字元数据
	Sealed    bool   // 是否已封存；封存不可撤销
}

// Item 是单件藏品的静态登记信息。
type Item struct {
	ID         string // 唯一藏品编号，同一编号不能产生第二件藏品
	SeriesID   string // 所属系列
	BatchNo    string // 批次号
	Metadata   string // 文字元数据
	IssuedTxID int64  // 发行记录在历史中的序号
}

// Holding 是一件藏品的当前持有状态。
type Holding struct {
	ItemID  string // 藏品编号
	OwnerID string // 当前持有人
	Version int64  // 当前持有版本：发行后为 1，每次成功转让加 1
}

// HistoryEntry 是一条发行或成功转让记录，按发生先后排列。
type HistoryEntry struct {
	Seq       int64  // 严格递增的记录序号，也即发生顺序
	Kind      string // "issue" 或 "transfer"（代转仍为 transfer）
	ItemID    string // 藏品编号
	Operator  string // 操作者账户编号
	Reason    string // 操作原因
	RequestID string // 请求号
	// FromID 是转让前持有人；发行前为空。
	FromID string
	// ToID 是转让后持有人（发行时为初始持有人）。
	ToID string
	// FromVersion 是转让前持有版本；发行前为 0（语义上为空）。
	FromVersion int64
	// ToVersion 是转让后持有版本（发行后为 1）。
	ToVersion int64
	// AuthID 是代转所使用的授权编号；直接转让与发行时为空。
	AuthID string
}

// IssueRequest 是发行请求的业务参数（幂等判定以此为准）。
type IssueRequest struct {
	Operator  string // 操作者，必须是系列创建账户且可用
	Reason    string // 原因
	RequestID string // 请求号，同一操作者在发行与转让之间共用
	ItemID    string // 唯一藏品编号
	SeriesID  string // 所属系列
	BatchNo   string // 批次号
	Metadata  string // 藏品文字元数据
	HolderID  string // 初始持有人，必须是已登记且可用的账户
}

// IssueBatchEntry 是整批发行中一件藏品的提交内容。
type IssueBatchEntry struct {
	ItemID   string // 唯一藏品编号，清单内不能重复
	Metadata string // 藏品文字元数据，可为空
	HolderID string // 初始持有人，必须是已登记且可用的账户
}

// IssueBatchRequest 是整批发行请求的业务参数（幂等判定以此为准，含条目
// 顺序）。整批共用操作者、原因、请求号、系列与批次号。
type IssueBatchRequest struct {
	Operator  string            // 操作者，必须是系列创建账户且可用
	Reason    string            // 原因
	RequestID string            // 请求号，与单件发行、转让等共用同一操作者的请求号范围
	SeriesID  string            // 所属系列，整批同一系列
	BatchNo   string            // 批次号，整批同一批次；不同请求可沿用同一批次号
	Entries   []IssueBatchEntry // 发行清单，按提交顺序依次发行；不能为空
}

// IssueBatchItem 是整批发行中一件藏品的发行结果。
type IssueBatchItem struct {
	ItemID  string // 藏品编号
	OwnerID string // 初始持有人
	Version int64  // 持有版本，恒为 1
	TxSeq   int64  // 发行历史序号；整批内连续递增
}

// IssueBatchResult 是整批发行请求的处理结果。
type IssueBatchResult struct {
	Items []IssueBatchItem // 各件的发行结果，按提交顺序排列；拒绝时为空
	// ItemID 是业务拒绝所涉及的藏品编号（如编号已占用、初始持有人不可用的
	// 那一件）；拒绝与具体某件无关时为空。
	ItemID   string
	Replayed bool  // 是否为重复提交回放的首次结果
	Err      error // 首次或本次的业务拒绝；成功时为 nil
}

// TransferRequest 是转让请求的业务参数。
type TransferRequest struct {
	Operator      string // 操作者，必须是当前持有人且可用
	Reason        string // 原因
	RequestID     string // 请求号
	ItemID        string // 藏品编号
	ExpectedOwner string // 期望当前持有人（通常即发起人）
	ExpectedVer   int64  // 期望当前持有版本
	ToID          string // 接收人，必须已登记、可用且不同于当前持有人
	// Price 是成交价款，以分计，非负；未填按 0。负数按参数错误拒绝。
	Price int64
}

// IssueResult 是发行请求的处理结果。
type IssueResult struct {
	ItemID   string // 藏品编号
	OwnerID  string // 初始持有人
	Version  int64  // 持有版本，恒为 1
	TxSeq    int64  // 发行历史序号
	Replayed bool   // 是否为重复提交回放的首次结果
	Err      error  // 首次或本次的业务拒绝；成功时为 nil
}

// TransferResult 是转让请求的处理结果。
type TransferResult struct {
	ItemID   string // 藏品编号
	FromID   string // 转让前持有人
	ToID     string // 转让后持有人
	Version  int64  // 转让后版本
	TxSeq    int64  // 转让历史序号
	Replayed bool   // 是否为重复提交回放的首次结果
	Err      error  // 业务拒绝；成功时为 nil
	// Price 是本笔成交价款（分）；Payables 是各版税收款账户的应付明细
	// （含零金额）；Remainder 是扣除应付后归转让前持有人的剩余收入。
	Price     int64
	Payables  []RoyaltyPayable
	Remainder int64
}

// TransferBatchEntry 是整批转让中一件藏品的提交内容。
type TransferBatchEntry struct {
	ItemID        string // 藏品编号，清单内不能重复
	ToID          string // 接收账户，必须已登记且可用；不同条目可重复
	ExpectedOwner string // 期望当前持有人；操作者必须是每件的当前持有人
	ExpectedVer   int64  // 期望当前持有版本，必须为正
	// Price 是本件成交价款，以分计，非负；未填按 0。负数按参数错误拒绝。
	Price int64
}

// TransferBatchRequest 是整批转让请求的业务参数（幂等判定以此为准，含
// 条目顺序）。整批共用操作者、原因与请求号；藏品可来自不同系列与发行
// 批次。
type TransferBatchRequest struct {
	Operator  string               // 操作者，必须是每件的当前持有人且可用
	Reason    string               // 原因
	RequestID string               // 请求号，与发行、单件转让、代转等共用同一操作者的请求号范围
	Entries   []TransferBatchEntry // 转让清单，按提交顺序处理与返回；不能为空
}

// TransferBatchItem 是整批转让中一件藏品的转让结果。
type TransferBatchItem struct {
	ItemID   string           // 藏品编号
	FromID   string           // 转让前持有人
	ToID     string           // 转让后持有人
	Version  int64            // 转让后版本（各件分别加一）
	TxSeq    int64            // 转让历史序号；整批内连续递增
	Price    int64            // 本件成交价款（分）
	Payables []RoyaltyPayable // 本件各版税收款账户的应付明细（含零金额）
	// Remainder 是本件扣除应付后归转让前持有人的剩余收入。
	Remainder int64
}

// TransferBatchResult 是整批转让请求的处理结果。
type TransferBatchResult struct {
	Items []TransferBatchItem // 各件的转让结果，按提交顺序排列；拒绝时为空
	// ItemID 是业务拒绝所涉及的藏品编号（清单顺序最前的失败藏品）；
	// 操作者层面的拒绝（未登记或停用）时为空。
	ItemID   string
	Replayed bool  // 是否为重复提交回放的首次结果
	Err      error // 首次或本次的业务拒绝；成功时为 nil
}

// 授权生命周期状态。
const (
	// AuthActive 表示授权已创建，尚未撤销、过期或使用。
	AuthActive = "active"
	// AuthRevoked 表示授权已被授权人撤销。
	AuthRevoked = "revoked"
	// AuthExpired 表示授权已过到期时间且尚未使用。
	AuthExpired = "expired"
	// AuthUsed 表示授权已用于一次成功代转。
	AuthUsed = "used"
)

// CreateAuthorizationRequest 是创建限时一次性代转授权的业务参数
// （幂等判定以此为准）。
type CreateAuthorizationRequest struct {
	Operator      string    // 操作者，必须是当前持有人（授权人）且可用
	Reason        string    // 原因
	RequestID     string    // 请求号，与发行、转让共用同一操作者的请求号范围
	AuthID        string    // 唯一授权编号
	ItemID        string    // 已发行藏品编号
	TrusteeID     string    // 受托账户，可凭授权发起代转
	ToID          string    // 固定接收账户，代转只能转入该账户
	ExpectedOwner string    // 期望当前持有人
	ExpectedVer   int64     // 期望当前持有版本；授权绑定该持有版本
	ExpiresAt     time.Time // 绝对到期时间，必须晚于当前时间
	// Price 是代转成交价款（分），在创建授权时确定，执行代转时不得改价；
	// 非负，未填按 0。
	Price int64
}

// RevokeAuthorizationRequest 是撤销授权的业务参数。
type RevokeAuthorizationRequest struct {
	Operator  string // 操作者，必须是授权人
	Reason    string // 原因
	RequestID string // 请求号
	AuthID    string // 待撤销的授权编号
}

// ProxyTransferRequest 是受托人凭授权发起代转的业务参数。
type ProxyTransferRequest struct {
	Operator  string // 操作者，必须是授权的受托人
	Reason    string // 原因
	RequestID string // 请求号
	AuthID    string // 使用的授权编号
}

// Authorization 是一份代转授权的当前内容与状态。
type Authorization struct {
	ID        string    // 授权编号
	ItemID    string    // 绑定的藏品
	GranterID string    // 授权人（创建时的持有人）
	TrusteeID string    // 受托账户
	ToID      string    // 固定接收账户
	ExpiresAt time.Time // 绝对到期时间
	GrantVer  int64     // 创建时绑定的持有版本
	// Price 是创建时确定的代转成交价款（分）；旧登记册中的授权按 0。
	Price     int64
	Status    string    // 当前状态：AuthActive/AuthRevoked/AuthExpired/AuthUsed
	UsedTxSeq int64     // 成功代转的历史序号；未使用为 0
	UsedAt    time.Time // 成功代转时间；未使用为零值
	CreatedAt time.Time // 创建时间
	RevokedAt time.Time // 撤销时间；未撤销为零值
}

// AuthorizationEvent 是授权变更记录中的一条。
type AuthorizationEvent struct {
	Seq        int64     // 授权变更记录内严格递增的序号
	AuthID     string    // 授权编号
	ItemID     string    // 藏品编号
	Kind       string    // "create" / "revoke" / "use"
	Operator   string    // 操作者账户编号（use 时为受托人）
	Reason     string    // 操作原因
	RequestID  string    // 请求号
	FromStatus string    // 变更前状态（create 前为空）
	ToStatus   string    // 变更后状态
	TxSeq      int64     // use 时关联的藏品转让历史序号，否则为 0
	OccurredAt time.Time // 发生时间
}

// CreateAuthorizationResult 是创建授权的结果。
type CreateAuthorizationResult struct {
	AuthID   string
	Status   string
	GrantVer int64
	Replayed bool
	Err      error
}

// RevokeAuthorizationResult 是撤销授权的结果。Replayed 为 true 表示该
// (操作者, 请求号) 曾成功或处于状态类业务拒绝；已撤销后再次撤销也回放
// 首次结果。
type RevokeAuthorizationResult struct {
	AuthID   string
	Status   string
	Replayed bool
	Err      error
}

// ProxyTransferResult 是代转的结果，字段语义与 TransferResult 一致。
type ProxyTransferResult struct {
	AuthID   string // 使用的授权编号
	ItemID   string // 藏品编号
	FromID   string // 转让前持有人
	ToID     string // 转让后持有人
	Version  int64  // 转让后版本
	TxSeq    int64  // 转让历史序号
	Replayed bool   // 是否为重复提交回放的首次结果
	Err      error  // 业务拒绝；成功时为 nil
	// Price 是授权创建时确定的成交价款（分）；Payables 是各版税收款账户
	// 的应付明细（含零金额）；Remainder 是归转让前持有人的剩余收入。
	Price     int64
	Payables  []RoyaltyPayable
	Remainder int64
}

// ---- 版税规则与应付记录 ----

// RoyaltyRateBase 是版税比例的分母：比例以万分之一为单位。
const RoyaltyRateBase = 10000

// RoyaltyShare 是版税规则中一个收款账户的份额。
type RoyaltyShare struct {
	AccountID string // 收款账户，规则内只能出现一次
	Rate      int64  // 比例，万分之一，取值 1..10000；全部份额合计不超过 10000
}

// SetRoyaltyRequest 是设置或清空系列版税规则的业务参数（幂等判定以此
// 为准）。Shares 为空表示清空规则，即不收版税。
type SetRoyaltyRequest struct {
	Operator  string         // 操作者，必须是系列创建账户且可用
	Reason    string         // 原因
	RequestID string         // 请求号，与发行、转让等共用同一操作者的请求号范围
	SeriesID  string         // 系列编号
	Shares    []RoyaltyShare // 新规则；空表示不收版税
}

// SetRoyaltyResult 是设置版税规则的结果。
type SetRoyaltyResult struct {
	SeriesID string         // 系列编号
	Shares   []RoyaltyShare // 本次生效的规则（按收款账户排序规范化后）
	Replayed bool           // 是否为重复提交回放的首次结果
	Err      error          // 业务拒绝；成功时为 nil
}

// RoyaltyEvent 是一次版税规则变更记录，包含操作者、原因、请求号与前后
// 内容；按发生先后排列。
type RoyaltyEvent struct {
	Seq        int64          // 规则变更记录内严格递增的序号
	SeriesID   string         // 系列编号
	Operator   string         // 操作者账户编号
	Reason     string         // 操作原因
	RequestID  string         // 请求号
	Before     []RoyaltyShare // 变更前规则（首次设置前为空）
	After      []RoyaltyShare // 变更后规则
	OccurredAt time.Time      // 发生时间
}

// RoyaltyPayable 是一笔转让中某个版税收款账户的应付明细。
type RoyaltyPayable struct {
	AccountID string // 收款账户
	Rate      int64  // 计算所用比例（万分之一）
	Amount    int64  // 应付金额（分）：价款乘比例除以 10000 向下取整，零金额也保留
}

// TransferRoyalty 是一笔成功转让的版税计算依据与全部金额。
type TransferRoyalty struct {
	TxSeq     int64            // 转让历史序号
	ItemID    string           // 藏品编号
	Price     int64            // 成交价款（分）
	Payables  []RoyaltyPayable // 各收款账户应付明细（含零金额；无规则时为空）
	Remainder int64            // 扣除应付后的剩余收入，归转让前持有人
	OwnerID   string           // 转让前持有人，即剩余收入的归属账户
}

// PayableEntry 是按收款账户查询时的一条应付明细。
type PayableEntry struct {
	TxSeq  int64  // 转让历史序号
	ItemID string // 藏品编号
	Price  int64  // 该笔成交价款（分）
	Rate   int64  // 计算所用比例（万分之一）
	Amount int64  // 应付金额（分）
}
