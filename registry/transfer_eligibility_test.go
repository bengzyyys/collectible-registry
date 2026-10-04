package registry

import (
	"errors"
	"testing"
)

// 直接转让资格规则的优先级由两个入口各自保留：
//   - 单件：藏品不存在 > 操作者登记/停用 > 接收账户登记/停用 > 收发同人
//     > 期望持有不符；
//   - 整批：操作者登记/停用（不附藏品编号）> 清单内第一件失败藏品的
//     藏品/接收账户/收发同人/持有信息问题。
//
// 本文件锁定多问题同时出现时的报告顺序，防止共用规则后两个入口的
// 既有优先级被改变。

// TestSingleTransferEligibilityPrecedence 覆盖单件转让的优先级组合。
func TestSingleTransferEligibilityPrecedence(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupWorld(t, r) // alice、bob 可用，系列 s1
	r.RegisterAccount("carol", "")
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}

	// 操作者停用而藏品不存在：仍先返回对象不存在。
	if err := r.DeactivateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Transfer(xferReq("alice", "nope", "bob", 1, "p1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing item must outrank inactive operator, got %v", err)
	}

	// 操作者停用 + 藏品存在 + 接收账户未登记：操作者问题优先。
	if _, err := r.Transfer(xferReq("alice", "i1", "ghost", 1, "p2")); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive operator must outrank missing recipient, got %v", err)
	}

	// 重新建一个全部可用的登记册验证后续顺序。
	r2 := mustCreate(t, tempDir(t))
	setupWorld(t, r2)
	r2.RegisterAccount("carol", "")
	if _, err := r2.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}

	// 接收人就是当前持有人，且期望版本填错：仍先返回收发同人。
	selfBadVer := xferReq("alice", "i1", "alice", 99, "p3")
	if _, err := r2.Transfer(selfBadVer); !errors.Is(err, ErrSameAccount) {
		t.Fatalf("same holder must outrank version mismatch, got %v", err)
	}

	// 接收账户停用 + 期望版本填错：接收账户问题优先于持有信息不符。
	if err := r2.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Transfer(xferReq("alice", "i1", "carol", 99, "p4")); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive recipient must outrank version mismatch, got %v", err)
	}

	// 系列封存不妨碍已发行藏品转让。
	if err := r2.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Transfer(xferReq("alice", "i1", "bob", 1, "p5")); err != nil {
		t.Fatalf("sealed series must not block transfer of issued item, got %v", err)
	}
}

// TestBatchTransferEligibilityPrecedence 覆盖整批转让的优先级组合。
func TestBatchTransferEligibilityPrecedence(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r) // bob 持有 i1..i4，carol 持有 i5
	entry := func(item, to string, ver int64) TransferBatchEntry {
		return tbtEntry(item, to, ver, 0)
	}

	// 操作者停用且条目引用不存在藏品：只返回账户停用错误，不附藏品编号。
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	res, err := r.TransferBatch(tbtReq("q1",
		entry("no-item", "dave", 1),
		entry("i2", "ghost", 1),
	))
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive operator must be reported first, got %v", err)
	}
	if res.ItemID != "" {
		t.Fatalf("operator-level error must not carry ItemID, got %q", res.ItemID)
	}

	// 操作者未登记：同样只返回账户错误且不附藏品编号。
	ghost := tbtReq("q2", entry("no-item", "dave", 1))
	ghost.Operator = "ghost-op"
	res, err = r.TransferBatch(ghost)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unregistered operator must be reported first, got %v", err)
	}
	if res.ItemID != "" {
		t.Fatalf("operator-level error must not carry ItemID, got %q", res.ItemID)
	}

	// 操作者可用后，只报告第一件失败的藏品：carol 是 i5 的持有人（合格），
	// 但不是 i1 的持有人——失败必须定位在第二件 i1，且第一件不被改动。
	good := TransferBatchRequest{
		Operator: "carol", Reason: "整批转出", RequestID: "q3",
		Entries: []TransferBatchEntry{
			{ItemID: "i5", ToID: "dave", ExpectedOwner: "carol", ExpectedVer: 1},
			{ItemID: "i1", ToID: "dave", ExpectedOwner: "carol", ExpectedVer: 1},
		},
	}
	// carol 不是 i1 的持有人：第一件 i5 合格，第二件 i1 持有信息不符。
	// 但整批必须全部成功——确认失败定位在第二件而非由 apply 阶段改动 i5。
	res, err = r.TransferBatch(good)
	if !errors.Is(err, ErrConflict) || res.ItemID != "i1" {
		t.Fatalf("want ErrConflict on i1, got item=%q err=%v", res.ItemID, err)
	}
	h5, _ := r.GetHolding("i5")
	if h5.OwnerID != "carol" || h5.Version != 1 {
		t.Fatalf("rejected batch must not change earlier valid entry: %+v", h5)
	}

	// 收发同人优先于同一条目的期望版本填错。
	res, err = r.TransferBatch(TransferBatchRequest{
		Operator: "bob", Reason: "整批转出", RequestID: "q4",
		Entries: []TransferBatchEntry{
			{ItemID: "i1", ToID: "bob", ExpectedOwner: "bob", ExpectedVer: 99},
		},
	})
	// bob 已停用，操作者错误应最先——重新激活一个可用登记册验证条目内顺序。
	if !errors.Is(err, ErrAccountInactive) || res.ItemID != "" {
		t.Fatalf("inactive operator still must dominate, got item=%q err=%v", res.ItemID, err)
	}

	r2 := mustCreate(t, tempDir(t))
	setupBatchTransferWorld(t, r2)
	res, err = r2.TransferBatch(TransferBatchRequest{
		Operator: "bob", Reason: "整批转出", RequestID: "q5",
		Entries: []TransferBatchEntry{
			{ItemID: "i1", ToID: "bob", ExpectedOwner: "bob", ExpectedVer: 99},
			{ItemID: "i2", ToID: "ghost", ExpectedOwner: "bob", ExpectedVer: 1},
		},
	})
	if !errors.Is(err, ErrSameAccount) || res.ItemID != "i1" {
		t.Fatalf("same-holder on i1 must outrank later entry errors, got item=%q err=%v", res.ItemID, err)
	}

	// 接收账户问题优先于收发同人之后的持有不符：i1 接收人停用且版本填错，
	// 返回停用而非冲突；i2 的藏品不存在排在后面不提前。
	if err := r2.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	res, err = r2.TransferBatch(TransferBatchRequest{
		Operator: "bob", Reason: "整批转出", RequestID: "q6",
		Entries: []TransferBatchEntry{
			{ItemID: "i1", ToID: "carol", ExpectedOwner: "bob", ExpectedVer: 99},
			{ItemID: "missing", ToID: "dave", ExpectedOwner: "bob", ExpectedVer: 1},
		},
	})
	if !errors.Is(err, ErrAccountInactive) || res.ItemID != "i1" {
		t.Fatalf("inactive recipient on i1 must win over version mismatch and later items, got item=%q err=%v", res.ItemID, err)
	}
}
