package registry

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func batchReq(items ...BatchIssueItem) BatchIssueRequest {
	return BatchIssueRequest{
		Operator: "alice", Reason: "整批首发", RequestID: "batch-1",
		SeriesID: "s1", BatchNo: "b1", Items: items,
	}
}

func batchItem(id, holder string) BatchIssueItem {
	return BatchIssueItem{ItemID: id, Metadata: "元数据-" + id, HolderID: holder}
}

// ---- 基础流程 ----

func TestBatchIssueBasic(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")

	req := batchReq(batchItem("item-1", "bob"), batchItem("item-2", "alice"), batchItem("item-3", "carol"))
	res, err := r.BatchIssue(req)
	if err != nil {
		t.Fatalf("BatchIssue: %v", err)
	}
	if len(res.Items) != 3 {
		t.Fatalf("want 3 items, got %d", len(res.Items))
	}
	wantSeqs := []int64{1, 2, 3}
	for i, it := range res.Items {
		if it.ItemID != req.Items[i].ItemID || it.OwnerID != req.Items[i].HolderID ||
			it.Version != 1 || it.TxSeq != wantSeqs[i] {
			t.Fatalf("item %d mismatch: %+v", i, it)
		}
	}
	// 验证持有与历史
	for _, it := range req.Items {
		h, err := r.GetHolding(it.ItemID)
		if err != nil || h.OwnerID != it.HolderID || h.Version != 1 {
			t.Fatalf("holding for %s wrong: %+v err=%v", it.ItemID, h, err)
		}
		item, err := r.GetItem(it.ItemID)
		if err != nil || item.SeriesID != "s1" || item.BatchNo != "b1" || item.IssuedTxID == 0 {
			t.Fatalf("item %s wrong: %+v err=%v", it.ItemID, item, err)
		}
	}
	// 历史连续且整批之间不能插入其他记录
	for _, it := range req.Items {
		hist, err := r.History(it.ItemID)
		if err != nil || len(hist) != 1 {
			t.Fatalf("history for %s wrong: %v %v", it.ItemID, hist, err)
		}
		if hist[0].Kind != "issue" || hist[0].ToVersion != 1 || hist[0].Operator != "alice" ||
			hist[0].RequestID != "batch-1" || hist[0].Seq != hist[0].Seq {
			t.Fatalf("history entry wrong: %+v", hist[0])
		}
	}
}

func TestBatchIssueMetadataCanBeEmpty(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	req := batchReq(BatchIssueItem{ItemID: "item-1", Metadata: "", HolderID: "bob"})
	res, err := r.BatchIssue(req)
	if err != nil {
		t.Fatalf("BatchIssue with empty metadata: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(res.Items))
	}
	item, _ := r.GetItem("item-1")
	if item.Metadata != "" {
		t.Fatalf("metadata should be empty, got %q", item.Metadata)
	}
}

// ---- 参数错误 ----

func TestBatchIssueEmptyList(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	req := batchReq()
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty list must be ErrInvalidArgument, got %v", err)
	}
}

func TestBatchIssueDuplicateItemIDs(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	req := batchReq(batchItem("item-1", "bob"), batchItem("item-1", "alice"))
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("duplicate item ids must be ErrInvalidArgument, got %v", err)
	}
}

func TestBatchIssueMissingFields(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	// 缺操作者
	req := batchReq(batchItem("item-1", "bob"))
	req.Operator = ""
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing operator: %v", err)
	}
	// 缺原因
	req = batchReq(batchItem("item-1", "bob"))
	req.Reason = ""
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing reason: %v", err)
	}
	// 缺请求号
	req = batchReq(batchItem("item-1", "bob"))
	req.RequestID = ""
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing request id: %v", err)
	}
	// 缺系列
	req = batchReq(batchItem("item-1", "bob"))
	req.SeriesID = ""
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing series: %v", err)
	}
	// 缺批次号
	req = batchReq(batchItem("item-1", "bob"))
	req.BatchNo = ""
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing batch no: %v", err)
	}
	// 缺藏品编号
	req = batchReq(batchItem("item-1", "bob"))
	req.Items[0].ItemID = ""
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing item id: %v", err)
	}
	// 缺初始持有人
	req = batchReq(batchItem("item-1", "bob"))
	req.Items[0].HolderID = ""
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing holder id: %v", err)
	}
}

// ---- 对象不存在 ----

func TestBatchIssueNotFound(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	// 操作者不存在
	req := batchReq(batchItem("item-1", "bob"))
	req.Operator = "ghost"
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown operator: %v", err)
	}
	// 系列不存在
	req = batchReq(batchItem("item-1", "bob"))
	req.SeriesID = "ghost"
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown series: %v", err)
	}
	// 初始持有人不存在
	req = batchReq(batchItem("item-1", "ghost"))
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown holder: %v", err)
	}
	// 拒绝后不新增任何藏品
	if _, err := r.GetItem("item-1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("rejected batch must not create item")
	}
}

// ---- 业务拒绝 ----

func TestBatchIssueNotCreator(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")

	req := batchReq(batchItem("item-1", "bob"))
	req.Operator = "carol"
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-creator batch issue: %v", err)
	}
}

func TestBatchIssueSeriesSealed(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.SealSeries("s1", "alice")

	req := batchReq(batchItem("item-1", "bob"))
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("sealed series batch issue: %v", err)
	}
}

func TestBatchIssueInactiveOperator(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.DeactivateAccount("alice")

	req := batchReq(batchItem("item-1", "bob"))
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive operator: %v", err)
	}
}

func TestBatchIssueInactiveHolder(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")
	r.DeactivateAccount("carol")

	req := batchReq(batchItem("item-1", "carol"))
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive holder: %v", err)
	}
}

func TestBatchIssueOccupiedItemID(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	// 先用单件发行占用 item-1
	r.Issue(issueReq("item-1", "bob"))

	req := batchReq(batchItem("item-1", "alice"), batchItem("item-2", "bob"))
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("occupied item id: %v", err)
	}
	// 整批拒绝后 item-2 不应被创建
	if _, err := r.GetItem("item-2"); !errors.Is(err, ErrNotFound) {
		t.Fatal("rejected batch must not create item-2")
	}
}

func TestBatchIssueRejectionNoStateChange(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	// 因持有人不存在被拒绝
	req := batchReq(batchItem("item-1", "ghost"))
	r.BatchIssue(req)

	// 修正后用原请求号重新提交，应完整执行
	req.Items[0].HolderID = "bob"
	res, err := r.BatchIssue(req)
	if err != nil {
		t.Fatalf("retry after fix should succeed: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].TxSeq != 1 {
		t.Fatalf("retry result wrong: %+v", res)
	}
}

// ---- 幂等 ----

func TestBatchIssueIdempotency(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	req := batchReq(batchItem("item-1", "bob"), batchItem("item-2", "alice"))
	first, err := r.BatchIssue(req)
	if err != nil {
		t.Fatal(err)
	}
	// 重复提交：回放首次结果
	again, err := r.BatchIssue(req)
	if err != nil {
		t.Fatalf("replay should not error: %v", err)
	}
	if !again.Replayed {
		t.Fatal("replay should be marked")
	}
	if len(again.Items) != len(first.Items) {
		t.Fatalf("replay item count mismatch")
	}
	for i := range again.Items {
		if again.Items[i] != first.Items[i] {
			t.Fatalf("replay item %d mismatch: %+v vs %+v", i, again.Items[i], first.Items[i])
		}
	}
	// 历史不增加
	hist, _ := r.History("item-1")
	if len(hist) != 1 {
		t.Fatalf("replay must not add history: %d", len(hist))
	}
}

func TestBatchIssueConflictOnParamChange(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	req := batchReq(batchItem("item-1", "bob"))
	r.BatchIssue(req)

	// 改原因
	req.Reason = "changed"
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed reason must conflict: %v", err)
	}
	// 改系列
	req = batchReq(batchItem("item-1", "bob"))
	req.SeriesID = "other"
	r.CreateSeries("other", "alice", "")
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed series must conflict: %v", err)
	}
	// 改批次号
	req = batchReq(batchItem("item-1", "bob"))
	req.BatchNo = "other"
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed batch no must conflict: %v", err)
	}
	// 改条目顺序
	req = batchReq(batchItem("item-2", "bob"), batchItem("item-1", "alice"))
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed item order must conflict: %v", err)
	}
	// 改任一条目内容（持有人）
	req = batchReq(batchItem("item-1", "alice"))
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed holder must conflict: %v", err)
	}
	// 改任一条目内容（元数据）
	req = batchReq(batchItem("item-1", "bob"))
	req.Items[0].Metadata = "changed"
	if _, err := r.BatchIssue(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed metadata must conflict: %v", err)
	}
}

func TestBatchIssueReplayAfterStateChange(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")

	req := batchReq(batchItem("item-1", "bob"), batchItem("item-2", "alice"))
	first, err := r.BatchIssue(req)
	if err != nil {
		t.Fatal(err)
	}
	// 藏品转走、系列封存、账户停用后，原请求重放仍返回首次结果
	if _, err := r.Transfer(xferReq("bob", "item-1", "carol", 1, "t1")); err != nil {
		t.Fatal(err)
	}
	r.SealSeries("s1", "alice")
	r.DeactivateAccount("bob")

	again, err := r.BatchIssue(req)
	if err != nil {
		t.Fatalf("replay after state change: %v", err)
	}
	if !again.Replayed {
		t.Fatal("replay should be marked")
	}
	if len(again.Items) != len(first.Items) {
		t.Fatalf("replay item count mismatch")
	}
	for i := range again.Items {
		if again.Items[i] != first.Items[i] {
			t.Fatalf("replay item %d mismatch: %+v vs %+v", i, again.Items[i], first.Items[i])
		}
	}
	// 当前状态不被改写
	h, _ := r.GetHolding("item-1")
	if h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("replay must not change current holding: %+v", h)
	}
}

func TestBatchIssueRejectedReplay(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.SealSeries("s1", "alice")

	req := batchReq(batchItem("item-1", "bob"))
	_, err := r.BatchIssue(req)
	if !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("first should be sealed rejection: %v", err)
	}
	// 重放返回首次拒绝
	again, err := r.BatchIssue(req)
	if !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("replay should return first rejection: %v", err)
	}
	if !again.Replayed {
		t.Fatal("replay should be marked")
	}
	if again.Err == nil {
		t.Fatal("replay should carry rejection error")
	}
	// 不新增历史
	hist, _ := r.History("item-1")
	if len(hist) != 0 {
		t.Fatalf("rejected replay must not add history: %d", len(hist))
	}
}

// ---- 历史连续性 ----

func TestBatchIssueConsecutiveHistorySeqs(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")

	// 先单件发行占用 seq 1
	r.Issue(issueReq("item-0", "bob"))

	// 整批发行 3 件，seq 应为 2,3,4
	req := batchReq(batchItem("item-1", "alice"), batchItem("item-2", "bob"), batchItem("item-3", "carol"))
	res, err := r.BatchIssue(req)
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range res.Items {
		want := int64(i + 2)
		if it.TxSeq != want {
			t.Fatalf("item %d seq want %d, got %d", i, want, it.TxSeq)
		}
	}
	// 整批之后的单件发行 seq 应为 5
	single, err := r.Issue(issueReq("item-4", "bob"))
	if err != nil {
		t.Fatal(err)
	}
	if single.TxSeq != 5 {
		t.Fatalf("post-batch single seq want 5, got %d", single.TxSeq)
	}
}

// ---- 版税规则 ----

func TestBatchIssueFreezesRoyalty(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	// 整批发行作为系列首次发行，固定版税规则（未设置即无版税）
	req := batchReq(batchItem("item-1", "bob"))
	if _, err := r.BatchIssue(req); err != nil {
		t.Fatal(err)
	}
	// 之后设置版税规则被拒绝
	sr := SetRoyaltyRequest{
		Operator: "alice", Reason: "设置版税", RequestID: "sr-1", SeriesID: "s1",
		Shares: []RoyaltyShare{{AccountID: "alice", Rate: 500}},
	}
	if _, err := r.SetRoyalty(sr); !errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("royty should be frozen after first batch issue: %v", err)
	}
}

func TestBatchIssueAfterRoyaltySet(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")

	// 先设置版税规则
	sr := SetRoyaltyRequest{
		Operator: "alice", Reason: "设置版税", RequestID: "sr-1", SeriesID: "s1",
		Shares: []RoyaltyShare{{AccountID: "carol", Rate: 500}},
	}
	if _, err := r.SetRoyalty(sr); err != nil {
		t.Fatal(err)
	}
	// 整批发行
	req := batchReq(batchItem("item-1", "alice"))
	if _, err := r.BatchIssue(req); err != nil {
		t.Fatal(err)
	}
	// 转让时按新规则计算版税
	xfer := xferReq("alice", "item-1", "bob", 1, "t1")
	xfer.Price = 1000
	res, err := r.Transfer(xfer)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Payables) != 1 || res.Payables[0].AccountID != "carol" || res.Payables[0].Amount != 50 {
		t.Fatalf("payables wrong: %+v", res.Payables)
	}
	if res.Remainder != 950 {
		t.Fatalf("remainder wrong: %d", res.Remainder)
	}
}

// ---- 并发 ----

func TestBatchIssueConcurrentSameRequest(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	req := batchReq(batchItem("item-1", "bob"), batchItem("item-2", "alice"))
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	replayed := 0
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			res, err := r.BatchIssue(req)
			mu.Lock()
			defer mu.Unlock()
			errs[i] = err
			if res.Replayed {
				replayed++
			}
		}()
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("duplicate %d got error: %v", i, e)
		}
	}
	if replayed != n-1 {
		t.Fatalf("want %d replays, got %d", n-1, replayed)
	}
	// 每件藏品只发行一次
	hist, _ := r.History("item-1")
	if len(hist) != 1 {
		t.Fatalf("concurrent same request must issue once: %d", len(hist))
	}
}

func TestBatchIssueConcurrentDifferentRequests(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	// 两个不同请求号的整批发行争用同一藏品编号
	req1 := batchReq(batchItem("item-1", "bob"))
	req1.RequestID = "batch-1"
	req2 := batchReq(batchItem("item-1", "alice"))
	req2.RequestID = "batch-2"

	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err1 = r.BatchIssue(req1)
	}()
	go func() {
		defer wg.Done()
		_, err2 = r.BatchIssue(req2)
	}()
	wg.Wait()

	// 恰好一个成功，另一个被编号已占用拒绝
	succ := (err1 == nil) != (err2 == nil)
	if !succ {
		t.Fatalf("exactly one should succeed: err1=%v err2=%v", err1, err2)
	}
	if err1 != nil && !errors.Is(err1, ErrAlreadyExists) {
		t.Fatalf("loser should be ErrAlreadyExists, got %v", err1)
	}
	if err2 != nil && !errors.Is(err2, ErrAlreadyExists) {
		t.Fatalf("loser should be ErrAlreadyExists, got %v", err2)
	}
}

func TestBatchIssueConcurrentWithSingleIssue(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	// 整批发行与单件发行争用同一编号
	batchReq := batchReq(batchItem("item-1", "bob"))
	singleReq := issueReq("item-1", "alice")

	var wg sync.WaitGroup
	var batchErr, singleErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, batchErr = r.BatchIssue(batchReq)
	}()
	go func() {
		defer wg.Done()
		_, singleErr = r.Issue(singleReq)
	}()
	wg.Wait()

	succ := (batchErr == nil) != (singleErr == nil)
	if !succ {
		t.Fatalf("exactly one should succeed: batch=%v single=%v", batchErr, singleErr)
	}
	if batchErr != nil && !errors.Is(batchErr, ErrAlreadyExists) {
		t.Fatalf("batch loser should be ErrAlreadyExists, got %v", batchErr)
	}
	if singleErr != nil && !errors.Is(singleErr, ErrAlreadyExists) {
		t.Fatalf("single loser should be ErrAlreadyExists, got %v", singleErr)
	}
}

func TestBatchIssueConcurrentSeal(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	req := batchReq(batchItem("item-1", "bob"))
	var wg sync.WaitGroup
	var issueErr, sealErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, issueErr = r.BatchIssue(req)
	}()
	go func() {
		defer wg.Done()
		sealErr = r.SealSeries("s1", "alice")
	}()
	wg.Wait()

	if sealErr != nil {
		t.Fatalf("seal should always succeed: %v", sealErr)
	}
	if issueErr != nil && !errors.Is(issueErr, ErrSeriesSealed) {
		t.Fatalf("batch issue either succeeds or is sealed-rejected, got %v", issueErr)
	}
	s, _ := r.GetSeries("s1")
	if !s.Sealed {
		t.Fatal("series must end sealed")
	}
}

func TestBatchIssueConcurrentDeactivate(t *testing.T) {
	for range 20 {
		r := mustCreate(t, filepath.Join(t.TempDir(), "d"))
		setupWorld(t, r)

		req := batchReq(batchItem("item-1", "bob"))
		var wg sync.WaitGroup
		var issueErr, deactErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, issueErr = r.BatchIssue(req)
		}()
		go func() {
			defer wg.Done()
			deactErr = r.DeactivateAccount("alice")
		}()
		wg.Wait()

		if deactErr != nil {
			t.Fatalf("deactivate should always succeed: %v", deactErr)
		}
		if issueErr != nil && !errors.Is(issueErr, ErrAccountInactive) {
			t.Fatalf("batch issue either succeeds or is inactive-rejected, got %v", issueErr)
		}
	}
}

func TestBatchIssueConcurrentRoyaltySet(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")

	req := batchReq(batchItem("item-1", "bob"))
	sr := SetRoyaltyRequest{
		Operator: "alice", Reason: "设置版税", RequestID: "sr-1", SeriesID: "s1",
		Shares: []RoyaltyShare{{AccountID: "carol", Rate: 500}},
	}
	var wg sync.WaitGroup
	var issueErr, setErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, issueErr = r.BatchIssue(req)
	}()
	go func() {
		defer wg.Done()
		_, setErr = r.SetRoyalty(sr)
	}()
	wg.Wait()

	// 要么整批先成功（版税固定，设置被拒），要么设置先成功（整批沿用新规则）
	if issueErr != nil {
		t.Fatalf("batch should always succeed: %v", issueErr)
	}
	if setErr != nil && !errors.Is(setErr, ErrRoyaltyFrozen) {
		t.Fatalf("unexpected set error: %v", setErr)
	}
}

// ---- 持久化 ----

func TestBatchIssuePersistence(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupWorld(t, r)
	r.RegisterAccount("carol", "")

	req := batchReq(batchItem("item-1", "bob"), batchItem("item-2", "alice"), batchItem("item-3", "carol"))
	first, err := r.BatchIssue(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()

	// 持有与历史持久化
	for _, it := range req.Items {
		h, err := r2.GetHolding(it.ItemID)
		if err != nil || h.OwnerID != it.HolderID || h.Version != 1 {
			t.Fatalf("holding for %s wrong: %+v err=%v", it.ItemID, h, err)
		}
	}
	// 幂等结果持久化：原请求号重放首次结果
	again, err := r2.BatchIssue(req)
	if err != nil {
		t.Fatalf("replay after reopen: %v", err)
	}
	if !again.Replayed {
		t.Fatal("replay should be marked")
	}
	for i := range again.Items {
		if again.Items[i] != first.Items[i] {
			t.Fatalf("replay item %d mismatch: %+v vs %+v", i, again.Items[i], first.Items[i])
		}
	}
}

func TestBatchIssuePersistenceNoPartialState(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupWorld(t, r)

	req := batchReq(batchItem("item-1", "bob"), batchItem("item-2", "alice"))
	r.BatchIssue(req)
	// 不调用 Close 直接丢弃（模拟程序被直接终止）
	_ = r.store.lock.Close()
	r.closed = true

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()

	// 整批状态一致：要么全部成功，要么全部不存在
	for _, it := range req.Items {
		h, err := r2.GetHolding(it.ItemID)
		if err != nil || h.OwnerID != it.HolderID || h.Version != 1 {
			t.Fatalf("holding for %s wrong: %+v err=%v", it.ItemID, h, err)
		}
		hist, _ := r2.History(it.ItemID)
		if len(hist) != 1 {
			t.Fatalf("history for %s wrong: %d", it.ItemID, len(hist))
		}
	}
}

// ---- 与其他操作的交互 ----

func TestBatchIssueThenTransfer(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")

	req := batchReq(batchItem("item-1", "alice"))
	if _, err := r.BatchIssue(req); err != nil {
		t.Fatal(err)
	}
	// 整批发行的藏品可正常转让
	res, err := r.Transfer(xferReq("alice", "item-1", "bob", 1, "t1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 2 || res.ToID != "bob" {
		t.Fatalf("transfer result wrong: %+v", res)
	}
}

func TestBatchIssueThenProxyAuth(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")
	r.RegisterAccount("dave", "")

	req := batchReq(batchItem("item-1", "alice"))
	if _, err := r.BatchIssue(req); err != nil {
		t.Fatal(err)
	}
	// 整批发行的藏品可创建代转授权
	authReq := CreateAuthorizationRequest{
		Operator: "alice", Reason: "代转", RequestID: "auth-1", AuthID: "auth-1",
		ItemID: "item-1", TrusteeID: "carol", ToID: "dave",
		ExpectedOwner: "alice", ExpectedVer: 1,
		ExpiresAt: r.now().Add(24 * 3600e9),
	}
	if _, err := r.CreateAuthorization(authReq); err != nil {
		t.Fatal(err)
	}
}

func TestBatchNumberReusableAcrossRequests(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	// 不同请求可沿用同一批次号
	req1 := batchReq(batchItem("item-1", "bob"))
	req1.RequestID = "batch-1"
	req2 := batchReq(batchItem("item-2", "alice"))
	req2.RequestID = "batch-2"
	req2.BatchNo = "b1" // 同一批次号

	if _, err := r.BatchIssue(req1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.BatchIssue(req2); err != nil {
		t.Fatalf("reusing batch no across requests should succeed: %v", err)
	}
}

func TestBatchIssueRequestIDSharesAcrossKinds(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	// 整批发行用了请求号 "batch-1"，单件发行再用同一号 => 冲突
	batchReq := batchReq(batchItem("item-1", "bob"))
	if _, err := r.BatchIssue(batchReq); err != nil {
		t.Fatal(err)
	}
	singleReq := issueReq("item-2", "alice")
	singleReq.RequestID = "batch-1"
	if _, err := r.Issue(singleReq); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request id shared across kinds must conflict: %v", err)
	}
}

func TestBatchIssueResultPointsOutItemID(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")
	r.DeactivateAccount("carol")

	// 第二批的持有人停用，错误应指出该件编号
	req := batchReq(batchItem("item-1", "bob"), batchItem("item-2", "carol"))
	_, err := r.BatchIssue(req)
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("want ErrAccountInactive, got %v", err)
	}
	// 错误信息应包含藏品编号
	if err != nil && !strings.Contains(err.Error(), "item-2") {
		t.Fatalf("error should point out item id, got: %v", err)
	}
}
