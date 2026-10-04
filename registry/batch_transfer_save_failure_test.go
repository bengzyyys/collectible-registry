package registry

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

// 本文件为整批转让补充"保存失败"场景的回归保障：业务检查全部通过后，
// 在本次内容原子替换已保存数据之前写入失败（原登记册仍可正常读取）时，
// 整批必须整体失败、不留任何部分转让，请求号与历史序号都不被消耗；
// 保存条件恢复后用完全相同的请求号、原因与条目重提，必须完整执行一次，
// 而不是回放一个实际没有保存的成功结果或报请求号冲突。
//
// 失败注入方式：在临时文件路径 .registry.json.tmp 上预先建一个目录，
// save 在 OpenFile 阶段即以 EISDIR 失败——失败发生在 rename 替换
// registry.json 之前，原快照完好可读；删除该目录即恢复保存条件。该方式
// 不依赖文件权限（root 也生效），也不会触碰原数据文件。

// batchSaveFailureReq 是各用例共用的整批请求：至少两件、接收账户互不相同，
// 且包含带版税规则的藏品（i1/i3 属 s1：carol 10% + dave 5%；i2 属 s2：
// carol 100%）。各件期望持有人 bob、版本 1。
func batchSaveFailureReq() TransferBatchRequest {
	return tbtReq("rb-fail",
		tbtEntry("i1", "carol", 1, 10000),
		tbtEntry("i2", "dave", 1, 333),
		tbtEntry("i3", "alice", 1, 500),
	)
}

// seedBatchSaveFailureWorld 建立一个已经存在成功转让与应付记录的登记册：
// setupBatchTransferWorld 发行 i1..i5（历史序号 1..5）后，再由 alice 发行
// i6（s1，序号 6）给 bob，并由 bob 以 10000 分把 i6 转让给 alice（序号 7）。
// 因此失败发生前最后一条历史序号是 7；carol/dave 各有一条序号 7 的应付
// （1000/500），余款 8500 归 bob。清单外藏品 i4（bob，版本 1）、i5
// （carol，版本 1）与已易手的 i6（alice，版本 2）用于核对失败不外溢。
func seedBatchSaveFailureWorld(t *testing.T, r *Registry) {
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

// blockBatchSave 让下一次 commit 的写入在替换数据文件之前确定性失败。
func blockBatchSave(t *testing.T, r *Registry) {
	t.Helper()
	if err := os.MkdirAll(tempFile(r.dir), dirMode); err != nil {
		t.Fatalf("注入保存失败: %v", err)
	}
}

// restoreBatchSave 恢复保存条件：移除占据临时文件路径的目录。
func restoreBatchSave(t *testing.T, r *Registry) {
	t.Helper()
	if err := os.RemoveAll(tempFile(r.dir)); err != nil {
		t.Fatalf("恢复保存条件: %v", err)
	}
}

// businessSentinels 是保存错误必须有别于业务拒绝的哨兵集合。
var businessSentinels = []error{
	ErrConflict, ErrAccountInactive, ErrSameAccount, ErrForbidden,
	ErrRequestConflict, ErrInvalidArgument, ErrNotFound, ErrSeriesSealed,
}

// assertSaveFailureError 确认返回的是保存错误而非任何业务拒绝。
func assertSaveFailureError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("保存失败必须返回错误，却返回了 nil")
	}
	for _, s := range businessSentinels {
		if errors.Is(err, s) {
			t.Fatalf("返回的应是保存错误，不能是业务拒绝 %v: %v", s, err)
		}
	}
}

// assertBatchStillPending 在（失败返回后的）同一个已打开登记册上核对：
// 整批如同从未发生，且失败前已有的内容、顺序与金额完整保留。
func assertBatchStillPending(t *testing.T, r *Registry) {
	t.Helper()
	req := batchSaveFailureReq()

	// 清单内每件仍属原持有人 bob，持有版本没有增加。
	for _, id := range []string{"i1", "i2", "i3"} {
		h, err := r.GetHolding(id)
		if err != nil {
			t.Fatalf("GetHolding %s: %v", id, err)
		}
		if h.OwnerID != "bob" || h.Version != 1 {
			t.Fatalf("失败后 %s 持有被改变: %+v", id, h)
		}
		hist, err := r.History(id)
		if err != nil {
			t.Fatalf("History %s: %v", id, err)
		}
		if len(hist) != 1 || hist[0].Kind != "issue" {
			t.Fatalf("失败后 %s 历史多出本次请求: %+v", id, hist)
		}
		for _, e := range hist {
			if e.RequestID == req.RequestID {
				t.Fatalf("失败后 %s 历史出现请求号 %s: %+v", id, req.RequestID, e)
			}
		}
	}

	// 历史序号不被消耗：下一条仍是 8（失败前最后一条为 7）。
	if r.state.NextSeq != 7 {
		t.Fatalf("NextSeq = %d，失败不应消耗历史序号，仍应为 7", r.state.NextSeq)
	}
	// 请求号未被占用：请求记录中没有这一次失败。
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("失败的整批不应登记请求结果，请求号应仍可使用")
	}

	// 相关收款账户不能多出本次转让的应付明细：旧记录（序号 7）的内容、
	// 顺序与金额都应保留。
	wantCarol := []PayableEntry{{TxSeq: 7, ItemID: "i6", Price: 10000, Rate: 1000, Amount: 1000}}
	wantDave := []PayableEntry{{TxSeq: 7, ItemID: "i6", Price: 10000, Rate: 500, Amount: 500}}
	carolPay, err := r.PayablesOf("carol")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(carolPay, wantCarol) {
		t.Fatalf("carol 应付 = %+v, want %+v", carolPay, wantCarol)
	}
	davePay, err := r.PayablesOf("dave")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(davePay, wantDave) {
		t.Fatalf("dave 应付 = %+v, want %+v", davePay, wantDave)
	}

	// 失败前那笔成功转让的版税依据与金额原样可读：余款归转让前持有人 bob。
	tr, err := r.TransferRoyalty(7)
	if err != nil {
		t.Fatalf("TransferRoyalty 7: %v", err)
	}
	if tr.ItemID != "i6" || tr.Price != 10000 || tr.Remainder != 8500 || tr.OwnerID != "bob" ||
		len(tr.Payables) != 2 {
		t.Fatalf("旧转让版税记录被破坏: %+v", tr)
	}

	// 清单外藏品的持有与已有记录不受影响。
	i4, _ := r.GetHolding("i4")
	if i4.OwnerID != "bob" || i4.Version != 1 {
		t.Fatalf("清单外 i4 受影响: %+v", i4)
	}
	i5, _ := r.GetHolding("i5")
	if i5.OwnerID != "carol" || i5.Version != 1 {
		t.Fatalf("清单外 i5 受影响: %+v", i5)
	}
	i6, _ := r.GetHolding("i6")
	if i6.OwnerID != "alice" || i6.Version != 2 {
		t.Fatalf("清单外 i6 受影响: %+v", i6)
	}
	hist6, _ := r.History("i6")
	if len(hist6) != 2 || hist6[1].Seq != 7 || hist6[1].ToID != "alice" {
		t.Fatalf("清单外 i6 历史受影响: %+v", hist6)
	}
}

// assertBatchRetrySuccess 核对保存恢复后用原请求号重提整批完整执行一次：
// 非回放、无请求号冲突；结果按原清单顺序，历史序号紧随失败前最后一条
// （7）连续为 8/9/10，每件只增加一个持有版本和一条转让历史，操作者、
// 原因与请求号一致；各件金额按自己的价款与所属系列规则计算，余款归
// 转让前持有人 bob；按收款账户查询只多出本次真正成功的明细。
func assertBatchRetrySuccess(t *testing.T, r *Registry, req TransferBatchRequest) TransferBatchResult {
	t.Helper()
	res, err := r.TransferBatch(req)
	if err != nil {
		t.Fatalf("保存恢复后用原请求重提应完整执行一次: %v", err)
	}
	if res.Replayed {
		t.Fatal("实际没有保存过成功结果，重提不应是回放，应完整执行一次")
	}
	if res.ItemID != "" || res.Err != nil || len(res.Items) != 3 {
		t.Fatalf("重提结果异常: %+v", res)
	}

	type wantItem struct {
		item, to   string
		ver, seq   int64
		price, rem int64
		payables   []RoyaltyPayable
	}
	want := []wantItem{
		{"i1", "carol", 2, 8, 10000, 8500, []RoyaltyPayable{
			{AccountID: "carol", Rate: 1000, Amount: 1000},
			{AccountID: "dave", Rate: 500, Amount: 500},
		}},
		{"i2", "dave", 2, 9, 333, 0, []RoyaltyPayable{
			{AccountID: "carol", Rate: 10000, Amount: 333},
		}},
		{"i3", "alice", 2, 10, 500, 425, []RoyaltyPayable{
			{AccountID: "carol", Rate: 1000, Amount: 50},
			{AccountID: "dave", Rate: 500, Amount: 25},
		}},
	}
	for i, w := range want {
		it := res.Items[i]
		if it.ItemID != w.item || it.FromID != "bob" || it.ToID != w.to ||
			it.Version != w.ver || it.TxSeq != w.seq ||
			it.Price != w.price || it.Remainder != w.rem {
			t.Fatalf("结果第 %d 件 = %+v, want item=%s to=%s ver=%d seq=%d price=%d rem=%d",
				i, it, w.item, w.to, w.ver, w.seq, w.price, w.rem)
		}
		if !reflect.DeepEqual(it.Payables, w.payables) {
			t.Fatalf("结果第 %d 件 %s 应付 = %+v, want %+v", i, w.item, it.Payables, w.payables)
		}
		if i > 0 && it.TxSeq != res.Items[i-1].TxSeq+1 {
			t.Fatalf("历史序号不连续: %+v", res.Items)
		}

		// 每件只增加一个持有版本，且只新增一条属于本次请求的转让历史。
		h, err := r.GetHolding(w.item)
		if err != nil {
			t.Fatal(err)
		}
		if h.OwnerID != w.to || h.Version != w.ver {
			t.Fatalf("成功后 %s 持有 = %+v", w.item, h)
		}
		hist, err := r.History(w.item)
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) != 2 {
			t.Fatalf("%s 历史应为发行+本次转让两条，得到 %+v", w.item, hist)
		}
		last := hist[1]
		if last.Seq != w.seq || last.Kind != "transfer" ||
			last.Operator != "bob" || last.Reason != req.Reason || last.RequestID != req.RequestID ||
			last.FromID != "bob" || last.ToID != w.to ||
			last.FromVersion != 1 || last.ToVersion != w.ver || last.AuthID != "" {
			t.Fatalf("%s 本次转让历史 = %+v", w.item, last)
		}
		tr, err := r.TransferRoyalty(w.seq)
		if err != nil {
			t.Fatalf("TransferRoyalty %d: %v", w.seq, err)
		}
		if tr.ItemID != w.item || tr.Price != w.price || tr.Remainder != w.rem ||
			tr.OwnerID != "bob" || !reflect.DeepEqual(tr.Payables, w.payables) {
			t.Fatalf("%s 版税记录 = %+v, payables want %+v", w.item, tr, w.payables)
		}
	}

	// 按收款账户查询：只能增加真正成功的三笔（序号 8/9/10）明细，旧的
	// 序号 7 记录内容、顺序、金额保持不变。
	wantCarol := []PayableEntry{
		{TxSeq: 7, ItemID: "i6", Price: 10000, Rate: 1000, Amount: 1000},
		{TxSeq: 8, ItemID: "i1", Price: 10000, Rate: 1000, Amount: 1000},
		{TxSeq: 9, ItemID: "i2", Price: 333, Rate: 10000, Amount: 333},
		{TxSeq: 10, ItemID: "i3", Price: 500, Rate: 1000, Amount: 50},
	}
	wantDave := []PayableEntry{
		{TxSeq: 7, ItemID: "i6", Price: 10000, Rate: 500, Amount: 500},
		{TxSeq: 8, ItemID: "i1", Price: 10000, Rate: 500, Amount: 500},
		{TxSeq: 10, ItemID: "i3", Price: 500, Rate: 500, Amount: 25},
	}
	if carolPay, err := r.PayablesOf("carol"); err != nil || !reflect.DeepEqual(carolPay, wantCarol) {
		t.Fatalf("carol 应付 = %+v, err %v, want %+v", carolPay, err, wantCarol)
	}
	if davePay, err := r.PayablesOf("dave"); err != nil || !reflect.DeepEqual(davePay, wantDave) {
		t.Fatalf("dave 应付 = %+v, err %v, want %+v", davePay, err, wantDave)
	}
	// alice 只是接收账户，不是任何系列的收款人：不应多出任何明细。
	if alicePay, err := r.PayablesOf("alice"); err != nil || len(alicePay) != 0 {
		t.Fatalf("非收款人 alice 应付 = %+v, err %v, 应为空", alicePay, err)
	}

	// 最后一条历史序号为 10，8/9/10 连续无空洞（NextSeq 记录最后已分配
	// 的序号）。
	if r.state.NextSeq != 10 {
		t.Fatalf("NextSeq = %d, want 10", r.state.NextSeq)
	}

	// 再用完全相同的请求号重提：回放首次成功结果，不再执行第二次。
	replay, err := r.TransferBatch(req)
	if err != nil {
		t.Fatalf("成功后重提应回放: %v", err)
	}
	if !replay.Replayed || len(replay.Items) != 3 {
		t.Fatalf("成功后重提 = %+v", replay)
	}
	for i := range want {
		if replay.Items[i].ItemID != res.Items[i].ItemID ||
			replay.Items[i].TxSeq != res.Items[i].TxSeq ||
			replay.Items[i].Version != res.Items[i].Version {
			t.Fatalf("回放内容与首次成功不一致: %+v vs %+v", replay.Items[i], res.Items[i])
		}
		h, _ := r.GetHolding(want[i].item)
		if h.Version != want[i].ver {
			t.Fatalf("回放又增加了持有版本: %+v", h)
		}
		if hist, _ := r.History(want[i].item); len(hist) != 2 {
			t.Fatalf("回放又产生了历史: %+v", hist)
		}
	}
	if carolPay, _ := r.PayablesOf("carol"); len(carolPay) != len(wantCarol) {
		t.Fatalf("回放又产生了应付明细: %+v", carolPay)
	}
	return res
}

// TestTransferBatchSaveFailureSameRegistry 覆盖核心场景：保存失败后调用者
// 继续使用同一个已打开的登记册，全部状态停留在整批之前；保存条件恢复后
// 用原请求号重提完整执行一次。
func TestTransferBatchSaveFailureSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedBatchSaveFailureWorld(t, r)
	req := batchSaveFailureReq()

	// 业务参数完全合规（账户登记可用、接收人不同、期望持有人与版本相符、
	// 价款非负），失败只可能来自写入阶段。
	blockBatchSave(t, r)
	res, err := r.TransferBatch(req)
	assertSaveFailureError(t, err)
	if len(res.Items) != 0 || res.ItemID != "" || res.Replayed || res.Err != nil {
		t.Fatalf("保存失败必须整体失败，不能返回任何一件已转让成功的结果: %+v", res)
	}

	// 同一个已打开的登记册上立即查询：整批从未发生，旧记录完整保留。
	assertBatchStillPending(t, r)

	// 保存条件恢复后用完全相同的请求号、原因和条目再次提交：原来的期望
	// 持有人与版本仍然有效，整批完整执行一次（非回放、无请求号冲突）。
	restoreBatchSave(t, r)
	assertBatchRetrySuccess(t, r, req)

	// 清单外藏品在失败与重试之后仍保持原状。
	i4, _ := r.GetHolding("i4")
	if i4.OwnerID != "bob" || i4.Version != 1 {
		t.Fatalf("清单外 i4 最终受影响: %+v", i4)
	}
	i5, _ := r.GetHolding("i5")
	if i5.OwnerID != "carol" || i5.Version != 1 {
		t.Fatalf("清单外 i5 最终受影响: %+v", i5)
	}
	i6, _ := r.GetHolding("i6")
	if i6.OwnerID != "alice" || i6.Version != 2 {
		t.Fatalf("清单外 i6 最终受影响: %+v", i6)
	}
}

// TestTransferBatchSaveFailureRetryAfterReopen 覆盖磁盘视角：保存失败后
// 原登记册仍可正常读取——关闭后重新 Open，看到的仍是整批前状态；此时用
// 原请求号重提同样完整执行一次，而不是回放未保存的成功。
func TestTransferBatchSaveFailureRetryAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	t.Cleanup(func() { _ = r.Close() })
	seedBatchSaveFailureWorld(t, r)
	req := batchSaveFailureReq()

	blockBatchSave(t, r)
	res, err := r.TransferBatch(req)
	assertSaveFailureError(t, err)
	if len(res.Items) != 0 || res.Replayed {
		t.Fatalf("保存失败必须整体失败: %+v", res)
	}

	// 原数据文件从未被替换：正常关闭并重新打开仍能读取整批前的状态。
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertBatchStillPending(t, r2)

	// 恢复保存条件后，在重开的登记册上用原请求重提：完整执行一次。
	restoreBatchSave(t, r2)
	assertBatchRetrySuccess(t, r2, req)
}
