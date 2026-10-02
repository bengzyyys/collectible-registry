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
	AuthEvents []authzEvent `json:"auth_events,omitempty"`
	// RoyaltyEvents 是按系列记录的版税规则设置/清空记录。
	RoyaltyEvents []royaltyEvent `json:"royalty_events,omitempty"`
	// RoyaltyRecords 是每次成功转让（含代转）的版税计算依据与应付明细，
	// 按转让序号排列。旧版本数据不补造，查询时按零价款处理。
	RoyaltyRecords []royaltyRecord `json:"royalty_records,omitempty"`
	NextSeq        int64           `json:"next_seq"`
	NextAuthSeq    int64           `json:"next_auth_seq,omitempty"`
	NextRoyaltySeq int64           `json:"next_royalty_seq,omitempty"`
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
	// Royalty 是首次发行前设置的版税规则；为空表示不收版税。
	Royalty *royaltyRule `json:"royalty,omitempty"`
	// RoyaltyFixed 在系列首次成功发行后置为 true，此后规则不可更改。
	RoyaltyFixed bool `json:"royalty_fixed,omitempty"`
}

// royaltyRule 是系列版税规则的落盘形式。
type royaltyRule struct {
	Entries []royaltyEntry `json:"entries"`
}

type royaltyEntry struct {
	Account string `json:"account"`
	Ratio   int    `json:"ratio"`
}

// royaltyEvent 是一次版税规则设置/清空记录。
type royaltyEvent struct {
	Seq        int64        `json:"seq"`
	SeriesID   string       `json:"series_id"`
	Operator   string       `json:"operator"`
	Reason     string       `json:"reason"`
	RequestID  string       `json:"request_id"`
	FromRule   *royaltyRule `json:"from_rule,omitempty"`
	ToRule     *royaltyRule `json:"to_rule,omitempty"`
	OccurredAt time.Time    `json:"occurred_at"`
}

// royaltyRecord 是一次成功转让的版税计算依据与应付明细，与持有变化、
// 历史在同一临界区内原子落盘。
type royaltyRecord struct {
	TxSeq     int64          `json:"tx_seq"`
	ItemID    string         `json:"item_id"`
	Price     int64          `json:"price"`     // 成交价款（分）
	PayerID   string         `json:"payer_id"`  // 余款归属（转让前持有人）
	Remainder int64          `json:"remainder"` // 价款扣除各收款人金额后的余款
	Payees    []royaltyPayee `json:"payees,omitempty"`
}

type royaltyPayee struct {
	Account string `json:"account"`
	Ratio   int    `json:"ratio"`
	Amount  int64  `json:"amount"`
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
	// Price 是创建授权时确定的代转成交价款（分）；执行时不得更改，
	// 旧版本数据写出的授权没有该字段，按零处理。
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

// request 保存每个 (操作者, 请求号) 的首次结果，用于幂等回放。
// 同一操作者的请求号在发行、转让、授权创建/撤销、代转与版税规则设置之间共用。
type request struct {
	Operator  string `json:"operator"`
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"` // "issue" / "transfer" / "auth_create" / "auth_revoke" / "proxy_transfer" / "royalty_set"
	// 规范化后的业务参数签名。签名一致才回放；不一致报请求号冲突。
	Params string `json:"params"`
	// 业务拒绝也落盘：相同参数重提返回首次的拒绝。
	Rejected bool   `json:"rejected"`
	Reason   string `json:"reject_reason"` // 哨兵错误对应的稳定标识
	// 成功结果。
	ItemID   string `json:"item_id,omitempty"`
	AuthID   string `json:"auth_id,omitempty"`
	SeriesID string `json:"series_id,omitempty"`
	FromID   string `json:"from_id,omitempty"`
	ToID     string `json:"to_id,omitempty"`
	Version  int64  `json:"version,omitempty"`
	TxSeq    int64  `json:"tx_seq,omitempty"`
}

func newSnapshot() *snapshot {
	return &snapshot{
		Version:  stateVersion,
		Accounts: make(map[string]account),
		Series:   make(map[string]series),
		Items:    make(map[string]item),
		Holdings: make(map[string]holding),
		Requests: make(map[string]request),
		Authzs:   make(map[string]authzRec),
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
	if s.NextRoyaltySeq < int64(len(s.RoyaltyEvents)) {
		return fmt.Errorf("%w: 版税变更序号不连续", ErrCorrupt)
	}
	// 兼容旧版本写入的快照：旧数据没有版税固定标记，凡有藏品发行的系列
	// 规则一律视为已固定（未设置即无版税），打开后不能再设置。
	for _, it := range s.Items {
		if ser, ok := s.Series[it.SeriesID]; ok && !ser.RoyaltyFixed {
			ser.RoyaltyFixed = true
			s.Series[it.SeriesID] = ser
		}
	}
	return nil
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
