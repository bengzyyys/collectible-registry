package registry

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// setupBatchWorld 建立 alice（系列创建者）、bob、carol 三个可用账户与
// 系列 s1。
func setupBatchWorld(t *testing.T, r *Registry) {
	t.Helper()
	for _, id := range []string{"alice", "bob", "carol"} {
		if err := r.RegisterAccount(id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.CreateSeries("s1", "alice", "系列"); err != nil {
		t.Fatal(err)
	}
}

func batchReq(rid string, entries ...IssueBatchEntry) IssueBatchRequest {
	return IssueBatchRequest{
		Operator: "alice", Reason: "整批首发", RequestID: rid,
		SeriesID: "s1", BatchNo: "b1", Entries: entries,
	}
}

func be(item, holder string) IssueBatchEntry {
	return IssueBatchEntry{ItemID: item, Metadata: "元-" + item, HolderID: holder}
}

// ---- 基本成功流程 ----

func TestIssueBatchSuccess(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)

	res, err := r.IssueBatch(batchReq("rb1",
		be("i1", "bob"), be("i2", "carol"), be("i3", "bob")))
	if err != nil {
		t.Fatalf("IssueBatch: %v", err)
	}
	if res.Replayed || res.Err != nil {
		t.Fatalf("unexpected result flags: %+v", res)
	}
	if len(res.Items) != 3 {
		t.Fatalf("want 3 items, got %d", len(res.Items))
	}
	wantOwners := []string{"bob", "carol", "bob"}
	for i, it := range res.Items {
		wantID := fmt.Sprintf("i%d", i+1)
		if it.ItemID != wantID || it.OwnerID != wantOwners[i] || it.Version != 1 {
			t.Errorf("item %d: %+v", i, it)
		}
		if it.TxSeq != int64(i+1) {
			t.Errorf("item %d: TxSeq = %d, want %d", i, it.TxSeq, i+1)
		}
	}
	// 历史序号连续递增。
	for i := 1; i < 3; i++ {
		if res.Items[i].TxSeq != res.Items[i-1].TxSeq+1 {
			t.Fatalf("TxSeq not consecutive: %+v", res.Items)
		}
	}

	// 各件可通过现有查询读取。
	for i, it := range res.Items {
		got, err := r.GetItem(it.ItemID)
		if err != nil {
			t.Fatalf("GetItem %s: %v", it.ItemID, err)
		}
		if got.SeriesID != "s1" || got.BatchNo != "b1" || got.Metadata != "元-"+it.ItemID {
			t.Errorf("GetItem %s: %+v", it.ItemID, got)
		}
		if got.IssuedTxID != it.TxSeq {
			t.Errorf("GetItem %s: IssuedTxID = %d, want %d", it.ItemID, got.IssuedTxID, it.TxSeq)
		}
		h, err := r.GetHolding(it.ItemID)
		if err != nil {
			t.Fatalf("GetHolding %s: %v", it.ItemID, err)
		}
		if h.OwnerID != wantOwners[i] || h.Version != 1 {
			t.Errorf("GetHolding %s: %+v", it.ItemID, h)
		}
		hist, err := r.History(it.ItemID)
		if err != nil {
			t.Fatalf("History %s: %v", it.ItemID, err)
		}
		if len(hist) != 1 || hist[0].Kind != "issue" || hist[0].ToID != wantOwners[i] ||
			hist[0].ToVersion != 1 || hist[0].RequestID != "rb1" || hist[0].Seq != it.TxSeq {
			t.Errorf("History %s: %+v", it.ItemID, hist)
		}
	}

	// 整批发行的藏品随后可按现有规则转让。
	xres, err := r.Transfer(xferReq("bob", "i1", "carol", 1, "rx1"))
	if err != nil {
		t.Fatalf("Transfer after batch: %v", err)
	}
	if xres.TxSeq != 4 {
		t.Fatalf("transfer TxSeq = %d, want 4 (紧随整批之后)", xres.TxSeq)
	}
}

// 批次号继续作为发行标记：不同请求可沿用同一批次号。
func TestIssueBatchSameBatchNoAcrossRequests(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)

	if _, err := r.IssueBatch(batchReq("rb1", be("i1", "bob"))); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	res, err := r.IssueBatch(batchReq("rb2", be("i2", "bob")))
	if err != nil {
		t.Fatalf("second batch with same batch_no: %v", err)
	}
	got, err := r.GetItem(res.Items[0].ItemID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BatchNo != "b1" {
		t.Fatalf("BatchNo = %q", got.BatchNo)
	}
	// 单件发行也可沿用同一批次号。
	if _, err := r.Issue(issueReq("i3", "bob")); err != nil {
		t.Fatalf("single Issue with same batch_no: %v", err)
	}
}

// ---- 参数错误（不占用请求号）----

func TestIssueBatchInvalidArgument(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)

	cases := map[string]IssueBatchRequest{
		"空清单":    batchReq("r1"),
		"编号重复":   batchReq("r2", be("i1", "bob"), be("i1", "carol")),
		"缺操作者":   {Reason: "r", RequestID: "r3", SeriesID: "s1", BatchNo: "b1", Entries: []IssueBatchEntry{be("i1", "bob")}},
		"缺原因":    {Operator: "alice", RequestID: "r4", SeriesID: "s1", BatchNo: "b1", Entries: []IssueBatchEntry{be("i1", "bob")}},
		"缺请求号":   {Operator: "alice", Reason: "r", SeriesID: "s1", BatchNo: "b1", Entries: []IssueBatchEntry{be("i1", "bob")}},
		"缺系列":    {Operator: "alice", Reason: "r", RequestID: "r6", BatchNo: "b1", Entries: []IssueBatchEntry{be("i1", "bob")}},
		"缺批次号":   {Operator: "alice", Reason: "r", RequestID: "r7", SeriesID: "s1", Entries: []IssueBatchEntry{be("i1", "bob")}},
		"条目缺编号":  batchReq("r8", IssueBatchEntry{Metadata: "m", HolderID: "bob"}),
		"条目缺持有人": batchReq("r9", IssueBatchEntry{ItemID: "i1", Metadata: "m"}),
	}
	for name, req := range cases {
		if _, err := r.IssueBatch(req); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}

	// 文字元数据可为空。
	res, err := r.IssueBatch(batchReq("r10", IssueBatchEntry{ItemID: "i1", HolderID: "bob"}))
	if err != nil {
		t.Fatalf("empty metadata should be allowed: %v", err)
	}
	got, _ := r.GetItem(res.Items[0].ItemID)
	if got.Metadata != "" {
		t.Fatalf("Metadata = %q", got.Metadata)
	}

	// 参数错误不占用请求号：修正后可用原号重新提交。
	bad := batchReq("r11", be("i2", "bob"), be("i2", "carol"))
	if _, err := r.IssueBatch(bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("dup: %v", err)
	}
	good := batchReq("r11", be("i2", "bob"), be("i3", "carol"))
	if _, err := r.IssueBatch(good); err != nil {
		t.Fatalf("resubmit with same request id after param fix: %v", err)
	}
}

// ---- 引用不存在（不占用请求号）----

func TestIssueBatchNotFound(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)

	// 操作者未登记。
	req := batchReq("rn1", be("i1", "bob"))
	req.Operator = "nobody"
	if _, err := r.IssueBatch(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("operator not found: %v", err)
	}
	// 系列未登记。
	req = batchReq("rn2", be("i1", "bob"))
	req.SeriesID = "no-such-series"
	if _, err := r.IssueBatch(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("series not found: %v", err)
	}
	// 任一初始持有人未登记，结果应指出该件编号。
	res, err := r.IssueBatch(batchReq("rn3", be("i1", "bob"), be("i2", "ghost")))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("holder not found: %v", err)
	}
	if res.ItemID != "i2" {
		t.Fatalf("result ItemID = %q, want i2", res.ItemID)
	}

	// 引用不存在不占用请求号：补建账户后可用原号重新提交。
	if err := r.RegisterAccount("ghost", ""); err != nil {
		t.Fatal(err)
	}
	res2, err := r.IssueBatch(batchReq("rn3", be("i1", "bob"), be("i2", "ghost")))
	if err != nil {
		t.Fatalf("resubmit after registering holder: %v", err)
	}
	if len(res2.Items) != 2 {
		t.Fatalf("want 2 items, got %+v", res2)
	}
}

// ---- 业务拒绝（沿用单件发行的对应错误，整批原子拒绝）----

func TestIssueBatchBusinessRejections(t *testing.T) {
	newReg := func(t *testing.T) *Registry {
		r := mustCreate(t, tempDir(t))
		setupBatchWorld(t, r)
		return r
	}

	t.Run("非系列创建账户", func(t *testing.T) {
		r := newReg(t)
		req := batchReq("r1", be("i1", "bob"))
		req.Operator = "bob"
		if _, err := r.IssueBatch(req); !errors.Is(err, ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("系列已封存", func(t *testing.T) {
		r := newReg(t)
		if err := r.SealSeries("s1", "alice"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.IssueBatch(batchReq("r1", be("i1", "bob"))); !errors.Is(err, ErrSeriesSealed) {
			t.Fatalf("err = %v, want ErrSeriesSealed", err)
		}
	})

	t.Run("操作者停用", func(t *testing.T) {
		r := newReg(t)
		if err := r.DeactivateAccount("alice"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.IssueBatch(batchReq("r1", be("i1", "bob"))); !errors.Is(err, ErrAccountInactive) {
			t.Fatalf("err = %v, want ErrAccountInactive", err)
		}
	})

	t.Run("初始持有人停用", func(t *testing.T) {
		r := newReg(t)
		if err := r.DeactivateAccount("carol"); err != nil {
			t.Fatal(err)
		}
		res, err := r.IssueBatch(batchReq("r1", be("i1", "bob"), be("i2", "carol")))
		if !errors.Is(err, ErrAccountInactive) {
			t.Fatalf("err = %v, want ErrAccountInactive", err)
		}
		if res.ItemID != "i2" {
			t.Fatalf("ItemID = %q, want i2", res.ItemID)
		}
	})

	t.Run("任一编号已占用", func(t *testing.T) {
		r := newReg(t)
		if _, err := r.Issue(issueReq("i2", "bob")); err != nil {
			t.Fatal(err)
		}
		res, err := r.IssueBatch(batchReq("r1", be("i1", "bob"), be("i2", "carol")))
		if !errors.Is(err, ErrAlreadyExists) {
			t.Fatalf("err = %v, want ErrAlreadyExists", err)
		}
		if res.ItemID != "i2" {
			t.Fatalf("ItemID = %q, want i2", res.ItemID)
		}
		// 整批拒绝不得新增任何藏品、持有或发行历史；未占用编号仍可后续发行。
		if _, err := r.GetItem("i1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("i1 should not exist after rejected batch: %v", err)
		}
		if _, err := r.GetHolding("i1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("i1 holding should not exist: %v", err)
		}
		hist, err := r.History("i2")
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) != 1 {
			t.Fatalf("i2 history = %+v, want only the original issue", hist)
		}
		if _, err := r.IssueBatch(batchReq("r2", be("i1", "bob"), be("i3", "carol"))); err != nil {
			t.Fatalf("previously free ids should be issuable: %v", err)
		}
	})
}

// ---- 幂等回放与请求号冲突 ----

func TestIssueBatchIdempotency(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)

	req := batchReq("rb1", be("i1", "bob"), be("i2", "carol"))
	first, err := r.IssueBatch(req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// 相同内容重提：回放首次结果。
	second, err := r.IssueBatch(req)
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
		if second.Items[i] != first.Items[i] {
			t.Errorf("item %d: replay %+v != first %+v", i, second.Items[i], first.Items[i])
		}
	}
	// 回放不产生额外历史。
	hist, err := r.History("i1")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("history after replay = %+v", hist)
	}

	// 改动任一条目内容、条目顺序、原因、系列、批次号均按请求号冲突拒绝。
	mutations := map[string]IssueBatchRequest{
		"改条目持有人": batchReq("rb1", be("i1", "carol"), be("i2", "carol")),
		"改条目元数据": batchReq("rb1", IssueBatchEntry{ItemID: "i1", Metadata: "x", HolderID: "bob"}, be("i2", "carol")),
		"改条目顺序":  batchReq("rb1", be("i2", "carol"), be("i1", "bob")),
		"改批次号":   {Operator: "alice", Reason: "整批首发", RequestID: "rb1", SeriesID: "s1", BatchNo: "b2", Entries: req.Entries},
		"改原因":    {Operator: "alice", Reason: "换个原因", RequestID: "rb1", SeriesID: "s1", BatchNo: "b1", Entries: req.Entries},
		"改系列":    {Operator: "alice", Reason: "整批首发", RequestID: "rb1", SeriesID: "s2", BatchNo: "b1", Entries: req.Entries},
	}
	for name, m := range mutations {
		if _, err := r.IssueBatch(m); !errors.Is(err, ErrRequestConflict) {
			t.Errorf("%s: err = %v, want ErrRequestConflict", name, err)
		}
	}

	// 与已有操作共用请求号：同一操作者的单件发行请求号不能用于整批。
	if _, err := r.Issue(issueReq("i9", "bob")); err != nil {
		t.Fatal(err)
	}
	cross := batchReq("req-i9", be("i10", "bob"))
	if _, err := r.IssueBatch(cross); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request id shared with single issue: %v", err)
	}
}

// 成功回放仍给出最初的发行结果，即使藏品已转走、系列已封存或账户已停用。
func TestIssueBatchReplayAfterStateChanges(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)

	req := batchReq("rb1", be("i1", "bob"), be("i2", "carol"))
	first, err := r.IssueBatch(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Transfer(xferReq("bob", "i1", "carol", 1, "rx1")); err != nil {
		t.Fatal(err)
	}
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}

	replay, err := r.IssueBatch(req)
	if err != nil {
		t.Fatalf("replay after state changes: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("should be Replayed")
	}
	for i := range first.Items {
		if replay.Items[i] != first.Items[i] {
			t.Errorf("item %d: replay %+v != first %+v", i, replay.Items[i], first.Items[i])
		}
	}
	// 回放不改写当前状态。
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("holding rewritten by replay: %+v", h)
	}
}

// 状态类拒绝也占用请求号并回放。
func TestIssueBatchRejectedReplay(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}

	req := batchReq("rb1", be("i1", "bob"), be("i2", "carol"))
	if _, err := r.IssueBatch(req); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("first: %v", err)
	}
	// 即使 carol 后来恢复可用（重新登记同编号不可能，这里换场景：封存），
	// 相同参数重提仍回放首次拒绝。直接重提验证 Replayed 标记。
	res, err := r.IssueBatch(req)
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("replay: %v", err)
	}
	if !res.Replayed || res.ItemID != "i2" {
		t.Fatalf("replay result = %+v", res)
	}
	// 参数不同则请求号冲突。
	other := batchReq("rb1", be("i1", "bob"))
	if _, err := r.IssueBatch(other); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflict: %v", err)
	}
}

// ---- 版税规则固定 ----

func TestIssueBatchFreezesRoyalty(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)

	// 未发行过的系列可设置版税；整批被拒不影响后续设置。
	if _, err := r.IssueBatch(batchReq("rb0", be("i1", "ghost"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found: %v", err)
	}
	set := SetRoyaltyRequest{
		Operator: "alice", Reason: "设置版税", RequestID: "rr1", SeriesID: "s1",
		Shares: []RoyaltyShare{{AccountID: "bob", Rate: 1000}},
	}
	if _, err := r.SetRoyalty(set); err != nil {
		t.Fatalf("SetRoyalty before any issue: %v", err)
	}

	// 整批成功后规则固定。
	if _, err := r.IssueBatch(batchReq("rb1", be("i1", "bob"))); err != nil {
		t.Fatal(err)
	}
	set.RequestID = "rr2"
	if _, err := r.SetRoyalty(set); !errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("SetRoyalty after batch issue: %v, want ErrRoyaltyFrozen", err)
	}

	// 版税规则对整批发行藏品的后续转让生效。
	xres, err := r.Transfer(TransferRequest{
		Operator: "bob", Reason: "卖出", RequestID: "rx1", ItemID: "i1",
		ExpectedOwner: "bob", ExpectedVer: 1, ToID: "carol", Price: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(xres.Payables) != 1 || xres.Payables[0].AccountID != "bob" ||
		xres.Payables[0].Amount != 1000 || xres.Remainder != 9000 {
		t.Fatalf("royalty on batch-issued item: %+v", xres)
	}
}

// 被拒的整批不固定版税规则。
func TestIssueBatchRejectionDoesNotFreezeRoyalty(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.IssueBatch(batchReq("rb1", be("i1", "bob"))); !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("sealed: %v", err)
	}
	// 系列仍未发行过任何藏品（虽然已封存也无法再设置，换未封存系列验证）。
	if err := r.CreateSeries("s2", "alice", ""); err != nil {
		t.Fatal(err)
	}
	req := batchReq("rb2", be("i2", "ghost"))
	req.SeriesID = "s2"
	if _, err := r.IssueBatch(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("holder not found: %v", err)
	}
	set := SetRoyaltyRequest{
		Operator: "alice", Reason: "设置", RequestID: "rr1", SeriesID: "s2",
		Shares: []RoyaltyShare{{AccountID: "bob", Rate: 500}},
	}
	if _, err := r.SetRoyalty(set); err != nil {
		t.Fatalf("rejected batch must not freeze royalty: %v", err)
	}
}

// ---- 持久化 ----

func TestIssueBatchPersistence(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupBatchWorld(t, r)
	req := batchReq("rb1", be("i1", "bob"), be("i2", "carol"))
	first, err := r.IssueBatch(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// 重开后登记、持有、历史完整保留，原请求重提回放完整结果。
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	replay, err := r2.IssueBatch(req)
	if err != nil {
		t.Fatalf("replay after reopen: %v", err)
	}
	if !replay.Replayed || len(replay.Items) != 2 {
		t.Fatalf("replay = %+v", replay)
	}
	for i := range first.Items {
		if replay.Items[i] != first.Items[i] {
			t.Errorf("item %d: %+v != %+v", i, replay.Items[i], first.Items[i])
		}
	}
	h, err := r2.GetHolding("i2")
	if err != nil || h.OwnerID != "carol" || h.Version != 1 {
		t.Fatalf("holding after reopen: %+v, %v", h, err)
	}
	hist, err := r2.History("i1")
	if err != nil || len(hist) != 1 || hist[0].Seq != first.Items[0].TxSeq {
		t.Fatalf("history after reopen: %+v, %v", hist, err)
	}
}

// ---- 并发 ----

// 并发整批提交同一请求只发行一次。
func TestIssueBatchConcurrentSameRequest(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)
	req := batchReq("rb1", be("i1", "bob"), be("i2", "carol"))

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	results := make([]IssueBatchResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := r.IssueBatch(req)
			results[i], errs[i] = res, err
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if len(results[i].Items) != 2 {
			t.Fatalf("goroutine %d: %+v", i, results[i])
		}
	}
	// 只发行一次：历史各一条。
	for _, id := range []string{"i1", "i2"} {
		hist, err := r.History(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) != 1 {
			t.Fatalf("%s issued %d times", id, len(hist))
		}
	}
}

// 不同请求与单件发行争用同一编号时，最多一个请求占用该编号。
func TestIssueBatchConcurrentIDContention(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, errs[i] = r.IssueBatch(batchReq(fmt.Sprintf("rb-%d", i), be("hot", "bob")))
			} else {
				req := issueReq("hot", "bob")
				req.RequestID = fmt.Sprintf("rs-%d", i)
				_, errs[i] = r.Issue(req)
			}
		}(i)
	}
	wg.Wait()
	succeeded, alreadyExists := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrAlreadyExists):
			alreadyExists++
		default:
			t.Fatalf("goroutine %d: unexpected err %v", i, err)
		}
	}
	if succeeded != 1 || alreadyExists != n-1 {
		t.Fatalf("succeeded = %d, alreadyExists = %d, want 1 and %d", succeeded, alreadyExists, n-1)
	}
	hist, err := r.History("hot")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("hot issued %d times", len(hist))
	}
}

// 整批发行的藏品可创建代转授权并代转。
func TestIssueBatchItemAuthorization(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)
	if _, err := r.IssueBatch(batchReq("rb1", be("i1", "bob"))); err != nil {
		t.Fatal(err)
	}
	expires := r.now().Add(3600e9)
	cres, err := r.CreateAuthorization(CreateAuthorizationRequest{
		Operator: "bob", Reason: "委托", RequestID: "ra1", AuthID: "a1",
		ItemID: "i1", TrusteeID: "carol", ToID: "alice",
		ExpectedOwner: "bob", ExpectedVer: 1, ExpiresAt: expires,
	})
	if err != nil {
		t.Fatalf("CreateAuthorization: %v", err)
	}
	if cres.Status != AuthActive {
		t.Fatalf("status = %q", cres.Status)
	}
	pres, err := r.ProxyTransfer(ProxyTransferRequest{
		Operator: "carol", Reason: "代转", RequestID: "rp1", AuthID: "a1",
	})
	if err != nil {
		t.Fatalf("ProxyTransfer: %v", err)
	}
	if pres.ItemID != "i1" || pres.ToID != "alice" || pres.Version != 2 {
		t.Fatalf("proxy result = %+v", pres)
	}
}
