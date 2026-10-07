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
//
// 状态类拒绝的落盘失败时，返回保存错误（保留实际写入错误）而非该业务
// 错误，结果为空（无转让条目、无失败藏品编号、不标回放、业务错误为空），
// 请求号不被这次未保存的拒绝占用；保存条件恢复后用完全相同的请求重提，
// 按当时的业务状态重新判断：拒绝条件仍在则重新保存此次拒绝并返回对应
// 业务错误，状态已变为满足请求则整批正常执行。
//
// 整批成功同样以一次保存完成为准：全部条目的持有换人、版本加一、转让
// 历史、版税应付、历史序号与请求结果都尚未原子替换原数据就发生保存错误
// （如数据位置暂时无法写入）时，返回实际保存错误而非整批成功，结果为空
// （不携带任何转让条目、失败藏品编号或业务错误，不标回放）。即使失败后
// 旧数据仍在却暂时无法读取或解析、状态未能按磁盘重建，这批转让也必须从
// 当前仍打开的登记册中整体撤销：清单内每件仍归提交前的持有人、版本不增加、
// 各账户持有列表与藏品历史保持原样，本次计算出的版税应付与余款不留存
// （按本次转让序号查版税返回对象不存在，收款账户的应付列表不多出明细），
// 请求号与历史序号都不被消耗；失败前已保存的历史、应付、系列规则以及清单
// 外藏品的持有人和版本完整保留，不会因撤销本批而删掉旧记录。保存条件尚未
// 恢复时用完全相同的请求再次提交，仍实际尝试保存并返回当次保存错误，不能
// 回放未保存的成功；随后另一项无关操作成功保存也不会把这批未保存的持有
// 变化、历史、应付或请求结果一并写入。读写恢复后用原请求号、原因和完整
// 清单重提，账户与持有条件仍满足时整批正常执行一次，各件版本只增加一次，
// 历史序号紧接已有记录，结果不标回放，此后相同提交才回放这次已保存的结果；
// 若其中一件已被另一笔合法转让转出，原整批按现有持有版本规则拒绝，其余
// 条目不发生转让。原数据仍可读取时的保存失败行为相同。
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
			if err := r.commit(); err != nil {
				// 拒绝结果落盘失败：这次拒绝没有被记住，不能只用业务错误
				// 掩盖保存错误——调用者稍后用原请求重提时必须按当时状态
				// 重新判断，而不是回放一个实际没有保存的拒绝。commit 失败
				// 时已按磁盘内容重建状态，请求记录随之撤销；此处再删一次
				// 以覆盖磁盘暂时不可读、状态未能重建的情形。与整批保存
				// 失败一样整体返回：没有转让条目、没有失败藏品编号、不标
				// 回放、结果中业务错误为空，error 保留实际写入错误。
				delete(r.state.Requests, key)
				return TransferBatchResult{}, err
			}
		}
		// 藏品或账户不存在等校验错误不占用请求号，也不改变任何一件的持有、
		// 版本、历史或应付。
		return TransferBatchResult{ItemID: itemID, Err: bizErr}, bizErr
	}

	// 回滚基点：本次整批成功的全部改动（每件持有换人、版本加一、转让历史、
	// 版税应付、历史序号、请求结果）都必须以一次保存完成为准。记录改动前
	// 每件的持有、历史长度与历史序号；保存失败且磁盘暂时不可读、状态未能
	// 按磁盘重建时据此整体撤销，否则后续任何一次成功保存都会把这批未保存
	// 的转让带进登记册，造成"已换人、可回放"的幻影整批。
	prevHoldings := make(map[string]holding, len(req.Entries))
	txSeqs := make([]int64, 0, len(req.Entries))
	for _, e := range req.Entries {
		prevHoldings[e.ItemID] = r.state.Holdings[e.ItemID]
	}
	prevNextSeq := r.state.NextSeq
	prevHistLen := len(r.state.History)

	// 在同一临界区内按清单顺序依次转让：各件版本分别加一、历史序号连续，
	// 与其他整批、单件转让、代转或发行互斥，整批记录之间不会插入别的记录。
	// 各件沿用与单件转让相同的持有、历史与版税落盘逻辑；同一收款账户在
	// 多件中出现也各自落盘明细，价款不合并。
	items := make([]TransferBatchItem, 0, len(req.Entries))
	stored := make([]batchTransferItemResult, 0, len(req.Entries))
	for _, e := range req.Entries {
		out := r.applyTransfer(e.ItemID, e.ToID, req.Operator, req.Reason, req.RequestID, "", e.Price)
		txSeqs = append(txSeqs, out.seq)
		items = append(items, TransferBatchItem{
			ItemID: e.ItemID, FromID: out.fromID, ToID: e.ToID, Version: out.toVer(),
			TxSeq: out.seq, Price: out.royalty.Price, Payables: publicPayables(out.royalty.Payees),
			Remainder: out.royalty.Remainder,
		})
		stored = append(stored, batchTransferItemResult{
			ItemID: e.ItemID, FromID: out.fromID, ToID: e.ToID,
			Version: out.toVer(), TxSeq: out.seq,
		})
	}
	// 持有变化、历史、应付与请求结果在同一临界区内一次落盘：保存失败时
	// 磁盘仍是整批前的完整状态，不会留下部分转让。
	r.state.Requests[key] = request{
		Operator: req.Operator, RequestID: req.RequestID, Kind: "transfer_batch",
		Params: sig, TransferBatch: stored,
	}
	if err := r.commit(); err != nil {
		// commit 失败时已尝试按磁盘内容重建状态：重建成功则本次整批改动已随
		// 旧状态整体撤销（持有未换人、无本次历史与应付、请求记录不存在、
		// 序号未消耗），无需再动；磁盘暂时不可读或无法解析、状态未能重建时
		// 本次改动仍在，据此把这批未保存的转让全部撤销，让当前已打开的登记
		// 册回到提交前状态，也不被随后另一次成功保存带入。返回空结果：无转让
		// 条目、无失败藏品编号、无业务错误、不标回放，error 保留实际写入错误。
		if _, pending := r.state.Requests[key]; pending {
			delete(r.state.Requests, key)
			for _, seq := range txSeqs {
				delete(r.state.Royalties, seq)
			}
			r.state.History = r.state.History[:prevHistLen]
			for itemID, h := range prevHoldings {
				r.state.Holdings[itemID] = h
			}
			r.state.NextSeq = prevNextSeq
		}
		return TransferBatchResult{}, err
	}
	return TransferBatchResult{Items: items}, nil
}

// checkTransferBatch 按整批转让的优先级执行与单件共用的直接转让资格
// 规则：操作者层面的错误（未登记或停用）最先返回且不关联具体藏品
// （ItemID 为空）；操作者可用后，按清单顺序逐件检查，只返回第一件失败
// 的藏品编号与错误，后面条目的错误不提前。任何一件不通过则整批拒绝。
func (r *Registry) checkTransferBatch(req TransferBatchRequest) (string, error) {
	return r.checkBatchTransferEligibility(req.Operator, req.Entries)
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
