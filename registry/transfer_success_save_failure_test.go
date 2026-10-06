package registry

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

// 本文件为单件转让补充"成功转让本身保存失败"场景的回归保障。既有的
// transfer_save_failure_test.go 覆盖的是状态类业务拒绝的落盘失败；这里覆盖
// 的是账户、藏品与持有版本检查全部通过、本次转让的持有换人/历史/版税应付/
// 请求结果尚未原子替换原数据就发生写入失败的情形——这样的转让实际没有保存，
// 绝不能成为登记册认可的交易，即使原数据在失败后暂时无法读取或解析。
//
// 失败返回必须保留实际写入错误并给出空结果（不携带藏品编号、前后持有人、
// 版本、历史序号、价款、应付或余款，业务错误为空，不标回放）。同一个仍
// 打开的登记册上：藏品继续属于提交前持有人、版本不增加、持有列表与藏品
// 历史保持原状、版税查询没有本次计算记录、收款账户不多出应付，请求号与
// 历史序号都不被消耗；失败前已有的成功转让、应付与其他藏品记录原样保留。
// 读写恢复后，即使用户先做一次无关操作成功保存，也不能把这笔未保存的转让
// 一并写入；用原请求重提按当时状态重新判断，条件仍满足时完整完成一次（版本
// 只加一次、序号紧接已有历史、按既定系列规则算金额、不标回放），只有此次
// 成功保存后再提才回放；恢复期间藏品被另一笔合法转让转出的，原请求按现有
// 版本冲突规则拒绝，不能回放先前未保存的成功。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件改写
// 为无法解析的内容，覆盖"原数据暂时无法读取、commit 无法按磁盘重建状态"。

// seedTransferSuccessWorld 建立一个已经存在成功转让与应付记录的登记册：
// setupBatchTransferWorld 发行 i1..i5（历史序号 1..5）后，alice 再发行
// i6（s1，序号 6）给 bob，bob 以 10000 分把 i6 转让给 alice（序号 7，
// carol 10%=1000、dave 5%=500，余款 8500 归 bob）。因此本文件各用例失败
// 发生前最后一条历史序号是 7；未保存转让若执行将占用序号 8。
func seedTransferSuccessWorld(t *testing.T, r *Registry) {
	t.Helper()
	setupBatchTransferWorld(t, r)
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "首发", RequestID: "ri-i6",
		ItemID: "i6", SeriesID: "s1", BatchNo: "b1",
		Metadata: "元-i6", HolderID: "bob",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "先存一笔成功转让", RequestID: "rt-prior", ItemID: "i6",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "alice", Price: 10000,
	}); err != nil {
		t.Fatal(err)
	}
}

// assertTransferSuccessStillPending 在（成功转让保存失败后的）同一个已打开
// 登记册上核对：这笔转让如同从未发生，而失败前已有的内容、顺序与金额完整
// 保留。item 为本次未保存转让的藏品，rid 为其请求号，price 为其价款。
func assertTransferSuccessStillPending(t *testing.T, r *Registry, item, rid string) {
	t.Helper()
	// 藏品仍属提交前持有人 bob，版本没有增加，历史只有发行一条，且不出现
	// 本次请求号。
	h, err := r.GetHolding(item)
	if err != nil {
		t.Fatalf("GetHolding %s: %v", item, err)
	}
	if h.OwnerID != "bob" || h.Version != 1 {
		t.Fatalf("保存失败后 %s 持有被改变: %+v", item, h)
	}
	hist, err := r.History(item)
	if err != nil {
		t.Fatalf("History %s: %v", item, err)
	}
	if len(hist) != 1 || hist[0].Kind != "issue" {
		t.Fatalf("保存失败后 %s 历史多出本次转让: %+v", item, hist)
	}
	for _, e := range hist {
		if e.RequestID == rid {
			t.Fatalf("保存失败后 %s 历史出现请求号 %s: %+v", item, rid, e)
		}
	}

	// 持有列表：bob 仍持有 i1..i4，i5 属 carol，已合法易手的 i6 属 alice。
	bobHoldings, err := r.HoldingsOf("bob")
	if err != nil {
		t.Fatal(err)
	}
	gotItems := map[string]bool{}
	for _, hh := range bobHoldings {
		gotItems[hh.ItemID] = true
		if hh.Version != 1 {
			t.Fatalf("保存失败后 bob 的 %s 版本被改变: %+v", hh.ItemID, hh)
		}
	}
	// bob 应恰好持有 i1..i4：未保存转让的藏品没有离开，也没有多出别的。
	wantBob := map[string]bool{"i1": true, "i2": true, "i3": true, "i4": true}
	if len(bobHoldings) != len(wantBob) {
		t.Fatalf("保存失败后 bob 持有清单数量异常: %+v", bobHoldings)
	}
	for id := range wantBob {
		if !gotItems[id] {
			t.Fatalf("保存失败后 bob 应仍持有 %s，实际 %+v", id, bobHoldings)
		}
	}

	// 历史序号不被消耗：下一条仍是 8（失败前最后一条为 7）；请求号空闲；
	// 版税查询没有序号 8 的计算记录。
	if r.state.NextSeq != 7 {
		t.Fatalf("NextSeq = %d，未保存转让不应消耗历史序号，仍应为 7", r.state.NextSeq)
	}
	if _, ok := r.state.Requests[requestKey("bob", rid)]; ok {
		t.Fatalf("未保存转让不应占用请求号 %s", rid)
	}
	if _, err := r.TransferRoyalty(8); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未保存转让不应留下序号 8 的版税计算记录: %v", err)
	}

	// 收款账户不能多出本次转让的应付：只保留失败前序号 7（i6，10000 分）
	// 的记录。
	wantCarol := []PayableEntry{{TxSeq: 7, ItemID: "i6", Price: 10000, Rate: 1000, Amount: 1000}}
	wantDave := []PayableEntry{{TxSeq: 7, ItemID: "i6", Price: 10000, Rate: 500, Amount: 500}}
	if carolPay, err := r.PayablesOf("carol"); err != nil || !reflect.DeepEqual(carolPay, wantCarol) {
		t.Fatalf("carol 应付 = %+v, err %v, want %+v", carolPay, err, wantCarol)
	}
	if davePay, err := r.PayablesOf("dave"); err != nil || !reflect.DeepEqual(davePay, wantDave) {
		t.Fatalf("dave 应付 = %+v, err %v, want %+v", davePay, err, wantDave)
	}

	// 失败前那笔成功转让与其他藏品原样保留。
	tr, err := r.TransferRoyalty(7)
	if err != nil {
		t.Fatalf("TransferRoyalty 7: %v", err)
	}
	if tr.ItemID != "i6" || tr.Price != 10000 || tr.Remainder != 8500 || tr.OwnerID != "bob" ||
		len(tr.Payables) != 2 {
		t.Fatalf("旧转让版税记录被破坏: %+v", tr)
	}
	i6, _ := r.GetHolding("i6")
	if i6.OwnerID != "alice" || i6.Version != 2 {
		t.Fatalf("清单外 i6 受影响: %+v", i6)
	}
	if hist6, _ := r.History("i6"); len(hist6) != 2 || hist6[1].Seq != 7 || hist6[1].ToID != "alice" {
		t.Fatalf("清单外 i6 历史受影响: %+v", hist6)
	}
	i5, _ := r.GetHolding("i5")
	if i5.OwnerID != "carol" || i5.Version != 1 {
		t.Fatalf("清单外 i5 受影响: %+v", i5)
	}
}

// TestTransferSuccessSaveFailureUnreadableSameRegistry 覆盖核心场景：检查全
// 部通过后的成功转让在写入阶段失败，且原数据同时被改写为无法解析（commit
// 无法按磁盘重建状态）。调用返回保存错误与空结果；同一登记册上转让如同
// 从未发生，旧记录完整。保存仍失败时重提依旧失败、不留痕。读写恢复后先
// 做一次无关操作（登记新账户）成功保存，也不把未保存转让带入；随后原请求
// 重提按当前状态完整执行一次（非回放、序号 8、金额按 s1 规则），再提才
// 回放。
func TestTransferSuccessSaveFailureUnreadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedTransferSuccessWorld(t, r)
	req := xferSaveReq("rt-ok", "i1", "bob", 1, "carol", 10000)

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	// 成功转让落盘失败：返回保存错误与空结果。
	res, err := r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)
	assertTransferSuccessStillPending(t, r, "i1", req.RequestID)

	// 保存条件未恢复、磁盘仍不可读时再次提交同一请求：仍失败在保存上，
	// 不能把上次未保存的成功当成已保存结果回放。
	res, err = r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)
	assertTransferSuccessStillPending(t, r, "i1", req.RequestID)

	// 恢复正常读写；先让一次无关操作成功保存，不能夹带这笔未保存的转让。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("eve", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertTransferSuccessStillPending(t, r, "i1", req.RequestID)

	// 用原操作者、请求号、原因和全部参数重提：条件仍满足，完整完成一次，
	// 不标回放，版本只加一次，序号紧接已有历史（8）。
	res, err = r.Transfer(req)
	if err != nil {
		t.Fatalf("恢复后重提应完整执行一次: %v", err)
	}
	wantPayables := []RoyaltyPayable{
		{AccountID: "carol", Rate: 1000, Amount: 1000},
		{AccountID: "dave", Rate: 500, Amount: 500},
	}
	if res.Replayed || res.Err != nil || res.ItemID != "i1" ||
		res.FromID != "bob" || res.ToID != "carol" || res.Version != 2 || res.TxSeq != 8 ||
		res.Price != 10000 || res.Remainder != 8500 || !reflect.DeepEqual(res.Payables, wantPayables) {
		t.Fatalf("重提结果异常: %+v", res)
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("重提成功后 i1 持有 = %+v", h)
	}
	if hist, _ := r.History("i1"); len(hist) != 2 || hist[1].Seq != 8 ||
		hist[1].RequestID != req.RequestID || hist[1].FromID != "bob" || hist[1].ToID != "carol" {
		t.Fatalf("重提成功后 i1 历史异常: %+v", hist)
	}
	tr, _ := r.TransferRoyalty(8)
	if tr.ItemID != "i1" || tr.Price != 10000 || tr.Remainder != 8500 || tr.OwnerID != "bob" ||
		!reflect.DeepEqual(tr.Payables, wantPayables) {
		t.Fatalf("本次转让版税记录异常: %+v", tr)
	}
	if r.state.NextSeq != 8 {
		t.Fatalf("NextSeq = %d, want 8", r.state.NextSeq)
	}

	// 只有此次成功保存后再提相同内容，才返回这一笔已保存的结果（回放），
	// 不再第二次换人、不重复计金额。
	replay, err := r.Transfer(req)
	if err != nil || !replay.Replayed || replay.Version != 2 || replay.TxSeq != 8 ||
		replay.Price != 10000 || replay.Remainder != 8500 {
		t.Fatalf("成功保存后再提应回放已保存结果: %+v, err %v", replay, err)
	}
	h, _ = r.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("回放不应再次改变持有: %+v", h)
	}
	if hist, _ := r.History("i1"); len(hist) != 2 {
		t.Fatalf("回放不应新增历史: %+v", hist)
	}
	if carolPay, _ := r.PayablesOf("carol"); len(carolPay) != 2 {
		t.Fatalf("回放不应重复计入应付: %+v", carolPay)
	}
}

// TestTransferSuccessSaveFailureReadableSameRegistry 覆盖：写入失败但原数据
// 仍可正常读取（commit 据磁盘内容重建状态）时，得到相同的失败结果与状态
// 保障——同一登记册上转让从未发生；恢复后原请求重提完整执行一次。
func TestTransferSuccessSaveFailureReadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedTransferSuccessWorld(t, r)
	req := xferSaveReq("rt-ok-r", "i1", "bob", 1, "carol", 10000)

	// 只阻断写入，不破坏数据文件：rename 之前失败，原快照完好可读。
	blockBatchSave(t, r)
	res, err := r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)
	assertTransferSuccessStillPending(t, r, "i1", req.RequestID)

	restoreBatchSave(t, r)
	res, err = r.Transfer(req)
	if err != nil || res.Replayed || res.ItemID != "i1" ||
		res.FromID != "bob" || res.ToID != "carol" || res.Version != 2 || res.TxSeq != 8 {
		t.Fatalf("可读磁盘失败恢复后重提应完整执行一次: %+v, err %v", res, err)
	}
	// 成功保存后再提才回放。
	replay, err := r.Transfer(req)
	if err != nil || !replay.Replayed || replay.TxSeq != 8 {
		t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
	}
}

// TestTransferSuccessSaveFailureRetryAfterReopen 覆盖磁盘视角：成功转让保存
// 失败（原数据可读）后关闭重开，看到的仍是转让前状态；恢复保存后用原请求
// 重提完整执行一次，而不是回放一个从未保存的成功。
func TestTransferSuccessSaveFailureRetryAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	t.Cleanup(func() { _ = r.Close() })
	seedTransferSuccessWorld(t, r)
	req := xferSaveReq("rt-ok-reopen", "i1", "bob", 1, "carol", 10000)

	blockBatchSave(t, r)
	res, err := r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)

	// 数据文件从未被替换：正常关闭并重新打开仍读到转让前状态。
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertTransferSuccessStillPending(t, r2, "i1", req.RequestID)

	restoreBatchSave(t, r2)
	res2, err := r2.Transfer(req)
	if err != nil || res2.Replayed || res2.TxSeq != 8 || res2.Version != 2 ||
		res2.FromID != "bob" || res2.ToID != "carol" {
		t.Fatalf("重开后重提应完整执行一次: %+v, err %v", res2, err)
	}
	replay, err := r2.Transfer(req)
	if err != nil || !replay.Replayed || replay.TxSeq != 8 {
		t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
	}
}

// TestTransferSuccessSaveFailureRetryConflict 覆盖：恢复期间藏品被另一笔合法
// 转让转出后，原请求必须按现有版本冲突规则拒绝，不能回放先前未保存的成功。
func TestTransferSuccessSaveFailureRetryConflict(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedTransferSuccessWorld(t, r)
	req := xferSaveReq("rt-ok-c", "i1", "bob", 1, "carol", 10000)

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}
	res, err := r.Transfer(req)
	assertTransferSaveFailureEmpty(t, res, err)
	assertTransferSuccessStillPending(t, r, "i1", req.RequestID)

	// 恢复读写后，bob 先把 i1 合法转让给 alice：i1 变为 alice 版本 2（序号 8）。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "恢复期间的合法转让", RequestID: "rt-intervene", ItemID: "i1",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "alice", Price: 5000,
	}); err != nil {
		t.Fatal(err)
	}

	// 原请求（仍期望 bob 版本 1）按当前持有状态应被版本冲突拒绝，不是回放
	// 那次未保存的成功；拒绝不改变持有。
	res, err = r.Transfer(req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("藏品已转出后原请求应按版本冲突拒绝: %v", err)
	}
	if res.Replayed || res.ItemID != "i1" || !errors.Is(res.Err, ErrConflict) ||
		res.Version != 0 || res.TxSeq != 0 {
		t.Fatalf("冲突拒绝的结果异常: %+v", res)
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "alice" || h.Version != 2 {
		t.Fatalf("冲突拒绝不应改变持有: %+v", h)
	}
	// 这是一次新保存的状态类拒绝（占用请求号），但没有新增转让或序号。
	prev, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]
	if !ok || !prev.Rejected || prev.TxSeq != 0 {
		t.Fatalf("冲突拒绝应已登记为拒绝结果: %+v", prev)
	}
	if r.state.NextSeq != 8 {
		t.Fatalf("冲突拒绝不应消耗历史序号: NextSeq=%d", r.state.NextSeq)
	}
	if _, err := r.TransferRoyalty(9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("冲突拒绝不应产生版税记录: %v", err)
	}

	// 此后原样重提回放这条已保存的冲突拒绝（仍不执行转让）。
	res, err = r.Transfer(req)
	if !errors.Is(err, ErrConflict) || !res.Replayed || !errors.Is(res.Err, ErrConflict) {
		t.Fatalf("冲突拒绝保存后再提应回放该拒绝: %+v, err %v", res, err)
	}
	h, _ = r.GetHolding("i1")
	if h.OwnerID != "alice" || h.Version != 2 {
		t.Fatalf("回放冲突拒绝不应改变持有: %+v", h)
	}
}

// TestTransferSuccessSaveFailureZeroPriceNoRoyalty 覆盖零价款与无版税系列的
// 转让在保存失败时同样不留任何应付或计算记录，恢复后重提沿用零价款/无版税
// 的既有行为成功一次。
func TestTransferSuccessSaveFailureZeroPriceNoRoyalty(t *testing.T) {
	cases := []struct {
		name        string
		item        string
		wantPayable []RoyaltyPayable // 重提成功后期望的应付明细（零金额也保留）
	}{
		{
			name:        "no_royalty_series_zero_price",
			item:        "i4", // s3 无版税
			wantPayable: []RoyaltyPayable{},
		},
		{
			name: "royalty_series_zero_price",
			item: "i1", // s1：carol 10% + dave 5%，零价款 => 零金额明细仍保留
			wantPayable: []RoyaltyPayable{
				{AccountID: "carol", Rate: 1000, Amount: 0},
				{AccountID: "dave", Rate: 500, Amount: 0},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustCreate(t, tempDir(t))
			seedTransferSuccessWorld(t, r)
			req := xferSaveReq("rt-zero-"+tc.item, tc.item, "bob", 1, "alice", 0)

			orig, err := os.ReadFile(dataFile(r.dir))
			if err != nil {
				t.Fatal(err)
			}
			blockBatchSave(t, r)
			if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
				t.Fatal(err)
			}
			res, err := r.Transfer(req)
			assertTransferSaveFailureEmpty(t, res, err)

			// 未保存的零价款转让同样不留下序号 8 记录、不新增任何应付。
			h, _ := r.GetHolding(tc.item)
			if h.OwnerID != "bob" || h.Version != 1 {
				t.Fatalf("保存失败后 %s 持有被改变: %+v", tc.item, h)
			}
			if _, err := r.TransferRoyalty(8); !errors.Is(err, ErrNotFound) {
				t.Fatalf("未保存转让不应留下版税记录: %v", err)
			}
			if carolPay, _ := r.PayablesOf("carol"); len(carolPay) != 1 || carolPay[0].TxSeq != 7 {
				t.Fatalf("未保存转让不应新增应付：%+v", carolPay)
			}
			if r.state.NextSeq != 7 {
				t.Fatalf("NextSeq = %d, want 7", r.state.NextSeq)
			}

			// 恢复后重提：零价款/无版税转让完整成功一次，余款为 0。
			if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
				t.Fatal(err)
			}
			restoreBatchSave(t, r)
			res, err = r.Transfer(req)
			if err != nil || res.Replayed || res.ItemID != tc.item ||
				res.Version != 2 || res.TxSeq != 8 || res.Price != 0 || res.Remainder != 0 {
				t.Fatalf("重提结果异常: %+v, err %v", res, err)
			}
			if !reflect.DeepEqual(res.Payables, tc.wantPayable) {
				t.Fatalf("%s 重提应付 = %+v, want %+v", tc.item, res.Payables, tc.wantPayable)
			}
			tr, _ := r.TransferRoyalty(8)
			if tr.ItemID != tc.item || tr.Price != 0 || tr.Remainder != 0 || tr.OwnerID != "bob" ||
				!reflect.DeepEqual(tr.Payables, tc.wantPayable) {
				t.Fatalf("重提版税记录异常: %+v", tr)
			}

			// 再提回放，不第二次执行。
			replay, err := r.Transfer(req)
			if err != nil || !replay.Replayed || replay.TxSeq != 8 {
				t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
			}
			if hist, _ := r.History(tc.item); len(hist) != 2 {
				t.Fatalf("回放不应新增历史: %+v", hist)
			}
		})
	}
}
