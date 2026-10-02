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
		if strings.TrimSpace(e.ToID) == "" {
			return fmt.Errorf("%w: 清单第 %d 条（藏品 %s）缺少接收账户", ErrInvalidArgument, i+1, e.ItemID)
		}
		if strings.TrimSpace(e.ExpectedOwner) == "" {
			return fmt.Errorf("%w: 清单第 %d 条（藏品 %s）缺少期望持有人", ErrInvalidArgument, i+1, e.ItemID)
		}
		if e.ExpectedVer <= 0 {
			return fmt.Errorf("%w: 清单第 %d 条（藏品 %s）期望版本 %d 必须为正整数",
				ErrInvalidArgument, i+1, e.ItemID, e.ExpectedVer)
		}
		if e.Price < 0 {
			return fmt.Errorf("%w: 清单第 %d 条（藏品 %s）成交价款 %d 不能为负",
				ErrInvalidArgument, i+1, e.ItemID, e.Price)
		}
		if seen[e.ItemID] {
			return fmt.Errorf("%w: 清单内藏品编号 %s 重复", ErrInvalidArgument, e.ItemID)
		}
		seen[e.ItemID] = true
	}
	return nil
}

func transferBatchParamsSig(req TransferBatchRequest) string {
	// 条目按提交顺序进入签名：改动任一条目内容或条目顺序都会得到不同
	// 签名，按请求号冲突拒绝。Price 用 omitempty：价款为 0 时签名与
	// 旧格式一致，改价则签名不同、按请求号冲突拒绝。
	type entry struct {
		ItemID        string `json:"item_id"`
		ExpectedOwner string `json:"expected_owner"`
		ExpectedVer   int64  `json:"expected_version"`
		ToID          string `json:"to_id"`
		Price         int64  `json:"price,omitempty"`
	}
	entries := make([]entry, 0, len(req.Entries))
	for _, e := range req.Entries {
		entries = append(entries, entry{e.ItemID, e.ExpectedOwner, e.ExpectedVer, e.ToID, e.Price})
	}
	b, _ := json.Marshal(struct {
		Kind    string  `json:"kind"`
		Reason  string  `json:"reason"`
		Entries []entry `json:"entries"`
	}{"transfer_batch", req.Reason, entries})
	return string(b)
}

// TransferBatch 由持有人一次提交多件藏品的转让，分别转给指定的接收账户，
// 整批共用操作者、原因与请求号。操作者必须是每件藏品的当前持有人；藏品
// 可来自不同系列与发行批次，接收账户可以重复。成功后按清单顺序返回每件
// 的前后持有人、新版本、转让历史序号、价款、版税明细与余款：各件版本
// 分别加一，历史序号连续递增，整批记录之间不会插入其他发行或转让。
//
// 整批只能全部成功或全部拒绝。空清单、清单内藏品编号重复、必填内容缺失、
// 非正期望版本或负价款按参数错误拒绝；操作者未登记或停用时只返回账户
// 错误；其余业务拒绝（藏品不存在、接收账户未登记或停用、收发同人、期望
// 持有人或版本不符等）返回清单顺序最前的失败藏品及对应错误。任何拒绝都
// 不改变任何一件的持有、版本、历史或应付。系列封存不阻止转让。
//
// 整批转让不把已有代转授权记为已使用；成功转出的藏品所绑定的旧版本授权
// 也不能在藏品转回后恢复使用（授权只在创建时绑定的持有版本上有效）。
//
// 版税按各件所属系列的既定规则分别计算：同一收款账户在多件中出现也分别
// 保留明细，不把价款合并后计算；零金额明细与余款归属沿用单件转让规则。
//
// 请求号与单件转让、发行等操作共用同一操作者的请求号范围：内容与条目
// 顺序相同的重复提交回放整批首次结果（成功或状态类业务拒绝）并标明
// Replayed；改动原因、条目顺序或任一条目参数返回 ErrRequestConflict。
// 参数错误与引用不存在不占用请求号。并发重复只完成一次；整批与其他
// 整批、单件转让或代转争用同一藏品版本时最多一方成功，失败整批中的其他
// 藏品也保持原状。成功结果返回前，持有变化、历史、应付与请求结果在
// 同一临界区内一次落盘。
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
			// 状态类业务拒绝（停用、接收账户不可用、收发同人、版本冲突等）
			// 占用请求号并落盘：相同参数重提永远返回这一次拒绝，即使状态
			// 后来变化。
			r.state.Requests[key] = request{
				Operator: req.Operator, RequestID: req.RequestID, Kind: "transfer_batch",
				Params: sig, Rejected: true, Reason: errCode(bizErr),
				ItemID: itemID,
			}
			_ = r.commit()
		}
		// 校验类错误（引用不存在等）不占用请求号，也不改变任何状态。
		return TransferBatchResult{ItemID: itemID, Err: bizErr}, bizErr
	}

	// 在同一临界区内依次为每件完成转让：持有变化、历史、版税应付与请求
	// 结果一次落盘，序号连续递增；与其他操作互斥，整批记录之间不会插入
	// 其他发行或转让。
	items := make([]TransferBatchItem, 0, len(req.Entries))
	stored := make([]transferBatchItemResult, 0, len(req.Entries))
	for _, e := range req.Entries {
		h := r.state.Holdings[e.ItemID]
		seq := r.state.NextSeq + 1
		r.state.NextSeq = seq
		from := h.OwnerID
		fromVer := h.Version
		h.OwnerID = e.ToID
		h.Version = fromVer + 1
		r.state.Holdings[e.ItemID] = h
		r.state.History = append(r.state.History, historyEntry{
			Seq: seq, Kind: "transfer", ItemID: e.ItemID, Operator: req.Operator,
			Reason: req.Reason, RequestID: req.RequestID,
			FromID: from, ToID: e.ToID, FromVersion: fromVer, ToVersion: fromVer + 1,
		})
		// 版税按各件所属系列的规则分别计算：同一收款账户在多件中出现也
		// 分别保留明细，不合并价款；余款归转让前持有人。
		it := r.state.Items[e.ItemID]
		royalty := newRoyaltyRec(seq, e.ItemID, it.SeriesID, e.Price,
			r.state.Series[it.SeriesID].Royalty, from)
		r.state.Royalties[seq] = royalty
		items = append(items, TransferBatchItem{
			ItemID: e.ItemID, FromID: from, ToID: e.ToID, Version: fromVer + 1,
			TxSeq: seq, Price: royalty.Price, Payables: publicPayables(royalty.Payees),
			Remainder: royalty.Remainder,
		})
		stored = append(stored, transferBatchItemResult{
			ItemID: e.ItemID, FromID: from, ToID: e.ToID, Version: fromVer + 1, TxSeq: seq,
		})
	}
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "transfer_batch",
		Params: sig, TransferBatch: stored,
	}
	if err := r.commit(); err != nil {
		return TransferBatchResult{}, err
	}
	return TransferBatchResult{Items: items}, nil
}

// checkTransferBatch 校验整批转让。操作者账户本身的问题（未登记或停用）
// 与具体藏品无关，返回空藏品编号；其余错误返回清单顺序最前的失败藏品
// 编号与错误本身。任何一件不通过则整批拒绝。
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
			return e.ItemID, fmt.Errorf("%w: 接收人 %s 已是当前持有人", ErrSameAccount, e.ToID)
		}
		// 操作者必须是每件的当前持有人；期望持有人或期望版本不符均拒绝。
		if h.OwnerID != e.ExpectedOwner || h.Version != e.ExpectedVer ||
			req.Operator != h.OwnerID {
			return e.ItemID, fmt.Errorf("%w: 藏品 %s 当前为 %s 版本 %d", ErrConflict,
				e.ItemID, h.OwnerID, h.Version)
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
	// 回放整批首次成功的转让结果：价款、应付明细与余款按各件转让序号从
	// 版税记录中重新取得，即使藏品后来再次易手、账户停用或进程重开，仍
	// 返回最初的整批结果。
	for _, bi := range prev.TransferBatch {
		item := TransferBatchItem{
			ItemID: bi.ItemID, FromID: bi.FromID, ToID: bi.ToID,
			Version: bi.Version, TxSeq: bi.TxSeq,
		}
		item.Price, item.Payables, item.Remainder = r.royaltyOfTx(bi.TxSeq)
		res.Items = append(res.Items, item)
	}
	return res, nil
}
