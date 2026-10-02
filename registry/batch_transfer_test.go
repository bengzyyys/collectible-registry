package registry

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

// setupBatchTransferWorld 建立 alice（系列创建者）、bob、carol、dave 四个
// 可用账户；系列 s1（版税 carol 10% + dave 5%）、s2（版税 carol 100%）、
// s3（无版税）；并发行 i1..i4 给 bob（分属不同系列与批次），i5 给 carol。
// 发行占用历史序号 1..5。
func setupBatchTransferWorld(t *testing.T, r *Registry) {
	t.Helper()
	for _, id := range []string{"alice", "bob", "carol", "dave"} {
		if err := r.RegisterAccount(id, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, sid := range []string{"s1", "s2", "s3"} {
		if err := r.CreateSeries(sid, "alice", "系列"+sid); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.SetRoyalty(SetRoyaltyRequest{
		Operator: "alice", Reason: "设置版税", RequestID: "rr-s1", SeriesID: "s1",
		Shares: []RoyaltyShare{{AccountID: "carol", Rate: 1000}, {AccountID: "dave", Rate: 500}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetRoyalty(SetRoyaltyRequest{
		Operator: "alice", Reason: "设置版税", RequestID: "rr-s2", SeriesID: "s2",
		Shares: []RoyaltyShare{{AccountID: "carol", Rate: 10000}},
	}); err != nil {
		t.Fatal(err)
	}
	issues := []struct {
		item, series, batch, holder string
	}{
		{"i1", "s1", "b1", "bob"},
		{"i2", "s2", "b2", "bob"},
		{"i3", "s1", "b1", "bob"},
		{"i4", "s3", "b9", "bob"},
		{"i5", "s1", "b1", "carol"},
	}
	for _, is := range issues {
		if _, err := r.Issue(IssueRequest{
			Operator: "alice", Reason: "首发", RequestID: "ri-" + is.item,
			ItemID: is.item, SeriesID: is.series, BatchNo: is.batch,
			Metadata: "元-" + is.item, HolderID: is.holder,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func tbtEntry(item, to string, ver int64, price int64) TransferBatchEntry {
	return TransferBatchEntry{
		ItemID: item, ToID: to, ExpectedOwner: "bob", ExpectedVer: ver, Price: price,
	}
}

func tbtReq(rid string, entries ...TransferBatchEntry) TransferBatchRequest {
	return TransferBatchRequest{
		Operator: "bob", Reason: "整批转出", RequestID: rid, Entries: entries,
	}
}

// ---- 基本成功流程 ----

func TestTransferBatchSuccess(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)

	req := tbtReq("rt1",
		tbtEntry("i1", "carol", 1, 10000),
		tbtEntry("i2", "dave", 1, 333),
		tbtEntry("i3", "carol", 1, 0), // 未填价款按零；接收账户 carol 重复
		tbtEntry("i4", "alice", 1, 500),
	)
	res, err := r.TransferBatch(req)
	if err != nil {
		t.Fatalf("TransferBatch: %v", err)
	}
	if res.Replayed || res.Err != nil || res.ItemID != "" {
		t.Fatalf("unexpected result flags: %+v", res)
	}
	if len(res.Items) != 4 {
		t.Fatalf("want 4 items, got %d", len(res.Items))
	}

	// 历史序号连续递增，且紧随发行序号之后。
	for i, it := range res.Items {
		if it.TxSeq != int64(6+i) {
			t.Errorf("item %d TxSeq = %d, want %d", i, it.TxSeq, 6+i)
		}
		if i > 0 && it.TxSeq != res.Items[i-1].TxSeq+1 {
			t.Fatalf("TxSeq not consecutive: %+v", res.Items)
		}
	}

	want := []struct {
		item, from, to string
		ver            int64
		price, rem     int64
		payees         []RoyaltyPayable
	}{
		{"i1", "bob", "carol", 2, 10000, 8500, []RoyaltyPayable{
			{AccountID: "carol", Rate: 1000, Amount: 1000},
			{AccountID: "dave", Rate: 500, Amount: 500},
		}},
		{"i2", "bob", "dave", 2, 333, 0, []RoyaltyPayable{
			{AccountID: "carol", Rate: 10000, Amount: 333},
		}},
		{"i3", "bob", "carol", 2, 0, 0, []RoyaltyPayable{
			{AccountID: "carol", Rate: 1000, Amount: 0}, // 零金额明细保留
			{AccountID: "dave", Rate: 500, Amount: 0},
		}},
		{"i4", "bob", "alice", 2, 500, 500, nil}, // 无版税规则：无明细，余款为全款
	}
	for i, w := range want {
		it := res.Items[i]
		if it.ItemID != w.item || it.FromID != w.from || it.ToID != w.to ||
			it.Version != w.ver || it.Price != w.price || it.Remainder != w.rem {
			t.Errorf("item %d = %+v", i, it)
		}
		if len(it.Payables) != len(w.payees) {
			t.Errorf("item %s payables = %+v, want %+v", w.item, it.Payables, w.payees)
			continue
		}
		for j := range w.payees {
			if it.Payables[j] != w.payees[j] {
				t.Errorf("item %s payable %d = %+v, want %+v", w.item, j, it.Payables[j], w.payees[j])
			}
		}
	}

	// 各件通过现有持有、历史、应付查询核对；历史保留整批共用的操作者、
	// 原因与请求号，且不带授权编号。
	for _, w := range want {
		h, err := r.GetHolding(w.item)
		if err != nil {
			t.Fatalf("GetHolding %s: %v", w.item, err)
		}
		if h.OwnerID != w.to || h.Version != w.ver {
			t.Errorf("holding %s = %+v", w.item, h)
		}
		hist, err := r.History(w.item)
		if err != nil {
			t.Fatalf("History %s: %v", w.item, err)
		}
		if len(hist) != 2 {
			t.Fatalf("history %s = %+v", w.item, hist)
		}
		last := hist[1]
		if last.Kind != "transfer" || last.Operator != "bob" || last.Reason != "整批转出" ||
			last.RequestID != "rt1" || last.FromID != "bob" || last.ToID != w.to ||
			last.FromVersion != 1 || last.ToVersion != w.ver || last.AuthID != "" {
			t.Errorf("history entry %s = %+v", w.item, last)
		}
		tr, err := r.TransferRoyalty(last.Seq)
		if err != nil {
			t.Fatalf("TransferRoyalty %d: %v", last.Seq, err)
		}
		if tr.ItemID != w.item || tr.Price != w.price || tr.Remainder != w.rem ||
			tr.OwnerID != "bob" {
			t.Errorf("royalty %s = %+v", w.item, tr)
		}
	}

	// 同一收款账户在多件中出现分别保留明细，不合并价款。
	carolPay, err := r.PayablesOf("carol")
	if err != nil {
		t.Fatal(err)
	}
	if len(carolPay) != 3 {
		t.Fatalf("carol payables = %+v, want 3 separate lines", carolPay)
	}
	gotAmounts := map[int64]int64{}
	for _, p := range carolPay {
		gotAmounts[p.TxSeq] = p.Amount
	}
	if gotAmounts[6] != 1000 || gotAmounts[7] != 333 || gotAmounts[8] != 0 {
		t.Fatalf("carol payable amounts = %+v", gotAmounts)
	}
	davePay, err := r.PayablesOf("dave")
	if err != nil {
		t.Fatal(err)
	}
	if len(davePay) != 2 || davePay[0].Amount != 500 || davePay[1].Amount != 0 {
		t.Fatalf("dave payables = %+v", davePay)
	}
}

// 系列封存不阻止整批转让。
func TestTransferBatchSealedSeriesAllowed(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	res, err := r.TransferBatch(tbtReq("rt1", tbtEntry("i1", "carol", 1, 100)))
	if err != nil {
		t.Fatalf("sealed series must not block transfer: %v", err)
	}
	if res.Items[0].ToID != "carol" || res.Items[0].Version != 2 {
		t.Fatalf("result = %+v", res.Items[0])
	}
}

// ---- 参数错误（不占用请求号）----

func TestTransferBatchInvalidArgument(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)

	cases := map[string]TransferBatchRequest{
		"空清单":      tbtReq("r0"),
		"编号重复":     tbtReq("r1", tbtEntry("i1", "carol", 1, 0), tbtEntry("i1", "dave", 1, 0)),
		"缺操作者":     {Reason: "r", RequestID: "r2", Entries: []TransferBatchEntry{tbtEntry("i1", "carol", 1, 0)}},
		"缺原因":      {Operator: "bob", RequestID: "r3", Entries: []TransferBatchEntry{tbtEntry("i1", "carol", 1, 0)}},
		"缺请求号":     {Operator: "bob", Reason: "r", Entries: []TransferBatchEntry{tbtEntry("i1", "carol", 1, 0)}},
		"条目缺编号":    tbtReq("r4", TransferBatchEntry{ToID: "carol", ExpectedOwner: "bob", ExpectedVer: 1}),
		"条目缺接收人":   tbtReq("r5", TransferBatchEntry{ItemID: "i1", ExpectedOwner: "bob", ExpectedVer: 1}),
		"条目缺期望持有人": tbtReq("r6", TransferBatchEntry{ItemID: "i1", ToID: "carol", ExpectedVer: 1}),
		"期望版本为零":   tbtReq("r7", TransferBatchEntry{ItemID: "i1", ToID: "carol", ExpectedOwner: "bob"}),
		"期望版本为负":   tbtReq("r8", TransferBatchEntry{ItemID: "i1", ToID: "carol", ExpectedOwner: "bob", ExpectedVer: -3}),
		"负价款":      tbtReq("r9", tbtEntry("i1", "carol", 1, -1)),
	}
	for name, req := range cases {
		if _, err := r.TransferBatch(req); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}

	// 参数错误不占用请求号：修正后可用原号重新提交。
	bad := tbtReq("r10", tbtEntry("i1", "carol", 1, -5))
	if _, err := r.TransferBatch(bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative price: %v", err)
	}
	if _, err := r.TransferBatch(tbtReq("r10", tbtEntry("i1", "carol", 1, 5))); err != nil {
		t.Fatalf("resubmit with same request id after param fix: %v", err)
	}
}

// ---- 引用不存在（不占用请求号；操作者错误不带藏品编号）----

func TestTransferBatchNotFound(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)

	// 操作者未登记：只返回账户错误，ItemID 为空；即使条目本身也有问题，
	// 操作者错误仍然优先。
	req := tbtReq("rn1", tbtEntry("i1", "carol", 1, 0), tbtEntry("ghost-item", "dave", 1, 0))
	req.Operator = "nobody"
	res, err := r.TransferBatch(req)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("operator not found: %v", err)
	}
	if res.ItemID != "" {
		t.Fatalf("operator error must not carry ItemID, got %q", res.ItemID)
	}

	// 藏品不存在：指出清单顺序最前的失败藏品。
	res, err = r.TransferBatch(tbtReq("rn2", tbtEntry("i1", "carol", 1, 0),
		TransferBatchEntry{ItemID: "no-item", ToID: "dave", ExpectedOwner: "bob", ExpectedVer: 1}))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("item not found: %v", err)
	}
	if res.ItemID != "no-item" {
		t.Fatalf("ItemID = %q, want no-item", res.ItemID)
	}

	// 接收账户未登记：指出该件。
	res, err = r.TransferBatch(tbtReq("rn3", tbtEntry("i1", "ghost", 1, 0)))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("recipient not found: %v", err)
	}
	if res.ItemID != "i1" {
		t.Fatalf("ItemID = %q, want i1", res.ItemID)
	}

	// 引用不存在不占用请求号：补建接收账户后用原号成功。
	if err := r.RegisterAccount("ghost", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.TransferBatch(tbtReq("rn3", tbtEntry("i1", "ghost", 1, 0))); err != nil {
		t.Fatalf("resubmit after registering recipient: %v", err)
	}
}

// ---- 业务拒绝：整批原子，且只返回清单最前的失败藏品 ----

func TestTransferBatchBusinessRejections(t *testing.T) {
	t.Run("操作者停用只返回账户错误", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		setupBatchTransferWorld(t, r)
		if err := r.DeactivateAccount("bob"); err != nil {
			t.Fatal(err)
		}
		res, err := r.TransferBatch(tbtReq("r1", tbtEntry("i1", "carol", 1, 0), tbtEntry("i2", "dave", 9, 0)))
		if !errors.Is(err, ErrAccountInactive) {
			t.Fatalf("err = %v, want ErrAccountInactive", err)
		}
		if res.ItemID != "" {
			t.Fatalf("inactive operator error must not carry ItemID, got %q", res.ItemID)
		}
	})

	t.Run("接收账户停用", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		setupBatchTransferWorld(t, r)
		if err := r.DeactivateAccount("dave"); err != nil {
			t.Fatal(err)
		}
		res, err := r.TransferBatch(tbtReq("r1", tbtEntry("i1", "carol", 1, 0), tbtEntry("i2", "dave", 1, 0)))
		if !errors.Is(err, ErrAccountInactive) {
			t.Fatalf("err = %v", err)
		}
		if res.ItemID != "i2" {
			t.Fatalf("ItemID = %q, want i2", res.ItemID)
		}
	})

	t.Run("收发同人", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		setupBatchTransferWorld(t, r)
		res, err := r.TransferBatch(tbtReq("r1", tbtEntry("i1", "bob", 1, 0)))
		if !errors.Is(err, ErrSameAccount) {
			t.Fatalf("err = %v", err)
		}
		if res.ItemID != "i1" {
			t.Fatalf("ItemID = %q", res.ItemID)
		}
	})

	t.Run("操作者不是持有人", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		setupBatchTransferWorld(t, r)
		// i5 属于 carol：bob 提交即冲突，即使期望持有人写的是 carol。
		opIsCarol := TransferBatchRequest{
			Operator: "bob", Reason: "整批转出", RequestID: "r1",
			Entries: []TransferBatchEntry{{
				ItemID: "i5", ToID: "alice", ExpectedOwner: "carol", ExpectedVer: 1,
			}},
		}
		res, err := r.TransferBatch(opIsCarol)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if res.ItemID != "i5" {
			t.Fatalf("ItemID = %q, want i5", res.ItemID)
		}
	})

	t.Run("期望版本不符返回最前失败件", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		setupBatchTransferWorld(t, r)
		// i1 合法、i2 版本不符、i3 接收人停用：必须报 i2。
		if err := r.DeactivateAccount("carol"); err != nil {
			t.Fatal(err)
		}
		res, err := r.TransferBatch(tbtReq("r1",
			tbtEntry("i1", "dave", 1, 0),
			tbtEntry("i2", "dave", 7, 0),
			tbtEntry("i3", "carol", 1, 0),
		))
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if res.ItemID != "i2" {
			t.Fatalf("ItemID = %q, want i2", res.ItemID)
		}

		// 任何拒绝都不改变任何一件：i1 也保持原持有与版本，无新历史、无应付，
		// 历史序号未被消耗。
		for _, id := range []string{"i1", "i2", "i3"} {
			h, err := r.GetHolding(id)
			if err != nil {
				t.Fatal(err)
			}
			if h.OwnerID != "bob" || h.Version != 1 {
				t.Fatalf("holding %s changed after rejection: %+v", id, h)
			}
			hist, err := r.History(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(hist) != 1 {
				t.Fatalf("history %s changed after rejection: %+v", id, hist)
			}
		}
		if ps, err := r.PayablesOf("dave"); err != nil || len(ps) != 0 {
			t.Fatalf("payables changed after rejection: %+v %v", ps, err)
		}
		// 下一笔转让的序号仍是 6，整批没有消耗序号。
		single, err := r.Transfer(TransferRequest{
			Operator: "bob", Reason: "卖出", RequestID: "rx1", ItemID: "i1",
			ExpectedOwner: "bob", ExpectedVer: 1, ToID: "dave",
		})
		if err != nil {
			t.Fatal(err)
		}
		if single.TxSeq != 6 {
			t.Fatalf("single TxSeq = %d, want 6", single.TxSeq)
		}
	})
}

// ---- 幂等回放与请求号冲突 ----

func TestTransferBatchIdempotency(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)

	req := tbtReq("rb1", tbtEntry("i1", "carol", 1, 10000), tbtEntry("i2", "dave", 1, 333))
	first, err := r.TransferBatch(req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// 完全相同的内容与顺序重提：回放首次成功结果。
	second, err := r.TransferBatch(req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed || second.ItemID != "" {
		t.Fatalf("replay flags = %+v", second)
	}
	if len(second.Items) != len(first.Items) {
		t.Fatalf("items = %+v", second.Items)
	}
	for i := range first.Items {
		if !reflect.DeepEqual(second.Items[i], first.Items[i]) {
			t.Errorf("item %d: replay %+v != first %+v", i, second.Items[i], first.Items[i])
		}
	}
	for _, id := range []string{"i1", "i2"} {
		hist, err := r.History(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) != 2 {
			t.Fatalf("%s history changed on replay: %+v", id, hist)
		}
	}

	// 改动原因、条目顺序或任一条目参数都是请求号冲突。
	mutations := map[string]TransferBatchRequest{
		"改原因":   {Operator: "bob", Reason: "别的原因", RequestID: "rb1", Entries: req.Entries},
		"改顺序":   tbtReq("rb1", tbtEntry("i2", "dave", 1, 333), tbtEntry("i1", "carol", 1, 10000)),
		"改价款":   tbtReq("rb1", tbtEntry("i1", "carol", 1, 9999), tbtEntry("i2", "dave", 1, 333)),
		"改接收人":  tbtReq("rb1", tbtEntry("i1", "dave", 1, 10000), tbtEntry("i2", "dave", 1, 333)),
		"改期望版本": tbtReq("rb1", tbtEntry("i1", "carol", 2, 10000), tbtEntry("i2", "dave", 1, 333)),
		"改期望持有人": func() TransferBatchRequest {
			q := tbtReq("rb1", tbtEntry("i1", "carol", 1, 10000), tbtEntry("i2", "dave", 1, 333))
			q.Entries[0].ExpectedOwner = "alice"
			return q
		}(),
	}
	for name, m := range mutations {
		if _, err := r.TransferBatch(m); !errors.Is(err, ErrRequestConflict) {
			t.Errorf("%s: err = %v, want ErrRequestConflict", name, err)
		}
	}

	// 与已有操作共用请求号：单件转让用过的号不能用于整批。
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "卖出", RequestID: "shared", ItemID: "i3",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.TransferBatch(tbtReq("shared", tbtEntry("i4", "carol", 1, 0))); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request id shared with single transfer: %v", err)
	}
}

// 成功回放不受之后易手或停用影响。
func TestTransferBatchReplayAfterStateChanges(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)

	req := tbtReq("rb1", tbtEntry("i1", "carol", 1, 10000), tbtEntry("i2", "dave", 1, 333))
	first, err := r.TransferBatch(req)
	if err != nil {
		t.Fatal(err)
	}
	// 藏品再次易手、原接收账户停用。
	if _, err := r.Transfer(TransferRequest{
		Operator: "carol", Reason: "再卖", RequestID: "rx2", ItemID: "i1",
		ExpectedOwner: "carol", ExpectedVer: 2, ToID: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}

	replay, err := r.TransferBatch(req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("should be Replayed")
	}
	for i := range first.Items {
		if !reflect.DeepEqual(replay.Items[i], first.Items[i]) {
			t.Errorf("item %d: replay %+v != first %+v", i, replay.Items[i], first.Items[i])
		}
	}
	// 回放不改写当前状态：i1 已在 alice 手中、版本 3。
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "alice" || h.Version != 3 {
		t.Fatalf("holding rewritten by replay: %+v", h)
	}
}

// 状态类拒绝也占用请求号并回放，即使后来状态变化。
func TestTransferBatchRejectedReplay(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)

	req := tbtReq("rb1", tbtEntry("i1", "carol", 1, 0), tbtEntry("i2", "dave", 9, 0))
	res, err := r.TransferBatch(req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("first: %v", err)
	}
	// i2 随后真的易手到版本 2，整批若现在执行仍会失败——但相同参数重提
	// 必须回放首次的拒绝，而不是基于新状态重新判定。
	if _, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "卖出", RequestID: "rx1", ItemID: "i2",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	res, err = r.TransferBatch(req)
	if !errors.Is(err, ErrConflict) || !res.Replayed || res.ItemID != "i2" {
		t.Fatalf("replay rejected = %+v, err %v", res, err)
	}
	// 改动参数则请求号冲突。
	if _, err := r.TransferBatch(tbtReq("rb1", tbtEntry("i1", "carol", 1, 0))); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflict: %v", err)
	}
}

// ---- 代转授权：整批不消耗授权，旧版本授权不恢复 ----

func TestTransferBatchAuthorizationInteraction(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)

	expires := r.now().Add(3600e9)
	if _, err := r.CreateAuthorization(CreateAuthorizationRequest{
		Operator: "bob", Reason: "委托", RequestID: "ra1", AuthID: "a1",
		ItemID: "i1", TrusteeID: "carol", ToID: "alice",
		ExpectedOwner: "bob", ExpectedVer: 1, ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}

	// 整批转让 i1：授权不被记为已使用。
	if _, err := r.TransferBatch(tbtReq("rt1", tbtEntry("i1", "dave", 1, 100))); err != nil {
		t.Fatal(err)
	}
	a, err := r.GetAuthorization("a1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != AuthActive || a.UsedTxSeq != 0 {
		t.Fatalf("authorization should remain active, got %+v", a)
	}
	events, err := r.AuthorizationHistory("i1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "create" {
		t.Fatalf("auth events = %+v", events)
	}

	// 授权绑定旧版本：代转立即冲突。
	if _, err := r.ProxyTransfer(ProxyTransferRequest{
		Operator: "carol", Reason: "代转", RequestID: "rp1", AuthID: "a1",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("proxy after batch: %v", err)
	}

	// 藏品转回 bob 后旧授权仍不可用。
	if _, err := r.Transfer(TransferRequest{
		Operator: "dave", Reason: "退回", RequestID: "rx2", ItemID: "i1",
		ExpectedOwner: "dave", ExpectedVer: 2, ToID: "bob",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(ProxyTransferRequest{
		Operator: "carol", Reason: "代转", RequestID: "rp2", AuthID: "a1",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("proxy after return: %v", err)
	}
	a, _ = r.GetAuthorization("a1")
	if a.Status != AuthActive {
		t.Fatalf("authorization should never become used: %+v", a)
	}
}

// ---- 持久化 ----

func TestTransferBatchPersistence(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupBatchTransferWorld(t, r)
	req := tbtReq("rb1", tbtEntry("i1", "carol", 1, 10000), tbtEntry("i2", "dave", 1, 333))
	first, err := r.TransferBatch(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r2.Close() })

	replay, err := r2.TransferBatch(req)
	if err != nil {
		t.Fatalf("replay after reopen: %v", err)
	}
	if !replay.Replayed || len(replay.Items) != 2 {
		t.Fatalf("replay = %+v", replay)
	}
	for i := range first.Items {
		if !reflect.DeepEqual(replay.Items[i], first.Items[i]) {
			t.Errorf("item %d: %+v != %+v", i, replay.Items[i], first.Items[i])
		}
	}
	h, err := r2.GetHolding("i1")
	if err != nil || h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("holding after reopen: %+v %v", h, err)
	}
	hist, err := r2.History("i2")
	if err != nil || len(hist) != 2 || hist[1].RequestID != "rb1" {
		t.Fatalf("history after reopen: %+v %v", hist, err)
	}
	// 应付明细与余款完整保留。
	for _, w := range first.Items {
		tr, err := r2.TransferRoyalty(w.TxSeq)
		if err != nil {
			t.Fatalf("TransferRoyalty %d: %v", w.TxSeq, err)
		}
		if tr.Price != w.Price || tr.Remainder != w.Remainder || len(tr.Payables) != len(w.Payables) {
			t.Fatalf("royalty %d after reopen = %+v, want %+v", w.TxSeq, tr, w)
		}
	}
}

// ---- 并发 ----

// 并发重复提交同一请求只完成一次，其余全部回放首次结果。
func TestTransferBatchConcurrentSameRequest(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	req := tbtReq("rb1", tbtEntry("i1", "carol", 1, 100), tbtEntry("i2", "dave", 1, 200))

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	results := make([]TransferBatchResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = r.TransferBatch(req)
		}(i)
	}
	wg.Wait()
	replayed := 0
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if len(results[i].Items) != 2 {
			t.Fatalf("goroutine %d: %+v", i, results[i])
		}
		if results[i].Replayed {
			replayed++
		}
	}
	if replayed != n-1 {
		t.Fatalf("replayed = %d, want %d", replayed, n-1)
	}
	// 每件只转让一次：版本 2、历史两条。
	for _, id := range []string{"i1", "i2"} {
		h, err := r.GetHolding(id)
		if err != nil {
			t.Fatal(err)
		}
		if h.Version != 2 {
			t.Fatalf("%s version = %d, want 2", id, h.Version)
		}
		hist, err := r.History(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) != 2 {
			t.Fatalf("%s history len = %d", id, len(hist))
		}
	}
	// 每件应付只记一次：i1（s1，carol 10%）与 i2（s2，carol 100%）各一条。
	carolPay, _ := r.PayablesOf("carol")
	if len(carolPay) != 2 {
		t.Fatalf("carol payables = %+v, want exactly 2 lines", carolPay)
	}
	amounts := map[int64]int64{}
	for _, p := range carolPay {
		amounts[p.TxSeq] = p.Amount
	}
	if amounts[6] != 10 || amounts[7] != 200 {
		t.Fatalf("carol payable amounts = %+v, want {6:10 7:200}", amounts)
	}
}

// 整批与单件转让、其他整批、代转争用同一藏品版本：最多一方成功；失败
// 整批中的其他藏品保持原状。
func TestTransferBatchConcurrentContention(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r)
	// 额外为 i2 准备一份代转授权（受托人 carol，接收人 alice）。
	expires := r.now().Add(3600e9)
	if _, err := r.CreateAuthorization(CreateAuthorizationRequest{
		Operator: "bob", Reason: "委托", RequestID: "ra1", AuthID: "a1",
		ItemID: "i2", TrusteeID: "carol", ToID: "alice",
		ExpectedOwner: "bob", ExpectedVer: 1, ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	run := func(i int, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fn()
		}()
	}
	// 整批 A：争 i1，另带 i3。
	run(0, func() error {
		_, e := r.TransferBatch(tbtReq("rb-a", tbtEntry("i1", "carol", 1, 1), tbtEntry("i3", "dave", 1, 1)))
		return e
	})
	// 单件转让：争 i1。
	run(1, func() error {
		_, e := r.Transfer(TransferRequest{
			Operator: "bob", Reason: "卖出", RequestID: "rt-s", ItemID: "i1",
			ExpectedOwner: "bob", ExpectedVer: 1, ToID: "dave",
		})
		return e
	})
	// 另一整批：争 i1 与 i2。
	run(2, func() error {
		_, e := r.TransferBatch(tbtReq("rb-c", tbtEntry("i1", "alice", 1, 1), tbtEntry("i2", "dave", 1, 1)))
		return e
	})
	// 代转：争 i2。
	run(3, func() error {
		_, e := r.ProxyTransfer(ProxyTransferRequest{
			Operator: "carol", Reason: "代转", RequestID: "rp-1", AuthID: "a1",
		})
		return e
	})
	wg.Wait()

	ok := 0
	for i, err := range errs {
		if err == nil {
			ok++
			continue
		}
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("goroutine %d unexpected error: %v", i, err)
		}
	}
	// i1 的三方争用（整批 A、单件、整批 C）恰好一方得手；i2 的两方争用
	// （整批 C、代转）恰好一方得手。整批 C 同时赢得两件时只有一个操作
	// 成功（ok=1），否则各由一方赢得（ok=2）。
	if ok != 1 && ok != 2 {
		t.Fatalf("success = %d, want 1 or 2 (errors %+v)", ok, errs)
	}
	// 整批 C 成功时 i1 与 i2 都在其名下；否则 i2 必由代转转入 alice。
	batchCWon := errs[2] == nil
	h1, _ := r.GetHolding("i1")
	h2, _ := r.GetHolding("i2")
	if batchCWon {
		if ok != 1 || h1.OwnerID != "alice" || h2.OwnerID != "dave" {
			t.Fatalf("batch C won: ok=%d h1=%+v h2=%+v", ok, h1, h2)
		}
	} else {
		if ok != 2 || h2.OwnerID != "alice" {
			t.Fatalf("batch C lost: ok=%d h2=%+v", ok, h2)
		}
	}

	for _, id := range []string{"i1", "i2"} {
		h, err := r.GetHolding(id)
		if err != nil {
			t.Fatal(err)
		}
		if h.Version != 2 {
			t.Fatalf("%s transferred more than once: %+v", id, h)
		}
	}
	// 失败整批中的其他藏品保持原状：整批 A 没得手 i1 时 i3 仍在 bob 手中。
	if h1.OwnerID != "carol" {
		h3, err := r.GetHolding("i3")
		if err != nil {
			t.Fatal(err)
		}
		if h3.OwnerID != "bob" || h3.Version != 1 {
			t.Fatalf("losing batch must leave other items untouched, i3 = %+v", h3)
		}
	}
	// 整批内序号连续且全局无空洞：成功条目数为 2（整批 C 或 单件+代转）
	// 或 3（整批 A + 代转），序号恰为从 6 开始的连续区间。
	seqs := map[int64]bool{}
	for _, id := range []string{"i1", "i2", "i3", "i4"} {
		hist, err := r.History(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range hist {
			if e.Kind == "transfer" {
				seqs[e.Seq] = true
			}
		}
	}
	if len(seqs) != 2 && len(seqs) != 3 {
		t.Fatalf("transfer seqs = %+v, want 2 or 3", seqs)
	}
	for s := int64(6); s < 6+int64(len(seqs)); s++ {
		if !seqs[s] {
			t.Fatalf("seq %d missing, seqs = %+v", s, seqs)
		}
	}
}

// 停用与转让在同一把锁上严格串行，只有两种先后：停用先完成则整批拒绝，
// 转让先完成则保留整批结果（回放不再受停用影响）。两种次序都确定性覆盖。
func TestTransferBatchRaceWithDeactivation(t *testing.T) {
	t.Run("转让先完成", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		setupBatchTransferWorld(t, r)
		req := tbtReq("rb1", tbtEntry("i1", "carol", 1, 1))
		if _, err := r.TransferBatch(req); err != nil {
			t.Fatalf("batch: %v", err)
		}
		// 转让落盘后接收账户才停用：结果保留。
		if err := r.DeactivateAccount("carol"); err != nil {
			t.Fatal(err)
		}
		h, _ := r.GetHolding("i1")
		if h.OwnerID != "carol" || h.Version != 2 {
			t.Fatalf("holding = %+v", h)
		}
		replay, err := r.TransferBatch(req)
		if err != nil || !replay.Replayed || replay.Items[0].ToID != "carol" {
			t.Fatalf("replay after deactivation: %+v %v", replay, err)
		}
	})

	t.Run("停用先完成", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		setupBatchTransferWorld(t, r)
		// 操作者停用先完成：整批只返回账户错误且原子拒绝。
		if err := r.DeactivateAccount("bob"); err != nil {
			t.Fatal(err)
		}
		res, err := r.TransferBatch(tbtReq("rb1", tbtEntry("i1", "carol", 1, 1)))
		if !errors.Is(err, ErrAccountInactive) {
			t.Fatalf("err = %v, want ErrAccountInactive", err)
		}
		if res.ItemID != "" {
			t.Fatalf("ItemID = %q, want empty", res.ItemID)
		}
		h, _ := r.GetHolding("i1")
		if h.OwnerID != "bob" || h.Version != 1 {
			t.Fatalf("holding changed: %+v", h)
		}
	})

	t.Run("接收账户停用先完成", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		setupBatchTransferWorld(t, r)
		if err := r.DeactivateAccount("carol"); err != nil {
			t.Fatal(err)
		}
		res, err := r.TransferBatch(tbtReq("rb1",
			tbtEntry("i1", "dave", 1, 1), tbtEntry("i3", "carol", 1, 1)))
		if !errors.Is(err, ErrAccountInactive) || res.ItemID != "i3" {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		// 整批原子拒绝：第一件 i1 也未转出。
		h, _ := r.GetHolding("i1")
		if h.OwnerID != "bob" || h.Version != 1 {
			t.Fatalf("i1 changed: %+v", h)
		}
	})
}
