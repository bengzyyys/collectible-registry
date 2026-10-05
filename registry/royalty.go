package registry

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// ---- 版税规则设置 ----

// normalizeShares 校验并规范化规则份额：收款账户去重、比例范围与合计
// 校验（均为无状态的参数校验），并按收款账户排序，使同一规则与书写
// 顺序无关。返回规范化后的份额；参数不合法时返回 ErrInvalidArgument。
func normalizeShares(shares []RoyaltyShare) ([]royaltyShare, error) {
	seen := make(map[string]bool, len(shares))
	out := make([]royaltyShare, 0, len(shares))
	var total int64
	for _, sh := range shares {
		if strings.TrimSpace(sh.AccountID) == "" {
			return nil, fmt.Errorf("%w: 版税收款账户不能为空", ErrInvalidArgument)
		}
		if sh.Rate < 1 || sh.Rate > RoyaltyRateBase {
			return nil, fmt.Errorf("%w: 账户 %s 的版税比例 %d 不在 1..%d 内",
				ErrInvalidArgument, sh.AccountID, sh.Rate, RoyaltyRateBase)
		}
		if seen[sh.AccountID] {
			return nil, fmt.Errorf("%w: 版税收款账户 %s 重复", ErrInvalidArgument, sh.AccountID)
		}
		seen[sh.AccountID] = true
		total += sh.Rate
		out = append(out, royaltyShare{AccountID: sh.AccountID, Rate: sh.Rate})
	}
	if total > RoyaltyRateBase {
		return nil, fmt.Errorf("%w: 版税比例合计 %d 超过 %d", ErrInvalidArgument, total, RoyaltyRateBase)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out, nil
}

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
		return fmt.Errorf("%w: 设置版税规则请求缺少必填字段 %s", ErrInvalidArgument, strings.Join(missing, ", "))
	}
	return nil
}

func setRoyaltyParamsSig(req SetRoyaltyRequest, shares []royaltyShare) string {
	b, _ := json.Marshal(struct {
		Kind     string         `json:"kind"`
		SeriesID string         `json:"series_id"`
		Shares   []royaltyShare `json:"shares"`
		Reason   string         `json:"reason"`
	}{"royalty_set", req.SeriesID, shares, req.Reason})
	return string(b)
}

// SetRoyalty 设置或清空系列的版税规则，并记录本次变更的操作者、原因、
// 请求号与前后内容。只有系列创建账户（且可用）可以设置；每个收款账户
// 只能出现一次，比例是 1..10000 的整数（万分之一），合计不超过 10000；
// Shares 为空表示清空规则、不收版税。首次成功发行后规则固定（未设置
// 即固定为无版税），封存后同样拒绝设置。
//
// 请求号与发行、转让等操作共用同一操作者的请求号范围：相同业务参数
// 重提返回首次结果，参数不同返回 ErrRequestConflict；参数错误与引用
// 不存在不占用请求号。
//
// 成功与状态类拒绝都以保存完成为准：本次规则、变更记录与请求结果尚未
// 写入原登记册而保存失败（如数据位置暂时无法写入）时，返回实际保存错误，
// 不报告设置成功，也不只返回原业务拒绝；结果为空（系列编号、份额与业务
// 错误均为空、不标回放），error 说明保存失败。即使失败后原数据暂时无法
// 读取、状态未能按磁盘重建，这次操作也不留下任何规则变化、变更记录或
// 请求号占用，变更序号不被消耗，后续其他操作成功保存也不会把这次未保存
// 的内容带进登记册。保存条件恢复后用完全相同的请求重提，按当时的业务
// 状态重新判断：拒绝条件仍在则重新保存此次拒绝并返回对应业务错误（首次
// 重提不标回放），状态已满足请求则正常设置。
func (r *Registry) SetRoyalty(req SetRoyaltyRequest) (SetRoyaltyResult, error) {
	if err := req.validatePresent(); err != nil {
		return SetRoyaltyResult{}, err
	}
	// 份额的形状校验（重复账户、比例范围、合计）不依赖状态，在取锁前
	// 完成；不合法时原规则不变，也不占用请求号。
	shares, err := normalizeShares(req.Shares)
	if err != nil {
		return SetRoyaltyResult{SeriesID: req.SeriesID, Err: err}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return SetRoyaltyResult{}, err
	}

	key := requestKey(req.Operator, req.RequestID)
	sig := setRoyaltyParamsSig(req, shares)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replaySetRoyalty(prev, sig)
	}

	if bizErr := r.checkSetRoyalty(req, shares); bizErr != nil {
		if !isValidationErr(bizErr) {
			// 状态类业务拒绝（停用、无权、封存、已固定等）占用请求号并
			// 落盘；参数错误与引用不存在不占用。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "royalty_set",
				Params: sig, Rejected: true, Reason: errCode(bizErr),
				SeriesID: req.SeriesID,
			}
			if err := r.commit(); err != nil {
				// 拒绝结果落盘失败：这次拒绝没有被记住，与发行、转让一致——
				// 不能只用业务错误掩盖保存错误，调用者稍后用原请求重提时
				// 必须按当时状态重新判断，而不是回放一个实际没有保存的
				// 拒绝。commit 失败时已按磁盘内容重建状态，请求记录随之
				// 撤销；此处再删一次以覆盖磁盘暂时不可读、状态未能重建
				// 的情形。整体返回空结果：无系列编号、无份额、不标回放、
				// 结果中业务错误为空，error 保留实际写入错误。
				delete(r.state.Requests, key)
				return SetRoyaltyResult{}, err
			}
		}
		return SetRoyaltyResult{SeriesID: req.SeriesID, Err: bizErr}, bizErr
	}

	s := r.state.Series[req.SeriesID]
	before := append([]royaltyShare(nil), s.Royalty...)
	// 回滚基点：保存失败且磁盘暂时不可读、状态未能按磁盘重建时，本次
	// 未保存的规则变化、变更记录与请求号占用都必须显式撤销，否则后续
	// 任何一次成功保存都会把这份未保存的规则带进登记册。
	prevRoyaltySeq := r.state.NextRoyaltySeq
	prevEvents := len(r.state.RoyaltyEvents)
	s.Royalty = shares
	r.state.Series[req.SeriesID] = s
	seq := prevRoyaltySeq + 1
	r.state.NextRoyaltySeq = seq
	r.state.RoyaltyEvents = append(r.state.RoyaltyEvents, royaltyEvent{
		Seq: seq, SeriesID: req.SeriesID, Operator: req.Operator,
		Reason: req.Reason, RequestID: req.RequestID,
		Before: before, After: append([]royaltyShare(nil), shares...),
		OccurredAt: r.now(),
	})
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "royalty_set",
		Params: sig, SeriesID: req.SeriesID, Shares: shares,
	}
	if err := r.commit(); err != nil {
		// commit 失败时已尝试按磁盘内容重建状态：重建成功则本次改动已
		// 随旧状态整体撤销（请求记录不存在），无需再动；磁盘暂时不可读、
		// 状态未能重建时请求记录仍在，据此把本次未保存的改动全部撤销——
		// 原规则（含原本就没有规则的空规则）完整恢复，变更记录与序号、
		// 请求号占用一并撤销。
		if _, ok := r.state.Requests[key]; ok {
			delete(r.state.Requests, key)
			cur := r.state.Series[req.SeriesID]
			cur.Royalty = before
			r.state.Series[req.SeriesID] = cur
			r.state.RoyaltyEvents = r.state.RoyaltyEvents[:prevEvents]
			r.state.NextRoyaltySeq = prevRoyaltySeq
		}
		return SetRoyaltyResult{}, err
	}
	return SetRoyaltyResult{SeriesID: req.SeriesID, Shares: publicShares(shares)}, nil
}

func (r *Registry) checkSetRoyalty(req SetRoyaltyRequest, shares []royaltyShare) error {
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
	if s.CreatorID != req.Operator {
		return fmt.Errorf("%w: 只有创建账户 %s 可以设置系列 %s 的版税规则",
			ErrForbidden, s.CreatorID, req.SeriesID)
	}
	if s.Sealed {
		return fmt.Errorf("%w: 系列 %s 已封存，不能设置版税规则", ErrSeriesSealed, req.SeriesID)
	}
	// 首次成功发行后规则固定：系列中已存在藏品即视为已固定。
	for _, it := range r.state.Items {
		if it.SeriesID == req.SeriesID {
			return fmt.Errorf("%w: 系列 %s 已发行藏品，版税规则不能再设置",
				ErrRoyaltyFrozen, req.SeriesID)
		}
	}
	for _, sh := range shares {
		payee, ok := r.state.Accounts[sh.AccountID]
		if !ok {
			return fmt.Errorf("%w: 版税收款账户 %s", ErrNotFound, sh.AccountID)
		}
		if !payee.Active {
			return fmt.Errorf("%w: 版税收款账户 %s", ErrAccountInactive, sh.AccountID)
		}
	}
	return nil
}

func (r *Registry) replaySetRoyalty(prev request, sig string) (SetRoyaltyResult, error) {
	if prev.Kind != "royalty_set" || prev.Params != sig {
		return SetRoyaltyResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := SetRoyaltyResult{SeriesID: prev.SeriesID, Shares: publicShares(prev.Shares), Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		res.Shares = nil
		return res, res.Err
	}
	return res, nil
}

// ---- 版税计算 ----

// royaltyAmount 计算单个收款账户的应付金额：价款乘比例除以 10000 向下
// 取整。价款与比例的乘积可能超出 int64，用 big.Int 保证整个价款范围
// 内结果准确；结果不超过价款本身，可安全转回 int64。
func royaltyAmount(price, rate int64) int64 {
	amt := big.NewInt(price)
	amt.Mul(amt, big.NewInt(rate))
	amt.Div(amt, big.NewInt(RoyaltyRateBase))
	return amt.Int64()
}

// newRoyaltyRec 按规则快照计算一笔转让的全部应付明细与剩余收入。
// 零金额的应付也保留明细；余款归转让前持有人（ownerID）。
func newRoyaltyRec(txSeq int64, itemID, seriesID string, price int64, shares []royaltyShare, ownerID string) royaltyRec {
	rec := royaltyRec{
		TxSeq: txSeq, ItemID: itemID, SeriesID: seriesID,
		Price: price, OwnerID: ownerID,
	}
	var total int64
	for _, sh := range shares {
		amt := royaltyAmount(price, sh.Rate)
		rec.Payees = append(rec.Payees, payableLine{
			AccountID: sh.AccountID, Rate: sh.Rate, Amount: amt,
		})
		total += amt
	}
	// 比例合计不超过 10000，应付合计不超过价款，余款非负且不溢出。
	rec.Remainder = price - total
	return rec
}

func publicShares(shares []royaltyShare) []RoyaltyShare {
	out := make([]RoyaltyShare, 0, len(shares))
	for _, sh := range shares {
		out = append(out, RoyaltyShare{AccountID: sh.AccountID, Rate: sh.Rate})
	}
	return out
}

func publicPayables(payees []payableLine) []RoyaltyPayable {
	out := make([]RoyaltyPayable, 0, len(payees))
	for _, p := range payees {
		out = append(out, RoyaltyPayable{AccountID: p.AccountID, Rate: p.Rate, Amount: p.Amount})
	}
	return out
}

// royaltyOfTx 返回某笔成功转让落盘的价款、应付明细与余款；旧登记册中
// 的转让没有记录，按价款 0、无明细处理。调用时需持有锁。
func (r *Registry) royaltyOfTx(txSeq int64) (int64, []RoyaltyPayable, int64) {
	rec, ok := r.state.Royalties[txSeq]
	if !ok {
		return 0, nil, 0
	}
	return rec.Price, publicPayables(rec.Payees), rec.Remainder
}

// ---- 版税查询 ----

// GetSeriesRoyalty 查询系列当前的版税规则（按收款账户排序）；未设置或
// 已清空时返回空列表。系列不存在时返回包裹 ErrNotFound 的错误。
func (r *Registry) GetSeriesRoyalty(seriesID string) ([]RoyaltyShare, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return nil, err
	}
	s, ok := r.state.Series[seriesID]
	if !ok {
		return nil, fmt.Errorf("%w: 系列 %s", ErrNotFound, seriesID)
	}
	return publicShares(s.Royalty), nil
}

// RoyaltyHistory 查询系列版税规则的历次变更，按发生先后排列；每条
// 记录包含操作者、原因、请求号与前后内容。系列不存在时返回包裹
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
		out = append(out, RoyaltyEvent{
			Seq: e.Seq, SeriesID: e.SeriesID, Operator: e.Operator,
			Reason: e.Reason, RequestID: e.RequestID,
			Before: publicShares(e.Before), After: publicShares(e.After),
			OccurredAt: e.OccurredAt,
		})
	}
	return out, nil
}

// TransferRoyalty 按转让历史序号查询一笔成功转让的版税计算依据与全部
// 金额：成交价款、各收款账户应付明细（含零金额）与归转让前持有人的
// 剩余收入。序号不是成功的转让记录（不存在或为发行记录）时返回包裹
// ErrNotFound 的错误；旧登记册中的转让按价款 0、无应付明细返回。
func (r *Registry) TransferRoyalty(txSeq int64) (TransferRoyalty, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return TransferRoyalty{}, err
	}
	if txSeq <= 0 {
		return TransferRoyalty{}, fmt.Errorf("%w: 转让序号 %d 不合法", ErrInvalidArgument, txSeq)
	}
	var entry *historyEntry
	for i := range r.state.History {
		if r.state.History[i].Seq == txSeq {
			entry = &r.state.History[i]
			break
		}
	}
	if entry == nil || entry.Kind != "transfer" {
		return TransferRoyalty{}, fmt.Errorf("%w: 转让记录 %d", ErrNotFound, txSeq)
	}
	out := TransferRoyalty{
		TxSeq: txSeq, ItemID: entry.ItemID, OwnerID: entry.FromID,
		Payables: make([]RoyaltyPayable, 0),
	}
	if rec, ok := r.state.Royalties[txSeq]; ok {
		out.Price = rec.Price
		out.Payables = publicPayables(rec.Payees)
		out.Remainder = rec.Remainder
		out.OwnerID = rec.OwnerID
	}
	return out, nil
}

// PayablesOf 查询某账户作为版税收款账户的全部应付明细，按转让序号
// 排列；没有明细时返回空列表。账户不存在时返回包裹 ErrNotFound 的
// 错误；停用账户的既有应付记录仍可查询。
func (r *Registry) PayablesOf(accountID string) ([]PayableEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return nil, err
	}
	if _, ok := r.state.Accounts[accountID]; !ok {
		return nil, fmt.Errorf("%w: 账户 %s", ErrNotFound, accountID)
	}
	out := make([]PayableEntry, 0)
	for _, rec := range r.state.Royalties {
		for _, p := range rec.Payees {
			if p.AccountID != accountID {
				continue
			}
			out = append(out, PayableEntry{
				TxSeq: rec.TxSeq, ItemID: rec.ItemID, Price: rec.Price,
				Rate: p.Rate, Amount: p.Amount,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TxSeq < out[j].TxSeq })
	return out, nil
}
