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
//
// 登记以一次保存完成为准：新账户尚未原子替换原数据就发生保存错误（如数据
// 位置暂时无法写入）时，返回本次实际的保存错误，不返回成功或编号冲突，也
// 不能用失败后重新读取原数据时的错误取代它。即使原数据仍在、却暂时无法
// 读取或解析、状态未能按磁盘重建，同一个仍打开的登记册也必须立即表现为该
// 编号从未登记：GetAccount 返回 ErrNotFound，藏品转让不能把它当成已登记的
// 接收账户；失败不占用新账户编号，保存条件未恢复时用该编号再次登记仍实际
// 尝试保存并返回当次保存错误，而不是因遗留账户返回 ErrAlreadyExists。原
// 数据仍可读取的普通保存失败同样撤销本次登记，不依赖重新读取成功。随后另
// 一次无关操作成功保存也不会把这个未保存的账户夹带落盘，关闭再打开登记册
// 后它仍不存在。读写恢复后用该编号重新登记，以本次提交的元数据为准，保存
// 成功才显示为可用账户。此前成功登记的账户及其元数据、可用或停用状态，以及
// 已有藏品的持有人、版本与历史都原样保留。空白编号仍返回 ErrInvalidArgument，
// 元数据允许为空；已成功登记的编号无论账户是否停用都返回 ErrAlreadyExists、
// 不覆盖其记录，这些业务拒绝不因数据位置恰好不可写而变成保存错误。
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
	if err := r.commit(); err != nil {
		// commit 失败时已尝试按磁盘内容重建状态：重建成功则新账户已随旧
		// 状态整体消失，删除是无害的兜底；磁盘暂时不可读、状态未能重建时
		// 新账户仍在，显式删除，让当前仍打开的登记册表现为该编号从未登记
		// ——GetAccount 返回 ErrNotFound、转让不把它当成已登记的接收账户、
		// 再次登记不会撞上遗留的 ErrAlreadyExists，也不被随后另一次成功
		// 保存夹带落盘。返回实际写入错误，而非成功、编号冲突或重新读取时
		// 的错误。
		delete(r.state.Accounts, id)
		return err
	}
	return nil
}

// DeactivateAccount 停用账户。停用后其原有藏品与历史仍可查询，但不能
// 再发行、发起或接收新的转让；停用不影响已经落盘的任何记录。
//
// 停用以保存成功为准：停用状态尚未原子替换原数据就发生写入错误（如数据
// 位置暂时无法写入）时，返回本次实际的保存错误，不返回成功，也不能用失败
// 后重新读取原数据时的错误取代它。即使原数据仍在、却暂时无法读取或解析、
// 状态未能按磁盘重建，同一个仍打开的登记册也必须保持停用前的样子：该账户
// 仍可用，编号与元数据不变，其已有藏品的持有人、版本与历史原样保留；其他
// 账户此前已经成功停用的状态不被恢复为可用。原数据仍可读取的普通保存失败
// 同样撤销本次变化，不依赖重新读取成功。随后另一次无关操作成功保存也不会
// 把这次未保存的停用夹带落盘。保存条件恢复后该账户仍按原有规则参与转让；
// 之后再次提交停用须重新完成保存，成功返回后才显示不可用，并按现有规则
// 拒绝其发起或接收新的转让。
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
		return nil // 停用是幂等的终态，已停用直接成功，不要求再次写入
	}
	// 保存停用前的账户记录：停用以一次保存完成为准。commit 失败且磁盘
	// 暂时不可读、状态未能按磁盘重建时据此显式撤销，否则这次未保存的停用
	// 会留在当前已打开的登记册中（GetAccount 显示不可用、转让被按停用
	// 拒绝），并被随后另一次成功保存夹带落盘。
	prev := a
	a.Active = false
	r.state.Accounts[id] = a
	if err := r.commit(); err != nil {
		// commit 失败时已尝试按磁盘内容重建状态：重建成功则账户已随旧状态
		// 整体恢复可用，写回的记录与磁盘一致；磁盘暂时不可读、状态未能重建
		// 时本次停用仍在，显式恢复为停用前记录，藏品持有、版本与历史本就未
		// 被触碰。返回实际写入错误，而非成功或重新读取时的错误。
		r.state.Accounts[id] = prev
		return err
	}
	return nil
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
//
// 登记以一次保存完成为准：新系列尚未原子替换原数据就发生保存错误（如数据
// 位置暂时无法写入）时，返回本次实际的保存错误，不返回成功或编号冲突，也
// 不能用失败后重新读取原数据时的错误取代它。即使原数据仍在、却暂时无法
// 读取或解析、状态未能按磁盘重建，同一个仍打开的登记册也必须立即表现为该
// 编号从未登记：GetSeries 返回 ErrNotFound，不能把它当成已存在的系列继续
// 发行或封存；失败不占用系列编号，保存条件未恢复时用该编号再次登记仍实际
// 尝试保存并返回当次保存错误，而不是因遗留系列返回 ErrAlreadyExists。原
// 数据仍可读取的普通保存失败同样撤销本次登记，不依赖重新读取成功。随后另
// 一次无关操作成功保存也不会把这个未保存的系列夹带落盘，关闭再打开登记册
// 后它仍不存在。读写恢复后用该编号重新登记，以本次提交的创建账户与文字元
// 数据为准，保存成功才显示为未封存的新系列，并可按已有规则发行藏品。此前
// 成功登记的账户、系列及其封存状态，以及已有藏品的持有人、版本与历史都原
// 样保留。空白编号返回 ErrInvalidArgument，元数据允许为空；创建账户不存
// 在或已停用分别返回 ErrNotFound、ErrAccountInactive；已成功登记的系列编号
// 返回 ErrAlreadyExists、不覆盖其创建账户、元数据或封存状态，这些业务拒绝
// 不因数据位置恰好不可写而变成保存错误。
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
	if err := r.commit(); err != nil {
		// commit 失败时已尝试按磁盘内容重建状态：重建成功则新系列已随旧
		// 状态整体消失，删除是无害的兜底；磁盘暂时不可读、状态未能重建时
		// 新系列仍在，显式删除，让当前仍打开的登记册表现为该编号从未登记
		// ——GetSeries 返回 ErrNotFound、发行与封存不把它当成已存在的系列、
		// 再次登记不会撞上遗留的 ErrAlreadyExists，也不被随后另一次成功
		// 保存夹带落盘。返回实际写入错误，而非成功、编号冲突或重新读取时
		// 的错误。
		delete(r.state.Series, id)
		return err
	}
	return nil
}

// SealSeries 封存系列。只有系列创建账户可以封存；封存后不能继续发行，
// 已发行藏品仍可转让，且封存不能撤销。
//
// 封存以一次保存完成为准：封存状态尚未原子替换原数据就发生写入错误（如
// 数据位置暂时无法写入）时，返回本次实际的保存错误，不返回成功，也不能用
// 失败后重新读取原数据时的错误取代它。即使原数据仍在、却暂时无法读取或
// 解析、状态未能按磁盘重建，同一个仍打开的登记册也必须保持封存前的样子：
// 该系列编号、创建账户、文字元数据不变且仍未封存，满足原有发行条件时仍可
// 发行；其已发行藏品的持有人、版本与发行、转让历史原样保留；其他系列此前
// 已经成功封存的封存状态不被解封。原数据仍可读取的普通保存失败同样撤销
// 本次变化，不依赖重新读取成功。保存条件未恢复时再次提交封存须重新尝试
// 保存并返回本次保存错误，不能因上次失败留下的状态直接宣告完成；随后另
// 一次无关操作成功保存也不会把这次未保存的封存夹带落盘。读写恢复后重新
// 提交封存，成功保存后才显示已封存，并按原有规则拒绝单件与整批发行。
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
		return nil // 封存不可撤销，已成功封存的系列重复封存直接成功，不要求再次写入
	}
	// 保存封存前的系列记录：封存以一次保存完成为准。commit 失败且磁盘
	// 暂时不可读、状态未能按磁盘重建时据此显式撤销，否则这次未保存的封存
	// 会留在当前已打开的登记册中（GetSeries 显示已封存、发行被按封存拒绝、
	// 重复封存被幂等分支直接放行），并被随后另一次成功保存夹带落盘。
	prev := s
	s.Sealed = true
	r.state.Series[seriesID] = s
	if err := r.commit(); err != nil {
		// commit 失败时已尝试按磁盘内容重建状态：重建成功则系列已随旧状态
		// 整体恢复未封存，写回的记录与磁盘一致；磁盘暂时不可读、状态未能
		// 重建时本次封存仍在，显式恢复为封存前记录，藏品持有、版本与历史
		// 本就未被触碰。返回实际写入错误，而非成功或重新读取时的错误。
		r.state.Series[seriesID] = prev
		return err
	}
	return nil
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
//
// 状态类拒绝的落盘失败时，与整批发行一致：返回保存错误（保留实际写入
// 错误）而非该业务错误，结果为空（无藏品编号、无持有人、无版本、无历史
// 序号、业务错误为空、不标回放），请求号不被这次未保存的拒绝占用；保存
// 条件恢复后用完全相同的请求重提，按当时的业务状态重新判断：拒绝条件仍
// 在则重新保存此次拒绝并返回对应业务错误，状态已变为满足请求则正常发行。
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
			if err := r.commit(); err != nil {
				// 拒绝结果落盘失败：这次拒绝没有被记住，与整批发行一致——
				// 不能只用业务错误掩盖保存错误，调用者稍后用原请求重提时
				// 必须按当时状态重新判断，而不是回放一个实际没有保存的
				// 拒绝。commit 失败时已按磁盘内容重建状态，请求记录随之
				// 撤销；此处再删一次以覆盖磁盘暂时不可读、状态未能重建
				// 的情形。整体返回空结果：无藏品编号、无持有人、无版本、
				// 无历史序号、不标回放、结果中业务错误为空，error 保留
				// 实际写入错误。
				delete(r.state.Requests, key)
				return IssueResult{}, err
			}
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

// checkIssue 按单件发行的优先级执行与整批共用的发行资格规则：参数合法
// 后，操作者的登记/停用问题优先于系列问题，系列问题内部依次为不存在、
// 已封存、操作者不是创建账户，藏品编号已占用又优先于初始持有人问题。
func (r *Registry) checkIssue(req IssueRequest) error {
	return r.checkSingleIssueEligibility(req.Operator, req.SeriesID, req.ItemID, req.HolderID)
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

// transferOutcome 是一笔成功转让落盘后的执行结果，供各转让入口组装
// 各自的返回结构与请求记录。
type transferOutcome struct {
	seq     int64      // 转让历史序号
	fromID  string     // 转让前持有人
	fromVer int64      // 转让前持有版本
	royalty royaltyRec // 版税应付记录（含价款、明细与余款）
}

// toVer 是转让后的持有版本。
func (o transferOutcome) toVer() int64 { return o.fromVer + 1 }

// applyTransfer 完成一笔成功转让的全部状态变化：持有版本加一并换主、
// 追加转让历史、按所属系列的版税规则快照计算应付明细与归转让前持有人
// 的余款。直接转让、授权代转与整批转让共用这一段逻辑，保证同一笔转让
// 的持有、历史与金额记录始终一致；authID 为空表示直接转让。调用方负责
// 前置校验、请求结果登记与落盘，调用时必须持有锁且校验已通过。
func (r *Registry) applyTransfer(itemID, toID, operator, reason, requestID, authID string, price int64) transferOutcome {
	h := r.state.Holdings[itemID]
	seq := r.state.NextSeq + 1
	r.state.NextSeq = seq
	from := h.OwnerID
	fromVer := h.Version
	h.OwnerID = toID
	h.Version = fromVer + 1
	r.state.Holdings[itemID] = h
	r.state.History = append(r.state.History, historyEntry{
		Seq: seq, Kind: "transfer", ItemID: itemID, Operator: operator,
		Reason: reason, RequestID: requestID,
		FromID: from, ToID: toID, FromVersion: fromVer, ToVersion: fromVer + 1,
		AuthID: authID,
	})
	// 版税按所属系列的规则快照计算；余款归转让前持有人。
	it := r.state.Items[itemID]
	royalty := newRoyaltyRec(seq, itemID, it.SeriesID, price,
		r.state.Series[it.SeriesID].Royalty, from)
	r.state.Royalties[seq] = royalty
	return transferOutcome{seq: seq, fromID: from, fromVer: fromVer, royalty: royalty}
}

// Transfer 由当前持有人发起转让。接收人必须已登记、可用且不同于当前
// 持有人；请求必须给出期望持有人与期望版本，藏品不存在或持有人/版本
// 不符时返回业务拒绝且不改变任何状态。成交价款（分）由持有人填写，
// 非负、未填按 0；成功时按所属系列的版税规则一次落盘各收款账户的
// 应付明细与归转让前持有人的余款。同一 (操作者, 请求号) 的相同
// 请求重复提交（即使藏品后来已易手）返回首次结果。
//
// 状态类拒绝的落盘失败时，与整批转让一致：返回保存错误（保留实际写入
// 错误）而非该业务错误，结果为空（无藏品编号、无持有变化、无历史序号、
// 无金额、业务错误为空、不标回放），请求号不被这次未保存的拒绝占用；
// 保存条件恢复后用完全相同的请求重提，按当时的业务状态重新判断：拒绝
// 条件仍在则重新保存此次拒绝并返回对应业务错误，状态已变为满足请求则
// 正常转出。
//
// 成功转让同样以保存完成为准：持有换人、版本加一、转让历史、版税应付、
// 历史序号与请求结果都尚未写入原登记册而保存失败（如数据位置暂时无法
// 写入）时，返回实际保存错误而非转让成功，结果为空（不携带藏品编号、前后
// 持有人、版本、历史序号、价款、应付或余款，业务错误为空，不标回放）。
// 即使失败后原数据暂时无法读取、状态未能按磁盘重建，这笔转让也必须从当前
// 仍打开的登记册中整体撤销：藏品仍属于提交前的持有人、版本不增加、持有
// 列表与藏品历史保持原状、版税查询没有本次计算记录、收款账户不多出应付，
// 请求号与历史序号都不被消耗；失败前已存在的成功转让、应付与其他藏品记录
// 原样保留，随后另一次无关操作成功保存也不会把这笔未保存的转让一并写入。
// 读写条件恢复后用原操作者、请求号、原因和全部转让参数重新提交，按当时的
// 持有与账户状态重新判断：条件仍满足时正常完成一次转让（版本只增加一次、
// 序号紧接已有历史、不标回放），此后再次提交相同内容才回放这一笔已保存的
// 结果；恢复期间藏品已被另一笔合法转让转出的，原请求按现有版本冲突规则
// 拒绝，不能回放先前未保存的成功。
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
			if err := r.commit(); err != nil {
				// 拒绝结果落盘失败：这次拒绝没有被记住，与整批转让一致——
				// 不能只用业务错误掩盖保存错误，调用者稍后用原请求重提时
				// 必须按当时状态重新判断，而不是回放一个实际没有保存的
				// 拒绝。commit 失败时已按磁盘内容重建状态，请求记录随之
				// 撤销；此处再删一次以覆盖磁盘暂时不可读、状态未能重建
				// 的情形。整体返回空结果：无藏品编号、无持有变化、无历史
				// 序号、无金额、不标回放、结果中业务错误为空，error 保留
				// 实际写入错误。
				delete(r.state.Requests, key)
				return TransferResult{}, err
			}
		}
		// 藏品或账户不存在等校验错误不占用请求号，也不改变持有或历史。
		return TransferResult{ItemID: req.ItemID, Err: bizErr}, bizErr
	}

	// 回滚基点：本次成功转让的全部改动（持有换人、版本加一、历史、版税
	// 应付、历史序号、请求结果）都必须以一次保存完成为准。记录改动前的
	// 持有、历史长度与历史序号，保存失败且磁盘暂时不可读、状态未能按磁盘
	// 重建时据此整体撤销，否则后续任何一次成功保存都会把这笔未保存的转让
	// 带进登记册，造成"已换人、可回放"的幻影交易。
	prevHolding := r.state.Holdings[req.ItemID]
	prevNextSeq := r.state.NextSeq
	prevHistLen := len(r.state.History)

	// 持有变化、历史、版税应付与请求结果在同一临界区内一次落盘。
	out := r.applyTransfer(req.ItemID, req.ToID, req.Operator, req.Reason, req.RequestID, "", req.Price)
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "transfer",
		Params: sig, ItemID: req.ItemID, FromID: out.fromID, ToID: req.ToID,
		Version: out.toVer(), TxSeq: out.seq,
	}
	if err := r.commit(); err != nil {
		// commit 失败时已尝试按磁盘内容重建状态：重建成功则本次改动已随
		// 旧状态整体撤销（持有未换人、无本次历史与应付、请求记录不存在、
		// 序号未消耗），无需再动；磁盘暂时不可读、状态未能重建时本次改动
		// 仍在，据此把这笔未保存的转让全部撤销，让当前已打开的登记册回到
		// 提交前状态，也不被随后另一次成功保存带入。返回空结果：无藏品
		// 编号、无前后持有人、无版本、无历史序号、无价款/应付/余款、不标
		// 回放、结果中业务错误为空，error 保留实际写入错误。
		if _, pending := r.state.Requests[key]; pending {
			delete(r.state.Requests, key)
			delete(r.state.Royalties, out.seq)
			r.state.History = r.state.History[:prevHistLen]
			r.state.Holdings[req.ItemID] = prevHolding
			r.state.NextSeq = prevNextSeq
		}
		return TransferResult{}, err
	}
	return TransferResult{ItemID: req.ItemID, FromID: out.fromID, ToID: req.ToID,
		Version: out.toVer(), TxSeq: out.seq,
		Price: out.royalty.Price, Payables: publicPayables(out.royalty.Payees),
		Remainder: out.royalty.Remainder}, nil
}

// checkTransfer 按单件转让的优先级执行与整批共用的直接转让资格规则：
// 参数合法后，藏品及持有记录不存在优先于账户错误，操作者的登记和停用
// 问题优先于接收账户问题，接收账户问题优先于收发同人，收发同人又优先
// 于持有信息不符。
func (r *Registry) checkTransfer(req TransferRequest) error {
	return r.checkSingleTransferEligibility(directTransferEligibility{
		itemID:        req.ItemID,
		operator:      req.Operator,
		toID:          req.ToID,
		expectedOwner: req.ExpectedOwner,
		expectedVer:   req.ExpectedVer,
	})
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
//
// 成功与状态类拒绝都以保存完成为准：本次结果尚未写入原登记册而保存失败
// （如数据位置暂时无法写入）时，返回实际保存错误，不报告创建成功，也不
// 只返回原业务拒绝；结果为空（授权编号与状态为空、绑定版本为零、不标
// 回放、业务错误为空），error 说明保存失败。即使失败后原数据暂时无法
// 读取、状态未能按磁盘重建，这次操作也不留下任何新增授权、创建记录或
// 请求号占用，授权历史序号不被消耗，后续其他操作成功保存也不会把这次
// 未保存的内容带进登记册。保存条件恢复后用完全相同的请求重提，按当时
// 的业务状态重新判断：拒绝条件仍在则重新保存此次拒绝并返回对应业务
// 错误（首次重提不标回放），状态已满足请求则正常创建。
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
			if err := r.commit(); err != nil {
				// 拒绝结果落盘失败：这次拒绝没有被记住，与发行、转让一致——
				// 不能只用业务错误掩盖保存错误，调用者稍后用原请求重提时
				// 必须按当时状态重新判断，而不是回放一个实际没有保存的
				// 拒绝。commit 失败时已按磁盘内容重建状态，请求记录随之
				// 撤销；此处再删一次以覆盖磁盘暂时不可读、状态未能重建
				// 的情形。整体返回空结果：授权编号与状态为空、绑定版本
				// 为零、不标回放、结果中业务错误为空，error 保留实际
				// 写入错误。
				delete(r.state.Requests, key)
				return CreateAuthorizationResult{}, err
			}
		}
		return CreateAuthorizationResult{AuthID: req.AuthID, Err: bizErr}, bizErr
	}

	h := r.state.Holdings[req.ItemID]
	a := authzRec{
		ID: req.AuthID, ItemID: req.ItemID, GranterID: req.Operator,
		TrusteeID: req.TrusteeID, ToID: req.ToID, ExpiresAt: req.ExpiresAt,
		GrantVer: h.Version, Price: req.Price, CreatedAt: now,
	}
	// 回滚基点：保存失败且磁盘暂时不可读、状态未能按磁盘重建时，本次
	// 未保存的授权、创建记录与请求号占用都必须显式撤销，否则后续任何
	// 一次成功保存都会把这份未保存的授权带进登记册。
	prevAuthSeq := r.state.NextAuthSeq
	prevEvents := len(r.state.AuthEvents)
	r.state.Authzs[req.AuthID] = a
	authSeq := prevAuthSeq + 1
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
		// commit 失败时已尝试按磁盘内容重建状态：重建成功则本次改动已
		// 随旧状态整体撤销（请求记录不存在），无需再动；磁盘暂时不可读、
		// 状态未能重建时请求记录仍在，据此把本次未保存的改动全部撤销。
		if _, ok := r.state.Requests[key]; ok {
			delete(r.state.Requests, key)
			delete(r.state.Authzs, req.AuthID)
			r.state.AuthEvents = r.state.AuthEvents[:prevEvents]
			r.state.NextAuthSeq = prevAuthSeq
		}
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

// RevokeAuthorization 撤销一份尚未使用的代转授权。仅授权人可以撤销
// （非授权人返回 ErrForbidden）；已使用的授权不能撤销
// （ErrAuthorizationUsed）。授权已撤销时再次撤销不新增授权变更记录，按
// 成功返回。到期但未使用的授权仍可撤销（变更记录从 expired 记为
// revoked）。请求号语义与其他操作一致。
//
// 无权（ErrForbidden）与已使用（ErrAuthorizationUsed）两类业务拒绝同样
// 需要落盘：拒绝结果尚未写入原登记册而保存失败（如数据位置暂时无法写入）
// 时，返回实际保存错误，不能只返回上述业务错误让调用者误以为拒绝结果已经
// 记住；结果为空（无授权编号、无状态、业务错误为空、不标回放），error 保留
// 保存失败的原因。这次未保存的拒绝不改变授权内容、授权变更记录、藏品当前
// 持有人和版本，也不占用操作者的本次请求号——即使保存失败后原登记册数据
// 暂时无法读取、状态未能按磁盘重建，未保存的拒绝也不能留在当前已打开的
// 登记册中，或随之后另一次正常操作被保存下来。保存条件恢复后用同一操作者、
// 请求号、授权编号和原因重新提交，按当时的授权情况重新处理：拒绝条件仍存在
// 时先成功保存此次拒绝再返回相应业务错误（首次重提不标回放），此后完全相同
// 的提交才回放它；失败保存的请求不适用请求号冲突规则，该操作者用同号撤销另
// 一份自己有权撤销的授权时不会遭遇请求号冲突。必填内容缺失及授权不存在仍
// 直接按原错误拒绝、不占用请求号，不因当前无法保存而变成保存错误。
//
// 撤销成功同样以保存完成为准：授权状态、撤销时间、撤销记录、授权历史序号
// 与请求结果都尚未写入原登记册而保存失败时，返回实际保存错误而非撤销成功，
// 结果为空（无授权编号、无状态、业务错误为空、不标回放）。即使失败后原数据
// 暂时无法读取、状态未能按磁盘重建，这份撤销也必须从当前仍打开的登记册中
// 整体撤销：保留撤销前的授权内容与撤销时间，不新增撤销记录，授权历史序号与
// 请求号都不被消耗；藏品的持有人、版本以及此前已保存的其他授权和历史原样
// 保留，随后另一次正常操作成功保存也不会把这份未保存的撤销一并写入。失败后
// 能否代转仍按原规则判断：持有版本未变、相关账户可用且未到期时受托人仍能
// 正常使用；已到期或藏品已易手则继续返回相应业务拒绝，不能为恢复撤销前状态
// 让旧授权重新可用。保存条件恢复后用同一操作者、授权编号、原因和请求号再次
// 撤销，按此时的授权处理而不回放那次未保存的成功：授权仍未使用且允许撤销时
// 本次才完成撤销并新增一条记录（记录本次生效的时间、操作者、原因与前后状态，
// 不标回放），再原样提交才回放这一已保存结果。仅授权人能撤销、已使用授权
// 拒绝撤销、已撤销授权再次撤销不新增记录的既有行为不变；这些已经保存的终态
// 不被本次失败恢复。
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
		if err := r.recordRevokeRejection(key, sig, req, bizErr); err != nil {
			return RevokeAuthorizationResult{}, err
		}
		return RevokeAuthorizationResult{AuthID: req.AuthID, Err: bizErr}, bizErr
	}
	switch authzCurrentStatus(a, now) {
	case AuthUsed:
		bizErr := fmt.Errorf("%w: 授权 %s 已使用，不能撤销", ErrAuthorizationUsed, req.AuthID)
		if err := r.recordRevokeRejection(key, sig, req, bizErr); err != nil {
			return RevokeAuthorizationResult{}, err
		}
		return RevokeAuthorizationResult{AuthID: req.AuthID, Err: bizErr}, bizErr
	case AuthRevoked:
		// 已撤销是幂等终态：不新增授权变更记录；该请求号仍登记为成功，
		// 相同请求重放回放同一结果。
		r.state.Requests[key] = request{
			Operator: req.Operator, RequestID: req.RequestID, Kind: "auth_revoke",
			Params: sig, AuthID: req.AuthID,
		}
		if err := r.commit(); err != nil {
			// 请求结果落盘失败：这次重放登记没有被记住，与撤销成功路径一致——
			// 显式撤销尚未保存的请求号占用，覆盖磁盘暂时不可读、状态未能按磁盘
			// 重建的情形，返回空结果与实际保存错误。已撤销终态本就存在于磁盘，
			// 无需回滚授权本身。
			delete(r.state.Requests, key)
			return RevokeAuthorizationResult{}, err
		}
		return RevokeAuthorizationResult{AuthID: req.AuthID, Status: AuthRevoked}, nil
	}

	// 回滚基点：撤销以一次保存完成为准。记录撤销前的授权记录、授权历史
	// 长度与历史序号，保存失败且磁盘暂时不可读、状态未能按磁盘重建时据此
	// 整体撤销，否则这份未保存的撤销会留在当前已打开的登记册中（授权显示
	// 已撤销、受托人被拒绝代转、重复撤销被幂等分支直接放行），并随之后另
	// 一次成功保存夹带落盘，原请求重提也会被误当成已保存成功直接回放。
	prevAuthz := a
	prevAuthSeq := r.state.NextAuthSeq
	prevEvents := len(r.state.AuthEvents)
	from := AuthActive
	if authzCurrentStatus(a, now) == AuthExpired {
		from = AuthExpired
	}
	a.Status = "revoked"
	a.RevokedAt = now
	r.state.Authzs[req.AuthID] = a
	authSeq := prevAuthSeq + 1
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
		// commit 失败时已尝试按磁盘内容重建状态：重建成功则本次改动已随
		// 旧状态整体撤销（授权仍未撤销、无本次撤销记录、请求记录不存在、
		// 序号未消耗），写回的记录与磁盘一致，无需再动；磁盘暂时不可读、
		// 状态未能重建时本次改动仍在，据此把这份未保存的撤销全部撤销，让
		// 当前已打开的登记册回到提交前状态——授权内容与撤销时间恢复原样，
		// 持有版本未变且账户可用、未到期时受托人仍可正常使用该授权；也不
		// 被随后另一次成功保存带入。返回实际写入错误而非成功或业务拒绝，
		// 结果为空：无授权编号、无状态、不标回放、结果中业务错误为空。
		if _, pending := r.state.Requests[key]; pending {
			delete(r.state.Requests, key)
			r.state.AuthEvents = r.state.AuthEvents[:prevEvents]
			r.state.NextAuthSeq = prevAuthSeq
			r.state.Authzs[req.AuthID] = prevAuthz
		}
		return RevokeAuthorizationResult{}, err
	}
	return RevokeAuthorizationResult{AuthID: req.AuthID, Status: AuthRevoked}, nil
}

// recordRevokeRejection 登记撤销授权的状态类业务拒绝（非授权人的
// ErrForbidden、授权人撤销已使用授权的 ErrAuthorizationUsed）并落盘。
// 拒绝结果以保存完成为准：落盘失败时显式撤销尚未保存的请求号占用并返回
// 实际保存错误，调用方必须返回空结果与该错误，不能只返回业务拒绝让调用者
// 误以为拒绝已经记住；落盘成功时返回 nil，由调用方返回业务拒绝本身。
func (r *Registry) recordRevokeRejection(key, sig string, req RevokeAuthorizationRequest, bizErr error) error {
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "auth_revoke",
		Params: sig, Rejected: true, Reason: errCode(bizErr), AuthID: req.AuthID,
	}
	if err := r.commit(); err != nil {
		// commit 失败时已尝试按磁盘内容重建状态，请求记录随之撤销；此处再
		// 删一次以覆盖磁盘暂时不可读、状态未能重建的情形——未保存的拒绝
		// 不能留在当前已打开的登记册中，也不能随随后另一次正常操作被保存。
		delete(r.state.Requests, key)
		return err
	}
	return nil
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
//
// 上述状态类拒绝以保存完成为准：拒绝结果尚未写入原登记册而保存失败（如
// 数据位置暂时无法写入）时，返回实际保存错误，不能只返回到期、无权或停用
// 等业务拒绝让调用者误以为这次拒绝已经记住；结果为空（不带授权或藏品编号、
// 不带转让和金额信息、不标回放，结果内的业务错误也为空），error 保留实际
// 写入失败原因。这次未保存的拒绝不改变授权内容、使用标记、授权变更记录，
// 也不改变藏品持有人、版本、转让历史与版税应付，并不占用操作者的本次请求
// 号——即使保存失败后原数据暂时无法读取、状态未能按磁盘重建，未保存的拒绝
// 也不能留在当前已打开的登记册中，或随随后另一次成功保存被记住。保存条件
// 恢复后用完全相同的请求重提，按重提时的业务状态重新判断，而不是回放失败
// 时的拒绝（例如到期拒绝保存失败、授权随后被撤销时，重提返回已撤销）；这次
// 重新处理若保存成功不标回放，此后原样重提才回放已保存的结果。已经成功保存
// 的拒绝仍按首次结果回放，改动该请求的业务内容仍报请求号冲突；必填内容缺失
// 或引用不存在沿用原错误且不占用请求号。
//
// 成功代转同样以保存完成为准：持有换人、版本加一、转让历史、版税应付、
// 历史序号、授权使用标记与使用记录、授权历史序号以及请求结果都尚未写入原
// 登记册而保存失败（如数据位置暂时无法写入）时，返回实际保存错误而非代转
// 成功，结果为空（不携带授权或藏品编号、前后持有人、版本、历史序号、价款、
// 应付或余款，业务错误为空，不标回放）。即使失败后原数据暂时无法读取、状态
// 未能按磁盘重建，这笔代转也必须从当前仍打开的登记册中整体撤销：藏品仍属于
// 提交前持有人、版本不增加、双方持有列表与藏品历史保持原状，授权没有使用
// 时间或关联转让序号、授权历史没有本次使用记录，版税查询没有本次计算记录、
// 收款账户不多出应付，请求号、藏品历史序号与授权历史序号都不被消耗；失败前
// 已存在的成功交易、授权与应付原样保留，随后另一次无关操作成功保存也不会把
// 这笔未保存的代转一并写入。读写条件恢复后受托人用原请求号、原因和授权编号
// 重提，按当时的授权、账户与持有状态重新处理：授权仍有效且条件仍满足时真正
// 完成一次代转（版本只增加一次、新增记录接续原有序号、价款仍取授权约定、
// 余款仍归授权人、不标回放），此后原样重提才回放这一笔已保存结果；授权已经
// 到期、撤销或持有版本已经变化时沿用对应拒绝，不能回放那次未保存的成功，也
// 不因恢复状态而延长授权有效期。
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
			// 状态类业务拒绝（无权、已撤销、已到期、已使用、停用、版本已
			// 变化）占用请求号并落盘：相同参数重提返回这一次拒绝。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "proxy_transfer",
				Params: sig, Rejected: true, Reason: errCode(bizErr), AuthID: req.AuthID,
			}
			if err := r.commit(); err != nil {
				// 拒绝结果落盘失败：这次拒绝没有被记住，与发行、转让、授权
				// 创建/撤销一致——不能用业务错误掩盖保存错误，调用者稍后用
				// 原请求重提时必须按当时状态重新判断，而不是回放一个实际没有
				// 保存的拒绝。此分支在写入前只动了请求记录，没有改变持有、
				// 授权或版税；commit 失败时已按磁盘内容重建状态，请求记录随之
				// 撤销，此处再删一次以覆盖磁盘暂时不可读、状态未能重建的情形。
				// 整体返回空结果：不带授权或藏品编号、不带转让和金额信息、不标
				// 回放、结果中业务错误为空，error 保留实际写入错误。
				delete(r.state.Requests, key)
				return ProxyTransferResult{}, err
			}
		}
		// 必填缺失、授权或账户不存在等校验错误不占用请求号，也不改变持有、
		// 授权状态或授权变更记录。
		return ProxyTransferResult{AuthID: req.AuthID, Err: bizErr}, bizErr
	}

	a := r.state.Authzs[req.AuthID]
	// 回滚基点：本次成功代转的全部改动（持有换人、版本加一、转让历史、
	// 版税应付、历史序号、授权使用标记与使用记录、授权历史序号、请求结果）
	// 都必须以一次保存完成为准。记录改动前的授权、持有、历史长度与两类
	// 历史序号，保存失败且磁盘暂时不可读、状态未能按磁盘重建时据此整体
	// 撤销，否则后续任何一次成功保存都会把这笔未保存的代转带进登记册，
	// 造成"已换人、授权已用、可回放"的幻影交易。
	prevAuthz := a
	prevHolding := r.state.Holdings[a.ItemID]
	prevNextSeq := r.state.NextSeq
	prevHistLen := len(r.state.History)
	prevNextAuthSeq := r.state.NextAuthSeq
	prevAuthEvents := len(r.state.AuthEvents)

	// 代转价款以授权创建时记载为准，执行时不得改价；余款归转让前持有
	// 人（授权人），不记给受托人。持有变化、历史、应付明细与授权使用
	// 状态在同一临界区内一次落盘；历史记录实际受托账户并可按授权编号
	// 追溯。
	out := r.applyTransfer(a.ItemID, a.ToID, req.Operator, req.Reason, req.RequestID, a.ID, a.Price)
	a.Status = "used"
	a.UsedTxSeq = out.seq
	a.UsedAt = now
	r.state.Authzs[a.ID] = a
	authSeq := prevNextAuthSeq + 1
	r.state.NextAuthSeq = authSeq
	r.state.AuthEvents = append(r.state.AuthEvents, authzEvent{
		Seq: authSeq, AuthID: a.ID, ItemID: a.ItemID, Kind: "use",
		Operator: req.Operator, Reason: req.Reason, RequestID: req.RequestID,
		FromStatus: AuthActive, ToStatus: AuthUsed, TxSeq: out.seq, OccurredAt: now,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "proxy_transfer",
		Params: sig, AuthID: a.ID, ItemID: a.ItemID, FromID: out.fromID, ToID: a.ToID,
		Version: out.toVer(), TxSeq: out.seq,
	}
	if err := r.commit(); err != nil {
		// commit 失败时已尝试按磁盘内容重建状态：重建成功则本次改动已随
		// 旧状态整体撤销（持有未换人、无本次历史与应付、授权未使用、无本次
		// 使用记录、请求记录不存在、两类序号未消耗），无需再动；磁盘暂时
		// 不可读、状态未能重建时本次改动仍在，据此把这笔未保存的代转全部
		// 撤销，让当前已打开的登记册回到提交前状态，也不被随后另一次成功
		// 保存带入。返回空结果：无授权或藏品编号、无前后持有人、无版本、无
		// 历史序号、无价款/应付/余款、不标回放、结果中业务错误为空，error
		// 保留实际写入错误。
		if _, pending := r.state.Requests[key]; pending {
			delete(r.state.Requests, key)
			delete(r.state.Royalties, out.seq)
			r.state.History = r.state.History[:prevHistLen]
			r.state.Holdings[a.ItemID] = prevHolding
			r.state.NextSeq = prevNextSeq
			r.state.AuthEvents = r.state.AuthEvents[:prevAuthEvents]
			r.state.NextAuthSeq = prevNextAuthSeq
			r.state.Authzs[a.ID] = prevAuthz
		}
		return ProxyTransferResult{}, err
	}
	return ProxyTransferResult{AuthID: a.ID, ItemID: a.ItemID, FromID: out.fromID,
		ToID: a.ToID, Version: out.toVer(), TxSeq: out.seq,
		Price: out.royalty.Price, Payables: publicPayables(out.royalty.Payees),
		Remainder: out.royalty.Remainder}, nil
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
