package registry

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

// 整批转让在内容替换已保存数据之前写入失败（原登记册仍可正常读取）时，
// 请求必须整体失败并返回保存错误，磁盘上的原登记册不留下任何部分转让；
// 恢复后用完全相同的请求号、原因与条目再次提交，应完整执行一次而不是
// 回放或请求号冲突。本用例在已有成功转让与应付记录的登记册上验证，并
// 同时确认清单外藏品不被失败影响。
//
// 失败注入方式：store.save 固定在登记册目录内创建临时文件
// .registry.json.tmp；在该路径放置一个目录后，临时文件创建必然失败，
// 失败发生在写入与 rename 替换之前，registry.json 始终完好可读。
func TestTransferBatchSaveFailureRollbackAndRetry(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupBatchTransferWorld(t, r)

	// 先制造两笔已落盘的成功转让及其应付记录，作为整批前的既有历史：
	// 序号 6：i1 bob@1 -> carol@2，价款 10000（s1：carol 10%、dave 5%）；
	// 序号 7：i1 carol@2 -> bob@3，价款 5000，使 i1 回到 bob 手中。
	prev, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "先前成交", RequestID: "rp-old", ItemID: "i1",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "carol", Price: 10000,
	})
	if err != nil {
		t.Fatalf("seed transfer: %v", err)
	}
	if prev.TxSeq != 6 {
		t.Fatalf("seed TxSeq = %d, want 6", prev.TxSeq)
	}
	back, err := r.Transfer(TransferRequest{
		Operator: "carol", Reason: "退回", RequestID: "rp-back", ItemID: "i1",
		ExpectedOwner: "carol", ExpectedVer: 2, ToID: "bob", Price: 5000,
	})
	if err != nil {
		t.Fatalf("return transfer: %v", err)
	}
	if back.TxSeq != 7 {
		t.Fatalf("return TxSeq = %d, want 7", back.TxSeq)
	}

	// 失败前各收款账户的既有应付明细（内容、顺序、金额稍后逐项比对）。
	snapshotPayables := func() map[string][]PayableEntry {
		out := map[string][]PayableEntry{}
		for _, acc := range []string{"carol", "dave", "alice"} {
			ps, err := r.PayablesOf(acc)
			if err != nil {
				t.Fatalf("PayablesOf %s: %v", acc, err)
			}
			out[acc] = append([]PayableEntry(nil), ps...)
		}
		return out
	}
	payBefore := snapshotPayables()
	// seq6 与 seq7 都有版税：carol 1000、500；dave 500、250。
	if len(payBefore["carol"]) != 2 || len(payBefore["dave"]) != 2 {
		t.Fatalf("seed payables = carol %+v dave %+v", payBefore["carol"], payBefore["dave"])
	}
	// 清单外藏品 i5（属于 carol，版本 1）作为不受影响的对照。
	i5Before, err := r.GetHolding("i5")
	if err != nil {
		t.Fatal(err)
	}
	i5HistBefore, err := r.History("i5")
	if err != nil {
		t.Fatal(err)
	}

	// 整批至少两件、接收账户不同，且含版税规则藏品：
	// i1（s1，carol10%+dave5%，bob@3 -> alice）、
	// i2（s2，carol100%，bob@1 -> dave）、
	// i4（s3 无版税，bob@1 -> carol）。
	req := TransferBatchRequest{
		Operator: "bob", Reason: "整批转出", RequestID: "rb-fail",
		Entries: []TransferBatchEntry{
			{ItemID: "i1", ToID: "alice", ExpectedOwner: "bob", ExpectedVer: 3, Price: 10000},
			{ItemID: "i2", ToID: "dave", ExpectedOwner: "bob", ExpectedVer: 1, Price: 333},
			{ItemID: "i4", ToID: "carol", ExpectedOwner: "bob", ExpectedVer: 1, Price: 500},
		},
	}

	// 注入保存失败：业务检查全部通过，仅最终一次落盘在替换前失败。
	blockPath := tempFile(r.dir)
	if err := os.Mkdir(blockPath, 0o755); err != nil {
		t.Fatalf("注入保存失败: %v", err)
	}
	res, err := r.TransferBatch(req)
	// 保存条件恢复：移除障碍目录，后续提交能真正落盘。
	if rmErr := os.Remove(blockPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		t.Fatalf("移除失败注入: %v", rmErr)
	}

	if err == nil {
		t.Fatalf("保存失败必须整体返回错误，得到结果 %+v", res)
	}
	// 保存错误与账户停用、版本冲突等业务拒绝是不同的使用条件。
	for _, biz := range []error{
		ErrConflict, ErrAccountInactive, ErrSameAccount, ErrRequestConflict,
		ErrInvalidArgument, ErrNotFound, ErrForbidden,
	} {
		if errors.Is(err, biz) {
			t.Fatalf("应返回保存错误而非业务拒绝 %v，got %v", biz, err)
		}
	}
	// 不能返回任何一件已经转让成功的结果。
	if len(res.Items) != 0 || res.ItemID != "" || res.Replayed || res.Err != nil {
		t.Fatalf("失败结果必须为空: %+v", res)
	}

	// 调用者继续使用同一个已打开的登记册查询：每件仍属于原持有人、
	// 版本没有增加，历史中没有这次请求。
	wantHolding := map[string]struct {
		owner string
		ver   int64
		histN int
	}{
		"i1": {"bob", 3, 3}, // 发行 + 两笔既有转让
		"i2": {"bob", 1, 1}, // 仅发行
		"i4": {"bob", 1, 1},
	}
	for item, want := range wantHolding {
		h, err := r.GetHolding(item)
		if err != nil {
			t.Fatalf("GetHolding %s: %v", item, err)
		}
		if h.OwnerID != want.owner || h.Version != want.ver {
			t.Errorf("失败后持有变化 %s = %+v, want owner=%s ver=%d", item, h, want.owner, want.ver)
		}
		hist, err := r.History(item)
		if err != nil {
			t.Fatalf("History %s: %v", item, err)
		}
		if len(hist) != want.histN {
			t.Fatalf("%s 历史条数 = %d，want %d：%+v", item, len(hist), want.histN, hist)
		}
		for _, e := range hist {
			if e.RequestID == "rb-fail" {
				t.Fatalf("%s 历史出现失败请求: %+v", item, e)
			}
		}
	}

	// 失败不消耗转让历史序号：磁盘仍是失败前状态，下一条从 8 开始。
	if r.state.NextSeq != 7 {
		t.Fatalf("NextSeq = %d, want 7（失败不消耗序号，下一条应为 8）", r.state.NextSeq)
	}
	for seq := int64(8); seq <= 10; seq++ {
		if _, ok := r.state.Royalties[seq]; ok {
			t.Fatalf("失败的整批不能留下序号 %d 的应付记录", seq)
		}
	}

	// 既有成功转让与应付记录完整保留：内容、顺序、金额都不变。
	for _, acc := range []string{"carol", "dave", "alice"} {
		ps, err := r.PayablesOf(acc)
		if err != nil {
			t.Fatalf("PayablesOf %s: %v", acc, err)
		}
		if len(ps) != len(payBefore[acc]) {
			t.Fatalf("账户 %s 既有应付条数被改动: before %+v after %+v", acc, payBefore[acc], ps)
		}
		for i := range ps {
			if ps[i] != payBefore[acc][i] {
				t.Fatalf("账户 %s 第 %d 条应付被改动:\nbefore %+v\nafter  %+v",
					acc, i, payBefore[acc][i], ps[i])
			}
		}
	}
	oldRoyalty, err := r.TransferRoyalty(6)
	if err != nil {
		t.Fatalf("TransferRoyalty 6: %v", err)
	}
	if oldRoyalty.ItemID != "i1" || oldRoyalty.Price != 10000 || oldRoyalty.Remainder != 8500 ||
		oldRoyalty.OwnerID != "bob" || len(oldRoyalty.Payables) != 2 {
		t.Fatalf("旧应付记录内容变化: %+v", oldRoyalty)
	}

	// 清单外藏品的持有与已有记录不受影响。
	i5After, err := r.GetHolding("i5")
	if err != nil || !reflect.DeepEqual(i5After, i5Before) {
		t.Fatalf("清单外 i5 持有变化: %+v %v", i5After, err)
	}
	i5HistAfter, err := r.History("i5")
	if err != nil || !reflect.DeepEqual(i5HistAfter, i5HistBefore) {
		t.Fatalf("清单外 i5 历史变化: %+v %v", i5HistAfter, err)
	}

	// 请求号没有被失败占用。
	if _, ok := r.state.Requests[requestKey("bob", "rb-fail")]; ok {
		t.Fatal("保存失败不能占用请求号")
	}

	// 保存条件恢复后，用完全相同的请求号、原因和条目再次提交：原来的
	// 期望持有人与版本仍然有效，整批正常执行一次——不是回放，也不报
	// 请求号冲突。
	retry, err := r.TransferBatch(req)
	if err != nil {
		t.Fatalf("恢复后用原请求重试必须完整执行一次: %v", err)
	}
	if retry.Replayed {
		t.Fatalf("不能回放一个实际没有保存的成功结果: %+v", retry)
	}
	if len(retry.Items) != 3 {
		t.Fatalf("retry items = %+v", retry.Items)
	}

	// 成功结果按原清单顺序返回；历史序号从失败前最后一条（7）之后连续
	// 排列为 8、9、10；每件只增加一个持有版本和一条转让历史。
	wantItems := []struct {
		item, from, to string
		ver            int64
		seq, price     int64
		rem            int64
		payees         []RoyaltyPayable
	}{
		{"i1", "bob", "alice", 4, 8, 10000, 8500, []RoyaltyPayable{
			{AccountID: "carol", Rate: 1000, Amount: 1000},
			{AccountID: "dave", Rate: 500, Amount: 500},
		}},
		{"i2", "bob", "dave", 2, 9, 333, 0, []RoyaltyPayable{
			{AccountID: "carol", Rate: 10000, Amount: 333},
		}},
		{"i4", "bob", "carol", 2, 10, 500, 500, []RoyaltyPayable{}},
	}
	for i, w := range wantItems {
		it := retry.Items[i]
		if it.ItemID != w.item || it.FromID != w.from || it.ToID != w.to ||
			it.Version != w.ver || it.TxSeq != w.seq || it.Price != w.price ||
			it.Remainder != w.rem {
			t.Errorf("retry item %d = %+v, want item=%s seq=%d price=%d rem=%d",
				i, it, w.item, w.seq, w.price, w.rem)
		}
		if !reflect.DeepEqual(it.Payables, w.payees) {
			t.Errorf("retry item %s payables = %+v, want %+v", w.item, it.Payables, w.payees)
		}
		// 持有与历史各只增加一次，操作者/原因/请求号与提交一致。
		h, err := r.GetHolding(w.item)
		if err != nil {
			t.Fatal(err)
		}
		if h.OwnerID != w.to || h.Version != w.ver {
			t.Errorf("holding %s = %+v", w.item, h)
		}
		hist, err := r.History(w.item)
		if err != nil {
			t.Fatal(err)
		}
		last := hist[len(hist)-1]
		if last.Seq != w.seq || last.Kind != "transfer" || last.Operator != "bob" ||
			last.Reason != "整批转出" || last.RequestID != "rb-fail" ||
			last.FromID != w.from || last.ToID != w.to ||
			last.ToVersion != w.ver || last.AuthID != "" {
			t.Errorf("%s 新历史 = %+v", w.item, last)
		}
		n := 0
		for _, e := range hist {
			if e.RequestID == "rb-fail" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s 属于本次请求的历史有 %d 条，want 1", w.item, n)
		}
		// 每件的应付金额与余款按它自己的价款及所属系列规则计算，余款归
		// 转让前持有人 bob。
		tr, err := r.TransferRoyalty(w.seq)
		if err != nil {
			t.Fatalf("TransferRoyalty %d: %v", w.seq, err)
		}
		if tr.Price != w.price || tr.Remainder != w.rem || tr.OwnerID != "bob" ||
			!reflect.DeepEqual(tr.Payables, w.payees) {
			t.Errorf("royalty seq %d = %+v", w.seq, tr)
		}
	}

	// 按收款账户查询：只能增加这次真正成功的转让对应的明细，旧记录的
	// 内容与顺序保留。
	carolPay, err := r.PayablesOf("carol")
	if err != nil {
		t.Fatal(err)
	}
	wantCarol := append(append([]PayableEntry(nil), payBefore["carol"]...),
		PayableEntry{TxSeq: 8, ItemID: "i1", Price: 10000, Rate: 1000, Amount: 1000},
		PayableEntry{TxSeq: 9, ItemID: "i2", Price: 333, Rate: 10000, Amount: 333},
	)
	if !reflect.DeepEqual(carolPay, wantCarol) {
		t.Fatalf("carol payables =\n%+v\nwant\n%+v", carolPay, wantCarol)
	}
	davePay, err := r.PayablesOf("dave")
	if err != nil {
		t.Fatal(err)
	}
	wantDave := append(append([]PayableEntry(nil), payBefore["dave"]...),
		PayableEntry{TxSeq: 8, ItemID: "i1", Price: 10000, Rate: 500, Amount: 500},
	)
	if !reflect.DeepEqual(davePay, wantDave) {
		t.Fatalf("dave payables =\n%+v\nwant\n%+v", davePay, wantDave)
	}

	// 成功后再用原请求重提：这次回放真正保存的成功结果。
	replay, err := r.TransferBatch(req)
	if err != nil {
		t.Fatalf("replay after real success: %v", err)
	}
	if !replay.Replayed || !reflect.DeepEqual(replay.Items, retry.Items) {
		t.Fatalf("replay = %+v, want replay of %+v", replay, retry)
	}

	// 重开登记册：失败当时磁盘始终是失败前内容、重试才完成替换，因此
	// 重开后是整批成功后的完整状态，原请求回放同一结果，原登记册一直
	// 可正常读取。
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("原登记册应仍可正常打开: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	reopen, err := r2.TransferBatch(req)
	if err != nil || !reopen.Replayed || !reflect.DeepEqual(reopen.Items, retry.Items) {
		t.Fatalf("reopen replay = %+v err=%v", reopen, err)
	}
	for _, w := range wantItems {
		h, err := r2.GetHolding(w.item)
		if err != nil || h.OwnerID != w.to || h.Version != w.ver {
			t.Fatalf("reopen holding %s = %+v %v", w.item, h, err)
		}
	}
}
