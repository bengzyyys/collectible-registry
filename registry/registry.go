package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Registry 是一个已打开的本地登记册。同一目录同时只能被一个 Registry
// 独占打开（目录内 flock）；单个 Registry 的方法可被多个 goroutine
// 并发调用，所有状态变更与落盘在同一把互斥锁内一次完成。
type Registry struct {
	dir    string
	store  *store
	state  *snapshot
	mu     sync.Mutex
	closed bool
	// now 返回当前时间，到期判断以此为准；生产环境为 time.Now，测试可替换。
	now func() time.Time
}

// Create 在 dir 新建并打开一个空登记册。调用者通过 dir 指定本机数据
// 位置；目录不存在会被创建。目录中已存在登记册数据时返回错误，
// 已有的数据不会被覆盖。
func Create(dir string) (*Registry, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: 数据目录不能为空", ErrInvalidArgument)
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("registry: 无法创建数据目录: %w", err)
	}
	if _, err := os.Stat(dataFile(dir)); err == nil {
		return nil, fmt.Errorf("%w: %s 已存在登记册数据，应用 Open 打开", ErrAlreadyExists, dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("registry: 无法访问数据目录: %w", err)
	}
	lock, err := acquireLock(dir)
	if err != nil {
		return nil, err
	}
	// 取锁后再确认一次，避免与另一个进程的 Create 竞争。
	if _, err := os.Stat(dataFile(dir)); err == nil {
		_ = releaseLock(lock)
		return nil, fmt.Errorf("%w: %s 已存在登记册数据", ErrAlreadyExists, dir)
	}
	st := &store{dir: dir, lock: lock}
	s := newSnapshot()
	if err := st.save(s); err != nil {
		_ = releaseLock(lock)
		return nil, err
	}
	return &Registry{dir: dir, store: st, state: s, now: time.Now}, nil
}

// Open 打开 dir 中的已有登记册。数据文件缺失视为空目录的新建场景，
// 应使用 Create；数据存在但无法读取或解析时返回包裹 ErrCorrupt 的
// 错误，不会被静默当成空登记册。
func Open(dir string) (*Registry, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: 数据目录不能为空", ErrInvalidArgument)
	}
	if info, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("registry: 无法打开数据目录: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s 不是目录", ErrInvalidArgument, dir)
	}
	lock, err := acquireLock(dir)
	if err != nil {
		return nil, err
	}
	// 数据文件不存在不是"空登记册"：明确报错，提示用 Create 新建，
	// 绝不把无数据目录静默当成空登记册。
	if _, err := os.Stat(dataFile(dir)); err != nil {
		_ = releaseLock(lock)
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s 中没有登记册数据，请用 Create 新建", ErrNotFound, dir)
		}
		return nil, fmt.Errorf("registry: 无法访问登记册数据: %w", err)
	}
	s, err := load(dir)
	if err != nil {
		_ = releaseLock(lock)
		return nil, err
	}
	return &Registry{dir: dir, store: &store{dir: dir, lock: lock}, state: s, now: time.Now}, nil
}

// Close 正常关闭登记册并释放独占锁。关闭后不能再调用其他方法。
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return releaseLock(r.store.lock)
}

func releaseLock(f *os.File) error {
	err := f.Close() // Close 同时释放 flock
	if err != nil {
		return fmt.Errorf("registry: 关闭登记册失败: %w", err)
	}
	return nil
}

func (r *Registry) checkOpen() error {
	if r.closed {
		return fmt.Errorf("registry: 登记册已关闭")
	}
	return nil
}

// commit 将当前状态原子落盘；落盘失败时以磁盘内容为准重新加载，
// 避免内存状态与已持久化状态不一致。
func (r *Registry) commit() error {
	if err := r.store.save(r.state); err != nil {
		if s, lerr := load(r.dir); lerr == nil {
			r.state = s
		}
		return err
	}
	return nil
}

// ---- 账户 ----

// RegisterAccount 以唯一编号登记账户，登记后即为可用状态。
func (r *Registry) RegisterAccount(id, metadata string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: 账户编号不能为空", ErrInvalidArgument)
	}
	if _, ok := r.state.Accounts[id]; ok {
		return fmt.Errorf("%w: 账户 %s", ErrAlreadyExists, id)
	}
	r.state.Accounts[id] = account{ID: id, Metadata: metadata, Active: true}
	return r.commit()
}

// DeactivateAccount 停用账户。停用后其原有藏品与历史仍可查询，但不能
// 再发行、发起或接收新的转让；停用不影响已经落盘的任何记录。
func (r *Registry) DeactivateAccount(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return err
	}
	a, ok := r.state.Accounts[id]
	if !ok {
		return fmt.Errorf("%w: 账户 %s", ErrNotFound, id)
	}
	if !a.Active {
		return nil // 停用是幂等的终态
	}
	a.Active = false
	r.state.Accounts[id] = a
	return r.commit()
}

// GetAccount 查询账户；账户不存在时返回包裹 ErrNotFound 的错误。
func (r *Registry) GetAccount(id string) (Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return Account{}, err
	}
	a, ok := r.state.Accounts[id]
	if !ok {
		return Account{}, fmt.Errorf("%w: 账户 %s", ErrNotFound, id)
	}
	return Account{ID: a.ID, Metadata: a.Metadata, Active: a.Active}, nil
}

// ---- 系列 ----

// CreateSeries 登记一个系列，记录创建账户与文字元数据。创建账户必须
// 已登记且可用；后续只有该账户能发行该系列的藏品或封存该系列。
func (r *Registry) CreateSeries(id, creatorID, metadata string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: 系列编号不能为空", ErrInvalidArgument)
	}
	creator, ok := r.state.Accounts[creatorID]
	if !ok {
		return fmt.Errorf("%w: 创建账户 %s", ErrNotFound, creatorID)
	}
	if !creator.Active {
		return fmt.Errorf("%w: 创建账户 %s", ErrAccountInactive, creatorID)
	}
	if _, ok := r.state.Series[id]; ok {
		return fmt.Errorf("%w: 系列 %s", ErrAlreadyExists, id)
	}
	r.state.Series[id] = series{ID: id, CreatorID: creatorID, Metadata: metadata}
	return r.commit()
}

// SealSeries 封存系列。只有系列创建账户可以封存；封存后不能继续发行，
// 已发行藏品仍可转让，且封存不能撤销。
func (r *Registry) SealSeries(seriesID, operator string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return err
	}
	s, ok := r.state.Series[seriesID]
	if !ok {
		return fmt.Errorf("%w: 系列 %s", ErrNotFound, seriesID)
	}
	if s.CreatorID != operator {
		return fmt.Errorf("%w: 只有创建账户 %s 可以封存系列 %s", ErrForbidden, s.CreatorID, seriesID)
	}
	if s.Sealed {
		return nil // 封存不可撤销，重复封存视为已处于封存态
	}
	s.Sealed = true
	r.state.Series[seriesID] = s
	return r.commit()
}

// GetSeries 查询系列；不存在时返回包裹 ErrNotFound 的错误。
func (r *Registry) GetSeries(id string) (Series, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return Series{}, err
	}
	s, ok := r.state.Series[id]
	if !ok {
		return Series{}, fmt.Errorf("%w: 系列 %s", ErrNotFound, id)
	}
	return Series{ID: s.ID, CreatorID: s.CreatorID, Metadata: s.Metadata, Sealed: s.Sealed}, nil
}

// ---- 发行 ----

func (req IssueRequest) validatePresent() error {
	missing := []string{}
	if strings.TrimSpace(req.Operator) == "" {
		missing = append(missing, "operator")
	}
	if strings.TrimSpace(req.Reason) == "" {
		missing = append(missing, "reason")
	}
	if strings.TrimSpace(req.RequestID) == "" {
		missing = append(missing, "request_id")
	}
	if strings.TrimSpace(req.ItemID) == "" {
		missing = append(missing, "item_id")
	}
	if strings.TrimSpace(req.SeriesID) == "" {
		missing = append(missing, "series_id")
	}
	if strings.TrimSpace(req.BatchNo) == "" {
		missing = append(missing, "batch_no")
	}
	if strings.TrimSpace(req.HolderID) == "" {
		missing = append(missing, "holder_id")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: 发行请求缺少必填字段 %s", ErrInvalidArgument, strings.Join(missing, ", "))
	}
	return nil
}

func issueParamsSig(req IssueRequest) string {
	b, _ := json.Marshal(struct {
		Kind     string `json:"kind"`
		ItemID   string `json:"item_id"`
		SeriesID string `json:"series_id"`
		BatchNo  string `json:"batch_no"`
		Metadata string `json:"metadata"`
		HolderID string `json:"holder_id"`
		Reason   string `json:"reason"`
	}{"issue", req.ItemID, req.SeriesID, req.BatchNo, req.Metadata, req.HolderID, req.Reason})
	return string(b)
}

// Issue 发行一件藏品。成功后持有版本为 1，初始持有人为 req.HolderID。
// 操作者必须是系列创建账户且可用，初始持有人必须已登记且可用，系列未
// 封存，藏品编号从未使用过。同一 (操作者, 请求号) 且业务参数相同的
// 重复提交返回首次结果；参数不同返回 ErrRequestConflict。
func (r *Registry) Issue(req IssueRequest) (IssueResult, error) {
	if err := req.validatePresent(); err != nil {
		return IssueResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return IssueResult{}, err
	}

	key := requestKey(req.Operator, req.RequestID)
	sig := issueParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayIssue(prev, sig)
	}

	bizErr := r.checkIssue(req)
	if bizErr != nil {
		if !isValidationErr(bizErr) {
			// 状态类业务拒绝（封存、停用、无权、编号已用等）占用请求号并
			// 落盘：相同参数重提永远返回这一次拒绝，即使状态后来变化。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "issue",
				Params: sig, Rejected: true, Reason: errCode(bizErr),
				ItemID: req.ItemID,
			}
			_ = r.commit()
		}
		// 校验类错误（引用不存在等）不占用请求号，也不改变任何状态。
		return IssueResult{ItemID: req.ItemID, Err: bizErr}, bizErr
	}

	seq := r.state.NextSeq + 1
	r.state.NextSeq = seq
	r.state.Items[req.ItemID] = item{
		ID: req.ItemID, SeriesID: req.SeriesID, BatchNo: req.BatchNo,
		Metadata: req.Metadata, IssuedTxID: seq,
	}
	r.state.Holdings[req.ItemID] = holding{ItemID: req.ItemID, OwnerID: req.HolderID, Version: 1}
	r.state.History = append(r.state.History, historyEntry{
		Seq: seq, Kind: "issue", ItemID: req.ItemID, Operator: req.Operator,
		Reason: req.Reason, RequestID: req.RequestID,
		FromID: "", ToID: req.HolderID, FromVersion: 0, ToVersion: 1,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "issue",
		Params: sig, ItemID: req.ItemID, ToID: req.HolderID, Version: 1, TxSeq: seq,
	}
	if err := r.commit(); err != nil {
		return IssueResult{}, err
	}
	return IssueResult{ItemID: req.ItemID, OwnerID: req.HolderID, Version: 1, TxSeq: seq}, nil
}

func (r *Registry) checkIssue(req IssueRequest) error {
	op, ok := r.state.Accounts[req.Operator]
	if !ok {
		return fmt.Errorf("%w: 操作者账户 %s", ErrNotFound, req.Operator)
	}
	if !op.Active {
		return fmt.Errorf("%w: 操作者账户 %s", ErrAccountInactive, req.Operator)
	}
	s, ok := r.state.Series[req.SeriesID]
	if !ok {
		return fmt.Errorf("%w: 系列 %s", ErrNotFound, req.SeriesID)
	}
	if s.Sealed {
		return fmt.Errorf("%w: 系列 %s", ErrSeriesSealed, req.SeriesID)
	}
	if s.CreatorID != req.Operator {
		return fmt.Errorf("%w: 只有系列创建账户 %s 可以发行", ErrForbidden, s.CreatorID)
	}
	if _, ok := r.state.Items[req.ItemID]; ok {
		return fmt.Errorf("%w: 藏品编号 %s 已被使用", ErrAlreadyExists, req.ItemID)
	}
	h, ok := r.state.Accounts[req.HolderID]
	if !ok {
		return fmt.Errorf("%w: 初始持有人账户 %s", ErrNotFound, req.HolderID)
	}
	if !h.Active {
		return fmt.Errorf("%w: 初始持有人账户 %s", ErrAccountInactive, req.HolderID)
	}
	return nil
}

func (r *Registry) replayIssue(prev request, sig string) (IssueResult, error) {
	if prev.Kind != "issue" || prev.Params != sig {
		return IssueResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := IssueResult{ItemID: prev.ItemID, OwnerID: prev.ToID, Version: prev.Version,
		TxSeq: prev.TxSeq, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		return res, res.Err
	}
	return res, nil
}

// ---- 转让 ----

func (req TransferRequest) validatePresent() error {
	missing := []string{}
	if strings.TrimSpace(req.Operator) == "" {
		missing = append(missing, "operator")
	}
	if strings.TrimSpace(req.Reason) == "" {
		missing = append(missing, "reason")
	}
	if strings.TrimSpace(req.RequestID) == "" {
		missing = append(missing, "request_id")
	}
	if strings.TrimSpace(req.ItemID) == "" {
		missing = append(missing, "item_id")
	}
	if strings.TrimSpace(req.ExpectedOwner) == "" {
		missing = append(missing, "expected_owner")
	}
	if strings.TrimSpace(req.ToID) == "" {
		missing = append(missing, "to_id")
	}
	if req.ExpectedVer <= 0 {
		missing = append(missing, "expected_version")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: 转让请求缺少必填字段 %s", ErrInvalidArgument, strings.Join(missing, ", "))
	}
	if req.Price < 0 {
		return fmt.Errorf("%w: 成交价款 %d 不能为负", ErrInvalidArgument, req.Price)
	}
	return nil
}

func transferParamsSig(req TransferRequest) string {
	// Price 用 omitempty：价款为 0 时签名与引入价款前的旧格式一致，
	// 旧登记册中落盘的请求仍可按原参数回放；改价则签名不同、按请求号
	// 冲突拒绝。
	b, _ := json.Marshal(struct {
		Kind          string `json:"kind"`
		ItemID        string `json:"item_id"`
		ExpectedOwner string `json:"expected_owner"`
		ExpectedVer   int64  `json:"expected_version"`
		ToID          string `json:"to_id"`
		Price         int64  `json:"price,omitempty"`
		Reason        string `json:"reason"`
	}{"transfer", req.ItemID, req.ExpectedOwner, req.ExpectedVer, req.ToID, req.Price, req.Reason})
	return string(b)
}

// Transfer 由当前持有人发起转让。接收人必须已登记、可用且不同于当前
// 持有人；请求必须给出期望持有人与期望版本，藏品不存在或持有人/版本
// 不符时返回业务拒绝且不改变任何状态。成交价款（分）由持有人填写，
// 非负、未填按 0；成功时按所属系列的版税规则一次落盘各收款账户的
// 应付明细与归转让前持有人的余款。同一 (操作者, 请求号) 的相同
// 请求重复提交（即使藏品后来已易手）返回首次结果。
func (r *Registry) Transfer(req TransferRequest) (TransferResult, error) {
	if err := req.validatePresent(); err != nil {
		return TransferResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return TransferResult{}, err
	}

	key := requestKey(req.Operator, req.RequestID)
	sig := transferParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayTransfer(prev, sig)
	}

	bizErr := r.checkTransfer(req)
	if bizErr != nil {
		if !isValidationErr(bizErr) {
			// 状态类业务拒绝（持有人/版本不符、账户停用、收发同人等）
			// 占用请求号并落盘，相同参数重提永远返回这一次拒绝。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "transfer",
				Params: sig, Rejected: true, Reason: errCode(bizErr),
				ItemID: req.ItemID,
			}
			_ = r.commit()
		}
		// 藏品或账户不存在等校验错误不占用请求号，也不改变持有或历史。
		return TransferResult{ItemID: req.ItemID, Err: bizErr}, bizErr
	}

	// 持有变化、历史、版税应付与请求结果在同一临界区内一次落盘：直接
	// 转让不关联授权，AuthID 为空。
	done := r.applyTransfer(transferMove{
		ItemID: req.ItemID, ToID: req.ToID, Operator: req.Operator,
		Reason: req.Reason, RequestID: req.RequestID, Price: req.Price,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "transfer",
		Params: sig, ItemID: done.ItemID, FromID: done.FromID, ToID: done.ToID,
		Version: done.Version, TxSeq: done.TxSeq,
	}
	if err := r.commit(); err != nil {
		return TransferResult{}, err
	}
	return TransferResult{ItemID: done.ItemID, FromID: done.FromID, ToID: done.ToID,
		Version: done.Version, TxSeq: done.TxSeq,
		Price: done.Price, Payables: done.Payables, Remainder: done.Remainder}, nil
}

func (r *Registry) checkTransfer(req TransferRequest) error {
	h, ok := r.state.Holdings[req.ItemID]
	if !ok {
		if _, itemExists := r.state.Items[req.ItemID]; !itemExists {
			return fmt.Errorf("%w: 藏品 %s", ErrNotFound, req.ItemID)
		}
		return fmt.Errorf("%w: 藏品 %s 没有持有记录", ErrNotFound, req.ItemID)
	}
	op, ok := r.state.Accounts[req.Operator]
	if !ok {
		return fmt.Errorf("%w: 操作者账户 %s", ErrNotFound, req.Operator)
	}
	if !op.Active {
		return fmt.Errorf("%w: 操作者账户 %s", ErrAccountInactive, req.Operator)
	}
	to, ok := r.state.Accounts[req.ToID]
	if !ok {
		return fmt.Errorf("%w: 接收账户 %s", ErrNotFound, req.ToID)
	}
	if !to.Active {
		return fmt.Errorf("%w: 接收账户 %s", ErrAccountInactive, req.ToID)
	}
	if req.ToID == h.OwnerID {
		return fmt.Errorf("%w: 接收人 %s 已是当前持有人", ErrSameAccount, req.ToID)
	}
	// 期望持有人或期望版本不符，包括发起人并非当前持有人的情况。
	if h.OwnerID != req.ExpectedOwner || h.Version != req.ExpectedVer ||
		req.Operator != h.OwnerID {
		return fmt.Errorf("%w: 藏品 %s 当前为 %s 版本 %d", ErrConflict,
			req.ItemID, h.OwnerID, h.Version)
	}
	return nil
}

func (r *Registry) replayTransfer(prev request, sig string) (TransferResult, error) {
	if prev.Kind != "transfer" || prev.Params != sig {
		return TransferResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := TransferResult{ItemID: prev.ItemID, FromID: prev.FromID, ToID: prev.ToID,
		Version: prev.Version, TxSeq: prev.TxSeq, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		return res, res.Err
	}
	// 回放首次成功的金额：即使藏品后来再次易手或进程重开，仍返回原
	// 价款与应付明细，不重复计入。
	res.Price, res.Payables, res.Remainder = r.royaltyOfTx(prev.TxSeq)
	return res, nil
}

// ---- 限时一次性代转授权 ----

// authzCurrentStatus 计算授权当前状态。撤销与使用是落盘的终态；到期不
// 落盘，按当前时间实时判断——从到期时间点起即视为已过期。
func authzCurrentStatus(a authzRec, now time.Time) string {
	switch a.Status {
	case "revoked":
		return AuthRevoked
	case "used":
		return AuthUsed
	}
	if !a.ExpiresAt.IsZero() && !now.Before(a.ExpiresAt) {
		return AuthExpired
	}
	return AuthActive
}

func (req CreateAuthorizationRequest) validatePresent() error {
	missing := []string{}
	if strings.TrimSpace(req.Operator) == "" {
		missing = append(missing, "operator")
	}
	if strings.TrimSpace(req.Reason) == "" {
		missing = append(missing, "reason")
	}
	if strings.TrimSpace(req.RequestID) == "" {
		missing = append(missing, "request_id")
	}
	if strings.TrimSpace(req.AuthID) == "" {
		missing = append(missing, "auth_id")
	}
	if strings.TrimSpace(req.ItemID) == "" {
		missing = append(missing, "item_id")
	}
	if strings.TrimSpace(req.TrusteeID) == "" {
		missing = append(missing, "trustee_id")
	}
	if strings.TrimSpace(req.ToID) == "" {
		missing = append(missing, "to_id")
	}
	if strings.TrimSpace(req.ExpectedOwner) == "" {
		missing = append(missing, "expected_owner")
	}
	if req.ExpectedVer <= 0 {
		missing = append(missing, "expected_version")
	}
	if req.ExpiresAt.IsZero() {
		missing = append(missing, "expires_at")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: 创建授权请求缺少必填字段 %s", ErrInvalidArgument, strings.Join(missing, ", "))
	}
	if req.Price < 0 {
		return fmt.Errorf("%w: 成交价款 %d 不能为负", ErrInvalidArgument, req.Price)
	}
	return nil
}

func createAuthzParamsSig(req CreateAuthorizationRequest) string {
	// Price 用 omitempty：价款为 0 时签名与旧格式一致，旧授权创建请求
	// 仍可回放；改价则按请求号冲突拒绝。
	b, _ := json.Marshal(struct {
		Kind          string `json:"kind"`
		AuthID        string `json:"auth_id"`
		ItemID        string `json:"item_id"`
		TrusteeID     string `json:"trustee_id"`
		ToID          string `json:"to_id"`
		ExpectedOwner string `json:"expected_owner"`
		ExpectedVer   int64  `json:"expected_version"`
		ExpiresAt     string `json:"expires_at"`
		Price         int64  `json:"price,omitempty"`
		Reason        string `json:"reason"`
	}{"auth_create", req.AuthID, req.ItemID, req.TrusteeID, req.ToID,
		req.ExpectedOwner, req.ExpectedVer,
		req.ExpiresAt.UTC().Format(time.RFC3339Nano), req.Price, req.Reason})
	return string(b)
}

// CreateAuthorization 由当前持有人为一件已发行藏品创建限时、一次性的
// 代转授权：指定唯一授权编号、受托账户、固定接收账户、绝对到期时间，
// 以及期望持有人与期望版本。授权绑定创建时的持有版本，不改变持有关系，
// 也不限制持有人继续直接转让。受托人不能是授权人；受托人可与接收人
// 相同，但接收人也不能是授权人。到期时间必须晚于当前时间。
//
// 请求号与发行、转让共用同一操作者的请求号范围：相同业务参数重提返回
// 首次结果（成功或状态类业务拒绝），参数变化返回 ErrRequestConflict；
// 参数错误与引用不存在不占用请求号。
func (r *Registry) CreateAuthorization(req CreateAuthorizationRequest) (CreateAuthorizationResult, error) {
	if err := req.validatePresent(); err != nil {
		return CreateAuthorizationResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return CreateAuthorizationResult{}, err
	}

	now := r.now()
	key := requestKey(req.Operator, req.RequestID)
	sig := createAuthzParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayCreateAuthz(prev, sig)
	}

	if bizErr := r.checkCreateAuthorization(req, now); bizErr != nil {
		if !isValidationErr(bizErr) {
			// 状态类业务拒绝（编号占用、停用、同人、版本不符、到期时间
			// 已过等）占用请求号并落盘；参数错误与引用不存在不占用。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "auth_create",
				Params: sig, Rejected: true, Reason: errCode(bizErr),
				AuthID: req.AuthID, ItemID: req.ItemID,
			}
			_ = r.commit()
		}
		return CreateAuthorizationResult{AuthID: req.AuthID, Err: bizErr}, bizErr
	}

	h := r.state.Holdings[req.ItemID]
	a := authzRec{
		ID: req.AuthID, ItemID: req.ItemID, GranterID: req.Operator,
		TrusteeID: req.TrusteeID, ToID: req.ToID, ExpiresAt: req.ExpiresAt,
		GrantVer: h.Version, Price: req.Price, CreatedAt: now,
	}
	r.state.Authzs[req.AuthID] = a
	authSeq := r.state.NextAuthSeq + 1
	r.state.NextAuthSeq = authSeq
	r.state.AuthEvents = append(r.state.AuthEvents, authzEvent{
		Seq: authSeq, AuthID: req.AuthID, ItemID: req.ItemID, Kind: "create",
		Operator: req.Operator, Reason: req.Reason, RequestID: req.RequestID,
		FromStatus: "", ToStatus: AuthActive, OccurredAt: now,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "auth_create",
		Params: sig, AuthID: req.AuthID, ItemID: req.ItemID, Version: h.Version,
	}
	if err := r.commit(); err != nil {
		return CreateAuthorizationResult{}, err
	}
	return CreateAuthorizationResult{AuthID: req.AuthID, Status: AuthActive, GrantVer: h.Version}, nil
}

func (r *Registry) checkCreateAuthorization(req CreateAuthorizationRequest, now time.Time) error {
	h, ok := r.state.Holdings[req.ItemID]
	if !ok {
		if _, itemExists := r.state.Items[req.ItemID]; !itemExists {
			return fmt.Errorf("%w: 藏品 %s", ErrNotFound, req.ItemID)
		}
		return fmt.Errorf("%w: 藏品 %s 没有持有记录", ErrNotFound, req.ItemID)
	}
	op, ok := r.state.Accounts[req.Operator]
	if !ok {
		return fmt.Errorf("%w: 授权人账户 %s", ErrNotFound, req.Operator)
	}
	trustee, ok := r.state.Accounts[req.TrusteeID]
	if !ok {
		return fmt.Errorf("%w: 受托人账户 %s", ErrNotFound, req.TrusteeID)
	}
	to, ok := r.state.Accounts[req.ToID]
	if !ok {
		return fmt.Errorf("%w: 接收账户 %s", ErrNotFound, req.ToID)
	}
	if req.TrusteeID == req.Operator {
		return fmt.Errorf("%w: 受托人不能是授权人 %s", ErrSameAccount, req.Operator)
	}
	if req.ToID == req.Operator {
		return fmt.Errorf("%w: 接收人不能是授权人 %s", ErrSameAccount, req.Operator)
	}
	if !op.Active {
		return fmt.Errorf("%w: 授权人账户 %s", ErrAccountInactive, req.Operator)
	}
	if !trustee.Active {
		return fmt.Errorf("%w: 受托人账户 %s", ErrAccountInactive, req.TrusteeID)
	}
	if !to.Active {
		return fmt.Errorf("%w: 接收账户 %s", ErrAccountInactive, req.ToID)
	}
	// 授权编号占用是对象级冲突，优先于当前时间与持有版本检查。
	if _, exists := r.state.Authzs[req.AuthID]; exists {
		return fmt.Errorf("%w: 授权编号 %s 已被占用", ErrAlreadyExists, req.AuthID)
	}
	// 到期时间不晚于当前时间即为参数错误；从到期时间点起不可使用。
	if !req.ExpiresAt.After(now) {
		return fmt.Errorf("%w: 到期时间 %s 必须晚于当前时间",
			ErrInvalidArgument, req.ExpiresAt.Format(time.RFC3339Nano))
	}
	if h.OwnerID != req.ExpectedOwner || h.Version != req.ExpectedVer ||
		req.Operator != h.OwnerID {
		return fmt.Errorf("%w: 藏品 %s 当前为 %s 版本 %d",
			ErrConflict, req.ItemID, h.OwnerID, h.Version)
	}
	return nil
}

func (r *Registry) replayCreateAuthz(prev request, sig string) (CreateAuthorizationResult, error) {
	if prev.Kind != "auth_create" || prev.Params != sig {
		return CreateAuthorizationResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := CreateAuthorizationResult{AuthID: prev.AuthID, GrantVer: prev.Version,
		Status: AuthActive, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		return res, res.Err
	}
	return res, nil
}

func revokeAuthzParamsSig(req RevokeAuthorizationRequest) string {
	b, _ := json.Marshal(struct {
		Kind   string `json:"kind"`
		AuthID string `json:"auth_id"`
		Reason string `json:"reason"`
	}{"auth_revoke", req.AuthID, req.Reason})
	return string(b)
}

// RevokeAuthorization 撤销一份尚未使用的代转授权。仅授权人可以撤销；
// 已使用的授权不能撤销（返回 ErrAuthorizationUsed）。授权已撤销时再次
// 撤销不新增授权变更记录，按成功返回。请求号语义与其他操作一致。
func (r *Registry) RevokeAuthorization(req RevokeAuthorizationRequest) (RevokeAuthorizationResult, error) {
	missing := []string{}
	if strings.TrimSpace(req.Operator) == "" {
		missing = append(missing, "operator")
	}
	if strings.TrimSpace(req.Reason) == "" {
		missing = append(missing, "reason")
	}
	if strings.TrimSpace(req.RequestID) == "" {
		missing = append(missing, "request_id")
	}
	if strings.TrimSpace(req.AuthID) == "" {
		missing = append(missing, "auth_id")
	}
	if len(missing) > 0 {
		return RevokeAuthorizationResult{}, fmt.Errorf("%w: 撤销授权请求缺少必填字段 %s",
			ErrInvalidArgument, strings.Join(missing, ", "))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return RevokeAuthorizationResult{}, err
	}

	now := r.now()
	key := requestKey(req.Operator, req.RequestID)
	sig := revokeAuthzParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayRevokeAuthz(prev, sig)
	}

	a, ok := r.state.Authzs[req.AuthID]
	if !ok {
		// 引用不存在不占用请求号。
		err := fmt.Errorf("%w: 授权 %s", ErrNotFound, req.AuthID)
		return RevokeAuthorizationResult{AuthID: req.AuthID, Err: err}, err
	}
	if a.GranterID != req.Operator {
		bizErr := fmt.Errorf("%w: 只有授权人 %s 可以撤销授权 %s",
			ErrForbidden, a.GranterID, req.AuthID)
		r.recordAuthzRequest(key, sig, req, "auth_revoke", bizErr)
		return RevokeAuthorizationResult{AuthID: req.AuthID, Err: bizErr}, bizErr
	}
	switch authzCurrentStatus(a, now) {
	case AuthUsed:
		bizErr := fmt.Errorf("%w: 授权 %s 已使用，不能撤销", ErrAuthorizationUsed, req.AuthID)
		r.recordAuthzRequest(key, sig, req, "auth_revoke", bizErr)
		return RevokeAuthorizationResult{AuthID: req.AuthID, Err: bizErr}, bizErr
	case AuthRevoked:
		// 已撤销是幂等终态：不新增授权变更记录；该请求号仍登记为成功，
		// 相同请求重放回放同一结果。
		r.state.Requests[key] = request{
			Operator: req.Operator, RequestID: req.RequestID, Kind: "auth_revoke",
			Params: sig, AuthID: req.AuthID,
		}
		if err := r.commit(); err != nil {
			return RevokeAuthorizationResult{}, err
		}
		return RevokeAuthorizationResult{AuthID: req.AuthID, Status: AuthRevoked}, nil
	}

	from := AuthActive
	if authzCurrentStatus(a, now) == AuthExpired {
		from = AuthExpired
	}
	a.Status = "revoked"
	a.RevokedAt = now
	r.state.Authzs[req.AuthID] = a
	authSeq := r.state.NextAuthSeq + 1
	r.state.NextAuthSeq = authSeq
	r.state.AuthEvents = append(r.state.AuthEvents, authzEvent{
		Seq: authSeq, AuthID: req.AuthID, ItemID: a.ItemID, Kind: "revoke",
		Operator: req.Operator, Reason: req.Reason, RequestID: req.RequestID,
		FromStatus: from, ToStatus: AuthRevoked, OccurredAt: now,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "auth_revoke",
		Params: sig, AuthID: req.AuthID,
	}
	if err := r.commit(); err != nil {
		return RevokeAuthorizationResult{}, err
	}
	return RevokeAuthorizationResult{AuthID: req.AuthID, Status: AuthRevoked}, nil
}

// recordAuthzRequest 记录授权类操作的状态类业务拒绝并尽力落盘。
func (r *Registry) recordAuthzRequest(key, sig string, req RevokeAuthorizationRequest, kind string, bizErr error) {
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: kind,
		Params: sig, Rejected: true, Reason: errCode(bizErr), AuthID: req.AuthID,
	}
	_ = r.commit()
}

func (r *Registry) replayRevokeAuthz(prev request, sig string) (RevokeAuthorizationResult, error) {
	if prev.Kind != "auth_revoke" || prev.Params != sig {
		return RevokeAuthorizationResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := RevokeAuthorizationResult{AuthID: prev.AuthID, Status: AuthRevoked, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		res.Status = ""
		return res, res.Err
	}
	return res, nil
}

func proxyTransferParamsSig(req ProxyTransferRequest) string {
	b, _ := json.Marshal(struct {
		Kind   string `json:"kind"`
		AuthID string `json:"auth_id"`
		Reason string `json:"reason"`
	}{"proxy_transfer", req.AuthID, req.Reason})
	return string(b)
}

// ProxyTransfer 由受托人凭授权发起一次性代转。藏品与接收人均以授权
// 记载为准，请求本身不能指定。成功后持有人变为授权中的接收人、持有
// 版本加一，授权记为已使用并关联该笔转让；代转同样出现在藏品历史中，
// 操作者记为实际受托账户并可通过授权编号追溯。
//
// 非受托人（ErrForbidden）、授权已撤销（ErrAuthorizationRevoked）、
// 已到期（ErrAuthorizationExpired）、已使用（ErrAuthorizationUsed）、
// 任一相关账户停用（ErrAccountInactive）以及持有版本已变化
// （ErrConflict，即使藏品回到授权人手中也不恢复）分别明确拒绝，拒绝
// 不改变持有或授权状态。成功代转用原请求号重放时，即使授权已到期、
// 账户已停用或藏品再次易手，仍返回首次的转让结果。
func (r *Registry) ProxyTransfer(req ProxyTransferRequest) (ProxyTransferResult, error) {
	missing := []string{}
	if strings.TrimSpace(req.Operator) == "" {
		missing = append(missing, "operator")
	}
	if strings.TrimSpace(req.Reason) == "" {
		missing = append(missing, "reason")
	}
	if strings.TrimSpace(req.RequestID) == "" {
		missing = append(missing, "request_id")
	}
	if strings.TrimSpace(req.AuthID) == "" {
		missing = append(missing, "auth_id")
	}
	if len(missing) > 0 {
		return ProxyTransferResult{}, fmt.Errorf("%w: 代转请求缺少必填字段 %s",
			ErrInvalidArgument, strings.Join(missing, ", "))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return ProxyTransferResult{}, err
	}

	now := r.now()
	key := requestKey(req.Operator, req.RequestID)
	sig := proxyTransferParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayProxyTransfer(prev, sig)
	}

	if bizErr := r.checkProxyTransfer(req, now); bizErr != nil {
		if !isValidationErr(bizErr) {
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "proxy_transfer",
				Params: sig, Rejected: true, Reason: errCode(bizErr), AuthID: req.AuthID,
			}
			_ = r.commit()
		}
		return ProxyTransferResult{AuthID: req.AuthID, Err: bizErr}, bizErr
	}

	a := r.state.Authzs[req.AuthID]
	// 持有、历史与版税应付与直接转让走同一套落盘逻辑：操作者是受托账户，
	// 历史通过 AuthID 追溯本授权，价款以授权创建时写定的金额为准，余款归
	// 转让前持有人（授权人）。
	done := r.applyTransfer(transferMove{
		ItemID: a.ItemID, ToID: a.ToID, Operator: req.Operator,
		Reason: req.Reason, RequestID: req.RequestID, Price: a.Price,
		AuthID: a.ID,
	})
	// 只有实际执行代转才把授权记为已使用并关联本次转让；直接转让与整批
	// 转让不经过这里，不会替本授权消耗。授权使用状态与持有、历史、应付、
	// 请求结果在同一临界区内一次落盘。
	a.Status = "used"
	a.UsedTxSeq = done.TxSeq
	a.UsedAt = now
	r.state.Authzs[a.ID] = a
	authSeq := r.state.NextAuthSeq + 1
	r.state.NextAuthSeq = authSeq
	r.state.AuthEvents = append(r.state.AuthEvents, authzEvent{
		Seq: authSeq, AuthID: a.ID, ItemID: a.ItemID, Kind: "use",
		Operator: req.Operator, Reason: req.Reason, RequestID: req.RequestID,
		FromStatus: AuthActive, ToStatus: AuthUsed, TxSeq: done.TxSeq, OccurredAt: now,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "proxy_transfer",
		Params: sig, AuthID: a.ID, ItemID: done.ItemID, FromID: done.FromID,
		ToID: done.ToID, Version: done.Version, TxSeq: done.TxSeq,
	}
	if err := r.commit(); err != nil {
		return ProxyTransferResult{}, err
	}
	return ProxyTransferResult{AuthID: a.ID, ItemID: done.ItemID, FromID: done.FromID,
		ToID: done.ToID, Version: done.Version, TxSeq: done.TxSeq,
		Price: done.Price, Payables: done.Payables, Remainder: done.Remainder}, nil
}

func (r *Registry) checkProxyTransfer(req ProxyTransferRequest, now time.Time) error {
	a, ok := r.state.Authzs[req.AuthID]
	if !ok {
		return fmt.Errorf("%w: 授权 %s", ErrNotFound, req.AuthID)
	}
	op, ok := r.state.Accounts[req.Operator]
	if !ok {
		return fmt.Errorf("%w: 受托人账户 %s", ErrNotFound, req.Operator)
	}
	granter, gOk := r.state.Accounts[a.GranterID]
	to, tOk := r.state.Accounts[a.ToID]
	if req.Operator != a.TrusteeID {
		return fmt.Errorf("%w: 账户 %s 不是授权 %s 的受托人",
			ErrForbidden, req.Operator, req.AuthID)
	}
	if !gOk {
		return fmt.Errorf("%w: 授权人账户 %s", ErrNotFound, a.GranterID)
	}
	if !tOk {
		return fmt.Errorf("%w: 接收账户 %s", ErrNotFound, a.ToID)
	}
	if !op.Active {
		return fmt.Errorf("%w: 受托人账户 %s", ErrAccountInactive, req.Operator)
	}
	if !granter.Active {
		return fmt.Errorf("%w: 授权人账户 %s", ErrAccountInactive, a.GranterID)
	}
	if !to.Active {
		return fmt.Errorf("%w: 接收账户 %s", ErrAccountInactive, a.ToID)
	}
	switch authzCurrentStatus(a, now) {
	case AuthRevoked:
		return fmt.Errorf("%w: 授权 %s", ErrAuthorizationRevoked, req.AuthID)
	case AuthExpired:
		return fmt.Errorf("%w: 授权 %s", ErrAuthorizationExpired, req.AuthID)
	case AuthUsed:
		return fmt.Errorf("%w: 授权 %s", ErrAuthorizationUsed, req.AuthID)
	}
	// 授权只在创建时绑定的持有版本上有效；版本单调递增，藏品即使回到
	// 授权人手中也不会回到旧版本，旧授权据此永久失效。
	h, ok := r.state.Holdings[a.ItemID]
	if !ok {
		return fmt.Errorf("%w: 藏品 %s 没有持有记录", ErrNotFound, a.ItemID)
	}
	if h.OwnerID != a.GranterID || h.Version != a.GrantVer {
		return fmt.Errorf("%w: 藏品 %s 当前为 %s 版本 %d，授权绑定 %s 版本 %d",
			ErrConflict, a.ItemID, h.OwnerID, h.Version, a.GranterID, a.GrantVer)
	}
	return nil
}

func (r *Registry) replayProxyTransfer(prev request, sig string) (ProxyTransferResult, error) {
	if prev.Kind != "proxy_transfer" || prev.Params != sig {
		return ProxyTransferResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := ProxyTransferResult{AuthID: prev.AuthID, ItemID: prev.ItemID, FromID: prev.FromID,
		ToID: prev.ToID, Version: prev.Version, TxSeq: prev.TxSeq, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		return res, res.Err
	}
	// 回放首次成功的金额：授权到期、账户停用或藏品再次易手后仍返回
	// 原价款与应付明细，不重复计入。
	res.Price, res.Payables, res.Remainder = r.royaltyOfTx(prev.TxSeq)
	return res, nil
}

// ---- 授权查询 ----

// GetAuthorization 按编号查询授权内容与撤销、使用状态。授权不存在时
// 返回包裹 ErrNotFound 的错误；返回的 Status 按当前时间实时计算，
// 到期但尚未终结的授权返回 AuthExpired。
func (r *Registry) GetAuthorization(authID string) (Authorization, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return Authorization{}, err
	}
	a, ok := r.state.Authzs[authID]
	if !ok {
		return Authorization{}, fmt.Errorf("%w: 授权 %s", ErrNotFound, authID)
	}
	return Authorization{
		ID: a.ID, ItemID: a.ItemID, GranterID: a.GranterID, TrusteeID: a.TrusteeID,
		ToID: a.ToID, ExpiresAt: a.ExpiresAt, GrantVer: a.GrantVer, Price: a.Price,
		Status:    authzCurrentStatus(a, r.now()),
		UsedTxSeq: a.UsedTxSeq, UsedAt: a.UsedAt, CreatedAt: a.CreatedAt,
		RevokedAt: a.RevokedAt,
	}, nil
}

// AuthorizationHistory 按藏品查看授权变更记录（创建、撤销、使用），
// 按发生先后排列；每条记录包含操作者、原因、请求号与前后状态。藏品
// 不存在时返回包裹 ErrNotFound 的错误。
func (r *Registry) AuthorizationHistory(itemID string) ([]AuthorizationEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return nil, err
	}
	if _, ok := r.state.Items[itemID]; !ok {
		return nil, fmt.Errorf("%w: 藏品 %s", ErrNotFound, itemID)
	}
	out := make([]AuthorizationEvent, 0)
	for _, e := range r.state.AuthEvents {
		if e.ItemID != itemID {
			continue
		}
		out = append(out, AuthorizationEvent{
			Seq: e.Seq, AuthID: e.AuthID, ItemID: e.ItemID, Kind: e.Kind,
			Operator: e.Operator, Reason: e.Reason, RequestID: e.RequestID,
			FromStatus: e.FromStatus, ToStatus: e.ToStatus, TxSeq: e.TxSeq,
			OccurredAt: e.OccurredAt,
		})
	}
	return out, nil
}

// ---- 查询 ----

// GetItem 查询单件藏品的静态登记信息；不存在时返回包裹 ErrNotFound 的错误。
func (r *Registry) GetItem(id string) (Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return Item{}, err
	}
	it, ok := r.state.Items[id]
	if !ok {
		return Item{}, fmt.Errorf("%w: 藏品 %s", ErrNotFound, id)
	}
	return Item{ID: it.ID, SeriesID: it.SeriesID, BatchNo: it.BatchNo,
		Metadata: it.Metadata, IssuedTxID: it.IssuedTxID}, nil
}

// GetHolding 查询藏品当前持有状态；藏品不存在时返回包裹 ErrNotFound 的错误。
func (r *Registry) GetHolding(itemID string) (Holding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return Holding{}, err
	}
	h, ok := r.state.Holdings[itemID]
	if !ok {
		return Holding{}, fmt.Errorf("%w: 藏品 %s", ErrNotFound, itemID)
	}
	return Holding{ItemID: h.ItemID, OwnerID: h.OwnerID, Version: h.Version}, nil
}

// HoldingsOf 查询某账户当前持有的全部藏品。账户不存在时返回包裹
// ErrNotFound 的错误；停用账户仍可正常查询。
func (r *Registry) HoldingsOf(accountID string) ([]Holding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return nil, err
	}
	if _, ok := r.state.Accounts[accountID]; !ok {
		return nil, fmt.Errorf("%w: 账户 %s", ErrNotFound, accountID)
	}
	out := make([]Holding, 0)
	for _, h := range r.state.Holdings {
		if h.OwnerID == accountID {
			out = append(out, Holding{ItemID: h.ItemID, OwnerID: h.OwnerID, Version: h.Version})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ItemID < out[j].ItemID })
	return out, nil
}

// History 查询藏品的发行与历次成功转让，按发生先后排列。发行记录的
// 前持有人为空、前版本为 0。重复提交不会产生额外历史。藏品不存在时
// 返回包裹 ErrNotFound 的错误。
func (r *Registry) History(itemID string) ([]HistoryEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return nil, err
	}
	if _, ok := r.state.Items[itemID]; !ok {
		return nil, fmt.Errorf("%w: 藏品 %s", ErrNotFound, itemID)
	}
	out := make([]HistoryEntry, 0)
	for _, e := range r.state.History {
		if e.ItemID != itemID {
			continue
		}
		out = append(out, HistoryEntry{
			Seq: e.Seq, Kind: e.Kind, ItemID: e.ItemID, Operator: e.Operator,
			Reason: e.Reason, RequestID: e.RequestID, FromID: e.FromID, ToID: e.ToID,
			FromVersion: e.FromVersion, ToVersion: e.ToVersion, AuthID: e.AuthID,
		})
	}
	return out, nil
}

// ---- 幂等辅助 ----

// requestKey 由完整的操作者账户编号与完整的请求号共同确定一条请求记录。
// 采用 JSON 数组编码而非直接拼接：账户编号或请求号本身允许包含零字符
// （U+0000）等任意字符，直接拼接会让 ("a", "b\x00c") 与 ("a\x00b", "c")
// 之类不同的组合得到相同的键，导致不同账户互相占用请求号、回放别人的
// 结果。JSON 编码对任意字符都无歧义，两个组成部分各自完整参与键的构成。
func requestKey(operator, requestID string) string {
	b, _ := json.Marshal([2]string{operator, requestID})
	return string(b)
}

// isValidationErr 判定是否为请求本身不成立的错误：必填缺失，或引用了
// 不存在的账户、系列、藏品。这类请求不占用请求号、不落任何记录，引用
// 对象后来补建后仍可用原请求号完整执行一次；其余拒绝（封存、停用、
// 版本冲突、无权、编号已用、收发同人等状态类业务拒绝）按首次结果记忆
// 并回放。
func isValidationErr(err error) bool {
	return errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrNotFound)
}

var errCodes = map[error]string{
	ErrNotFound:             "not_found",
	ErrAlreadyExists:        "already_exists",
	ErrAccountInactive:      "account_inactive",
	ErrSeriesSealed:         "series_sealed",
	ErrConflict:             "conflict",
	ErrSameAccount:          "same_account",
	ErrForbidden:            "forbidden",
	ErrAuthorizationRevoked: "authorization_revoked",
	ErrAuthorizationExpired: "authorization_expired",
	ErrAuthorizationUsed:    "authorization_used",
	ErrRoyaltyFrozen:        "royalty_frozen",
	ErrSplitIntentRejected:  "split_intent_rejected",
	ErrSplitIntentWithdrawn: "split_intent_withdrawn",
	ErrSplitIntentInvalid:   "split_intent_invalid",
	ErrSplitIntentExpired:   "split_intent_expired",
	ErrSplitAnswered:        "split_answered",
}

var codeErrs = map[string]error{
	"not_found":              ErrNotFound,
	"already_exists":         ErrAlreadyExists,
	"account_inactive":       ErrAccountInactive,
	"series_sealed":          ErrSeriesSealed,
	"conflict":               ErrConflict,
	"same_account":           ErrSameAccount,
	"forbidden":              ErrForbidden,
	"authorization_revoked":  ErrAuthorizationRevoked,
	"authorization_expired":  ErrAuthorizationExpired,
	"authorization_used":     ErrAuthorizationUsed,
	"royalty_frozen":         ErrRoyaltyFrozen,
	"split_intent_rejected":  ErrSplitIntentRejected,
	"split_intent_withdrawn": ErrSplitIntentWithdrawn,
	"split_intent_invalid":   ErrSplitIntentInvalid,
	"split_intent_expired":   ErrSplitIntentExpired,
	"split_answered":         ErrSplitAnswered,
}

func errCode(err error) string {
	for sentinel, code := range errCodes {
		if errors.Is(err, sentinel) {
			return code
		}
	}
	return "rejected"
}

func codeErr(code string) error {
	if e, ok := codeErrs[code]; ok {
		return e
	}
	return errors.New("registry: 请求曾被拒绝")
}
