package registry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// newTestRegistry 创建一个临时目录中的登记册，并预置两个可用账户 alice、bob。
func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "reg")
	r, err := Create(dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	seedAccount(t, r, "alice")
	seedAccount(t, r, "bob")
	return r
}

func seedAccount(t *testing.T, r *Registry, id string) {
	t.Helper()
	_, err := r.RegisterAccount(RegisterAccountRequest{
		Operator: "op-seed", RequestNo: "seed-" + id, Reason: "seed", AccountID: id,
	})
	if err != nil {
		t.Fatalf("seed account %s: %v", id, err)
	}
}

func errCode(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ---------- 基本流程 ----------

func TestAccountLifecycle(t *testing.T) {
	r := newTestRegistry(t)

	acc, err := r.GetAccount("alice")
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if !acc.Active {
		t.Fatal("alice 应为可用状态")
	}

	// 重复登记
	_, err = r.RegisterAccount(RegisterAccountRequest{
		Operator: "op", RequestNo: "r1", Reason: "x", AccountID: "alice",
	})
	if errCode(err) != ErrAlreadyExists {
		t.Fatalf("重复登记应返回 already_exists，实际: %v", err)
	}

	// 不存在的账户
	_, err = r.GetAccount("nobody")
	if errCode(err) != ErrNotFound {
		t.Fatalf("查询不存在账户应返回 not_found，实际: %v", err)
	}

	// 停用后仍可查询
	deactivated, err := r.DeactivateAccount(DeactivateAccountRequest{
		Operator: "op", RequestNo: "d1", Reason: "停用", AccountID: "alice",
	})
	if err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if deactivated.Active || deactivated.DeactivatedAt == nil {
		t.Fatal("停用后账户应为不可用且有停用时间")
	}
	got, _ := r.GetAccount("alice")
	if got.Active {
		t.Fatal("停用状态应持久可见")
	}

	// 重复停用
	_, err = r.DeactivateAccount(DeactivateAccountRequest{
		Operator: "op", RequestNo: "d2", Reason: "再停用", AccountID: "alice",
	})
	if errCode(err) != ErrConflict {
		t.Fatalf("重复停用应返回 conflict，实际: %v", err)
	}
}

func TestSeriesAndIssue(t *testing.T) {
	r := newTestRegistry(t)

	_, err := r.CreateSeries(CreateSeriesRequest{
		Operator: "alice", RequestNo: "s1", Reason: "创建系列",
		SeriesID: "series-1", CreatorID: "alice",
		Metadata: map[string]string{"name": "首套", "issuer": "alice"},
	})
	if err != nil {
		t.Fatalf("CreateSeries: %v", err)
	}

	// 非创建账户不能发行
	_, err = r.Issue(IssueRequest{
		Operator: "bob", RequestNo: "i0", Reason: "冒名发行",
		ArtifactID: "art-x", SeriesID: "series-1", BatchNo: "b1", InitialHolder: "bob",
	})
	if errCode(err) != ErrForbidden {
		t.Fatalf("非创建者发行应返回 forbidden，实际: %v", err)
	}

	art, err := r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "i1", Reason: "首发",
		ArtifactID: "art-1", SeriesID: "series-1", BatchNo: "batch-1",
		Metadata: map[string]string{"name": "第一件"}, InitialHolder: "alice",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if art.Holder != "alice" || art.Version != 1 {
		t.Fatalf("发行后应持有 alice 且版本为 1，实际: holder=%s version=%d", art.Holder, art.Version)
	}

	// 重复发行同一编号
	_, err = r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "i2", Reason: "重复",
		ArtifactID: "art-1", SeriesID: "series-1", BatchNo: "batch-2", InitialHolder: "alice",
	})
	if errCode(err) != ErrAlreadyExists {
		t.Fatalf("重复发行应返回 already_exists，实际: %v", err)
	}

	// 不存在的系列
	_, err = r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "i3", Reason: "野系列",
		ArtifactID: "art-2", SeriesID: "nope", BatchNo: "b", InitialHolder: "alice",
	})
	if errCode(err) != ErrNotFound {
		t.Fatalf("不存在系列应返回 not_found，实际: %v", err)
	}

	// 不存在的初始持有人
	_, err = r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "i4", Reason: "无持有人",
		ArtifactID: "art-3", SeriesID: "series-1", BatchNo: "b", InitialHolder: "nobody",
	})
	if errCode(err) != ErrNotFound {
		t.Fatalf("不存在持有人应返回 not_found，实际: %v", err)
	}

	// 停用账户不能作为初始持有人
	_, _ = r.DeactivateAccount(DeactivateAccountRequest{
		Operator: "op", RequestNo: "d-alice", Reason: "停用 alice", AccountID: "alice",
	})
	_, err = r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "i5", Reason: "停用后发行",
		ArtifactID: "art-4", SeriesID: "series-1", BatchNo: "b", InitialHolder: "alice",
	})
	if errCode(err) != ErrInactive {
		t.Fatalf("停用账户作为持有人应返回 inactive，实际: %v", err)
	}
}

func TestTransferFlow(t *testing.T) {
	r := newTestRegistry(t)
	_, _ = r.CreateSeries(CreateSeriesRequest{
		Operator: "alice", RequestNo: "s1", Reason: "系列",
		SeriesID: "s", CreatorID: "alice",
	})
	_, _ = r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "i1", Reason: "发行",
		ArtifactID: "a", SeriesID: "s", BatchNo: "b", InitialHolder: "alice",
	})

	// 版本错误
	_, err := r.Transfer(TransferRequest{
		Operator: "alice", RequestNo: "t0", Reason: "抢跑",
		ArtifactID: "a", From: "alice", To: "bob", ExpectedVersion: 99,
	})
	if errCode(err) != ErrVersionMismatch {
		t.Fatalf("版本不符应返回 version_mismatch，实际: %v", err)
	}

	// 持有人错误
	_, err = r.Transfer(TransferRequest{
		Operator: "bob", RequestNo: "t1", Reason: "错持有人",
		ArtifactID: "a", From: "bob", To: "alice", ExpectedVersion: 1,
	})
	if errCode(err) != ErrHolderMismatch {
		t.Fatalf("持有人不符应返回 holder_mismatch，实际: %v", err)
	}

	// 自我转让
	_, err = r.Transfer(TransferRequest{
		Operator: "alice", RequestNo: "t2", Reason: "自转",
		ArtifactID: "a", From: "alice", To: "alice", ExpectedVersion: 1,
	})
	if errCode(err) != ErrConflict {
		t.Fatalf("自我转让应返回 conflict，实际: %v", err)
	}

	// 不存在的藏品
	_, err = r.Transfer(TransferRequest{
		Operator: "alice", RequestNo: "t3", Reason: "无藏品",
		ArtifactID: "ghost", From: "alice", To: "bob", ExpectedVersion: 1,
	})
	if errCode(err) != ErrNotFound {
		t.Fatalf("不存在藏品应返回 not_found，实际: %v", err)
	}

	// 不存在的接收人
	_, err = r.Transfer(TransferRequest{
		Operator: "alice", RequestNo: "t4", Reason: "收不到",
		ArtifactID: "a", From: "alice", To: "nobody", ExpectedVersion: 1,
	})
	if errCode(err) != ErrNotFound {
		t.Fatalf("不存在接收人应返回 not_found，实际: %v", err)
	}

	// 成功转让
	h, err := r.Transfer(TransferRequest{
		Operator: "alice", RequestNo: "t5", Reason: "转让给 bob",
		ArtifactID: "a", From: "alice", To: "bob", ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if h.Holder != "bob" || h.Version != 2 {
		t.Fatalf("转让后应持有 bob 且版本为 2，实际: holder=%s version=%d", h.Holder, h.Version)
	}

	// 再转让给第三方
	_, _ = r.RegisterAccount(RegisterAccountRequest{
		Operator: "op", RequestNo: "seed-carol", Reason: "seed", AccountID: "carol",
	})
	h2, err := r.Transfer(TransferRequest{
		Operator: "bob", RequestNo: "t6", Reason: "转让给 carol",
		ArtifactID: "a", From: "bob", To: "carol", ExpectedVersion: 2,
	})
	if err != nil {
		t.Fatalf("Transfer 2: %v", err)
	}
	if h2.Holder != "carol" || h2.Version != 3 {
		t.Fatalf("再转让后应持有 carol 且版本为 3，实际: holder=%s version=%d", h2.Holder, h2.Version)
	}

	// 历史顺序与内容
	hist, err := r.GetHistory("a")
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("历史应有 3 条，实际 %d", len(hist))
	}
	want := []struct {
		op, from, to string
		version      int
	}{
		{"issue", "", "alice", 1},
		{"transfer", "alice", "bob", 2},
		{"transfer", "bob", "carol", 3},
	}
	for i, w := range want {
		if hist[i].Op != w.op || hist[i].From != w.from || hist[i].To != w.to || hist[i].Version != w.version {
			t.Fatalf("历史第 %d 条不符: %+v", i, hist[i])
		}
		if hist[i].Seq != i+1 {
			t.Fatalf("历史顺序号应为 %d，实际 %d", i+1, hist[i].Seq)
		}
		if hist[i].Operator == "" || hist[i].Reason == "" {
			t.Fatalf("历史第 %d 条缺少操作者或原因", i)
		}
	}

	// 停用账户不能接收转让
	_, _ = r.DeactivateAccount(DeactivateAccountRequest{
		Operator: "op", RequestNo: "d-bob", Reason: "停用 bob", AccountID: "bob",
	})
	_, err = r.Transfer(TransferRequest{
		Operator: "carol", RequestNo: "t7", Reason: "转给停用者",
		ArtifactID: "a", From: "carol", To: "bob", ExpectedVersion: 3,
	})
	if errCode(err) != ErrInactive {
		t.Fatalf("停用账户接收转让应返回 inactive，实际: %v", err)
	}
}

func TestSealedSeriesBlocksIssue(t *testing.T) {
	r := newTestRegistry(t)
	_, _ = r.CreateSeries(CreateSeriesRequest{
		Operator: "alice", RequestNo: "s1", Reason: "系列",
		SeriesID: "s", CreatorID: "alice",
	})
	_, _ = r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "i1", Reason: "发行",
		ArtifactID: "a1", SeriesID: "s", BatchNo: "b", InitialHolder: "alice",
	})

	// 封存
	_, err := r.SealSeries(SealSeriesRequest{
		Operator: "alice", RequestNo: "seal1", Reason: "封存", SeriesID: "s",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// 非创建者不能封存
	_, err = r.SealSeries(SealSeriesRequest{
		Operator: "bob", RequestNo: "seal2", Reason: "冒封", SeriesID: "s",
	})
	if errCode(err) != ErrForbidden {
		t.Fatalf("非创建者封存应返回 forbidden，实际: %v", err)
	}
	// 重复封存
	_, err = r.SealSeries(SealSeriesRequest{
		Operator: "alice", RequestNo: "seal3", Reason: "再封", SeriesID: "s",
	})
	if errCode(err) != ErrConflict {
		t.Fatalf("重复封存应返回 conflict，实际: %v", err)
	}
	// 封存后不能发行
	_, err = r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "i2", Reason: "封存后发行",
		ArtifactID: "a2", SeriesID: "s", BatchNo: "b", InitialHolder: "alice",
	})
	if errCode(err) != ErrSealed {
		t.Fatalf("封存后发行应返回 sealed，实际: %v", err)
	}
	// 已发行藏品仍可转让
	h, err := r.Transfer(TransferRequest{
		Operator: "alice", RequestNo: "t1", Reason: "封存后转让",
		ArtifactID: "a1", From: "alice", To: "bob", ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("封存后已发行藏品应可转让: %v", err)
	}
	if h.Holder != "bob" || h.Version != 2 {
		t.Fatalf("转让后应为 bob/2，实际: %+v", h)
	}
}

func TestMissingRequiredFields(t *testing.T) {
	r := newTestRegistry(t)
	cases := []struct {
		name string
		fn   func(no string) error
	}{
		{"登记账户缺编号", func(no string) error {
			_, e := r.RegisterAccount(RegisterAccountRequest{Operator: "o", RequestNo: no, Reason: "x"})
			return e
		}},
		{"创建系列缺编号", func(no string) error {
			_, e := r.CreateSeries(CreateSeriesRequest{Operator: "o", RequestNo: no, Reason: "x", CreatorID: "alice"})
			return e
		}},
		{"发行缺藏品编号", func(no string) error {
			_, e := r.Issue(IssueRequest{Operator: "o", RequestNo: no, Reason: "x", SeriesID: "s", BatchNo: "b", InitialHolder: "alice"})
			return e
		}},
		{"发行缺系列", func(no string) error {
			_, e := r.Issue(IssueRequest{Operator: "o", RequestNo: no, Reason: "x", ArtifactID: "a", BatchNo: "b", InitialHolder: "alice"})
			return e
		}},
		{"发行缺批次号", func(no string) error {
			_, e := r.Issue(IssueRequest{Operator: "o", RequestNo: no, Reason: "x", ArtifactID: "a", SeriesID: "s", InitialHolder: "alice"})
			return e
		}},
		{"发行缺持有人", func(no string) error {
			_, e := r.Issue(IssueRequest{Operator: "o", RequestNo: no, Reason: "x", ArtifactID: "a", SeriesID: "s", BatchNo: "b"})
			return e
		}},
		{"转让缺藏品", func(no string) error {
			_, e := r.Transfer(TransferRequest{Operator: "o", RequestNo: no, Reason: "x", From: "alice", To: "bob", ExpectedVersion: 1})
			return e
		}},
		{"转让缺接收人", func(no string) error {
			_, e := r.Transfer(TransferRequest{Operator: "o", RequestNo: no, Reason: "x", ArtifactID: "a", From: "alice", ExpectedVersion: 1})
			return e
		}},
		{"转让版本为0", func(no string) error {
			_, e := r.Transfer(TransferRequest{Operator: "o", RequestNo: no, Reason: "x", ArtifactID: "a", From: "alice", To: "bob", ExpectedVersion: 0})
			return e
		}},
		{"缺操作者", func(no string) error {
			_, e := r.RegisterAccount(RegisterAccountRequest{RequestNo: no, Reason: "x", AccountID: "z"})
			return e
		}},
		{"缺请求号", func(no string) error {
			_, e := r.RegisterAccount(RegisterAccountRequest{Operator: "o", Reason: "x", AccountID: "z"})
			return e
		}},
		{"缺原因", func(no string) error {
			_, e := r.RegisterAccount(RegisterAccountRequest{Operator: "o", RequestNo: no, AccountID: "z"})
			return e
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if errCode(c.fn(fmt.Sprintf("mr-%d", i))) != ErrInvalidRequest {
				t.Fatalf("应返回 invalid_request，实际: %v", c.fn(fmt.Sprintf("mr-%d", i)))
			}
		})
	}
}

// ---------- 幂等 ----------

func TestIdempotentRetry(t *testing.T) {
	r := newTestRegistry(t)

	// 首次登记
	acc1, err := r.RegisterAccount(RegisterAccountRequest{
		Operator: "op", RequestNo: "r1", Reason: "登记 carol", AccountID: "carol",
	})
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	// 同参数重试：返回首次结果，不产生副作用
	acc2, err := r.RegisterAccount(RegisterAccountRequest{
		Operator: "op", RequestNo: "r1", Reason: "登记 carol", AccountID: "carol",
	})
	if err != nil {
		t.Fatalf("retry register: %v", err)
	}
	if acc1.ID != acc2.ID || acc1.CreatedAt != acc2.CreatedAt {
		t.Fatalf("重试应返回相同结果: %+v vs %+v", acc1, acc2)
	}

	// 业务拒绝也被保存并重放：发行引用不存在的系列
	_, err = r.Issue(IssueRequest{
		Operator: "op", RequestNo: "r2", Reason: "失败请求",
		ArtifactID: "bad", SeriesID: "ghost", BatchNo: "b", InitialHolder: "carol",
	})
	if errCode(err) != ErrNotFound {
		t.Fatalf("首次应 not_found: %v", err)
	}
	// 此后即使系列被创建，同请求号重试仍返回首次的拒绝
	_, _ = r.CreateSeries(CreateSeriesRequest{
		Operator: "carol", RequestNo: "s-ghost", Reason: "补建系列",
		SeriesID: "ghost", CreatorID: "carol",
	})
	_, err = r.Issue(IssueRequest{
		Operator: "op", RequestNo: "r2", Reason: "失败请求",
		ArtifactID: "bad", SeriesID: "ghost", BatchNo: "b", InitialHolder: "carol",
	})
	if errCode(err) != ErrNotFound {
		t.Fatalf("重试应返回首次拒绝 not_found，实际: %v", err)
	}
	// 失败请求没有改变任何东西
	if _, err := r.GetArtifact("bad"); errCode(err) != ErrNotFound {
		t.Fatalf("失败请求不应产生藏品")
	}
}

func TestRequestConflict(t *testing.T) {
	r := newTestRegistry(t)
	_, _ = r.CreateSeries(CreateSeriesRequest{
		Operator: "alice", RequestNo: "s1", Reason: "系列",
		SeriesID: "s", CreatorID: "alice",
	})

	// 发行成功后，同一请求号用于不同业务参数 → 冲突
	_, err := r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "rx", Reason: "发行",
		ArtifactID: "a1", SeriesID: "s", BatchNo: "b", InitialHolder: "alice",
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	_, err = r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "rx", Reason: "发行",
		ArtifactID: "a2", SeriesID: "s", BatchNo: "b", InitialHolder: "alice",
	})
	if errCode(err) != ErrRequestConflict {
		t.Fatalf("不同业务参数应返回 request_conflict，实际: %v", err)
	}

	// 同一请求号在发行与转让之间共用
	_, err = r.Transfer(TransferRequest{
		Operator: "alice", RequestNo: "rx", Reason: "转让",
		ArtifactID: "a1", From: "alice", To: "bob", ExpectedVersion: 1,
	})
	if errCode(err) != ErrRequestConflict {
		t.Fatalf("发行用过的请求号再用于转让应返回 request_conflict，实际: %v", err)
	}

	// 即使藏品后来换了持有人，重试仍返回首次成功结果
	h1, err := r.Transfer(TransferRequest{
		Operator: "alice", RequestNo: "t1", Reason: "首次转让",
		ArtifactID: "a1", From: "alice", To: "bob", ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	// a1 现在由 bob 持有；alice 再用同请求号重试（参数与首次相同）
	h2, err := r.Transfer(TransferRequest{
		Operator: "alice", RequestNo: "t1", Reason: "首次转让",
		ArtifactID: "a1", From: "alice", To: "bob", ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("重试应返回首次成功结果: %v", err)
	}
	if h2.Holder != h1.Holder || h2.Version != h1.Version {
		t.Fatalf("重试结果应与首次一致: %+v vs %+v", h1, h2)
	}
	// 历史只增加一条
	hist, _ := r.GetHistory("a1")
	if len(hist) != 2 {
		t.Fatalf("重复提交不应增加历史，实际 %d 条", len(hist))
	}
}

// ---------- 持久化 ----------

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reg")
	r, err := Create(dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, _ = r.RegisterAccount(RegisterAccountRequest{Operator: "op", RequestNo: "ra", Reason: "x", AccountID: "alice"})
	_, _ = r.RegisterAccount(RegisterAccountRequest{Operator: "op", RequestNo: "rb", Reason: "x", AccountID: "bob"})
	_, _ = r.CreateSeries(CreateSeriesRequest{
		Operator: "alice", RequestNo: "s1", Reason: "系列",
		SeriesID: "s", CreatorID: "alice", Metadata: map[string]string{"k": "v"},
	})
	_, _ = r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "i1", Reason: "发行",
		ArtifactID: "a", SeriesID: "s", BatchNo: "b1",
		Metadata: map[string]string{"m": "n"}, InitialHolder: "alice",
	})
	_, _ = r.Transfer(TransferRequest{
		Operator: "alice", RequestNo: "t1", Reason: "转让",
		ArtifactID: "a", From: "alice", To: "bob", ExpectedVersion: 1,
	})
	_, _ = r.DeactivateAccount(DeactivateAccountRequest{Operator: "op", RequestNo: "d1", Reason: "停用", AccountID: "alice"})
	_, _ = r.SealSeries(SealSeriesRequest{Operator: "alice", RequestNo: "seal1", Reason: "封存", SeriesID: "s"})
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r2.Close() }()

	// 账户
	acc, err := r2.GetAccount("alice")
	if err != nil || acc.Active {
		t.Fatalf("停用状态应保留: %+v err=%v", acc, err)
	}
	// 系列
	s, err := r2.GetSeries("s")
	if err != nil || !s.Sealed || s.Metadata["k"] != "v" {
		t.Fatalf("系列封存与元数据应保留: %+v err=%v", s, err)
	}
	// 藏品与持有
	art, err := r2.GetArtifact("a")
	if err != nil || art.Holder != "bob" || art.Version != 2 || art.Metadata["m"] != "n" {
		t.Fatalf("藏品状态应保留: %+v err=%v", art, err)
	}
	h, err := r2.GetHolding("a")
	if err != nil || h.Holder != "bob" || h.Version != 2 {
		t.Fatalf("持有应保留: %+v err=%v", h, err)
	}
	// 历史
	hist, err := r2.GetHistory("a")
	if err != nil || len(hist) != 2 {
		t.Fatalf("历史应保留 2 条: %+v err=%v", hist, err)
	}
	// 请求结果保留：同请求号重试返回首次结果
	h2, err := r2.Transfer(TransferRequest{
		Operator: "alice", RequestNo: "t1", Reason: "转让",
		ArtifactID: "a", From: "alice", To: "bob", ExpectedVersion: 1,
	})
	if err != nil || h2.Version != 2 {
		t.Fatalf("重开后重试应返回已保存结果: %+v err=%v", h2, err)
	}
	// 封存后不能发行（状态保留）
	_, err = r2.Issue(IssueRequest{
		Operator: "bob", RequestNo: "i2", Reason: "发行",
		ArtifactID: "a2", SeriesID: "s", BatchNo: "b", InitialHolder: "bob",
	})
	if errCode(err) != ErrSealed {
		t.Fatalf("重开后封存状态应阻止发行: %v", err)
	}
}

func TestOpenMissingAndCorrupt(t *testing.T) {
	// 不存在的目录
	if _, err := Open(filepath.Join(t.TempDir(), "nope")); errCode(err) != ErrNotFound {
		t.Fatalf("打开不存在目录应返回 not_found: %v", err)
	}

	// 空目录（没有清单）
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(empty); errCode(err) != ErrCorruptData {
		t.Fatalf("打开空目录应返回 corrupt_data: %v", err)
	}

	// 损坏的清单
	bad := filepath.Join(t.TempDir(), "bad")
	r, err := Create(bad)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if err := os.WriteFile(filepath.Join(bad, manifestFileName), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(bad); errCode(err) != ErrCorruptData {
		t.Fatalf("损坏清单应返回 corrupt_data: %v", err)
	}

	// 损坏的日志（中间位）
	badlog := filepath.Join(t.TempDir(), "badlog")
	rl, err := Create(badlog)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = rl.RegisterAccount(RegisterAccountRequest{Operator: "o", RequestNo: "r", Reason: "x", AccountID: "alice"})
	_ = rl.Close()
	logPath := filepath.Join(badlog, logFileName)
	data, _ := os.ReadFile(logPath)
	// 在第一条记录之后插入垃圾
	corrupted := append(append([]byte{}, data[:len(data)/2]...), []byte("{garbage}\n")...)
	corrupted = append(corrupted, data[len(data)/2:]...)
	if err := os.WriteFile(logPath, corrupted, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(badlog); errCode(err) != ErrCorruptData {
		t.Fatalf("损坏日志应返回 corrupt_data: %v", err)
	}
}

func TestTornWriteRecovery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reg")
	r, err := Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = r.RegisterAccount(RegisterAccountRequest{Operator: "o", RequestNo: "r1", Reason: "x", AccountID: "alice"})
	_, _ = r.RegisterAccount(RegisterAccountRequest{Operator: "o", RequestNo: "r2", Reason: "x", AccountID: "bob"})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// 追写一条“撕裂”的尾部记录（不完整、无换行），模拟进程在写入途中被终止
	logPath := filepath.Join(dir, logFileName)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(`{"v":1,"hash":"deadbeef","record":{"kind":"account_register","operator":"o","request_no":"r3"`)); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("含撕裂尾部的日志应能打开并恢复: %v", err)
	}
	defer func() { _ = r2.Close() }()

	// 已有状态完好
	if _, err := r2.GetAccount("alice"); err != nil {
		t.Fatalf("已有账户应保留: %v", err)
	}
	// 撕裂的请求没有生效：原请求号重试应完整执行一次
	acc, err := r2.RegisterAccount(RegisterAccountRequest{
		Operator: "o", RequestNo: "r3", Reason: "x", AccountID: "carol",
	})
	if err != nil {
		t.Fatalf("撕裂后重试应完整执行: %v", err)
	}
	if acc.ID != "carol" {
		t.Fatalf("重试结果不符: %+v", acc)
	}
	// 再重试返回已保存结果
	acc2, err := r2.RegisterAccount(RegisterAccountRequest{
		Operator: "o", RequestNo: "r3", Reason: "x", AccountID: "carol",
	})
	if err != nil || acc2.ID != "carol" {
		t.Fatalf("重试应返回已保存结果: %+v err=%v", acc2, err)
	}
}

// ---------- 并发 ----------

func TestConcurrentTransfersSameVersion(t *testing.T) {
	r := newTestRegistry(t)
	_, _ = r.CreateSeries(CreateSeriesRequest{
		Operator: "alice", RequestNo: "s1", Reason: "系列",
		SeriesID: "s", CreatorID: "alice",
	})
	_, _ = r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "i1", Reason: "发行",
		ArtifactID: "a", SeriesID: "s", BatchNo: "b", InitialHolder: "alice",
	})

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := r.Transfer(TransferRequest{
				Operator: "alice", RequestNo: fmt.Sprintf("ct-%d", i), Reason: "并发转让",
				ArtifactID: "a", From: "alice", To: "bob", ExpectedVersion: 1,
			})
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if success != 1 {
		t.Fatalf("同一版本的并发转让应最多一笔成功，实际成功 %d 笔", success)
	}
	h, err := r.GetHolding("a")
	if err != nil || h.Holder != "bob" || h.Version != 2 {
		t.Fatalf("成功后持有应为 bob/2: %+v err=%v", h, err)
	}
	hist, _ := r.GetHistory("a")
	if len(hist) != 2 {
		t.Fatalf("历史应只有 2 条（发行+一次转让），实际 %d", len(hist))
	}
}

func TestConcurrentSealAndIssue(t *testing.T) {
	for iter := 0; iter < 10; iter++ {
		dir := filepath.Join(t.TempDir(), "reg")
		r, err := Create(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = r.RegisterAccount(RegisterAccountRequest{Operator: "o", RequestNo: "ra", Reason: "x", AccountID: "alice"})
		_, _ = r.CreateSeries(CreateSeriesRequest{
			Operator: "alice", RequestNo: "s1", Reason: "系列",
			SeriesID: "s", CreatorID: "alice",
		})

		var wg sync.WaitGroup
		wg.Add(2)
		var issueErr, sealErr error
		go func() {
			defer wg.Done()
			_, sealErr = r.SealSeries(SealSeriesRequest{Operator: "alice", RequestNo: "seal", Reason: "封存", SeriesID: "s"})
		}()
		go func() {
			defer wg.Done()
			_, issueErr = r.Issue(IssueRequest{
				Operator: "alice", RequestNo: "issue", Reason: "发行",
				ArtifactID: "a", SeriesID: "s", BatchNo: "b", InitialHolder: "alice",
			})
		}()
		wg.Wait()

		if sealErr != nil {
			t.Fatalf("迭代 %d: 封存失败: %v", iter, sealErr)
		}
		s, _ := r.GetSeries("s")
		_, artErr := r.GetArtifact("a")
		artExists := artErr == nil

		// 不变量：发行成功 ⇒ 藏品存在；发行被拒（sealed）⇒ 系列必已封存
		if issueErr == nil && !artExists {
			t.Fatalf("迭代 %d: 发行成功但藏品不存在", iter)
		}
		if issueErr != nil && errCode(issueErr) == ErrSealed && !s.Sealed {
			t.Fatalf("迭代 %d: 发行被拒但系列未封存", iter)
		}
		if artExists && s.Sealed {
			// 封存与发行都发生了：全序一致，藏品存在且系列已封存，合法
		}
		_ = r.Close()
	}
}

func TestConcurrentDeactivateAndTransfer(t *testing.T) {
	for iter := 0; iter < 10; iter++ {
		dir := filepath.Join(t.TempDir(), "reg")
		r, err := Create(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = r.RegisterAccount(RegisterAccountRequest{Operator: "o", RequestNo: "ra", Reason: "x", AccountID: "alice"})
		_, _ = r.RegisterAccount(RegisterAccountRequest{Operator: "o", RequestNo: "rb", Reason: "x", AccountID: "bob"})
		_, _ = r.CreateSeries(CreateSeriesRequest{
			Operator: "alice", RequestNo: "s1", Reason: "系列",
			SeriesID: "s", CreatorID: "alice",
		})
		_, _ = r.Issue(IssueRequest{
			Operator: "alice", RequestNo: "i1", Reason: "发行",
			ArtifactID: "a", SeriesID: "s", BatchNo: "b", InitialHolder: "alice",
		})

		var wg sync.WaitGroup
		wg.Add(2)
		var transferErr, deactErr error
		go func() {
			defer wg.Done()
			_, transferErr = r.Transfer(TransferRequest{
				Operator: "alice", RequestNo: "t1", Reason: "转让",
				ArtifactID: "a", From: "alice", To: "bob", ExpectedVersion: 1,
			})
		}()
		go func() {
			defer wg.Done()
			_, deactErr = r.DeactivateAccount(DeactivateAccountRequest{
				Operator: "o", RequestNo: "d1", Reason: "停用", AccountID: "alice",
			})
		}()
		wg.Wait()

		acc, _ := r.GetAccount("alice")
		h, _ := r.GetHolding("a")

		// 不变量：转让成功 ⇒ 藏品在 bob 且版本 2；转让被拒（inactive）⇒ alice 必已停用
		if transferErr == nil {
			if h.Holder != "bob" || h.Version != 2 {
				t.Fatalf("迭代 %d: 转让成功但持有状态异常: %+v", iter, h)
			}
		} else if errCode(transferErr) == ErrInactive {
			if acc.Active {
				t.Fatalf("迭代 %d: 转让因停用被拒，但 alice 仍可用", iter)
			}
		}
		// 停用本身不应失败（账户存在）
		if deactErr != nil {
			t.Fatalf("迭代 %d: 停用失败: %v", iter, deactErr)
		}
		_ = r.Close()
	}
}

func TestDeactivatedOperatorBlocked(t *testing.T) {
	r := newTestRegistry(t)
	_, _ = r.CreateSeries(CreateSeriesRequest{
		Operator: "alice", RequestNo: "s1", Reason: "系列",
		SeriesID: "s", CreatorID: "alice",
	})
	_, _ = r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "i1", Reason: "发行",
		ArtifactID: "a", SeriesID: "s", BatchNo: "b", InitialHolder: "alice",
	})

	// 转让必须由当前持有人发起
	_, err := r.Transfer(TransferRequest{
		Operator: "bob", RequestNo: "t0", Reason: "代发起",
		ArtifactID: "a", From: "alice", To: "bob", ExpectedVersion: 1,
	})
	if errCode(err) != ErrForbidden {
		t.Fatalf("非持有人发起转让应返回 forbidden，实际: %v", err)
	}

	// 停用 alice（创建者/持有人）
	_, _ = r.DeactivateAccount(DeactivateAccountRequest{
		Operator: "op", RequestNo: "d1", Reason: "停用 alice", AccountID: "alice",
	})

	// 停用的创建者不能发行
	_, err = r.Issue(IssueRequest{
		Operator: "alice", RequestNo: "i2", Reason: "停用后发行",
		ArtifactID: "a2", SeriesID: "s", BatchNo: "b", InitialHolder: "alice",
	})
	if errCode(err) != ErrInactive {
		t.Fatalf("停用创建者发行应返回 inactive，实际: %v", err)
	}

	// 停用的持有人不能发起转让
	_, err = r.Transfer(TransferRequest{
		Operator: "alice", RequestNo: "t1", Reason: "停用后发起",
		ArtifactID: "a", From: "alice", To: "bob", ExpectedVersion: 1,
	})
	if errCode(err) != ErrInactive {
		t.Fatalf("停用持有人发起转让应返回 inactive，实际: %v", err)
	}

	// 藏品持有与历史未受影响
	h, _ := r.GetHolding("a")
	if h.Holder != "alice" || h.Version != 1 {
		t.Fatalf("失败操作不应改变持有: %+v", h)
	}
	hist, _ := r.GetHistory("a")
	if len(hist) != 1 {
		t.Fatalf("失败操作不应增加历史: %d 条", len(hist))
	}
}

func TestCloseThenOperations(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetAccount("alice"); errCode(err) != ErrClosed {
		t.Fatalf("关闭后查询应返回 closed: %v", err)
	}
	_, err := r.RegisterAccount(RegisterAccountRequest{Operator: "o", RequestNo: "r", Reason: "x", AccountID: "z"})
	if errCode(err) != ErrClosed {
		t.Fatalf("关闭后写入应返回 closed: %v", err)
	}
	// 重复关闭安全
	if err := r.Close(); err != nil {
		t.Fatalf("重复关闭应安全: %v", err)
	}
}
