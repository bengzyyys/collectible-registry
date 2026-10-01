package registry

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
	Kind      string // "issue" 或 "transfer"
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
