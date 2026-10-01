package registry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "data")
}

func mustCreate(t *testing.T, dir string) *Registry {
	t.Helper()
	r, err := Create(dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// setupWorld 建立两个可用账户与一个由 alice 创建的系列。
func setupWorld(t *testing.T, r *Registry) {
	t.Helper()
	if err := r.RegisterAccount("alice", "创作者"); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterAccount("bob", "买家"); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSeries("s1", "alice", "首批系列"); err != nil {
		t.Fatal(err)
	}
}

func issueReq(item, holder string) IssueRequest {
	return IssueRequest{
		Operator: "alice", Reason: "首发", RequestID: "req-" + item,
		ItemID: item, SeriesID: "s1", BatchNo: "b1", Metadata: "元数据", HolderID: holder,
	}
}

func xferReq(op, item, to string, ver int64, rid string) TransferRequest {
	return TransferRequest{
		Operator: op, Reason: "卖出", RequestID: rid, ItemID: item,
		ExpectedOwner: op, ExpectedVer: ver, ToID: to,
	}
}

// ---- 基础流程 ----

func TestBaselineReady(t *testing.T) {
	if !Ready() {
		t.Fatal("baseline not ready")
	}
}

func TestRegistrationAndIssue(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)

	res, err := r.Issue(issueReq("item-1", "bob"))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if res.Version != 1 || res.OwnerID != "bob" || res.TxSeq != 1 {
		t.Fatalf("unexpected issue result: %+v", res)
	}
	h, err := r.GetHolding("item-1")
	if err != nil || h.OwnerID != "bob" || h.Version != 1 {
		t.Fatalf("holding wrong: %+v err=%v", h, err)
	}
	hist, err := r.History("item-1")
	if err != nil || len(hist) != 1 {
		t.Fatalf("history wrong: %v %v", hist, err)
	}
	if e := hist[0]; e.Kind != "issue" || e.FromID != "" || e.FromVersion != 0 ||
		e.ToID != "bob" || e.ToVersion != 1 || e.Operator != "alice" || e.Reason != "首发" {
		t.Fatalf("issue history entry wrong: %+v", e)
	}
}

func TestTransferFlowAndVersions(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")

	if _, err := r.Issue(issueReq("item-1", "alice")); err != nil {
		t.Fatal(err)
	}
	res, err := r.Transfer(xferReq("alice", "item-1", "bob", 1, "t1"))
	if err != nil {
		t.Fatalf("transfer1: %v", err)
	}
	if res.FromID != "alice" || res.ToID != "bob" || res.Version != 2 {
		t.Fatalf("transfer result wrong: %+v", res)
	}
	res, err = r.Transfer(xferReq("bob", "item-1", "carol", 2, "t2"))
	if err != nil {
		t.Fatalf("transfer2: %v", err)
	}
	if res.Version != 3 || res.ToID != "carol" {
		t.Fatalf("transfer result wrong: %+v", res)
	}
	hist, _ := r.History("item-1")
	if len(hist) != 3 {
		t.Fatalf("want 3 history entries, got %d", len(hist))
	}
	if e := hist[2]; e.Kind != "transfer" || e.FromID != "bob" || e.ToID != "carol" ||
		e.FromVersion != 2 || e.ToVersion != 3 {
		t.Fatalf("last entry wrong: %+v", e)
	}
}

func TestHoldingsOfIncludesInactive(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	if _, err := r.Issue(issueReq("item-1", "bob")); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	hs, err := r.HoldingsOf("bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 1 || hs[0].ItemID != "item-1" {
		t.Fatalf("inactive account should still see holdings: %+v", hs)
	}
	_, err = r.History("item-1")
	if err != nil {
		t.Fatalf("inactive account history query failed: %v", err)
	}
}

// ---- 不存在明确返回 ----

func TestNotFound(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	if _, err := r.GetAccount("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := r.GetSeries("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := r.GetItem("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := r.GetHolding("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := r.History("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := r.HoldingsOf("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ---- 账户停用 ----

func TestDeactivationBlocks(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	// 停用的创建账户不能再发行
	if _, err := r.Issue(issueReq("i2", "bob")); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("want ErrAccountInactive, got %v", err)
	}
	// 不能发起转让
	if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "t1")); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("want ErrAccountInactive, got %v", err)
	}
	// 不能接收转让：先激活视角验证接收限制——重新激活 alice 不支持，
	// 因此让持有在 bob 手中，carol 停用后不能接收。
	r2 := mustCreate(t, filepath.Join(t.TempDir(), "d2"))
	setupWorld(t, r2)
	r2.RegisterAccount("carol", "")
	r2.Issue(issueReq("j1", "bob"))
	if err := r2.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	_, err := r2.Transfer(xferReq("bob", "j1", "carol", 1, "t1"))
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive receiver must be rejected, got %v", err)
	}
}

// ---- 系列封存 ----

func TestSealBlocksIssueButTransferContinues(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Issue(issueReq("i2", "alice")); !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("sealed series must not issue, got %v", err)
	}
	// 已发行藏品仍可转让
	if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "t1")); err != nil {
		t.Fatalf("transfer after seal must succeed: %v", err)
	}
	s, _ := r.GetSeries("s1")
	if !s.Sealed {
		t.Fatal("series should be sealed")
	}
}

func TestOnlyCreatorCanIssueOrSeal(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	if err := r.SealSeries("s1", "bob"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-creator seal must fail, got %v", err)
	}
	req := issueReq("i1", "bob")
	req.Operator = "bob"
	if _, err := r.Issue(req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-creator issue must fail, got %v", err)
	}
}

func TestSealIrreversibleViaReissueOrdering(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.SealSeries("s1", "alice")
	// 再次封存不报错（保持终态），但没有"解封"入口；直接验证发行仍被拒。
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatalf("idempotent seal failed: %v", err)
	}
	if _, err := r.Issue(issueReq("i1", "bob")); !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("issue still blocked: %v", err)
	}
}

// ---- 唯一编号 ----

func TestUniqueItemID(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	if _, err := r.Issue(issueReq("i1", "bob")); err != nil {
		t.Fatal(err)
	}
	// 换请求号也不能用同一藏品编号发行第二件
	req := issueReq("i1", "bob")
	req.RequestID = "other"
	if _, err := r.Issue(req); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate item id must fail, got %v", err)
	}
}

func TestUniqueAccountAndSeries(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	if err := r.RegisterAccount("alice", "x"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("got %v", err)
	}
	if err := r.CreateSeries("s1", "alice", "x"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("got %v", err)
	}
}

// ---- 转让校验 ----

func TestTransferValidation(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")
	r.Issue(issueReq("i1", "alice"))

	// 不存在的藏品
	if _, err := r.Transfer(xferReq("alice", "nope", "bob", 1, "x1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	// 发起人不是当前持有人
	if _, err := r.Transfer(xferReq("bob", "i1", "carol", 1, "x2")); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-holder transfer must conflict, got %v", err)
	}
	// 版本不符
	if _, err := r.Transfer(xferReq("alice", "i1", "carol", 9, "x3")); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong version must conflict, got %v", err)
	}
	// 期望持有人填错
	bad := xferReq("alice", "i1", "carol", 1, "x4")
	bad.ExpectedOwner = "bob"
	if _, err := r.Transfer(bad); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong expected owner must conflict, got %v", err)
	}
	// 自我转让
	if _, err := r.Transfer(xferReq("alice", "i1", "alice", 1, "x5")); !errors.Is(err, ErrSameAccount) {
		t.Fatalf("self transfer must fail, got %v", err)
	}
	// 失败后持有与历史不变
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("failed transfers must not change holding: %+v", h)
	}
	hist, _ := r.History("i1")
	if len(hist) != 1 {
		t.Fatalf("failed transfers must not add history: %d", len(hist))
	}
}

func TestTransferToUnknownAccount(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.Issue(issueReq("i1", "alice"))
	if _, err := r.Transfer(xferReq("alice", "i1", "ghost", 1, "x1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("transfer to unregistered account must fail, got %v", err)
	}
}

func TestIssueUnknownSeriesOrHolder(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	req := issueReq("i1", "bob")
	req.SeriesID = "nope"
	if _, err := r.Issue(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("issue into unknown series must fail, got %v", err)
	}
	req = issueReq("i1", "ghost")
	if _, err := r.Issue(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("issue to unknown holder must fail, got %v", err)
	}
	if _, err := r.GetItem("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed issue must not create the item")
	}
}

func TestMissingFields(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	if err := r.RegisterAccount("  ", "x"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank account id: %v", err)
	}
	req := issueReq("i1", "bob")
	req.RequestID = ""
	if _, err := r.Issue(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing request id: %v", err)
	}
	req = issueReq("i1", "bob")
	req.Reason = "  "
	if _, err := r.Issue(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank reason: %v", err)
	}
	treq := xferReq("alice", "i1", "bob", 1, "")
	if _, err := r.Transfer(treq); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing request id: %v", err)
	}
}

// ---- 幂等 ----

func TestIssueIdempotency(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	req := issueReq("i1", "bob")
	r1, err := r.Issue(req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := r.Issue(req)
	if err != nil {
		t.Fatalf("replay should not error: %v", err)
	}
	if !r2.Replayed || r2.TxSeq != r1.TxSeq || r2.Version != 1 {
		t.Fatalf("replay mismatch: %+v vs %+v", r1, r2)
	}
	hist, _ := r.History("i1")
	if len(hist) != 1 {
		t.Fatalf("duplicate issue adds history: %d", len(hist))
	}
	// 任一业务参数改变 -> 冲突
	req.Metadata = "changed"
	if _, err := r.Issue(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed params must conflict, got %v", err)
	}
}

func TestTransferIdempotencyAfterOwnershipChange(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")
	r.Issue(issueReq("i1", "alice"))

	req := xferReq("alice", "i1", "bob", 1, "t1")
	first, err := r.Transfer(req)
	if err != nil {
		t.Fatal(err)
	}
	// 后续真实流转使藏品到了 carol 手中，版本 3
	if _, err := r.Transfer(xferReq("bob", "i1", "carol", 2, "t2")); err != nil {
		t.Fatal(err)
	}
	// 原请求号 + 原业务参数再提交，返回首次成功结果，尽管当前版本/持有人已不同
	again, err := r.Transfer(req)
	if err != nil {
		t.Fatalf("idempotent replay after ownership change: %v", err)
	}
	if !again.Replayed || again.Version != 2 || again.ToID != "bob" ||
		again.TxSeq != first.TxSeq {
		t.Fatalf("replayed result != first: %+v vs %+v", first, again)
	}
	hist, _ := r.History("i1")
	if len(hist) != 3 {
		t.Fatalf("replay must not add history (issue+t1+t2=3): %d", len(hist))
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 3 {
		t.Fatalf("replay must not change current holding: %+v", h)
	}
}

func TestSharedRequestIDAcrossKinds(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.Issue(issueReq("i1", "alice"))
	// issue 用了请求号 "req-i1"（issueReq 生成），transfer 再用同一号 => 冲突
	req := xferReq("alice", "i1", "bob", 1, "req-i1")
	if _, err := r.Transfer(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request id shared across kinds must conflict, got %v", err)
	}
}

func TestRejectedRequestReplaysSameRejection(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")
	r.Issue(issueReq("i1", "alice"))

	// 一次版本不符的转让被拒绝
	bad := xferReq("alice", "i1", "carol", 5, "bad-ver")
	_, err := r.Transfer(bad)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v", err)
	}
	// 即使后来真的发生了相符的其他转让使版本变化……
	if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "ok1")); err != nil {
		t.Fatal(err)
	}
	// ……原拒绝请求重提仍返回首次的业务拒绝，而不是现在才成立的其他错误或成功
	res, err := r.Transfer(bad)
	if !errors.Is(err, ErrConflict) || !res.Replayed {
		t.Fatalf("rejected request must replay first rejection: res=%+v err=%v", res, err)
	}
	hist, _ := r.History("i1")
	if len(hist) != 2 {
		t.Fatalf("rejection replay adds history: %d", len(hist))
	}
}

// ---- 并发 ----

func TestConcurrentSameVersionTransfers(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.RegisterAccount("carol", "")
	r.RegisterAccount("dave", "")
	r.Issue(issueReq("i1", "alice"))

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			to := "bob"
			if i%2 == 0 {
				to = "carol"
			}
			if i%3 == 0 {
				to = "dave"
			}
			_, errs[i] = r.Transfer(xferReq("alice", "i1", to, 1, fmt.Sprintf("c%d", i)))
		}()
	}
	wg.Wait()
	succ := 0
	for _, e := range errs {
		if e == nil {
			succ++
		}
	}
	if succ != 1 {
		t.Fatalf("exactly one concurrent same-version transfer should succeed, got %d", succ)
	}
	h, _ := r.GetHolding("i1")
	if h.Version != 2 {
		t.Fatalf("version should be 2 after one success, got %d", h.Version)
	}
	hist, _ := r.History("i1")
	if len(hist) != 2 {
		t.Fatalf("exactly one transfer history, got %d", len(hist))
	}
}

func TestConcurrentDuplicateRequest(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	r.Issue(issueReq("i1", "alice"))
	req := xferReq("alice", "i1", "bob", 1, "dup")

	const n = 16
	var wg sync.WaitGroup
	res := make([]error, n)
	replayed := 0
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			out, err := r.Transfer(req)
			mu.Lock()
			defer mu.Unlock()
			res[i] = err
			if out.Replayed {
				replayed++
			}
		}()
	}
	wg.Wait()
	for i, e := range res {
		if e != nil {
			t.Fatalf("duplicate %d got error: %v", i, e)
		}
	}
	if replayed != n-1 {
		t.Fatalf("want %d replays, got %d", n-1, replayed)
	}
	hist, _ := r.History("i1")
	if len(hist) != 2 {
		t.Fatalf("duplicate concurrent submissions must execute once, history=%d", len(hist))
	}
}

// 封存与发行、停用与转让同时发生：存在一个完整先后次序，先完成的
// 封存/停用阻止随后不再允许的操作；反之亦然，恰有一个结果。
func TestConcurrentSealVsIssue(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r)
	var wg sync.WaitGroup
	var issueErr, sealErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, issueErr = r.Issue(issueReq("i1", "bob"))
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
		t.Fatalf("issue either succeeds or is sealed-rejected, got %v", issueErr)
	}
	// 若发行先成功，则系列中恰有一件藏品；再发行必被封存拒绝。
	if issueErr == nil {
		if _, err := r.Issue(issueReq("i2", "bob")); !errors.Is(err, ErrSeriesSealed) {
			t.Fatalf("post-seal issue must fail, got %v", err)
		}
	}
	s, _ := r.GetSeries("s1")
	if !s.Sealed {
		t.Fatal("series must end sealed")
	}
}

func TestConcurrentDeactivateVsTransfer(t *testing.T) {
	for range 20 {
		r := mustCreate(t, filepath.Join(t.TempDir(), "d"))
		setupWorld(t, r)
		r.RegisterAccount("carol", "")
		r.Issue(issueReq("i1", "alice"))
		var wg sync.WaitGroup
		var xferErr, deactErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, xferErr = r.Transfer(xferReq("alice", "i1", "carol", 1, "x1"))
		}()
		go func() {
			defer wg.Done()
			deactErr = r.DeactivateAccount("alice")
		}()
		wg.Wait()
		if deactErr != nil {
			t.Fatalf("deactivate should always succeed: %v", deactErr)
		}
		if xferErr != nil && !errors.Is(xferErr, ErrAccountInactive) {
			t.Fatalf("transfer either succeeds or is blocked, got %v", xferErr)
		}
		h, _ := r.GetHolding("i1")
		hist, _ := r.History("i1")
		if xferErr == nil {
			if h.OwnerID != "carol" || h.Version != 2 || len(hist) != 2 {
				t.Fatalf("transfer-first state inconsistent: %+v hist=%d", h, len(hist))
			}
		} else {
			if h.OwnerID != "alice" || h.Version != 1 || len(hist) != 1 {
				t.Fatalf("deactivate-first state inconsistent: %+v hist=%d", h, len(hist))
			}
		}
	}
}

// ---- 持久化 ----

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupWorld(t, r)
	r.RegisterAccount("carol", "")
	r.Issue(issueReq("i1", "alice"))
	r.Transfer(xferReq("alice", "i1", "bob", 1, "t1"))
	r.SealSeries("s1", "alice")
	r.DeactivateAccount("carol")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()
	a, err := r2.GetAccount("carol")
	if err != nil || a.Active {
		t.Fatalf("deactivation must persist: %+v %v", a, err)
	}
	s, _ := r2.GetSeries("s1")
	if !s.Sealed {
		t.Fatal("sealed state must persist")
	}
	h, _ := r2.GetHolding("i1")
	if h.OwnerID != "bob" || h.Version != 2 {
		t.Fatalf("holding must persist: %+v", h)
	}
	hist, _ := r2.History("i1")
	if len(hist) != 2 {
		t.Fatalf("history must persist: %d", len(hist))
	}
	// 幂等结果也要保留：原转让请求号重放首次成功结果
	again, err := r2.Transfer(xferReq("alice", "i1", "bob", 1, "t1"))
	if err != nil || !again.Replayed || again.Version != 2 {
		t.Fatalf("idempotent result must persist across reopen: %+v %v", again, err)
	}
	// 封存状态继续阻止发行
	if _, err := r2.Issue(issueReq("i2", "bob")); !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("seal still blocks after reopen: %v", err)
	}
}

// TestPersistenceNoOrphanState 验证"换人必有历史"：快照是单次原子替换，
// 磁盘上不会出现持有已变但历史缺失的中间状态。
func TestPersistenceNoOrphanState(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupWorld(t, r)
	r.Issue(issueReq("i1", "alice"))
	r.Transfer(xferReq("alice", "i1", "bob", 1, "t1"))
	// 不调用 Close 直接丢弃（模拟程序被直接终止；flock 由内核释放）。
	_ = r.store.lock.Close()
	r.closed = true

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	h, _ := r2.GetHolding("i1")
	hist, _ := r2.History("i1")
	if h.OwnerID != "bob" || len(hist) != 2 || hist[1].ToID != "bob" {
		t.Fatalf("state/history must be atomically consistent: holding=%+v hist=%v", h, hist)
	}
}

// ---- 数据损坏 / 打开语义 ----

func TestCorruptDataRejected(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupWorld(t, r)
	r.Close()

	if err := os.WriteFile(filepath.Join(dir, "registry.json"), []byte("{not json"), fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt data must yield ErrCorrupt, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry.json"), []byte(""), fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("empty data must yield ErrCorrupt, got %v", err)
	}
}

func TestCreateRefusesExisting(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	r.Close()
	if _, err := Create(dir); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Create over existing registry must fail, got %v", err)
	}
}

func TestOpenEmptyDirIsNotBlankRegistry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open on dir with no data must report missing, got %v", err)
	}
	// 报错之后目录仍可正常 Create
	r, err := Create(dir)
	if err != nil {
		t.Fatalf("Create after failed Open should work: %v", err)
	}
	r.Close()
}

func TestExclusiveLock(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	defer r.Close()
	if _, err := Open(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second concurrent open must be ErrLocked, got %v", err)
	}
}

// ---- 重试语义 ----

// 未收到结果的请求用原请求号重试：若首次实际已成功落盘，则回放；
// 这里通过"成功后重开再重试"模拟调用方未收到响应的情形。
func TestRetryAfterUncertainOutcome(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupWorld(t, r)
	r.Issue(issueReq("i1", "alice"))
	req := xferReq("alice", "i1", "bob", 1, "t1")
	if _, err := r.Transfer(req); err != nil {
		t.Fatal(err)
	}
	_ = r.store.lock.Close()
	r.closed = true

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	out, err := r2.Transfer(req)
	if err != nil || !out.Replayed || out.Version != 2 {
		t.Fatalf("retry must return saved result: %+v %v", out, err)
	}
	hist, _ := r2.History("i1")
	if len(hist) != 2 {
		t.Fatalf("retry executed twice: %d", len(hist))
	}
}
