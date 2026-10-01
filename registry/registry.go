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
	return &Registry{dir: dir, store: st, state: s}, nil
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
	return &Registry{dir: dir, store: &store{dir: dir, lock: lock}, state: s}, nil
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
	return nil
}

func transferParamsSig(req TransferRequest) string {
	b, _ := json.Marshal(struct {
		Kind          string `json:"kind"`
		ItemID        string `json:"item_id"`
		ExpectedOwner string `json:"expected_owner"`
		ExpectedVer   int64  `json:"expected_version"`
		ToID          string `json:"to_id"`
		Reason        string `json:"reason"`
	}{"transfer", req.ItemID, req.ExpectedOwner, req.ExpectedVer, req.ToID, req.Reason})
	return string(b)
}

// Transfer 由当前持有人发起转让。接收人必须已登记、可用且不同于当前
// 持有人；请求必须给出期望持有人与期望版本，藏品不存在或持有人/版本
// 不符时返回业务拒绝且不改变任何状态。同一 (操作者, 请求号) 的相同
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

	h := r.state.Holdings[req.ItemID]
	seq := r.state.NextSeq + 1
	r.state.NextSeq = seq
	from := h.OwnerID
	fromVer := h.Version
	h.OwnerID = req.ToID
	h.Version = fromVer + 1
	r.state.Holdings[req.ItemID] = h
	r.state.History = append(r.state.History, historyEntry{
		Seq: seq, Kind: "transfer", ItemID: req.ItemID, Operator: req.Operator,
		Reason: req.Reason, RequestID: req.RequestID,
		FromID: from, ToID: req.ToID, FromVersion: fromVer, ToVersion: fromVer + 1,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "transfer",
		Params: sig, ItemID: req.ItemID, FromID: from, ToID: req.ToID,
		Version: fromVer + 1, TxSeq: seq,
	}
	if err := r.commit(); err != nil {
		return TransferResult{}, err
	}
	return TransferResult{ItemID: req.ItemID, FromID: from, ToID: req.ToID,
		Version: fromVer + 1, TxSeq: seq}, nil
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
	return res, nil
}

// ---- 代转授权 ----

func (req CreateAuthRequest) validatePresent() error {
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
	if strings.TrimSpace(req.Trustee) == "" {
		missing = append(missing, "trustee")
	}
	if strings.TrimSpace(req.Receiver) == "" {
		missing = append(missing, "receiver")
	}
	if strings.TrimSpace(req.ExpectedOwner) == "" {
		missing = append(missing, "expected_owner")
	}
	if req.ExpectedVer <= 0 {
		missing = append(missing, "expected_version")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: 授权请求缺少必填字段 %s", ErrInvalidArgument, strings.Join(missing, ", "))
	}
	// 到期时间按实际时间点判断：不晚于当前时间点的授权不能创建。
	if !req.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("%w: 授权到期时间必须晚于当前时间", ErrInvalidArgument)
	}
	return nil
}

func createAuthParamsSig(req CreateAuthRequest) string {
	b, _ := json.Marshal(struct {
		Kind          string `json:"kind"`
		AuthID        string `json:"auth_id"`
		ItemID        string `json:"item_id"`
		Trustee       string `json:"trustee"`
		Receiver      string `json:"receiver"`
		ExpiresAt     string `json:"expires_at"`
		ExpectedOwner string `json:"expected_owner"`
		ExpectedVer   int64  `json:"expected_version"`
		Reason        string `json:"reason"`
	}{"create_auth", req.AuthID, req.ItemID, req.Trustee, req.Receiver,
		req.ExpiresAt.UTC().Format(time.RFC3339Nano), req.ExpectedOwner, req.ExpectedVer, req.Reason})
	return string(b)
}

// CreateAuth 由当前持有人为一件已发行藏品创建限时、一次性的代转授权。
// 授权不改变持有关系，同一藏品可有多份授权，各自绑定创建时的持有版本。
// 授权人、受托人、接收人都必须已登记且可用；受托人或接收人是授权人时
// 返回同账户错误（两者可以相同）。同一 (操作者, 请求号) 且业务参数相同
// 的重复提交返回首次结果；参数不同返回 ErrRequestConflict。
func (r *Registry) CreateAuth(req CreateAuthRequest) (CreateAuthResult, error) {
	if err := req.validatePresent(); err != nil {
		return CreateAuthResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return CreateAuthResult{}, err
	}

	key := requestKey(req.Operator, req.RequestID)
	sig := createAuthParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayCreateAuth(prev, sig)
	}

	bizErr := r.checkCreateAuth(req)
	if bizErr != nil {
		if !isValidationErr(bizErr) {
			// 状态类业务拒绝（编号占用、停用、同账户、版本冲突等）占用
			// 请求号并落盘：相同参数重提永远返回这一次拒绝。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "create_auth",
				Params: sig, Rejected: true, Reason: errCode(bizErr),
				ItemID: req.ItemID, AuthID: req.AuthID,
			}
			_ = r.commit()
		}
		// 引用不存在等校验错误不占用请求号，也不生成授权。
		return CreateAuthResult{AuthID: req.AuthID, ItemID: req.ItemID, Err: bizErr}, bizErr
	}

	h := r.state.Holdings[req.ItemID]
	seq := r.state.NextSeq + 1
	r.state.NextSeq = seq
	r.state.Auths[req.AuthID] = auth{
		ID: req.AuthID, ItemID: req.ItemID, Authorizer: req.Operator,
		Trustee: req.Trustee, Receiver: req.Receiver, ExpiresAt: req.ExpiresAt,
		HolderID: h.OwnerID, Version: h.Version,
	}
	r.state.AuthHistory = append(r.state.AuthHistory, authEvent{
		Seq: seq, Kind: "create", AuthID: req.AuthID, ItemID: req.ItemID,
		Operator: req.Operator, Reason: req.Reason, RequestID: req.RequestID,
		Before: authState{}, After: authState{Exists: true},
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "create_auth",
		Params: sig, AuthID: req.AuthID, ItemID: req.ItemID,
		Version: h.Version, TxSeq: seq,
	}
	if err := r.commit(); err != nil {
		return CreateAuthResult{}, err
	}
	return CreateAuthResult{AuthID: req.AuthID, ItemID: req.ItemID,
		Version: h.Version, TxSeq: seq}, nil
}

func (r *Registry) checkCreateAuth(req CreateAuthRequest) error {
	if _, ok := r.state.Items[req.ItemID]; !ok {
		return fmt.Errorf("%w: 藏品 %s", ErrNotFound, req.ItemID)
	}
	op, ok := r.state.Accounts[req.Operator]
	if !ok {
		return fmt.Errorf("%w: 授权人账户 %s", ErrNotFound, req.Operator)
	}
	if !op.Active {
		return fmt.Errorf("%w: 授权人账户 %s", ErrAccountInactive, req.Operator)
	}
	tr, ok := r.state.Accounts[req.Trustee]
	if !ok {
		return fmt.Errorf("%w: 受托人账户 %s", ErrNotFound, req.Trustee)
	}
	if !tr.Active {
		return fmt.Errorf("%w: 受托人账户 %s", ErrAccountInactive, req.Trustee)
	}
	rc, ok := r.state.Accounts[req.Receiver]
	if !ok {
		return fmt.Errorf("%w: 接收人账户 %s", ErrNotFound, req.Receiver)
	}
	if !rc.Active {
		return fmt.Errorf("%w: 接收人账户 %s", ErrAccountInactive, req.Receiver)
	}
	if req.Trustee == req.Operator {
		return fmt.Errorf("%w: 受托人 %s 与授权人相同", ErrSameAccount, req.Trustee)
	}
	if req.Receiver == req.Operator {
		return fmt.Errorf("%w: 接收人 %s 与授权人相同", ErrSameAccount, req.Receiver)
	}
	if _, ok := r.state.Auths[req.AuthID]; ok {
		return fmt.Errorf("%w: 授权编号 %s 已被使用", ErrAlreadyExists, req.AuthID)
	}
	h, ok := r.state.Holdings[req.ItemID]
	if !ok {
		return fmt.Errorf("%w: 藏品 %s 没有持有记录", ErrNotFound, req.ItemID)
	}
	if h.OwnerID != req.ExpectedOwner || h.Version != req.ExpectedVer ||
		req.Operator != h.OwnerID {
		return fmt.Errorf("%w: 藏品 %s 当前为 %s 版本 %d", ErrConflict,
			req.ItemID, h.OwnerID, h.Version)
	}
	return nil
}

func (r *Registry) replayCreateAuth(prev request, sig string) (CreateAuthResult, error) {
	if prev.Kind != "create_auth" || prev.Params != sig {
		return CreateAuthResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := CreateAuthResult{AuthID: prev.AuthID, ItemID: prev.ItemID,
		Version: prev.Version, TxSeq: prev.TxSeq, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		return res, res.Err
	}
	return res, nil
}

func (req RevokeAuthRequest) validatePresent() error {
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
		return fmt.Errorf("%w: 撤销请求缺少必填字段 %s", ErrInvalidArgument, strings.Join(missing, ", "))
	}
	return nil
}

func revokeAuthParamsSig(req RevokeAuthRequest) string {
	b, _ := json.Marshal(struct {
		Kind   string `json:"kind"`
		AuthID string `json:"auth_id"`
		Reason string `json:"reason"`
	}{"revoke_auth", req.AuthID, req.Reason})
	return string(b)
}

// RevokeAuth 由授权人撤销尚未使用的授权。已撤销时再次撤销不增加记录；
// 已使用的授权拒绝撤销。同一 (操作者, 请求号) 的相同请求重复提交返回
// 首次结果；参数不同返回 ErrRequestConflict。
func (r *Registry) RevokeAuth(req RevokeAuthRequest) (RevokeAuthResult, error) {
	if err := req.validatePresent(); err != nil {
		return RevokeAuthResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return RevokeAuthResult{}, err
	}

	key := requestKey(req.Operator, req.RequestID)
	sig := revokeAuthParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayRevokeAuth(prev, sig)
	}

	bizErr, alreadyRevoked := r.checkRevokeAuth(req)
	if bizErr != nil {
		if !isValidationErr(bizErr) {
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "revoke_auth",
				Params: sig, Rejected: true, Reason: errCode(bizErr), AuthID: req.AuthID,
			}
			_ = r.commit()
		}
		return RevokeAuthResult{AuthID: req.AuthID, Err: bizErr}, bizErr
	}
	if alreadyRevoked {
		// 已撤销是幂等终态：再次撤销不增加记录，直接返回成功。
		return RevokeAuthResult{AuthID: req.AuthID}, nil
	}

	a := r.state.Auths[req.AuthID]
	seq := r.state.NextSeq + 1
	r.state.NextSeq = seq
	before := authState{Exists: true, Revoked: a.Revoked, Used: a.Used}
	a.Revoked = true
	r.state.Auths[req.AuthID] = a
	after := authState{Exists: true, Revoked: true, Used: a.Used}
	r.state.AuthHistory = append(r.state.AuthHistory, authEvent{
		Seq: seq, Kind: "revoke", AuthID: req.AuthID, ItemID: a.ItemID,
		Operator: req.Operator, Reason: req.Reason, RequestID: req.RequestID,
		Before: before, After: after,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "revoke_auth",
		Params: sig, AuthID: req.AuthID, TxSeq: seq,
	}
	if err := r.commit(); err != nil {
		return RevokeAuthResult{}, err
	}
	return RevokeAuthResult{AuthID: req.AuthID, TxSeq: seq}, nil
}

func (r *Registry) checkRevokeAuth(req RevokeAuthRequest) (error, bool) {
	a, ok := r.state.Auths[req.AuthID]
	if !ok {
		return fmt.Errorf("%w: 授权 %s", ErrNotFound, req.AuthID), false
	}
	if req.Operator != a.Authorizer {
		return fmt.Errorf("%w: 只有授权人 %s 可以撤销授权 %s",
			ErrForbidden, a.Authorizer, req.AuthID), false
	}
	if a.Used {
		return fmt.Errorf("%w: 授权 %s 已使用，不能撤销", ErrAuthUsed, req.AuthID), false
	}
	if a.Revoked {
		return nil, true
	}
	return nil, false
}

func (r *Registry) replayRevokeAuth(prev request, sig string) (RevokeAuthResult, error) {
	if prev.Kind != "revoke_auth" || prev.Params != sig {
		return RevokeAuthResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := RevokeAuthResult{AuthID: prev.AuthID, TxSeq: prev.TxSeq, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		return res, res.Err
	}
	return res, nil
}

func (req DelegateTransferRequest) validatePresent() error {
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
		return fmt.Errorf("%w: 代转请求缺少必填字段 %s", ErrInvalidArgument, strings.Join(missing, ", "))
	}
	return nil
}

func delegateTransferParamsSig(req DelegateTransferRequest) string {
	b, _ := json.Marshal(struct {
		Kind   string `json:"kind"`
		AuthID string `json:"auth_id"`
		Reason string `json:"reason"`
	}{"delegate_transfer", req.AuthID, req.Reason})
	return string(b)
}

// DelegateTransfer 由受托人凭授权发起代转，只能操作授权中指定的藏品与
// 接收人。成功后持有人变为接收人、版本加一，授权记为已使用并关联这笔
// 转让。非受托人、授权已撤销/已到期/已使用、三方账户停用或持有版本变化
// 都明确拒绝且可区分原因。同一 (操作者, 请求号) 的相同请求重复提交
// （即使授权已到期、被撤销或藏品已再次易手）返回首次结果；参数不同
// 返回 ErrRequestConflict。
func (r *Registry) DelegateTransfer(req DelegateTransferRequest) (DelegateTransferResult, error) {
	if err := req.validatePresent(); err != nil {
		return DelegateTransferResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return DelegateTransferResult{}, err
	}

	key := requestKey(req.Operator, req.RequestID)
	sig := delegateTransferParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayDelegateTransfer(prev, sig)
	}

	bizErr := r.checkDelegateTransfer(req)
	if bizErr != nil {
		if !isValidationErr(bizErr) {
			// 状态类业务拒绝占用请求号并落盘，相同参数重提永远返回这一次拒绝。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "delegate_transfer",
				Params: sig, Rejected: true, Reason: errCode(bizErr), AuthID: req.AuthID,
			}
			_ = r.commit()
		}
		return DelegateTransferResult{AuthID: req.AuthID, Err: bizErr}, bizErr
	}

	a := r.state.Auths[req.AuthID]
	h := r.state.Holdings[a.ItemID]
	seq := r.state.NextSeq + 1
	r.state.NextSeq = seq
	from := h.OwnerID
	fromVer := h.Version
	h.OwnerID = a.Receiver
	h.Version = fromVer + 1
	r.state.Holdings[a.ItemID] = h
	// 代转仍记入原藏品历史，操作者为实际受托人，并通过授权编号可追溯。
	r.state.History = append(r.state.History, historyEntry{
		Seq: seq, Kind: "transfer", ItemID: a.ItemID, Operator: req.Operator,
		Reason: req.Reason, RequestID: req.RequestID,
		FromID: from, ToID: a.Receiver, FromVersion: fromVer, ToVersion: fromVer + 1,
		AuthID: a.ID,
	})
	before := authState{Exists: true, Revoked: a.Revoked, Used: a.Used}
	a.Used = true
	a.UsedTxID = seq
	r.state.Auths[req.AuthID] = a
	after := authState{Exists: true, Revoked: a.Revoked, Used: true}
	r.state.AuthHistory = append(r.state.AuthHistory, authEvent{
		Seq: seq, Kind: "use", AuthID: a.ID, ItemID: a.ItemID,
		Operator: req.Operator, Reason: req.Reason, RequestID: req.RequestID,
		Before: before, After: after,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "delegate_transfer",
		Params: sig, AuthID: a.ID, ItemID: a.ItemID,
		FromID: from, ToID: a.Receiver, Version: fromVer + 1, TxSeq: seq,
	}
	if err := r.commit(); err != nil {
		return DelegateTransferResult{}, err
	}
	return DelegateTransferResult{AuthID: a.ID, ItemID: a.ItemID, FromID: from,
		ToID: a.Receiver, Version: fromVer + 1, TxSeq: seq}, nil
}

func (r *Registry) checkDelegateTransfer(req DelegateTransferRequest) error {
	a, ok := r.state.Auths[req.AuthID]
	if !ok {
		return fmt.Errorf("%w: 授权 %s", ErrNotFound, req.AuthID)
	}
	if req.Operator != a.Trustee {
		return fmt.Errorf("%w: 只有受托人 %s 可以发起授权 %s 的代转",
			ErrForbidden, a.Trustee, req.AuthID)
	}
	if a.Revoked {
		return fmt.Errorf("%w: 授权 %s 已撤销", ErrAuthRevoked, req.AuthID)
	}
	// 到期时间按实际时间点判断：从该时间点起不可使用。
	if !a.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("%w: 授权 %s 已到期", ErrAuthExpired, req.AuthID)
	}
	if a.Used {
		return fmt.Errorf("%w: 授权 %s 已使用", ErrAuthUsed, req.AuthID)
	}
	if acc, ok := r.state.Accounts[a.Authorizer]; !ok {
		return fmt.Errorf("%w: 授权人账户 %s", ErrNotFound, a.Authorizer)
	} else if !acc.Active {
		return fmt.Errorf("%w: 授权人账户 %s", ErrAccountInactive, a.Authorizer)
	}
	if acc, ok := r.state.Accounts[a.Trustee]; !ok {
		return fmt.Errorf("%w: 受托人账户 %s", ErrNotFound, a.Trustee)
	} else if !acc.Active {
		return fmt.Errorf("%w: 受托人账户 %s", ErrAccountInactive, a.Trustee)
	}
	if acc, ok := r.state.Accounts[a.Receiver]; !ok {
		return fmt.Errorf("%w: 接收人账户 %s", ErrNotFound, a.Receiver)
	} else if !acc.Active {
		return fmt.Errorf("%w: 接收人账户 %s", ErrAccountInactive, a.Receiver)
	}
	h, ok := r.state.Holdings[a.ItemID]
	if !ok {
		return fmt.Errorf("%w: 藏品 %s 没有持有记录", ErrNotFound, a.ItemID)
	}
	if h.OwnerID != a.Authorizer || h.Version != a.Version {
		return fmt.Errorf("%w: 藏品 %s 当前为 %s 版本 %d，授权 %s 绑定 %s 版本 %d",
			ErrConflict, a.ItemID, h.OwnerID, h.Version, a.ID, a.Authorizer, a.Version)
	}
	return nil
}

func (r *Registry) replayDelegateTransfer(prev request, sig string) (DelegateTransferResult, error) {
	if prev.Kind != "delegate_transfer" || prev.Params != sig {
		return DelegateTransferResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := DelegateTransferResult{AuthID: prev.AuthID, ItemID: prev.ItemID,
		FromID: prev.FromID, ToID: prev.ToID, Version: prev.Version,
		TxSeq: prev.TxSeq, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		return res, res.Err
	}
	return res, nil
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

// GetAuth 按编号查询授权内容与撤销、使用状态；授权不存在时返回包裹
// ErrNotFound 的错误。
func (r *Registry) GetAuth(id string) (Auth, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return Auth{}, err
	}
	a, ok := r.state.Auths[id]
	if !ok {
		return Auth{}, fmt.Errorf("%w: 授权 %s", ErrNotFound, id)
	}
	return Auth{
		ID: a.ID, ItemID: a.ItemID, Authorizer: a.Authorizer, Trustee: a.Trustee,
		Receiver: a.Receiver, ExpiresAt: a.ExpiresAt, HolderID: a.HolderID,
		Version: a.Version, Revoked: a.Revoked, Used: a.Used, UsedTxID: a.UsedTxID,
	}, nil
}

// AuthHistory 查询某藏品的授权变更记录，按发生先后排列，包含操作者、
// 原因、请求号与前后状态。藏品不存在时返回包裹 ErrNotFound 的错误。
func (r *Registry) AuthHistory(itemID string) ([]AuthHistoryEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return nil, err
	}
	if _, ok := r.state.Items[itemID]; !ok {
		return nil, fmt.Errorf("%w: 藏品 %s", ErrNotFound, itemID)
	}
	out := make([]AuthHistoryEntry, 0)
	for _, e := range r.state.AuthHistory {
		if e.ItemID != itemID {
			continue
		}
		out = append(out, AuthHistoryEntry{
			Seq: e.Seq, Kind: e.Kind, AuthID: e.AuthID, ItemID: e.ItemID,
			Operator: e.Operator, Reason: e.Reason, RequestID: e.RequestID,
			Before: AuthState{Exists: e.Before.Exists, Revoked: e.Before.Revoked, Used: e.Before.Used},
			After:  AuthState{Exists: e.After.Exists, Revoked: e.After.Revoked, Used: e.After.Used},
		})
	}
	return out, nil
}

// ---- 幂等辅助 ----

func requestKey(operator, requestID string) string {
	return operator + "\x00" + requestID
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
	ErrNotFound:        "not_found",
	ErrAlreadyExists:   "already_exists",
	ErrAccountInactive: "account_inactive",
	ErrSeriesSealed:    "series_sealed",
	ErrConflict:        "conflict",
	ErrSameAccount:     "same_account",
	ErrForbidden:       "forbidden",
	ErrAuthRevoked:     "auth_revoked",
	ErrAuthExpired:     "auth_expired",
	ErrAuthUsed:        "auth_used",
}

var codeErrs = map[string]error{
	"not_found":        ErrNotFound,
	"already_exists":   ErrAlreadyExists,
	"account_inactive": ErrAccountInactive,
	"series_sealed":    ErrSeriesSealed,
	"conflict":         ErrConflict,
	"same_account":     ErrSameAccount,
	"forbidden":        ErrForbidden,
	"auth_revoked":     ErrAuthRevoked,
	"auth_expired":     ErrAuthExpired,
	"auth_used":        ErrAuthUsed,
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
