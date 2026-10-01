package registry

import "time"

// Account 是登记册中的账户。账户用唯一编号登记，初始可用；
// 停用后仍可查询其原有藏品与历史，但不能发行、发起或接收转让。
type Account struct {
	// ID 是账户的唯一编号。
	ID string `json:"id"`
	// Active 表示账户是否可用。
	Active bool `json:"active"`
	// CreatedAt 是账户登记时间。
	CreatedAt time.Time `json:"created_at"`
	// DeactivatedAt 是账户停用时间，未停用则为空。
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
}

// Series 是藏品系列。系列记录创建账户与文字元数据，
// 仅创建账户可以发行或封存；封存不可撤销，封存后不能继续发行。
type Series struct {
	// ID 是系列的唯一编号。
	ID string `json:"id"`
	// CreatorID 是创建该系列的账户编号。
	CreatorID string `json:"creator_id"`
	// Metadata 是系列的文字元数据。
	Metadata map[string]string `json:"metadata,omitempty"`
	// Sealed 表示系列是否已封存。
	Sealed bool `json:"sealed"`
	// CreatedAt 是系列创建时间。
	CreatedAt time.Time `json:"created_at"`
	// SealedAt 是系列封存时间，未封存则为空。
	SealedAt *time.Time `json:"sealed_at,omitempty"`
}

// Artifact 是单件藏品。藏品发行后持有版本为 1，
// 每次成功转让版本加 1。
type Artifact struct {
	// ID 是藏品的唯一编号，同一编号不能产生第二件藏品。
	ID string `json:"id"`
	// SeriesID 是藏品所属系列编号。
	SeriesID string `json:"series_id"`
	// BatchNo 是藏品的批次号。
	BatchNo string `json:"batch_no"`
	// Metadata 是藏品的文字元数据。
	Metadata map[string]string `json:"metadata,omitempty"`
	// Holder 是当前持有人账户编号。
	Holder string `json:"holder"`
	// Version 是当前持有版本，发行后为 1。
	Version int `json:"version"`
	// IssuedAt 是发行时间。
	IssuedAt time.Time `json:"issued_at"`
	// IssuedBy 是发行操作者。
	IssuedBy string `json:"issued_by"`
	// IssueReason 是发行原因。
	IssueReason string `json:"issue_reason"`
}

// Holding 是藏品的当前持有状态。
type Holding struct {
	// ArtifactID 是藏品编号。
	ArtifactID string `json:"artifact_id"`
	// Holder 是当前持有人账户编号。
	Holder string `json:"holder"`
	// Version 是当前持有版本。
	Version int `json:"version"`
}

// HistoryEntry 是藏品历史中的一条记录，按顺序追加。
// 发行记录的前持有人为空，版本为 1。
type HistoryEntry struct {
	// ArtifactID 是藏品编号。
	ArtifactID string `json:"artifact_id"`
	// Seq 是历史顺序号，从 1 开始。
	Seq int `json:"seq"`
	// Op 是操作类型："issue" 或 "transfer"。
	Op string `json:"op"`
	// Operator 是操作者。
	Operator string `json:"operator"`
	// Reason 是原因。
	Reason string `json:"reason"`
	// From 是前持有人，发行时为空。
	From string `json:"from"`
	// To 是后持有人。
	To string `json:"to"`
	// Version 是该次操作后的持有版本。
	Version int `json:"version"`
	// At 是操作时间。
	At time.Time `json:"at"`
}

// RegisterAccountRequest 是登记账户的请求。
type RegisterAccountRequest struct {
	// Operator 是操作者（必填）。
	Operator string
	// RequestNo 是操作者维度的请求号（必填），与发行/转让共用同一命名空间。
	RequestNo string
	// Reason 是原因（必填）。
	Reason string
	// AccountID 是登记的账户编号（必填）。
	AccountID string
}

// DeactivateAccountRequest 是停用账户的请求。
type DeactivateAccountRequest struct {
	Operator  string
	RequestNo string
	Reason    string
	// AccountID 是要停用的账户编号（必填）。
	AccountID string
}

// CreateSeriesRequest 是创建系列的请求。
type CreateSeriesRequest struct {
	Operator  string
	RequestNo string
	Reason    string
	// SeriesID 是系列编号（必填）。
	SeriesID string
	// CreatorID 是创建账户编号（必填），必须为已登记且可用的账户。
	CreatorID string
	// Metadata 是系列文字元数据（可选）。
	Metadata map[string]string
}

// SealSeriesRequest 是封存系列的请求。
type SealSeriesRequest struct {
	Operator  string
	RequestNo string
	Reason    string
	// SeriesID 是要封存的系列编号（必填）。
	SeriesID string
}

// IssueRequest 是发行藏品的请求。
type IssueRequest struct {
	Operator  string
	RequestNo string
	Reason    string
	// ArtifactID 是发行的藏品编号（必填），同一编号不能发行第二件。
	ArtifactID string
	// SeriesID 是所属系列编号（必填），系列必须存在且未封存。
	SeriesID string
	// BatchNo 是批次号（必填）。
	BatchNo string
	// Metadata 是藏品文字元数据（可选）。
	Metadata map[string]string
	// InitialHolder 是初始持有人账户编号（必填），必须已登记且可用。
	InitialHolder string
}

// TransferRequest 是转让藏品的请求。转让由当前持有人发起，
// 接收人必须已登记、可用且不同于当前持有人。
type TransferRequest struct {
	Operator  string
	RequestNo string
	Reason    string
	// ArtifactID 是转让的藏品编号（必填）。
	ArtifactID string
	// From 是当前持有人账户编号（必填），必须与登记一致。
	From string
	// To 是接收人账户编号（必填），必须已登记、可用且不同于 From。
	To string
	// ExpectedVersion 是期望的当前持有版本（必填），必须与登记一致。
	ExpectedVersion int
}
