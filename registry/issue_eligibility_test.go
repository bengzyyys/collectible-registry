package registry

import (
	"errors"
	"testing"
)

// 发行资格规则的优先级由两个入口各自保留：
//   - 单件/整批共用：操作者登记/停用 > 系列不存在 > 系列已封存 > 操作者
//     不是创建账户；藏品编号与初始持有人的问题不能提前覆盖这些拒绝。
//   - 单件：藏品编号已占用 > 初始持有人未登记/停用；拒绝结果仍带本次
//     请求的藏品编号。
//   - 整批：操作者与系列层面的问题不附藏品编号；条目层面编号占用始终
//     优先于初始持有人问题（即使占用发生在清单更后的条目）；同一类问题
//     报告清单顺序最前的那件。
//
// 本文件锁定多个业务问题同时出现时的报告顺序，防止单件与整批共用同一
// 套资格规则后既有优先级被改变。

// TestSingleIssueEligibilityPrecedence 覆盖单件发行的优先级组合。
func TestSingleIssueEligibilityPrecedence(t *testing.T) {
	// 操作者停用，且系列也不存在、编号已占用、持有人未登记：操作者问题优先。
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r) // alice、bob、carol 可用，系列 s1
	if _, err := r.Issue(issueReq("taken", "bob")); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	req := issueReq("taken", "ghost")
	req.RequestID = "q-inactive-op"
	req.SeriesID = "no-series"
	res, err := r.Issue(req)
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive operator must outrank missing series/item/holder, got %v", err)
	}
	// 单件拒绝仍返回本次请求的藏品编号。
	if res.ItemID != "taken" {
		t.Fatalf("single-issue rejection must carry request ItemID, got %q", res.ItemID)
	}

	// 操作者停用且系列已封存：仍先报操作者停用。
	r2 := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r2)
	if err := r2.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := r2.DeactivateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Issue(issueReq("i1", "bob")); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive operator must outrank sealed series, got %v", err)
	}

	// 操作者可用但不是创建账户，且系列已封存：封存优先于非创建账户。
	r3 := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r3)
	if err := r3.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	nonCreator := issueReq("i1", "carol")
	nonCreator.Operator = "bob"
	if _, err := r3.Issue(nonCreator); !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("sealed series must outrank non-creator operator, got %v", err)
	}

	// 系列不存在优先于编号已占用与持有人未登记。
	r4 := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r4)
	if _, err := r4.Issue(issueReq("taken", "bob")); err != nil {
		t.Fatal(err)
	}
	missingSeries := issueReq("taken", "ghost")
	missingSeries.RequestID = "q-missing-series"
	missingSeries.SeriesID = "no-series"
	res, err = r4.Issue(missingSeries)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing series must outrank taken item and missing holder, got %v", err)
	}
	if res.ItemID != "taken" {
		t.Fatalf("single-issue rejection must carry request ItemID, got %q", res.ItemID)
	}

	// 编号已占用且初始持有人停用：仍报告编号占用。
	r5 := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r5)
	if _, err := r5.Issue(issueReq("taken", "bob")); err != nil {
		t.Fatal(err)
	}
	if err := r5.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	takenInactiveHolder := issueReq("taken", "carol")
	takenInactiveHolder.RequestID = "q-taken-inactive-holder"
	res, err = r5.Issue(takenInactiveHolder)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("taken item id must outrank inactive holder, got %v", err)
	}
	if res.ItemID != "taken" {
		t.Fatalf("single-issue rejection must carry request ItemID, got %q", res.ItemID)
	}
}

// TestBatchIssueEligibilityPrecedence 覆盖整批发行的优先级组合。
func TestBatchIssueEligibilityPrecedence(t *testing.T) {
	// 操作者层面的问题最先报告且不附藏品编号，即使条目同时存在编号占用
	// 与持有人问题。
	r := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r)
	if _, err := r.Issue(issueReq("taken", "bob")); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	res, err := r.IssueBatch(batchReq("q1",
		be("i1", "ghost"), be("taken", "bob")))
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive operator must be reported first, got %v", err)
	}
	if res.ItemID != "" {
		t.Fatalf("operator-level error must not carry ItemID, got %q", res.ItemID)
	}

	// 操作者未登记：同样只返回账户错误且不附藏品编号。
	r2 := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r2)
	if _, err := r2.Issue(issueReq("taken", "bob")); err != nil {
		t.Fatal(err)
	}
	ghostOp := batchReq("q2", be("i1", "ghost"), be("taken", "bob"))
	ghostOp.Operator = "ghost-op"
	res, err = r2.IssueBatch(ghostOp)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unregistered operator must be reported first, got %v", err)
	}
	if res.ItemID != "" {
		t.Fatalf("operator-level error must not carry ItemID, got %q", res.ItemID)
	}

	// 系列不存在、已封存、操作者非创建账户：都不附藏品编号，且封存优先于
	// 非创建账户。
	r3 := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r3)
	missingSeries := batchReq("q3", be("i1", "ghost"), be("taken", "bob"))
	missingSeries.SeriesID = "no-series"
	if res, err := r3.IssueBatch(missingSeries); !errors.Is(err, ErrNotFound) || res.ItemID != "" {
		t.Fatalf("missing series must dominate item issues, got item=%q err=%v", res.ItemID, err)
	}

	if err := r3.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	nonCreator := batchReq("q4", be("i1", "ghost"))
	nonCreator.Operator = "bob"
	if res, err := r3.IssueBatch(nonCreator); !errors.Is(err, ErrSeriesSealed) || res.ItemID != "" {
		t.Fatalf("sealed series must outrank non-creator, got item=%q err=%v", res.ItemID, err)
	}

	r4 := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r4)
	nonCreatorOK := batchReq("q5", be("i1", "ghost"))
	nonCreatorOK.Operator = "bob"
	if res, err := r4.IssueBatch(nonCreatorOK); !errors.Is(err, ErrForbidden) || res.ItemID != "" {
		t.Fatalf("non-creator operator must be rejected without ItemID, got item=%q err=%v", res.ItemID, err)
	}

	// 跨条目优先级：前一件初始持有人未登记，后一件编号已占用——仍报告后
	// 一件编号被占用（编号占用在整批中先于持有人统一检查）。
	r5 := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r5)
	if _, err := r5.Issue(issueReq("taken", "bob")); err != nil {
		t.Fatal(err)
	}
	res, err = r5.IssueBatch(batchReq("q6",
		be("i1", "ghost"), be("taken", "carol")))
	if !errors.Is(err, ErrAlreadyExists) || res.ItemID != "taken" {
		t.Fatalf("later entry's taken id must outrank earlier missing holder, got item=%q err=%v", res.ItemID, err)
	}
	// 整批拒绝是原子的：前一件（持有人未登记）也不得被建立。
	if _, err := r5.GetItem("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("earlier entry must not be created on rejected batch: %v", err)
	}
	if _, err := r5.GetHolding("i1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("earlier entry holding must not exist: %v", err)
	}
	// 未占用编号继续可用。
	if _, err := r5.IssueBatch(batchReq("q7", be("i1", "bob"))); err != nil {
		t.Fatalf("previously free id should stay issuable: %v", err)
	}

	// 前一件持有人停用、后一件编号已占用：同样报告后一件占用。
	r6 := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r6)
	if _, err := r6.Issue(issueReq("taken", "bob")); err != nil {
		t.Fatal(err)
	}
	if err := r6.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	res, err = r6.IssueBatch(batchReq("q8",
		be("i1", "carol"), be("taken", "bob")))
	if !errors.Is(err, ErrAlreadyExists) || res.ItemID != "taken" {
		t.Fatalf("later taken id must outrank earlier inactive holder, got item=%q err=%v", res.ItemID, err)
	}

	// 没有编号占用时，持有人问题按清单顺序报告最前的那件，条目的先后优先
	// 于"未登记/停用"的子类型：前一件未登记、后一件停用报前一件未登记……
	r7 := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r7)
	if err := r7.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	res, err = r7.IssueBatch(batchReq("q9",
		be("i1", "ghost"), be("i2", "carol")))
	if !errors.Is(err, ErrNotFound) || res.ItemID != "i1" {
		t.Fatalf("earlier missing holder must be reported first, got item=%q err=%v", res.ItemID, err)
	}
	// ……反之，前一件停用、后一件未登记则报前一件停用。
	res, err = r7.IssueBatch(batchReq("q10",
		be("i3", "carol"), be("i4", "ghost")))
	if !errors.Is(err, ErrAccountInactive) || res.ItemID != "i3" {
		t.Fatalf("earlier inactive holder must be reported first, got item=%q err=%v", res.ItemID, err)
	}

	// 同类问题（编号占用）报告清单顺序最前的那件。
	r8 := mustCreate(t, tempDir(t))
	setupBatchWorld(t, r8)
	for _, id := range []string{"t1", "t2"} {
		if _, err := r8.Issue(issueReq(id, "bob")); err != nil {
			t.Fatal(err)
		}
	}
	res, err = r8.IssueBatch(batchReq("q11",
		be("new1", "ghost"), be("t1", "ghost"), be("t2", "ghost")))
	if !errors.Is(err, ErrAlreadyExists) || res.ItemID != "t1" {
		t.Fatalf("first taken id in list order must be reported, got item=%q err=%v", res.ItemID, err)
	}
}
