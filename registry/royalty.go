package registry

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// maxRoyaltyRatio 是版税比例的上限：单位为万分之一，合计不得超过 10000。
const maxRoyaltyRatio = 10000

func (req SetRoyaltyRequest) validatePresent() error {
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
	if strings.TrimSpace(req.SeriesID) == "" {
		missing = append(missing, "series_id")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: 版税设置请求缺少必填字段 %s", ErrInvalidArgument, strings.Join(missing, ", "))
	}
	return nil
}

// validateEntries 校验收款条目：账户非空、比例为 1 至 10000 的整数、
// 同一账户不重复、合计不超过 10000。任一不成立均为参数错误，不占用请求号。
func validateEntries(entries []RoyaltyEntry) error {
	seen := make(map[string]bool)
	sum := 0
	for _, e := range entries {
		if strings.TrimSpace(e.Account) == "" {
			return fmt.Errorf("%w: 版税收款账户不能为空", ErrInvalidArgument)
		}
		if e.Ratio < 1 || e.Ratio > maxRoyaltyRatio {
			return fmt.Errorf("%w: 收款账户 %s 的比例 %d 必须在 1 至 10000 之间",
				ErrInvalidArgument, e.Account, e.Ratio)
		}
		if seen[e.Account] {
			return fmt.Errorf("%w: 收款账户 %s 在版税规则中重复出现", ErrInvalidArgument, e.Account)
		}
		seen[e.Account] = true
		sum += e.Ratio
	}
	if sum > maxRoyaltyRatio {
		return fmt.Errorf("%w: 版税比例合计 %d 超过 10000", ErrInvalidArgument, sum)
	}
	return nil
}

func setRoyaltyParamsSig(req SetRoyaltyRequest) string {
	// 条目顺序不影响规则语义，签名前按账户编号排序，使相同规则幂等。
	entries := make([]RoyaltyEntry, len(req.Entries))
	copy(entries, req.Entries)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Account < entries[j].Account })
	b, _ := json.Marshal(struct {
		Kind     string         `json:"kind"`
		SeriesID string         `json:"series_id"`
		Entries  []RoyaltyEntry `json:"entries"`
		Reason   string         `json:"reason"`
	}{"royalty_set", req.SeriesID, entries, req.Reason})
	return string(b)
}

// SetSeriesRoyalty 由系列创建账户在首次发行前设置或清空版税规则。收款
// 账户必须已登记且可用；每个账户只能出现一次，比例为 1 至 10000 的整数
// （万分之一），合计不超过 10000；空规则表示不收版税。重复账户、比例
// 不合法或合计超限按参数错误拒绝，原规则不变。系列已封存或已有藏品
// 发行（规则已固定）后拒绝设置。
//
// 请求号与发行、转让、授权操作共用同一操作者的请求号范围：相同业务
// 参数重提返回首次结果（成功或状态类业务拒绝），参数变化返回
// ErrRequestConflict；参数错误与引用不存在不占用请求号。
func (r *Registry) SetSeriesRoyalty(req SetRoyaltyRequest) (SetRoyaltyResult, error) {
	if err := req.validatePresent(); err != nil {
		return SetRoyaltyResult{}, err
	}
	if err := validateEntries(req.Entries); err != nil {
		return SetRoyaltyResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return SetRoyaltyResult{}, err
	}

	key := requestKey(req.Operator, req.RequestID)
	sig := setRoyaltyParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replaySetRoyalty(prev, sig)
	}

	if bizErr := r.checkSetRoyalty(req); bizErr != nil {
		if !isValidationErr(bizErr) {
			// 状态类业务拒绝（停用、无权、封存、规则已固定、收款账户停用）
			// 占用请求号并落盘；参数错误与引用不存在不占用。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "royalty_set",
				Params: sig, Rejected: true, Reason: errCode(bizErr),
				SeriesID: req.SeriesID,
			}
			_ = r.commit()
		}
		return SetRoyaltyResult{SeriesID: req.SeriesID, Err: bizErr}, bizErr
	}

	now := r.now()
	ser := r.state.Series[req.SeriesID]
	from := cloneRoyaltyRule(ser.Royalty)
	to := &royaltyRule{Entries: make([]royaltyEntry, 0, len(req.Entries))}
	for _, e := range req.Entries {
		to.Entries = append(to.Entries, royaltyEntry{Account: e.Account, Ratio: e.Ratio})
	}
	if len(to.Entries) == 0 {
		to = nil // 空规则表示不收版税
	}
	ser.Royalty = to
	r.state.Series[req.SeriesID] = ser

	seq := r.state.NextRoyaltySeq + 1
	r.state.NextRoyaltySeq = seq
	r.state.RoyaltyEvents = append(r.state.RoyaltyEvents, royaltyEvent{
		Seq: seq, SeriesID: req.SeriesID, Operator: req.Operator,
		Reason: req.Reason, RequestID: req.RequestID,
		FromRule: from, ToRule: cloneRoyaltyRule(to), OccurredAt: now,
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "royalty_set",
		Params: sig, SeriesID: req.SeriesID,
	}
	if err := r.commit(); err != nil {
		return SetRoyaltyResult{}, err
	}
	return SetRoyaltyResult{SeriesID: req.SeriesID}, nil
}

func (r *Registry) checkSetRoyalty(req SetRoyaltyRequest) error {
	ser, ok := r.state.Series[req.SeriesID]
	if !ok {
		return fmt.Errorf("%w: 系列 %s", ErrNotFound, req.SeriesID)
	}
	op, ok := r.state.Accounts[req.Operator]
	if !ok {
		return fmt.Errorf("%w: 操作者账户 %s", ErrNotFound, req.Operator)
	}
	// 收款账户必须已登记（存在性为引用类校验，不占用请求号）。
	for _, e := range req.Entries {
		if _, ok := r.state.Accounts[e.Account]; !ok {
			return fmt.Errorf("%w: 收款账户 %s", ErrNotFound, e.Account)
		}
	}
	if !op.Active {
		return fmt.Errorf("%w: 操作者账户 %s", ErrAccountInactive, req.Operator)
	}
	for _, e := range req.Entries {
		a := r.state.Accounts[e.Account]
		if !a.Active {
			return fmt.Errorf("%w: 收款账户 %s", ErrAccountInactive, e.Account)
		}
	}
	// 只有系列创建账户可以设置版税规则。
	if ser.CreatorID != req.Operator {
		return fmt.Errorf("%w: 只有创建账户 %s 可以设置系列 %s 的版税规则",
			ErrForbidden, ser.CreatorID, req.SeriesID)
	}
	// 系列已封存后拒绝设置。
	if ser.Sealed {
		return fmt.Errorf("%w: 系列 %s 已封存，不能设置版税规则", ErrSeriesSealed, req.SeriesID)
	}
	// 首次成功发行后规则固定：未设置即固定为无版税。
	if ser.RoyaltyFixed {
		return fmt.Errorf("%w: 系列 %s 已有藏品发行，版税规则已固定", ErrConflict, req.SeriesID)
	}
	return nil
}

func (r *Registry) replaySetRoyalty(prev request, sig string) (SetRoyaltyResult, error) {
	if prev.Kind != "royalty_set" || prev.Params != sig {
		return SetRoyaltyResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := SetRoyaltyResult{SeriesID: prev.SeriesID, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		return res, res.Err
	}
	return res, nil
}

func cloneRoyaltyRule(rule *royaltyRule) *royaltyRule {
	if rule == nil {
		return nil
	}
	out := &royaltyRule{Entries: make([]royaltyEntry, len(rule.Entries))}
	copy(out.Entries, rule.Entries)
	return out
}

// royaltyRuleForItem 返回藏品所属系列的当前版税规则；系列不存在时返回 nil。
func (r *Registry) royaltyRuleForItem(itemID string) *royaltyRule {
	it, ok := r.state.Items[itemID]
	if !ok {
		return nil
	}
	ser, ok := r.state.Series[it.SeriesID]
	if !ok {
		return nil
	}
	return ser.Royalty
}

// royaltyAmount 计算单条收款的应付金额：价款 * 比例 / 10000 向下取整。
// 价款为非负 int64，直接相乘可能溢出；拆成价款 = q*10000 + r 后
// amount = q*ratio + r*ratio/10000，两个中间量均不超过 int64 上限，
// 且结果本身不超过价款，因此整个价款范围内都准确。
func royaltyAmount(price int64, ratio int) int64 {
	q := price / maxRoyaltyRatio
	rem := price % maxRoyaltyRatio
	return q*int64(ratio) + (rem*int64(ratio))/maxRoyaltyRatio
}

// buildRoyaltyRecord 生成一次转让的版税计算依据与明细。零金额的收款人
// 也保留明细；余款（价款扣除全部收款人金额）归转让前持有人；代转时
// 转让前持有人是授权人，不会记给受托人。
func (r *Registry) buildRoyaltyRecord(txSeq int64, itemID, payerID string, price int64, rule *royaltyRule) royaltyRecord {
	rec := royaltyRecord{
		TxSeq: txSeq, ItemID: itemID, Price: price, PayerID: payerID,
		Payees: make([]royaltyPayee, 0),
	}
	if rule != nil {
		var sum int64
		for _, e := range rule.Entries {
			amount := royaltyAmount(price, e.Ratio)
			sum += amount
			rec.Payees = append(rec.Payees, royaltyPayee{
				Account: e.Account, Ratio: e.Ratio, Amount: amount,
			})
		}
		rec.Remainder = price - sum
	}
	return rec
}

// GetSeriesRoyalty 查询系列当前版税规则。系列不存在时返回包裹
// ErrNotFound 的错误；Fixed 表示规则是否已因首次发行而固定。
func (r *Registry) GetSeriesRoyalty(seriesID string) (SeriesRoyalty, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return SeriesRoyalty{}, err
	}
	ser, ok := r.state.Series[seriesID]
	if !ok {
		return SeriesRoyalty{}, fmt.Errorf("%w: 系列 %s", ErrNotFound, seriesID)
	}
	out := SeriesRoyalty{
		SeriesID: seriesID, Fixed: ser.RoyaltyFixed,
		Entries: make([]RoyaltyEntry, 0),
	}
	if ser.Royalty != nil {
		for _, e := range ser.Royalty.Entries {
			out.Entries = append(out.Entries, RoyaltyEntry{Account: e.Account, Ratio: e.Ratio})
		}
	}
	return out, nil
}

// RoyaltyHistory 按系列查看版税规则的设置/清空记录，按发生先后排列；
// 每条包含操作者、原因、请求号与前后内容。系列不存在时返回包裹
// ErrNotFound 的错误。
func (r *Registry) RoyaltyHistory(seriesID string) ([]RoyaltyEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return nil, err
	}
	if _, ok := r.state.Series[seriesID]; !ok {
		return nil, fmt.Errorf("%w: 系列 %s", ErrNotFound, seriesID)
	}
	out := make([]RoyaltyEvent, 0)
	for _, e := range r.state.RoyaltyEvents {
		if e.SeriesID != seriesID {
			continue
		}
		ev := RoyaltyEvent{
			Seq: e.Seq, SeriesID: e.SeriesID, Operator: e.Operator,
			Reason: e.Reason, RequestID: e.RequestID, OccurredAt: e.OccurredAt,
		}
		if e.FromRule != nil {
			ev.FromRule = toPublicEntries(e.FromRule.Entries)
		}
		if e.ToRule != nil {
			ev.ToRule = toPublicEntries(e.ToRule.Entries)
		}
		out = append(out, ev)
	}
	return out, nil
}

func toPublicEntries(entries []royaltyEntry) []RoyaltyEntry {
	out := make([]RoyaltyEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, RoyaltyEntry{Account: e.Account, Ratio: e.Ratio})
	}
	return out
}

// GetTransferRoyalty 按转让记录（藏品编号 + 转让历史序号）查询版税计算
// 依据与全部金额。藏品或转让记录不存在时返回包裹 ErrNotFound 的错误；
// 旧版本数据中的成交没有补造明细，按价款零、无收款明细返回。
func (r *Registry) GetTransferRoyalty(itemID string, txSeq int64) (TransferRoyalty, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return TransferRoyalty{}, err
	}
	if _, ok := r.state.Items[itemID]; !ok {
		return TransferRoyalty{}, fmt.Errorf("%w: 藏品 %s", ErrNotFound, itemID)
	}
	for _, rec := range r.state.RoyaltyRecords {
		if rec.ItemID == itemID && rec.TxSeq == txSeq {
			return toPublicTransferRoyalty(rec), nil
		}
	}
	// 旧成交不补造应付明细：确认该序号确为该藏品的转让记录后，返回
	// 零价款、无收款明细的计算依据。
	for _, e := range r.state.History {
		if e.ItemID == itemID && e.Seq == txSeq && e.Kind == "transfer" {
			return TransferRoyalty{
				TxSeq: txSeq, ItemID: itemID, Price: 0,
				PayerID: e.FromID, Payees: make([]RoyaltyPayee, 0),
			}, nil
		}
	}
	return TransferRoyalty{}, fmt.Errorf("%w: 藏品 %s 的转让记录 %d", ErrNotFound, itemID, txSeq)
}

func toPublicTransferRoyalty(rec royaltyRecord) TransferRoyalty {
	out := TransferRoyalty{
		TxSeq: rec.TxSeq, ItemID: rec.ItemID, Price: rec.Price,
		PayerID: rec.PayerID, Payees: make([]RoyaltyPayee, 0, len(rec.Payees)),
	}
	for _, p := range rec.Payees {
		out.Payees = append(out.Payees, RoyaltyPayee{
			Account: p.Account, Ratio: p.Ratio, Amount: p.Amount,
		})
	}
	return out
}

// RoyaltyPayablesOf 按收款账户查看应付明细，按转让序号排列。账户不存在
// 时返回包裹 ErrNotFound 的错误；账户没有任何应付明细时返回空列表。
// 仅作为收款人的账户后来停用不影响其明细查询。
func (r *Registry) RoyaltyPayablesOf(accountID string) ([]RoyaltyPayable, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return nil, err
	}
	if _, ok := r.state.Accounts[accountID]; !ok {
		return nil, fmt.Errorf("%w: 账户 %s", ErrNotFound, accountID)
	}
	out := make([]RoyaltyPayable, 0)
	for _, rec := range r.state.RoyaltyRecords {
		for _, p := range rec.Payees {
			if p.Account != accountID {
				continue
			}
			out = append(out, RoyaltyPayable{
				TxSeq: rec.TxSeq, ItemID: rec.ItemID, Ratio: p.Ratio,
				Price: rec.Price, Amount: p.Amount,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TxSeq != out[j].TxSeq {
			return out[i].TxSeq < out[j].TxSeq
		}
		return out[i].ItemID < out[j].ItemID
	})
	return out, nil
}
