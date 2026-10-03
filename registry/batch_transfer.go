package registry

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---- 整批转让 ----

func (req TransferBatchRequest) validatePresent() error {
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
	if len(missing) > 0 {
		return fmt.Errorf("%w: 整批转让请求缺少必填字段 %s", ErrInvalidArgument, strings.Join(missing, ", "))
	}
	if len(req.Entries) == 0 {
		return fmt.Errorf("%w: 整批转让清单不能为空", ErrInvalidArgument)
	}
	seen := make(map[string]bool, len(req.Entries))
	for i, e := range req.Entries {
		if strings.TrimSpace(e.ItemID) == "" {
			return fmt.Errorf("%w: 清单第 %d 条缺少藏品编号", ErrInvalidArgument, i+1)
		}
		if seen[e.ItemID] {
			return fmt.Errorf("%w: 清单内藏品编号 %s 重复", ErrInvalidArgument, e.ItemID)
		}
		seen[e.ItemID] = true
		if strings.TrimSpace(e.ExpectedOwner) == "" {
			return fmt.Errorf("%w: 清单第 %d 条（藏品 %s）缺少期望持有人",
				ErrInvalidArgument, i+1, e.ItemID)
		}
		if strings.TrimSpace(e.ToID) == "" {
			return fmt.Errorf("%w: 清单第 %d 条（藏品 %s）缺少接收账户",
				ErrInvalidArgument, i+1, e.ItemID)
		}
		if e.ExpectedVer <= 0 {
			return fmt.Errorf("%w: 清单第 %d 条（藏品 %s）期望版本必须为正，得到 %d",
				ErrInvalidArgument, i+1, e.ItemID, e.ExpectedVer)
		}
		if e.Price < 0 {
			return fmt.Errorf("%w: 清单第 %d 条（藏品 %s）成交价款 %d 不能为负",
				ErrInvalidArgument, i+1, e.ItemID, e.Price)
		}
	}
	return nil
}

func transferBatchParamsSig(req TransferBatchRequest) string {
	// 条目按提交顺序进入签名：改动任一条目内容或条目顺序都会得到不同
	// 签名，按请求号冲突拒绝。Price 用 omitempty，与单件转让一致。
	type entry struct {
		ItemID        string `json:"item_id"`
		ToID          string `json:"to_id"`
		ExpectedOwner string `json:"expected_owner"`
		ExpectedVer   int64  `json:"expected_version"`
		Price         int64  `json:"price,omitempty"`
	}
	entries := make([]entry, 0, len(req.Entries))
	for _, e := range req.Entries {
		entries = append(entries, entry{
			ItemID: e.ItemID, ToID: e.ToID, ExpectedOwner: e.ExpectedOwner,
			ExpectedVer: e.ExpectedVer, Price: e.Price,
		})
	}
	b, _ := json.Marshal(struct {
		Kind    string  `json:"kind"`
		Reason  string  `json:"reason"`
		Entries []entry `json:"entries"`
	}{"transfer_batch", req.Reason, entries})
	return string(b)
}

// TransferBatch 由持有人一次提交多件藏品，分别转给各自指定的接收账户，
// 整批共用操作者、原因与请求号。藏品可来自不同系列与发行批次，接收账户
// 可以重复；操作者必须是每件的当前持有人。
//
// 整批只能全部成功或全部拒绝：全部条目在同一临界区内按清单顺序依次转让，
// 各件版本分别加一，历史序号连续，整批记录之间不能插入其他发行或转让；
// 版税按各件所属系列首次发行时固定的规则快照分别计算，同一收款账户在
// 多件中出现也各自保留明细，不把价款合并后计算；零金额明细与归转让前
// 持有人的余款沿用单件转让规则。整批转让不使用任何代转授权，也不把已有
// 授权记为已使用；成功转出后，绑定旧版本的授权即使在藏品转回后也不能
// 恢复使用。系列封存不阻止转让。
//
// 空清单、重复藏品编号、必填内容缺失、非正期望版本或负价款按参数错误
// 拒绝。每件沿用单件转让的账户登记、停用、收发不同人与持有版本规则；
// 多件存在业务拒绝时，返回清单顺序最前的失败藏品及其错误；操作者未
// 登记或停用时只返回账户错误（ItemID 为空）。任何拒绝都不改变任何一件
// 的持有、版本、历史或应付。
//
// 请求号与已有操作共用同一操作者的范围：完全相同的内容及顺序重提，回放
// 首次成功或状态类拒绝并标明 Replayed（即使藏品后来再次易手或账户停用）；
// 改动原因、条目顺序或任一条目参数返回 ErrRequestConflict。参数错误与
// 引用不存在不占用请求号。并发重复只能完成一次；与其他整批、单件转让或
// 代转争用同一藏品版本时，最多一方成功。
func (r *Registry) TransferBatch(req TransferBatchRequest) (TransferBatchResult, error) {
	if err := req.validatePresent(); err != nil {
		return TransferBatchResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkOpen(); err != nil {
		return TransferBatchResult{}, err
	}

	key := requestKey(req.Operator, req.RequestID)
	sig := transferBatchParamsSig(req)
	if prev, ok := r.state.Requests[key]; ok {
		return r.replayTransferBatch(prev, sig)
	}

	itemID, bizErr := r.checkTransferBatch(req)
	if bizErr != nil {
		if !isValidationErr(bizErr) {
			// 状态类业务拒绝（停用、收发同人、持有版本不符等）占用请求号
			// 并落盘：相同参数重提永远返回这一次拒绝，即使状态后来变化。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "transfer_batch",
				Params: sig, Rejected: true, Reason: errCode(bizErr),
				ItemID: itemID,
			}
			_ = r.commit()
		}
		// 藏品或账户不存在等校验错误不占用请求号，也不改变任何一件的持有、
		// 版本、历史或应付。
		return TransferBatchResult{ItemID: itemID, Err: bizErr}, bizErr
	}

	// 在同一临界区内按清单顺序依次转让：各件版本分别加一、历史序号连续，
	// 与其他整批、单件转让、代转或发行互斥，整批记录之间不会插入别的记录。
	// 每件的持有、历史与版税应付都走与单件转让、代转相同的 applyTransfer，
	// 整批不关联、也不消耗任何代转授权。
	items := make([]TransferBatchItem, 0, len(req.Entries))
	stored := make([]batchTransferItemResult, 0, len(req.Entries))
	for _, e := range req.Entries {
		done := r.applyTransfer(transferMove{
			ItemID: e.ItemID, ToID: e.ToID, Operator: req.Operator,
			Reason: req.Reason, RequestID: req.RequestID, Price: e.Price,
		})
		items = append(items, TransferBatchItem{
			ItemID: done.ItemID, FromID: done.FromID, ToID: done.ToID,
			Version: done.Version, TxSeq: done.TxSeq, Price: done.Price,
			Payables: done.Payables, Remainder: done.Remainder,
		})
		stored = append(stored, batchTransferItemResult{
			ItemID: done.ItemID, FromID: done.FromID, ToID: done.ToID,
			Version: done.Version, TxSeq: done.TxSeq,
		})
	}
	// 持有变化、历史、应付与请求结果在同一临界区内一次落盘：保存失败时
	// 磁盘仍是整批前的完整状态，不会留下部分转让。
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "transfer_batch",
		Params: sig, TransferBatch: stored,
	}
	if err := r.commit(); err != nil {
		return TransferBatchResult{}, err
	}
	return TransferBatchResult{Items: items}, nil
}

// checkTransferBatch 校验整批转让。操作者层面的错误（未登记或停用）最先
// 返回且不关联具体藏品（ItemID 为空）；其余按清单顺序逐件沿用单件转让
// 规则，返回第一件失败的藏品编号与错误。任何一件不通过则整批拒绝。
func (r *Registry) checkTransferBatch(req TransferBatchRequest) (string, error) {
	op, ok := r.state.Accounts[req.Operator]
	if !ok {
		return "", fmt.Errorf("%w: 操作者账户 %s", ErrNotFound, req.Operator)
	}
	if !op.Active {
		return "", fmt.Errorf("%w: 操作者账户 %s", ErrAccountInactive, req.Operator)
	}
	for _, e := range req.Entries {
		h, ok := r.state.Holdings[e.ItemID]
		if !ok {
			if _, itemExists := r.state.Items[e.ItemID]; !itemExists {
				return e.ItemID, fmt.Errorf("%w: 藏品 %s", ErrNotFound, e.ItemID)
			}
			return e.ItemID, fmt.Errorf("%w: 藏品 %s 没有持有记录", ErrNotFound, e.ItemID)
		}
		to, ok := r.state.Accounts[e.ToID]
		if !ok {
			return e.ItemID, fmt.Errorf("%w: 接收账户 %s", ErrNotFound, e.ToID)
		}
		if !to.Active {
			return e.ItemID, fmt.Errorf("%w: 接收账户 %s", ErrAccountInactive, e.ToID)
		}
		if e.ToID == h.OwnerID {
			return e.ItemID, fmt.Errorf("%w: 接收人 %s 已是藏品 %s 的当前持有人",
				ErrSameAccount, e.ToID, e.ItemID)
		}
		// 期望持有人或期望版本不符，包括操作者并非该件当前持有人的情况；
		// 系列封存不阻止转让，这里不检查封存状态。
		if h.OwnerID != e.ExpectedOwner || h.Version != e.ExpectedVer ||
			req.Operator != h.OwnerID {
			return e.ItemID, fmt.Errorf("%w: 藏品 %s 当前为 %s 版本 %d",
				ErrConflict, e.ItemID, h.OwnerID, h.Version)
		}
	}
	return "", nil
}

func (r *Registry) replayTransferBatch(prev request, sig string) (TransferBatchResult, error) {
	if prev.Kind != "transfer_batch" || prev.Params != sig {
		return TransferBatchResult{}, fmt.Errorf("%w: 操作者 %s 的请求号 %s 已用于不同请求",
			ErrRequestConflict, prev.Operator, prev.RequestID)
	}
	res := TransferBatchResult{ItemID: prev.ItemID, Replayed: true}
	if prev.Rejected {
		res.Err = codeErr(prev.Reason)
		return res, res.Err
	}
	// 回放整批首次成功的转让结果：即使藏品后来再次易手、账户停用或进程
	// 重开，仍返回原前后持有人、版本、序号与原价款/应付明细，不改写当前
	// 状态，也不重复计入应付。
	for _, bi := range prev.TransferBatch {
		price, payables, remainder := r.royaltyOfTx(bi.TxSeq)
		res.Items = append(res.Items, TransferBatchItem{
			ItemID: bi.ItemID, FromID: bi.FromID, ToID: bi.ToID, Version: bi.Version,
			TxSeq: bi.TxSeq, Price: price, Payables: payables, Remainder: remainder,
		})
	}
	return res, nil
}
