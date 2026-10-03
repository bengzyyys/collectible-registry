package registry

// 本文件集中实现一笔成功转让的共同落盘内容：持有变化、转让历史、按系列
// 既定规则计算的版税应付与余款，并分配连续的历史序号。直接转让
// （Transfer）、授权代转（ProxyTransfer）与整批转让（TransferBatch）
// 成功时都通过 applyTransfer 完成同一组修改，保证同一笔成功转让在三个
// 入口下的持有、历史与金额记录完全一致；调用者只负责各自的校验、请求号
// 记忆与（代转专有的）授权状态更新。applyTransfer 必须在持有登记册互斥
// 锁时调用，本身不执行落盘——由调用者与请求结果（及授权使用状态）在
// 同一临界区内一次 commit。

// transferMove 是一笔成功转让的落盘输入。接收人与新版本由 ToID 与当前
// 持有版本决定；Operator 是实际操作者（直接转让即转出持有人，代转为
// 受托账户）；AuthID 仅代转非空，用于在历史中追溯所用授权；Price 是本
// 笔成交价款（代转时取授权创建时写定的金额）。
type transferMove struct {
	ItemID    string
	ToID      string
	Operator  string
	Reason    string
	RequestID string
	Price     int64
	AuthID    string
}

// appliedTransfer 是 applyTransfer 的结果：历史序号、前后持有人、新版本
// 以及本笔的价款、各收款账户应付明细（含零金额）与归转让前持有人的余款。
type appliedTransfer struct {
	TxSeq     int64
	ItemID    string
	FromID    string
	ToID      string
	Version   int64
	Price     int64
	Payables  []RoyaltyPayable
	Remainder int64
}

// applyTransfer 执行一笔已通过全部业务校验的成功转让：
//   - 持有人变为 move.ToID，持有版本只在当前版本上加一；
//   - 追加一条 transfer 历史，记录实际操作者、原因、请求号，以及代转所
//     用的授权编号；
//   - 按藏品所属系列的既定版税规则快照，分别为每个收款账户向下取整计算
//     应付（零金额也保留明细），余款归转让前持有人。
//
// 历史序号取自全局递增序号并立即占用，使整批内多笔转让序号连续。修改只
// 写入内存快照，调用者须随后统一 commit；任何校验都应在调用前完成，
// applyTransfer 不返回业务错误。
func (r *Registry) applyTransfer(move transferMove) appliedTransfer {
	h := r.state.Holdings[move.ItemID]
	seq := r.state.NextSeq + 1
	r.state.NextSeq = seq
	from := h.OwnerID
	fromVer := h.Version

	h.OwnerID = move.ToID
	h.Version = fromVer + 1
	r.state.Holdings[move.ItemID] = h

	r.state.History = append(r.state.History, historyEntry{
		Seq: seq, Kind: "transfer", ItemID: move.ItemID, Operator: move.Operator,
		Reason: move.Reason, RequestID: move.RequestID,
		FromID: from, ToID: move.ToID, FromVersion: fromVer, ToVersion: fromVer + 1,
		AuthID: move.AuthID,
	})

	// 版税按本件所属系列首次发行时固定的规则快照计算；同一系列规则或
	// 同一收款账户在多笔转让中出现也各自落盘明细，价款不合并。余款归
	// 转让前持有人（代转时即授权人，不是受托人）。
	it := r.state.Items[move.ItemID]
	royalty := newRoyaltyRec(seq, move.ItemID, it.SeriesID, move.Price,
		r.state.Series[it.SeriesID].Royalty, from)
	r.state.Royalties[seq] = royalty

	return appliedTransfer{
		TxSeq: seq, ItemID: move.ItemID, FromID: from, ToID: move.ToID,
		Version: fromVer + 1, Price: royalty.Price,
		Payables: publicPayables(royalty.Payees), Remainder: royalty.Remainder,
	}
}
