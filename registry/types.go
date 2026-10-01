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

// TransferRequest 是转让请求的业务参数。
type TransferRequest struct {
	Operator      string // 操作者，必须是当前持有人且可用
	Reason        string // 原因
	RequestID     string // 请求号
	ItemID        string // 藏品编号
	ExpectedOwner string // 期望当前持有人（通常即发起人）
	ExpectedVer   int64  // 期望当前持有版本
	ToID          string // 接收人，必须已登记、可用且不同于当前持有人
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
}
