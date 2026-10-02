package registry

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// setupTransferBatchWorld 建立 alice、bob、carol、dave 四个可用账户与
// 两个系列（s1、s2，创建账户均为 alice）。
func setupTransferBatchWorld(t *testing.T, r *Registry) {
	t.Helper()
	for _, id := range []string{"alice", "bob", "carol", "dave"} {
		if err := r.RegisterAccount(id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.CreateSeries("s1", "alice", "系列1"); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSeries("s2", "alice", "系列2"); err != nil {
		t.Fatal(err)
	}
}

func tbReq(rid string, entries ...TransferBatchEntry) TransferBatchRequest {
	return TransferBatchRequest{
		Operator: "alice", Reason: "整批转让", RequestID: rid, Entries: entries,
	}
}

// tbe 构造一条整批转让条目；price 可选，未填按 0。
func tbe(item, to, owner string, ver int64, price ...int64) TransferBatchEntry {
	e := TransferBatchEntry{
		ItemID: item, ToID: to, ExpectedOwner: owner, ExpectedVer: ver,
	}
	if len(price) > 0 {
		e.Price = price[0]
	}
	return e
}

// issueToAlice 向 alice 发行指定藏品（s1/b1），版本 1。
func issueToAlice(t *testing.T, r *Registry, item, series string) {
	t.Helper()
	req := issueReq(item, "alice")
	req.SeriesID = series
	if _, err := r.Issue(req); err != nil {
		t.Fatalf("issue %s: %v", item, err)
	}
}

// ---- 基本成功流程 ----

func TestTransferBatchSuccess(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupTransferBatchWorld(t, r)
	// 藏品来自不同系列与发行批次，初始持有人均为 alice。
	issueToAlice(t, r, "i1", "s1")
	issueToAlice(t, r, "i2", "s2")
	// 不同批次号：再发行一件 s1/b2 的藏品。
	req := issueReq("i3", "alice")
	req.SeriesID = "s1"
	req.BatchNo = "b2"
	if _, err := r.Issue(req); err != nil {
		t.Fatal(err)
	}

	// 接收账户可以重复：bob 接收两件。
	res, err := r.TransferBatch(tbReq("rb1",
		tbe("i1", "bob", "alice", 1),
		tbe("i2", "carol", "alice", 1),
		tbe("i3", "bob", "alice", 1)))
	if err != nil {
		t.Fatalf("TransferBatch: %v", err)
	}
	if res.Replayed || res.Err != nil {
		t.Fatalf("unexpected result flags: %+v", res)
	}
	if len(res.Items) != 3 {
		t.Fatalf("want 3 items, got %d", len(res.Items))
	}
	want := []struct {
		item, from, to string
		ver, txSeq     int64
	}{
		{"i1", "alice", "bob", 2, 4},
		{"i2", "alice", "carol", 2, 5},
		{"i3", "alice", "bob", 2, 6},
	}
	for i, w := range want {
		it := res.Items[i]
		if it.ItemID != w.item || it.FromID != w.from || it.ToID != w.to ||
			it.Version != w.ver || it.TxSeq != w.txSeq {
			t.Errorf("item %d: %+v, want %+v", i, it, w)
		}
		if it.Price != 0 || len(it.Payables) != 0 || it.Remainder != 0 {
			t.Errorf("item %d: unexpected money fields: %+v", i, it)
		}
	}

	// 各件持有版本分别加一，历史序号连续递增。
	for i := 1; i < 3; i++ {
		if res.Items[i].TxSeq != res.Items[i-1].TxSeq+1 {
			t.Fatalf("TxSeq not consecutive: %+v", res.Items)
		}
	}

	// 各件可通过现有持有、历史查询核对；历史保留整批共用的操作者、原因、请求号。
	for _, w := range want {
		h, err := r.GetHolding(w.item)
		if err != nil {
			t.Fatal(err)
		}
		if h.OwnerID != w.to || h.Version != 2 {
			t.Errorf("GetHolding %s: %+v", w.item, h)
		}
		hist, err := r.History(w.item)
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) != 2 {
			t.Fatalf("%s history = %+v, want issue + transfer", w.item, hist)
		}
		tr := hist[1]
		if tr.Kind != "transfer" || tr.Operator != "alice" || tr.Reason != "整批转让" ||
			tr.RequestID != "rb1" || tr.FromID != "alice" || tr.ToID != w.to ||
			tr.FromVersion != 1 || tr.ToVersion != 2 || tr.Seq != w.txSeq {
			t.Errorf("history entry wrong: %+v", tr)
		}
	}
}

// 整批转让不要求系列未封存：系列封存不阻止转让。
func TestTransferBatchSealedSeries(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupTransferBatchWorld(t, r)
	issueToAlice(t, r, "i1", "s1")
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	res, err := r.TransferBatch(tbReq("rb1", tbe("i1", "bob", "alice", 1)))
	if err != nil {
		t.Fatalf("transfer after seal must succeed: %v", err)
	}
	if res.Items[0].ToID != "bob" || res.Items[0].Version != 2 {
		t.Fatalf("result = %+v", res.Items[0])
	}
}

// ---- 版税：各件按所属系列规则分别计算 ----

func TestTransferBatchRoyaltyPerItem(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupTransferBatchWorld(t, r)
	// s1 规则：bob 10%、carol 5%；s2 规则：dave 20%。
	for _, sr := range []struct {
		series string
		shares []RoyaltyShare
	}{
		{"s1", []RoyaltyShare{{AccountID: "bob", Rate: 1000}, {AccountID: "carol", Rate: 500}}},
		{"s2", []RoyaltyShare{{AccountID: "dave", Rate: 2000}}},
	} {
		if _, err := r.SetRoyalty(SetRoyaltyRequest{
			Operator: "alice", Reason: "设置版税", RequestID: "rr-" + sr.series,
			SeriesID: sr.series, Shares: sr.shares,
		}); err != nil {
			t.Fatal(err)
		}
	}
	issueToAlice(t, r, "i1", "s1")
	issueToAlice(t, r, "i2", "s1")
	issueToAlice(t, r, "i3", "s2")

	// 注意：接收人 bob 同时是 s1 的版税收款人——收款人可以同时是转让参与者。
	res, err := r.TransferBatch(tbReq("rb1",
		tbe("i1", "bob", "alice", 1, 10000),
		tbe("i2", "carol", "alice", 1, 20000),
		tbe("i3", "dave", "alice", 1, 5000)))
	if err != nil {
		t.Fatal(err)
	}
	wantPayables := [][]RoyaltyPayable{
		{{AccountID: "bob", Rate: 1000, Amount: 1000}, {AccountID: "carol", Rate: 500, Amount: 500}},
		{{AccountID: "bob", Rate: 1000, Amount: 2000}, {AccountID: "carol", Rate: 500, Amount: 1000}},
		{{AccountID: "dave", Rate: 2000, Amount: 1000}},
	}
	wantRemainders := []int64{8500, 17000, 4000}
	for i, it := range res.Items {
		if it.Price != []int64{10000, 20000, 5000}[i] {
			t.Errorf("item %d: Price = %d", i, it.Price)
		}
		if len(it.Payables) != len(wantPayables[i]) {
			t.Fatalf("item %d: Payables = %+v, want %+v", i, it.Payables, wantPayables[i])
		}
		for j, p := range it.Payables {
			if p != wantPayables[i][j] {
				t.Errorf("item %d payable %d: %+v, want %+v", i, j, p, wantPayables[i][j])
			}
		}
		if it.Remainder != wantRemainders[i] {
			t.Errorf("item %d: Remainder = %d, want %d", i, it.Remainder, wantRemainders[i])
		}
	}

	// 同一收款账户在多件中出现也分别保留明细，不合并价款计算：
	// bob 在 i1 中应付 1000、在 i2 中应付 2000，而不是合并 30000 价款后算 3000。
	bobEntries, err := r.PayablesOf("bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(bobEntries) != 2 {
		t.Fatalf("bob payables = %+v, want 2 separate entries", bobEntries)
	}
	if bobEntries[0].Amount != 1000 || bobEntries[1].Amount != 2000 {
		t.Fatalf("bob payables merged? %+v", bobEntries)
	}

	// 各笔应付可按转让序号查询核对。
	for _, it := range res.Items {
		tr, err := r.TransferRoyalty(it.TxSeq)
		if err != nil {
			t.Fatal(err)
		}
		if tr.ItemID != it.ItemID || tr.Price != it.Price || tr.Remainder != it.Remainder {
			t.Errorf("TransferRoyalty %d: %+v", it.TxSeq, tr)
		}
	}
}

// 零金额明细也保留；余款归转让前持有人。
func TestTransferBatchZeroAmountPayables(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupTransferBatchWorld(t, r)
	if _, err := r.SetRoyalty(SetRoyaltyRequest{
		Operator: "alice", Reason: "设置版税", RequestID: "rr1", SeriesID: "s1",
		Shares: []RoyaltyShare{{AccountID: "bob", Rate: 1000}},
	}); err != nil {
		t.Fatal(err)
	}
	issueToAlice(t, r, "i1", "s1")
	// 价款为 0（未填）：应付明细零金额也保留，余款为 0。
	res, err := r.TransferBatch(tbReq("rb1", tbe("i1", "carol", "alice", 1)))
	if err != nil {
		t.Fatal(err)
	}
	it := res.Items[0]
	if it.Price != 0 || len(it.Payables) != 1 || it.Payables[0].Amount != 0 || it.Remainder != 0 {
		t.Fatalf("zero-price result should keep zero payable: %+v", it)
	}
}

// ---- 参数错误（不占用请求号）----

func TestTransferBatchInvalidArgument(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupTransferBatchWorld(t, r)
	issueToAlice(t, r, "i1", "s1")
	issueToAlice(t, r, "i2", "s1")

	cases := map[string]TransferBatchRequest{
		"空清单":      tbReq("r1"),
		"编号重复":     tbReq("r2", tbe("i1", "bob", "alice", 1), tbe("i1", "carol", "alice", 1)),
		"缺操作者":     {Reason: "r", RequestID: "r3", Entries: []TransferBatchEntry{tbe("i1", "bob", "alice", 1)}},
		"缺原因":      {Operator: "alice", RequestID: "r4", Entries: []TransferBatchEntry{tbe("i1", "bob", "alice", 1)}},
		"缺请求号":     {Operator: "alice", Reason: "r", Entries: []TransferBatchEntry{tbe("i1", "bob", "alice", 1)}},
		"条目缺编号":    tbReq("r5", TransferBatchEntry{ToID: "bob", ExpectedOwner: "alice", ExpectedVer: 1}),
		"条目缺接收":    tbReq("r6", TransferBatchEntry{ItemID: "i1", ExpectedOwner: "alice", ExpectedVer: 1}),
		"条目缺期望持有人": tbReq("r7", TransferBatchEntry{ItemID: "i1", ToID: "bob", ExpectedVer: 1}),
		"期望版本为零":   tbReq("r8", tbe("i1", "bob", "alice", 0)),
		"期望版本为负":   tbReq("r9", tbe("i1", "bob", "alice", -1)),
		"价款为负":     tbReq("r10", tbe("i1", "bob", "alice", 1, -5)),
	}
	for name, req := range cases {
		if _, err := r.TransferBatch(req); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}

	// 参数错误不占用请求号：修正后可用原号重新提交。
	bad := tbReq("r11", tbe("i1", "bob", "alice", 1), tbe("i1", "carol", "alice", 1))
	if _, err := r.TransferBatch(bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("dup: %v", err)
	}
	good := tbReq("r11", tbe("i1", "bob", "alice", 1), tbe("i2", "carol", "alice", 1))
	if _, err := r.TransferBatch(good); err != nil {
		t.Fatalf("resubmit with same request id after param fix: %v", err)
	}
}

// ---- 引用不存在（不占用请求号）----

func TestTransferBatchNotFound(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupTransferBatchWorld(t, r)
	issueToAlice(t, r, "i1", "s1")

	// 操作者未登记：只返回账户错误，与具体藏品无关。
	req := tbReq("rn1", tbe("i1", "bob", "alice", 1))
	req.Operator = "nobody"
	res, err := r.TransferBatch(req)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("operator not found: %v", err)
	}
	if res.ItemID != "" {
		t.Fatalf("operator-level error should have empty ItemID, got %q", res.ItemID)
	}

	// 藏品不存在：返回清单顺序最前的失败藏品编号。
	res, err = r.TransferBatch(tbReq("rn2",
		tbe("nope", "bob", "alice", 1),
		tbe("i1", "carol", "alice", 1)))
	if !errors.Is(err, ErrNotFound) || res.ItemID != "nope" {
		t.Fatalf("item not found: res=%+v err=%v", res, err)
	}

	// 接收账户未登记。
	res, err = r.TransferBatch(tbReq("rn3", tbe("i1", "ghost", "alice", 1)))
	if !errors.Is(err, ErrNotFound) || res.ItemID != "i1" {
		t.Fatalf("receiver not found: res=%+v err=%v", res, err)
	}

	// 引用不存在不占用请求号：补建账户后可用原号重新提交。
	if err := r.RegisterAccount("ghost", ""); err != nil {
		t.Fatal(err)
	}
	res2, err := r.TransferBatch(tbReq("rn3", tbe("i1", "ghost", "alice", 1)))
	if err != nil {
		t.Fatalf("resubmit after registering receiver: %v", err)
	}
	if len(res2.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", res2)
	}
}

// ---- 业务拒绝（整批原子拒绝，返回清单顺序最前的失败藏品）----

func TestTransferBatchBusinessRejections(t *testing.T) {
	newReg := func(t *testing.T) *Registry {
		r := mustCreate(t, tempDir(t))
		setupTransferBatchWorld(t, r)
		issueToAlice(t, r, "i1", "s1")
		issueToAlice(t, r, "i2", "s1")
		return r
	}

	t.Run("操作者停用", func(t *testing.T) {
		r := newReg(t)
		if err := r.DeactivateAccount("alice"); err != nil {
			t.Fatal(err)
		}
		res, err := r.TransferBatch(tbReq("r1", tbe("i1", "bob", "alice", 1)))
		if !errors.Is(err, ErrAccountInactive) {
			t.Fatalf("err = %v, want ErrAccountInactive", err)
		}
		if res.ItemID != "" {
			t.Fatalf("operator-level deactivation should have empty ItemID, got %q", res.ItemID)
		}
	})

	t.Run("接收账户停用", func(t *testing.T) {
		r := newReg(t)
		if err := r.DeactivateAccount("carol"); err != nil {
			t.Fatal(err)
		}
		res, err := r.TransferBatch(tbReq("r1",
			tbe("i1", "bob", "alice", 1),
			tbe("i2", "carol", "alice", 1)))
		if !errors.Is(err, ErrAccountInactive) {
			t.Fatalf("err = %v, want ErrAccountInactive", err)
		}
		if res.ItemID != "i2" {
			t.Fatalf("ItemID = %q, want i2", res.ItemID)
		}
	})

	t.Run("收发同人", func(t *testing.T) {
		r := newReg(t)
		res, err := r.TransferBatch(tbReq("r1",
			tbe("i1", "bob", "alice", 1),
			tbe("i2", "alice", "alice", 1)))
		if !errors.Is(err, ErrSameAccount) {
			t.Fatalf("err = %v, want ErrSameAccount", err)
		}
		if res.ItemID != "i2" {
			t.Fatalf("ItemID = %q, want i2", res.ItemID)
		}
	})

	t.Run("期望版本不符", func(t *testing.T) {
		r := newReg(t)
		res, err := r.TransferBatch(tbReq("r1",
			tbe("i1", "bob", "alice", 1),
			tbe("i2", "carol", "alice", 9)))
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if res.ItemID != "i2" {
			t.Fatalf("ItemID = %q, want i2", res.ItemID)
		}
	})

	t.Run("操作者非当前持有人", func(t *testing.T) {
		r := newReg(t)
		// i1 的持有人是 alice，但操作者填 bob（bob 发起整批转让）。
		req := tbReq("r1", tbe("i1", "carol", "alice", 1))
		req.Operator = "bob"
		res, err := r.TransferBatch(req)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if res.ItemID != "i1" {
			t.Fatalf("ItemID = %q, want i1", res.ItemID)
		}
	})

	t.Run("期望持有人填错", func(t *testing.T) {
		r := newReg(t)
		res, err := r.TransferBatch(tbReq("r1", tbe("i1", "bob", "bob", 1)))
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if res.ItemID != "i1" {
			t.Fatalf("ItemID = %q, want i1", res.ItemID)
		}
	})

	// 整批拒绝是原子的：不改变任何一件的持有、版本、历史或应付。
	t.Run("拒绝后状态不变", func(t *testing.T) {
		r := newReg(t)
		if _, err := r.TransferBatch(tbReq("r1",
			tbe("i1", "bob", "alice", 1),
			tbe("i2", "carol", "alice", 9))); !errors.Is(err, ErrConflict) {
			t.Fatalf("want conflict: %v", err)
		}
		for _, item := range []string{"i1", "i2"} {
			h, _ := r.GetHolding(item)
			if h.OwnerID != "alice" || h.Version != 1 {
				t.Errorf("%s holding changed after rejection: %+v", item, h)
			}
			hist, _ := r.History(item)
			if len(hist) != 1 {
				t.Errorf("%s history = %+v, want only issue", item, hist)
			}
		}
	})
}

// ---- 幂等回放与请求号冲突 ----

func TestTransferBatchIdempotency(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupTransferBatchWorld(t, r)
	issueToAlice(t, r, "i1", "s1")
	issueToAlice(t, r, "i2", "s1")

	req := tbReq("rb1", tbe("i1", "bob", "alice", 1), tbe("i2", "carol", "alice", 1))
	first, err := r.TransferBatch(req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// 相同内容重提：回放首次结果，不产生额外历史。
	second, err := r.TransferBatch(req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed {
		t.Fatal("replay should be marked Replayed")
	}
	if len(second.Items) != len(first.Items) {
		t.Fatalf("replay items = %+v", second.Items)
	}
	for i := range first.Items {
		if !reflect.DeepEqual(second.Items[i], first.Items[i]) {
			t.Errorf("item %d: replay %+v != first %+v", i, second.Items[i], first.Items[i])
		}
	}
	for _, item := range []string{"i1", "i2"} {
		hist, _ := r.History(item)
		if len(hist) != 2 {
			t.Fatalf("%s history after replay = %+v", item, hist)
		}
	}

	// 改动原因、条目顺序或任一条目参数均按请求号冲突拒绝。
	reasonChanged := tbReq("rb1", tbe("i1", "bob", "alice", 1), tbe("i2", "carol", "alice", 1))
	reasonChanged.Reason = "换个原因"
	mutations := map[string]TransferBatchRequest{
		"改原因":   reasonChanged,
		"改条目顺序": tbReq("rb1", tbe("i2", "carol", "alice", 1), tbe("i1", "bob", "alice", 1)),
		"改接收人":  tbReq("rb1", tbe("i1", "carol", "alice", 1), tbe("i2", "carol", "alice", 1)),
		"改期望版本": tbReq("rb1", tbe("i1", "bob", "alice", 2), tbe("i2", "carol", "alice", 1)),
		"改价款":   tbReq("rb1", tbe("i1", "bob", "alice", 1, 100), tbe("i2", "carol", "alice", 1)),
		"增减条目":  tbReq("rb1", tbe("i1", "bob", "alice", 1)),
	}
	for name, m := range mutations {
		if _, err := r.TransferBatch(m); !errors.Is(err, ErrRequestConflict) {
			t.Errorf("%s: err = %v, want ErrRequestConflict", name, err)
		}
	}

	// 与已有操作共用请求号：同一操作者的单件转让请求号不能用于整批。
	issueToAlice(t, r, "i3", "s1")
	single := xferReq("alice", "i3", "bob", 1, "rx1")
	if _, err := r.Transfer(single); err != nil {
		t.Fatal(err)
	}
	cross := tbReq("rx1", tbe("i2", "carol", "alice", 1))
	if _, err := r.TransferBatch(cross); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request id shared with single transfer: %v", err)
	}
}

// 成功回放仍给出最初的整批结果，即使藏品已再次易手或账户已停用。
func TestTransferBatchReplayAfterStateChanges(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupTransferBatchWorld(t, r)
	issueToAlice(t, r, "i1", "s1")
	issueToAlice(t, r, "i2", "s1")

	req := tbReq("rb1", tbe("i1", "bob", "alice", 1), tbe("i2", "carol", "alice", 1))
	first, err := r.TransferBatch(req)
	if err != nil {
		t.Fatal(err)
	}
	// 藏品再次易手，账户停用。
	if _, err := r.Transfer(xferReq("bob", "i1", "carol", 2, "rx1")); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}

	replay, err := r.TransferBatch(req)
	if err != nil {
		t.Fatalf("replay after state changes: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("should be Replayed")
	}
	for i := range first.Items {
		if !reflect.DeepEqual(replay.Items[i], first.Items[i]) {
			t.Errorf("item %d: replay %+v != first %+v", i, replay.Items[i], first.Items[i])
		}
	}
	// 回放不改写当前状态。
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 3 {
		t.Fatalf("holding rewritten by replay: %+v", h)
	}
}

// 状态类拒绝也占用请求号并回放。
func TestTransferBatchRejectedReplay(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupTransferBatchWorld(t, r)
	issueToAlice(t, r, "i1", "s1")
	issueToAlice(t, r, "i2", "s1")
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}

	req := tbReq("rb1", tbe("i1", "bob", "alice", 1), tbe("i2", "carol", "alice", 1))
	if _, err := r.TransferBatch(req); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("first: %v", err)
	}
	// 相同参数重放仍返回首次拒绝并标明重复。
	res, err := r.TransferBatch(req)
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("replay: %v", err)
	}
	if !res.Replayed || res.ItemID != "i2" {
		t.Fatalf("replay result = %+v", res)
	}
	// 参数不同则请求号冲突。
	other := tbReq("rb1", tbe("i1", "bob", "alice", 1))
	if _, err := r.TransferBatch(other); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflict: %v", err)
	}
}

// ---- 持久化 ----

func TestTransferBatchPersistence(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupTransferBatchWorld(t, r)
	issueToAlice(t, r, "i1", "s1")
	issueToAlice(t, r, "i2", "s1")
	req := tbReq("rb1", tbe("i1", "bob", "alice", 1, 1000), tbe("i2", "carol", "alice", 1))
	first, err := r.TransferBatch(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// 重开后持有、历史、应付完整保留，原请求重提回放完整整批结果。
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
	h, err := r2.GetHolding("i2")
	if err != nil || h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("holding after reopen: %+v, %v", h, err)
	}
	hist, err := r2.History("i1")
	if err != nil || len(hist) != 2 || hist[1].RequestID != "rb1" {
		t.Fatalf("history after reopen: %+v, %v", hist, err)
	}
}

// 保存失败不能留下部分转让：这里通过重开后重试验证整批结果完整。
func TestTransferBatchRetryAfterUncertainOutcome(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupTransferBatchWorld(t, r)
	issueToAlice(t, r, "i1", "s1")
	issueToAlice(t, r, "i2", "s1")
	req := tbReq("rb1", tbe("i1", "bob", "alice", 1), tbe("i2", "carol", "alice", 1))
	if _, err := r.TransferBatch(req); err != nil {
		t.Fatal(err)
	}
	// 模拟进程被直接终止。
	_ = r.store.lock.Close()
	r.closed = true

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	out, err := r2.TransferBatch(req)
	if err != nil || !out.Replayed || len(out.Items) != 2 {
		t.Fatalf("retry must return saved batch result: %+v %v", out, err)
	}
	for _, item := range []string{"i1", "i2"} {
		hist, _ := r2.History(item)
		if len(hist) != 2 {
			t.Fatalf("%s history = %+v, want issue + one transfer", item, hist)
		}
	}
}

// ---- 并发 ----

// 并发整批提交同一请求只完成一次。
func TestTransferBatchConcurrentSameRequest(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupTransferBatchWorld(t, r)
	issueToAlice(t, r, "i1", "s1")
	issueToAlice(t, r, "i2", "s1")
	req := tbReq("rb1", tbe("i1", "bob", "alice", 1), tbe("i2", "carol", "alice", 1))

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	results := make([]TransferBatchResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := r.TransferBatch(req)
			results[i], errs[i] = res, err
		}(i)
	}
	wg.Wait()
	replayed := 0
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if results[i].Replayed {
			replayed++
		}
		if len(results[i].Items) != 2 {
			t.Fatalf("goroutine %d: %+v", i, results[i])
		}
	}
	if replayed != n-1 {
		t.Fatalf("want %d replays, got %d", n-1, replayed)
	}
	// 每件只转让一次。
	for _, item := range []string{"i1", "i2"} {
		hist, _ := r.History(item)
		if len(hist) != 2 {
			t.Fatalf("%s history = %+v, want issue + one transfer", item, hist)
		}
	}
}

// 整批与单件转让争用同一藏品版本：最多一方成功，失败整批中的其他藏品
// 也保持原状。
func TestTransferBatchConcurrentWithSingleTransfer(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupTransferBatchWorld(t, r)
	issueToAlice(t, r, "i1", "s1")
	issueToAlice(t, r, "i2", "s1")

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				// 整批转让 i1、i2。
				_, errs[i] = r.TransferBatch(tbReq(fmt.Sprintf("rb-%d", i),
					tbe("i1", "bob", "alice", 1), tbe("i2", "carol", "alice", 1)))
			} else {
				// 单件转让 i1。
				_, errs[i] = r.Transfer(xferReq("alice", "i1", "bob", 1, fmt.Sprintf("rx-%d", i)))
			}
		}(i)
	}
	wg.Wait()
	succ, conflicts := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			succ++
		case errors.Is(err, ErrConflict), errors.Is(err, ErrSameAccount):
			// 整批落败时，其条目可能因版本不符（ErrConflict）或接收人已
			// 是当前持有人（ErrSameAccount）被拒；单件落败时为版本不符。
			conflicts++
		default:
			t.Fatalf("goroutine %d: unexpected err %v", i, err)
		}
	}
	// 成功总数恰为 1：要么一个整批（i1、i2 同时转让），要么一个单件。
	if succ != 1 {
		t.Fatalf("succeeded = %d, want 1", succ)
	}
	if conflicts != n-1 {
		t.Fatalf("conflicts = %d, want %d", conflicts, n-1)
	}
	// 若整批成功：i1、i2 都转让；若单件成功：只有 i1 转让，i2 保持原状。
	h1, _ := r.GetHolding("i1")
	h2, _ := r.GetHolding("i2")
	if h1.Version != 2 {
		t.Fatalf("i1 version = %d, want 2", h1.Version)
	}
	if h2.Version != 1 && h2.Version != 2 {
		t.Fatalf("i2 version = %d, want 1 (batch lost) or 2 (batch won)", h2.Version)
	}
	if h2.Version == 1 && h2.OwnerID != "alice" {
		t.Fatalf("i2 owner = %s, want alice when batch lost", h2.OwnerID)
	}
}

// 操作者或接收账户停用先完成时整批拒绝；转让先完成时保留整批结果。
func TestTransferBatchConcurrentDeactivate(t *testing.T) {
	for range 20 {
		r := mustCreate(t, filepath.Join(t.TempDir(), "d"))
		setupTransferBatchWorld(t, r)
		issueToAlice(t, r, "i1", "s1")
		var wg sync.WaitGroup
		var batchErr, deactErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, batchErr = r.TransferBatch(tbReq("rb1", tbe("i1", "bob", "alice", 1)))
		}()
		go func() {
			defer wg.Done()
			deactErr = r.DeactivateAccount("alice")
		}()
		wg.Wait()
		if deactErr != nil {
			t.Fatalf("deactivate should always succeed: %v", deactErr)
		}
		h, _ := r.GetHolding("i1")
		hist, _ := r.History("i1")
		if batchErr == nil {
			if h.OwnerID != "bob" || h.Version != 2 || len(hist) != 2 {
				t.Fatalf("transfer-first state inconsistent: %+v hist=%d", h, len(hist))
			}
		} else {
			if !errors.Is(batchErr, ErrAccountInactive) {
				t.Fatalf("batch either succeeds or is blocked, got %v", batchErr)
			}
			if h.OwnerID != "alice" || h.Version != 1 || len(hist) != 1 {
				t.Fatalf("deactivate-first state inconsistent: %+v hist=%d", h, len(hist))
			}
		}
	}
}

// ---- 与代转授权的关系 ----

// 整批转让不把已有代转授权记为已使用；但授权绑定创建时的持有版本，藏品
// 转出后旧授权永久失效，即使藏品转回也不恢复。
func TestTransferBatchDoesNotConsumeAuthorization(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupTransferBatchWorld(t, r)
	issueToAlice(t, r, "i1", "s1")
	expires := r.now().Add(3600e9)
	if _, err := r.CreateAuthorization(CreateAuthorizationRequest{
		Operator: "alice", Reason: "委托", RequestID: "ra1", AuthID: "a1",
		ItemID: "i1", TrusteeID: "bob", ToID: "carol",
		ExpectedOwner: "alice", ExpectedVer: 1, ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}

	// 整批转让 i1 给 dave：授权不被记为已使用。
	if _, err := r.TransferBatch(tbReq("rb1", tbe("i1", "dave", "alice", 1))); err != nil {
		t.Fatal(err)
	}
	a, err := r.GetAuthorization("a1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != AuthActive {
		t.Fatalf("authorization should remain active after batch transfer, got %q", a.Status)
	}

	// 旧授权绑定版本 1，藏品已到版本 2：代转被拒（版本冲突）。
	if _, err := r.ProxyTransfer(ProxyTransferRequest{
		Operator: "bob", Reason: "代转", RequestID: "rp1", AuthID: "a1",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("proxy after transfer should conflict, got %v", err)
	}

	// 藏品转回 alice 后，旧授权仍不恢复（版本已不是 1）。
	if _, err := r.Transfer(xferReq("dave", "i1", "alice", 2, "rx1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(ProxyTransferRequest{
		Operator: "bob", Reason: "代转", RequestID: "rp2", AuthID: "a1",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("proxy after item returns should still conflict, got %v", err)
	}
}
