package registry

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// ---- 请求键的无歧义性 ----

// TestRequestKeyInjective 验证 requestKey 对含 NUL（U+0000）的账户编号与
// 请求号仍是无歧义的：题述两组内容
//   - 账户 "a"      + 请求号 "b\x00c"
//   - 账户 "a\x00b" + 请求号 "c"
//
// 在旧的单 NUL 拼接下会塌缩成同一键，修复后必须得到两个不同的键。
func TestRequestKeyInjective(t *testing.T) {
	pairs := [][2]string{
		{"a", "b\x00c"},
		{"a\x00b", "c"},
		{"", "\x00"},
		{"\x00", ""},
		{"ab\x00cd", "ef"},
		{"ab", "\x00cdef"},
		{"1\x002", "\x003\x00"},
	}
	seen := make(map[string][2]string)
	for _, p := range pairs {
		k := requestKey(p[0], p[1])
		if other, dup := seen[k]; dup && other != p {
			t.Fatalf("requestKey 发生碰撞: %q 与 %q 都得到 %q", other, p, k)
		}
		seen[k] = p
		// 同一对内容必须稳定得到同一键。
		if requestKey(p[0], p[1]) != k {
			t.Fatalf("requestKey 不稳定: %q", p)
		}
	}
	if requestKey("a", "b\x00c") == requestKey("a\x00b", "c") {
		t.Fatal("题述的 NUL 混淆对仍被认成同一请求")
	}
}

// ---- 不同账户的请求号互不占用 ----

// nulWorld 登记含 NUL 的账户甲 "a"、账户乙 "a\x00b" 与接收人 bob，
// 并分别由甲、乙创建各自的系列。
func nulWorld(t *testing.T, r *Registry) {
	t.Helper()
	for _, id := range []string{"a", "a\x00b", "bob"} {
		if err := r.RegisterAccount(id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.CreateSeries("sA", "a", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateSeries("sB", "a\x00b", ""); err != nil {
		t.Fatal(err)
	}
}

func nulIssueReq(op, rid, item, series, holder string) IssueRequest {
	return IssueRequest{
		Operator: op, Reason: "发行", RequestID: rid,
		ItemID: item, SeriesID: series, BatchNo: "batch", Metadata: "元数据", HolderID: holder,
	}
}

// TestIssueRequestsWithNULIDsDoNotCollide 覆盖题述主场景：两位操作者各自
// 合法发行互不影响；同账户同号重提回放自己并标明重复；改内容按
// ErrRequestConflict 拒绝；另一账户提交相同业务内容时按自身业务权限处理，
// 不回放别人的成功。
func TestIssueRequestsWithNULIDsDoNotCollide(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	nulWorld(t, r)

	// 账户甲：编号 "a"，请求号 "b\x00c"，发行藏品 x。
	reqA := nulIssueReq("a", "b\x00c", "x", "sA", "a")
	resA, err := r.Issue(reqA)
	if err != nil || resA.ItemID != "x" || resA.TxSeq != 1 || resA.Replayed {
		t.Fatalf("账户甲首次发行: %+v %v", resA, err)
	}

	// 账户乙：编号 "a\x00b"，请求号 "c"。旧键与甲完全相同，
	// 修复后必须视为独立请求：合法发行藏品 y，取得自己的历史序号。
	reqB := nulIssueReq("a\x00b", "c", "y", "sB", "a\x00b")
	resB, err := r.Issue(reqB)
	if err != nil || resB.ItemID != "y" || resB.TxSeq != 2 || resB.Replayed {
		t.Fatalf("账户乙首次发行不能被账户甲占用请求号: %+v %v", resB, err)
	}

	// 各自用原参数重提，只回放自己的首次成功并标明重复，不新增藏品与历史。
	againA, err := r.Issue(reqA)
	if err != nil || !againA.Replayed || againA.ItemID != "x" || againA.TxSeq != 1 {
		t.Fatalf("账户甲回放自己的发行: %+v %v", againA, err)
	}
	againB, err := r.Issue(reqB)
	if err != nil || !againB.Replayed || againB.ItemID != "y" || againB.TxSeq != 2 {
		t.Fatalf("账户乙回放自己的发行: %+v %v", againB, err)
	}
	if h, _ := r.GetHolding("x"); h.OwnerID != "a" || h.Version != 1 {
		t.Fatalf("藏品 x 持有状态异常: %+v", h)
	}
	if h, _ := r.GetHolding("y"); h.OwnerID != "a\x00b" || h.Version != 1 {
		t.Fatalf("藏品 y 持有状态异常: %+v", h)
	}

	// 同一账户同一请求号改用于另一件藏品 → ErrRequestConflict。
	conflict := reqA
	conflict.ItemID = "z"
	if _, err := r.Issue(conflict); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("同账户改内容应按请求号冲突拒绝，got %v", err)
	}
	// 乙把同一请求号用于别的操作内容同样冲突。
	conflictB := reqB
	conflictB.Metadata = "不同内容"
	if _, err := r.Issue(conflictB); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("账户乙改内容应按请求号冲突拒绝，got %v", err)
	}
	// 被冲突拒绝不影响原回放：甲的原请求仍回放 x。
	if again, err := r.Issue(reqA); err != nil || !again.Replayed || again.ItemID != "x" {
		t.Fatalf("冲突后原请求回放异常: %+v %v", again, err)
	}
}

// TestIssueSameContentAcrossNULAccountsNotReplayed 验证另一账户用相同请求号
// 提交与已有请求完全相同的业务内容时，按自己的账户状态与业务权限处理
// （乙不是 sA 的创建账户 → ErrForbidden），不能回放甲的成功；该拒绝归乙
// 自己所有，甲的成功回放不受影响。
func TestIssueSameContentAcrossNULAccountsNotReplayed(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	nulWorld(t, r)

	reqA := nulIssueReq("a", "b\x00c", "x", "sA", "bob")
	if _, err := r.Issue(reqA); err != nil {
		t.Fatal(err)
	}

	// 乙用与甲相同的业务参数（藏品、系列、批次、持有人、原因全部相同）和
	// 旧实现下相同的键提交：必须重新经过乙自己的业务校验。
	reqBSame := nulIssueReq("a\x00b", "c", "x", "sA", "bob")
	res, err := r.Issue(reqBSame)
	if !errors.Is(err, ErrForbidden) || res.Replayed {
		t.Fatalf("乙不能回放甲的成功，应按自身权限被拒绝: %+v %v", res, err)
	}
	// 重提回放的是乙自己的状态类拒绝，仍标明重复。
	again, err := r.Issue(reqBSame)
	if !errors.Is(err, ErrForbidden) || !again.Replayed {
		t.Fatalf("乙应回放自己的拒绝: %+v %v", again, err)
	}
	// 甲仍回放自己的成功。
	againA, err := r.Issue(reqA)
	if err != nil || !againA.Replayed || againA.ItemID != "x" {
		t.Fatalf("甲的成功回放不能被乙影响: %+v %v", againA, err)
	}
	// 乙换一个请求号即可合法发行自己的藏品。
	reqBLegit := nulIssueReq("a\x00b", "c2", "y", "sB", "a\x00b")
	resB, err := r.Issue(reqBLegit)
	if err != nil || resB.ItemID != "y" || resB.Replayed {
		t.Fatalf("乙的合法发行: %+v %v", resB, err)
	}
}

// TestTransferRequestsWithNULIDsDoNotCollide 验证转让同样按完整账户编号与
// 请求号区分：两位持有人用旧键相同的请求号分别转让各自藏品，都应成功并
// 取得各自的转让结果。
func TestTransferRequestsWithNULIDsDoNotCollide(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	nulWorld(t, r)

	if _, err := r.Issue(nulIssueReq("a", "issue-x", "x", "sA", "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Issue(nulIssueReq("a\x00b", "issue-y", "y", "sB", "a\x00b")); err != nil {
		t.Fatal(err)
	}

	// op "x"-形碰撞对：账户 "a" + 请求号 "b\x00c" 与账户 "a\x00b" + 请求号 "c"。
	tA := TransferRequest{
		Operator: "a", Reason: "转让", RequestID: "b\x00c",
		ItemID: "x", ExpectedOwner: "a", ExpectedVer: 1, ToID: "bob",
	}
	tB := TransferRequest{
		Operator: "a\x00b", Reason: "转让", RequestID: "c",
		ItemID: "y", ExpectedOwner: "a\x00b", ExpectedVer: 1, ToID: "bob",
	}
	resA, err := r.Transfer(tA)
	if err != nil || resA.ItemID != "x" || resA.TxSeq == 0 || resA.Replayed {
		t.Fatalf("甲转让 x: %+v %v", resA, err)
	}
	resB, err := r.Transfer(tB)
	if err != nil || resB.ItemID != "y" || resB.TxSeq == 0 || resB.Replayed {
		t.Fatalf("乙转让 y 不能被甲占用请求号: %+v %v", resB, err)
	}
	if resA.TxSeq == resB.TxSeq {
		t.Fatalf("两笔转让应取得不同历史序号: %d", resA.TxSeq)
	}
	if h, _ := r.GetHolding("x"); h.OwnerID != "bob" || h.Version != 2 {
		t.Fatalf("x 持有状态异常: %+v", h)
	}
	if h, _ := r.GetHolding("y"); h.OwnerID != "bob" || h.Version != 2 {
		t.Fatalf("y 持有状态异常: %+v", h)
	}
	// 即使藏品已易手，各自重提仍回放自己的首次成功。
	againA, err := r.Transfer(tA)
	if err != nil || !againA.Replayed || againA.ItemID != "x" || againA.ToID != "bob" {
		t.Fatalf("甲回放自己的转让: %+v %v", againA, err)
	}
	againB, err := r.Transfer(tB)
	if err != nil || !againB.Replayed || againB.ItemID != "y" || againB.ToID != "bob" {
		t.Fatalf("乙回放自己的转让: %+v %v", againB, err)
	}
}

// ---- 修复前快照的兼容与迁移 ----

// TestMigrateLegacyNULCollisionRequests 手写一份修复前的快照：请求表使用
// 旧的单 NUL 拼接键（甲乙两组内容在旧格式下共享同一键，表中只可能留下
// 先占账户甲的记录）。打开后：
//   - 甲用原参数仍取回自己的首次成功，记录仍归甲所有；
//   - 乙的 (账户, 请求号) 已独立，可正常完成自己的发行，不覆盖甲的记录；
//   - 重开后双方回放各自的结果，新键落盘为 requests_v2，旧 requests 不再写出；
//   - 已有藏品、持有与历史保持原样。
func TestMigrateLegacyNULCollisionRequests(t *testing.T) {
	dir := tempDir(t)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatal(err)
	}

	reqA := nulIssueReq("a", "b\x00c", "x", "sA", "a")
	// 修复前的键：操作者与请求号用单个 NUL 直接拼接。
	legacyKey := "a" + "\x00" + "b\x00c"
	legacy := map[string]any{
		"version": 1,
		"accounts": map[string]account{
			"a":      {ID: "a", Active: true},
			"a\x00b": {ID: "a\x00b", Active: true},
		},
		"series": map[string]series{
			"sA": {ID: "sA", CreatorID: "a"},
		},
		"items": map[string]item{
			"x": {ID: "x", SeriesID: "sA", BatchNo: "batch", IssuedTxID: 1},
		},
		"holdings": map[string]holding{
			"x": {ItemID: "x", OwnerID: "a", Version: 1},
		},
		"history": []historyEntry{
			{Seq: 1, Kind: "issue", ItemID: "x", Operator: "a", Reason: "发行",
				RequestID: "b\x00c", ToID: "a", FromVersion: 0, ToVersion: 1},
		},
		"requests": map[string]request{
			legacyKey: {
				Operator: "a", RequestID: "b\x00c", Kind: "issue",
				Params: issueParamsSig(reqA), ItemID: "x",
				ToID: "a", Version: 1, TxSeq: 1,
			},
		},
		"next_seq": 1,
	}
	b, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataFile(dir), b, fileMode); err != nil {
		t.Fatal(err)
	}

	r, err := Open(dir)
	if err != nil {
		t.Fatalf("含 NUL 请求记录的旧登记册必须能打开: %v", err)
	}

	// 甲的旧请求仍归甲所有，原参数取回原结果。
	againA, err := r.Issue(reqA)
	if err != nil || !againA.Replayed || againA.ItemID != "x" || againA.TxSeq != 1 {
		t.Fatalf("甲的旧请求必须原样回放: %+v %v", againA, err)
	}

	// 乙补齐自己的系列后发行——其 (账户, 请求号) 与甲独立，不能被占用或覆盖。
	if err := r.CreateSeries("sB", "a\x00b", ""); err != nil {
		t.Fatal(err)
	}
	reqB := nulIssueReq("a\x00b", "c", "y", "sB", "a\x00b")
	resB, err := r.Issue(reqB)
	if err != nil || resB.Replayed || resB.ItemID != "y" || resB.TxSeq != 2 {
		t.Fatalf("乙的混淆对必须独立处理，完成自己的发行: %+v %v", resB, err)
	}
	// 甲的藏品、持有与历史保持原样，乙的发行只新增自己的记录。
	if h, _ := r.GetHolding("x"); h.OwnerID != "a" || h.Version != 1 {
		t.Fatalf("甲的持有关系被迁移改变: %+v", h)
	}
	if hist, _ := r.History("x"); len(hist) != 1 || hist[0].RequestID != "b\x00c" {
		t.Fatalf("甲的历史被改变: %+v", hist)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// 重开后双方各自回放，互不影响。
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	againA2, err := r2.Issue(reqA)
	if err != nil || !againA2.Replayed || againA2.ItemID != "x" || againA2.TxSeq != 1 {
		t.Fatalf("重开后甲仍回放自己的结果: %+v %v", againA2, err)
	}
	againB2, err := r2.Issue(reqB)
	if err != nil || !againB2.Replayed || againB2.ItemID != "y" || againB2.TxSeq != 2 {
		t.Fatalf("重开后乙仍回放自己的结果: %+v %v", againB2, err)
	}

	// 落盘内容应使用无歧义新键，旧的单 NUL 键表不再写出。
	raw, err := os.ReadFile(dataFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"requests_v2"`) {
		t.Fatal("迁移后的请求表应以 requests_v2 落盘")
	}
	if strings.Contains(string(raw), "\n  \"requests\":") {
		t.Fatal("旧 requests 表不应继续写出")
	}
}

// TestRejectedRequestWithNULIDsPersistsAndReplays 验证状态类拒绝也按账户
// 区分归属：甲的停用拒绝回放与乙的同请求号无关。
func TestRejectedRequestWithNULIDsPersistsAndReplays(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	nulWorld(t, r)

	// 甲先发行 x，然后停用自己；甲用含 NUL 的请求号再次发行 → 状态类拒绝，
	// 占用甲的请求号并落盘。
	if _, err := r.Issue(nulIssueReq("a", "first", "x", "sA", "a")); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("a"); err != nil {
		t.Fatal(err)
	}
	reqARej := nulIssueReq("a", "b\x00c", "x2", "sA", "a")
	if _, err := r.Issue(reqARej); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("甲停用后发行应被拒绝: %v", err)
	}

	// 乙用旧键相同的请求号发行自己的藏品：按乙自己的可用状态处理，成功。
	reqB := nulIssueReq("a\x00b", "c", "y", "sB", "a\x00b")
	resB, err := r.Issue(reqB)
	if err != nil || resB.Replayed || resB.ItemID != "y" {
		t.Fatalf("乙不能回放甲的拒绝，应按自己的状态成功发行: %+v %v", resB, err)
	}
	// 甲重提仍回放自己的停用拒绝；乙重提回放自己的成功，即使甲已停用。
	if res, err := r.Issue(reqARej); !errors.Is(err, ErrAccountInactive) || !res.Replayed {
		t.Fatalf("甲应回放自己的拒绝: %+v %v", res, err)
	}
	if res, err := r.Issue(reqB); err != nil || !res.Replayed || res.ItemID != "y" {
		t.Fatalf("乙应回放自己的成功: %+v %v", res, err)
	}
}
