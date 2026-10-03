package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// stateVersion 是快照格式的版本标记，未来不兼容变更时可据此识别。
const stateVersion = 1

const (
	dirMode  = 0o755
	fileMode = 0o644
)

// snapshot 是登记册的全量落盘结构。账户、系列、藏品、持有、历史、
// 授权、授权变更记录与请求结果都在同一个快照中，一次操作一次原子
// 替换，保证"藏品已换人就一定有对应历史"。
type snapshot struct {
	Version  int                 `json:"version"`
	Accounts map[string]account  `json:"accounts"`
	Series   map[string]series   `json:"series"`
	Items    map[string]item     `json:"items"`
	Holdings map[string]holding  `json:"holdings"`
	History  []historyEntry      `json:"history"`
	Requests map[string]request  `json:"requests"`
	Authzs   map[string]authzRec `json:"authzs,omitempty"`
	// AuthEvents 是按藏品记录的授权变更（创建/撤销/使用），在藏品
	// 历史之外单独编号，不影响既有历史序号。
	AuthEvents  []authzEvent `json:"auth_events,omitempty"`
	NextSeq     int64        `json:"next_seq"`
	NextAuthSeq int64        `json:"next_auth_seq,omitempty"`
	// Royalties 以转让历史序号为键，记录每笔成功转让的版税计算依据与
	// 应付明细；旧登记册中的转让没有记录，查询时按价款 0 处理，不补造。
	Royalties map[int64]royaltyRec `json:"royalties,omitempty"`
	// RoyaltyEvents 是版税规则变更记录，单独编号。
	RoyaltyEvents  []royaltyEvent `json:"royalty_events,omitempty"`
	NextRoyaltySeq int64          `json:"next_royalty_seq,omitempty"`
	// Intents 是拆分意向表；只记录未来拆分的约定，不影响持有与版税。
	Intents map[string]intentRec `json:"intents,omitempty"`
	// IntentEvents 是拆分意向变更记录（创建/首次答复/撤回），单独编号。
	IntentEvents  []intentEvent `json:"intent_events,omitempty"`
	NextIntentSeq int64         `json:"next_intent_seq,omitempty"`
}

type account struct {
	ID       string `json:"id"`
	Metadata string `json:"metadata"`
	Active   bool   `json:"active"`
}

type series struct {
	ID        string `json:"id"`
	CreatorID string `json:"creator_id"`
	Metadata  string `json:"metadata"`
	Sealed    bool   `json:"sealed"`
	// Royalty 是当前版税规则（按收款账户排序）；空表示不收版税。
	// 首次成功发行或封存后固定，不能再设置。
	Royalty []royaltyShare `json:"royalty,omitempty"`
}

// royaltyShare 是版税规则中一个收款账户的份额。
type royaltyShare struct {
	AccountID string `json:"account_id"`
	Rate      int64  `json:"rate"`
}

// royaltyRec 是一笔成功转让的版税应付记录，与持有变化、历史、请求结果
// 在同一临界区内一次落盘。
type royaltyRec struct {
	TxSeq     int64         `json:"tx_seq"`
	ItemID    string        `json:"item_id"`
	SeriesID  string        `json:"series_id"`
	Price     int64         `json:"price"`
	Payees    []payableLine `json:"payees,omitempty"`
	Remainder int64         `json:"remainder"`
	// OwnerID 是转让前持有人，即剩余收入的归属账户（代转时是授权人，
	// 不是受托人）。
	OwnerID string `json:"owner_id"`
}

// payableLine 是一笔转让中某个收款账户的应付明细，零金额也保留。
type payableLine struct {
	AccountID string `json:"account_id"`
	Rate      int64  `json:"rate"`
	Amount    int64  `json:"amount"`
}

// royaltyEvent 是版税规则变更记录：每次成功设置或清空各一条。
type royaltyEvent struct {
	Seq        int64          `json:"seq"`
	SeriesID   string         `json:"series_id"`
	Operator   string         `json:"operator"`
	Reason     string         `json:"reason"`
	RequestID  string         `json:"request_id"`
	Before     []royaltyShare `json:"before,omitempty"`
	After      []royaltyShare `json:"after,omitempty"`
	OccurredAt time.Time      `json:"occurred_at"`
}

type item struct {
	ID         string `json:"id"`
	SeriesID   string `json:"series_id"`
	BatchNo    string `json:"batch_no"`
	Metadata   string `json:"metadata"`
	IssuedTxID int64  `json:"issued_tx_id"`
}

type holding struct {
	ItemID  string `json:"item_id"`
	OwnerID string `json:"owner_id"`
	Version int64  `json:"version"`
}

type historyEntry struct {
	Seq         int64  `json:"seq"`
	Kind        string `json:"kind"`
	ItemID      string `json:"item_id"`
	Operator    string `json:"operator"`
	Reason      string `json:"reason"`
	RequestID   string `json:"request_id"`
	FromID      string `json:"from_id"`
	ToID        string `json:"to_id"`
	FromVersion int64  `json:"from_version"`
	ToVersion   int64  `json:"to_version"`
	// AuthID 非空表示该转让是受托人凭授权完成的代转。
	AuthID string `json:"auth_id,omitempty"`
}

// authzRec 是一份限时一次性代转授权的落盘状态。授权在创建时绑定藏品
// 当时的持有版本（GrantVer）；持有关系后续无论怎样变化，授权都不会
// 恢复或改变。
type authzRec struct {
	ID        string    `json:"id"`
	ItemID    string    `json:"item_id"`
	GranterID string    `json:"granter_id"`
	TrusteeID string    `json:"trustee_id"`
	ToID      string    `json:"to_id"`
	ExpiresAt time.Time `json:"expires_at"`
	GrantVer  int64     `json:"grant_ver"`
	// Price 是创建授权时确定的代转成交价款（分）；旧数据中的授权没有
	// 该字段，按 0 处理。
	Price int64 `json:"price,omitempty"`
	// Status 只记录非时间派生的终态："revoked" 或 "used"；为空表示
	// 尚未终结，当前是否有效由到期时间实时判断。
	Status    string    `json:"status,omitempty"`
	UsedTxSeq int64     `json:"used_tx_seq,omitempty"`
	UsedAt    time.Time `json:"used_at,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	RevokedAt time.Time `json:"revoked_at,omitempty"`
}

// authzEvent 是授权变更记录：创建、撤销、使用各一条。
type authzEvent struct {
	Seq        int64     `json:"seq"`
	AuthID     string    `json:"auth_id"`
	ItemID     string    `json:"item_id"`
	Kind       string    `json:"kind"` // "create" / "revoke" / "use"
	Operator   string    `json:"operator"`
	Reason     string    `json:"reason"`
	RequestID  string    `json:"request_id"`
	FromStatus string    `json:"from_status,omitempty"`
	ToStatus   string    `json:"to_status"`
	TxSeq      int64     `json:"tx_seq,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// intentRec 是一份藏品拆分意向的落盘状态。意向在创建时绑定藏品当时的
// 持有版本（GrantVer）；持有版本变化或发起人、任一参与账户停用后意向
// 失效，且不因藏品转回原持有人而恢复。
type intentRec struct {
	ID          string        `json:"id"`
	ItemID      string        `json:"item_id"`
	InitiatorID string        `json:"initiator_id"`
	Shares      []intentShare `json:"shares"` // 按账户排序的分配方案
	GrantVer    int64         `json:"grant_ver"`
	ExpiresAt   time.Time     `json:"expires_at"`
	// Status 只记录非时间派生的终态："rejected" 或 "withdrawn"；为空表示
	// 尚未终结，当前是否有效由持有版本、账户状态与到期时间实时判断。
	Status    string    `json:"status,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}

// intentShare 是拆分意向中一个参与账户的份额与答复。
type intentShare struct {
	AccountID string `json:"account_id"`
	Share     int64  `json:"share"`
	// Answer 是该账户的答复："agree" / "reject"；空表示尚未答复。
	// 发起人列入方案时创建即为 "agree"。
	Answer string `json:"answer,omitempty"`
}

// intentEvent 是拆分意向变更记录：创建、首次答复、撤回各一条；重复答复
// 与重复撤回不新增记录。
type intentEvent struct {
	Seq        int64     `json:"seq"`
	IntentID   string    `json:"intent_id"`
	ItemID     string    `json:"item_id"`
	Kind       string    `json:"kind"` // "create" / "answer" / "withdraw"
	Operator   string    `json:"operator"`
	Reason     string    `json:"reason"`
	RequestID  string    `json:"request_id"`
	Answer     string    `json:"answer,omitempty"`
	FromStatus string    `json:"from_status,omitempty"`
	ToStatus   string    `json:"to_status"`
	OccurredAt time.Time `json:"occurred_at"`
}

// request 保存每个 (操作者, 请求号) 的首次结果，用于幂等回放。
// 同一操作者的请求号在发行、转让、授权创建/撤销、代转与版税规则设置
// 之间共用。
type request struct {
	Operator  string `json:"operator"`
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"` // "issue" / "issue_batch" / "transfer" / "transfer_batch" / "auth_create" / "auth_revoke" / "proxy_transfer" / "royalty_set" / "intent_create" / "intent_answer" / "intent_withdraw"
	// 规范化后的业务参数签名。签名一致才回放；不一致报请求号冲突。
	Params string `json:"params"`
	// 业务拒绝也落盘：相同参数重提返回首次的拒绝。
	Rejected bool   `json:"rejected"`
	Reason   string `json:"reject_reason"` // 哨兵错误对应的稳定标识
	// 成功结果。
	ItemID  string `json:"item_id,omitempty"`
	AuthID  string `json:"auth_id,omitempty"`
	FromID  string `json:"from_id,omitempty"`
	ToID    string `json:"to_id,omitempty"`
	Version int64  `json:"version,omitempty"`
	TxSeq   int64  `json:"tx_seq,omitempty"`
	// IntentID 与 Status 记录拆分意向类请求的结果（意向编号与操作后的
	// 意向状态）。
	IntentID string `json:"intent_id,omitempty"`
	Status   string `json:"status,omitempty"`
	// SeriesID 与 Shares 记录版税规则设置的结果（本次生效的规则）。
	SeriesID string         `json:"series_id,omitempty"`
	Shares   []royaltyShare `json:"shares,omitempty"`
	// Batch 记录整批发行各件的首次结果（按提交顺序），仅 kind 为
	// issue_batch 时使用；旧登记册中的请求没有该字段。
	Batch []batchItemResult `json:"batch,omitempty"`
	// TransferBatch 记录整批转让各件的首次结果（按提交顺序），仅 kind
	// 为 transfer_batch 时使用；版税金额回放时从 Royalties 按序号补全。
	TransferBatch []batchTransferItemResult `json:"transfer_batch,omitempty"`
}

// batchItemResult 是整批发行中一件藏品落盘的首次发行结果，用于幂等回放。
type batchItemResult struct {
	ItemID  string `json:"item_id"`
	OwnerID string `json:"owner_id"`
	TxSeq   int64  `json:"tx_seq"`
}

// batchTransferItemResult 是整批转让中一件藏品落盘的首次转让结果，用于
// 幂等回放；价款、应付明细与余款按 TxSeq 从版税应付记录补全。
type batchTransferItemResult struct {
	ItemID  string `json:"item_id"`
	FromID  string `json:"from_id"`
	ToID    string `json:"to_id"`
	Version int64  `json:"version"`
	TxSeq   int64  `json:"tx_seq"`
}

func newSnapshot() *snapshot {
	return &snapshot{
		Version:   stateVersion,
		Accounts:  make(map[string]account),
		Series:    make(map[string]series),
		Items:     make(map[string]item),
		Holdings:  make(map[string]holding),
		Requests:  make(map[string]request),
		Authzs:    make(map[string]authzRec),
		Royalties: make(map[int64]royaltyRec),
		Intents:   make(map[string]intentRec),
	}
}

func (s *snapshot) validate() error {
	if s.Version != stateVersion {
		return fmt.Errorf("%w: 不支持的快照版本 %d", ErrCorrupt, s.Version)
	}
	if s.Accounts == nil || s.Series == nil || s.Items == nil ||
		s.Holdings == nil || s.Requests == nil {
		return fmt.Errorf("%w: 快照内容不完整", ErrCorrupt)
	}
	if s.NextSeq < int64(len(s.History)) {
		return fmt.Errorf("%w: 历史序号不连续", ErrCorrupt)
	}
	if s.Authzs == nil {
		// 兼容旧版本写入的快照：授权表按需惰性建立。
		s.Authzs = make(map[string]authzRec)
	}
	if s.NextAuthSeq < int64(len(s.AuthEvents)) {
		return fmt.Errorf("%w: 授权变更序号不连续", ErrCorrupt)
	}
	if s.Royalties == nil {
		// 兼容旧版本写入的快照：版税应付表按需惰性建立；旧转让没有
		// 应付记录，查询时按价款 0 处理，不补造明细。
		s.Royalties = make(map[int64]royaltyRec)
	}
	if s.NextRoyaltySeq < int64(len(s.RoyaltyEvents)) {
		return fmt.Errorf("%w: 版税规则变更序号不连续", ErrCorrupt)
	}
	if s.Intents == nil {
		// 兼容旧版本写入的快照：拆分意向表按需惰性建立。
		s.Intents = make(map[string]intentRec)
	}
	if s.NextIntentSeq < int64(len(s.IntentEvents)) {
		return fmt.Errorf("%w: 拆分意向变更序号不连续", ErrCorrupt)
	}
	s.rekeyRequests()
	return nil
}

// rekeyRequests 按每条请求记录自身保存的完整操作者账户编号与请求号重建
// 请求表的键。旧版本快照的键由账户编号与请求号直接拼接而成，含零字符
// （U+0000）时不同账户的不同请求会得到相同的键；落盘记录本身始终保存
// 真实的提交账户与请求号，据此重建后旧请求仍归原提交账户所有，使用原
// 参数即可回放原结果，碰巧混淆的另一组编号也不会占用或覆盖原请求。
// 该重建对任意字符组合都是幂等的，每次加载都执行。
func (s *snapshot) rekeyRequests() {
	reqs := make(map[string]request, len(s.Requests))
	for _, req := range s.Requests {
		reqs[requestKey(req.Operator, req.RequestID)] = req
	}
	s.Requests = reqs
}

// store 负责一个登记册目录的加锁、读取与原子写入。
type store struct {
	dir  string
	lock *os.File // 跨进程 flock
}

func dataFile(dir string) string { return filepath.Join(dir, "registry.json") }
func lockFile(dir string) string { return filepath.Join(dir, "registry.lock") }
func tempFile(dir string) string { return filepath.Join(dir, ".registry.json.tmp") }

// acquireLock 在登记册目录中以非阻塞方式取独占 flock。
func acquireLock(dir string) (*os.File, error) {
	f, err := os.OpenFile(lockFile(dir), os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, fmt.Errorf("registry: 无法打开锁文件: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("registry: 无法锁定登记册: %w", err)
	}
	return f, nil
}

// load 读取快照。文件不存在时返回空快照；文件存在但无法读取或解析时
// 返回 ErrCorrupt，绝不静默当成空登记册。
func load(dir string) (*snapshot, error) {
	b, err := os.ReadFile(dataFile(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newSnapshot(), nil
		}
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: 数据文件为空", ErrCorrupt)
	}
	var s snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// save 以 temp 文件 + fsync + rename + 目录 fsync 的方式原子替换快照。
// 在持有全局锁时调用：写入与状态修改在同一临界区，进程被杀时要么
// 完整保留旧状态，要么完整呈现新状态。
func (st *store) save(s *snapshot) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("registry: 序列化失败: %w", err)
	}
	tmp := tempFile(st.dir)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode)
	if err != nil {
		return fmt.Errorf("registry: 无法写入登记册: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("registry: 写入失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("registry: 落盘失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("registry: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, dataFile(st.dir)); err != nil {
		return fmt.Errorf("registry: 替换数据文件失败: %w", err)
	}
	if dirFd, err := os.Open(st.dir); err == nil {
		_ = dirFd.Sync()
		dirFd.Close()
	}
	ok = true
	return nil
}
