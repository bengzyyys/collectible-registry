package registry

import (
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// royaltyWorld 建立 alice（创作者/持有人）、bob、carol、dave 四个可用
// 账户与系列 s1。
func royaltyWorld(t *testing.T, r *Registry) {
	t.Helper()
	setupWorld(t, r)
	for _, id := range []string{"carol", "dave"} {
		if err := r.RegisterAccount(id, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func setRoyaltyReq(seriesID, operator, rid string, entries ...RoyaltyEntry) SetRoyaltyRequest {
	return SetRoyaltyRequest{
		Operator: operator, Reason: "设置版税", RequestID: rid,
		SeriesID: seriesID, Entries: entries,
	}
}

// ---- 规则设置与查询 ----

func TestSetSeriesRoyalty(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	entries := []RoyaltyEntry{{Account: "bob", Ratio: 100}, {Account: "carol", Ratio: 200}}
	res, err := r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "set1", entries...))
	if err != nil {
		t.Fatal(err)
	}
	if res.SeriesID != "s1" || res.Replayed {
		t.Fatalf("unexpected result: %+v", res)
	}
	sr, err := r.GetSeriesRoyalty("s1")
	if err != nil {
		t.Fatal(err)
	}
	if sr.Fixed || len(sr.Entries) != 2 || sr.Entries[0] != (RoyaltyEntry{Account: "bob", Ratio: 100}) {
		t.Fatalf("rule wrong: %+v", sr)
	}
	// 变更记录包含操作者、原因、请求号与前后内容
	evs, err := r.RoyaltyHistory("s1")
	if err != nil || len(evs) != 1 {
		t.Fatalf("royalty history: %v %v", evs, err)
	}
	e := evs[0]
	if e.Operator != "alice" || e.Reason != "设置版税" || e.RequestID != "set1" ||
		len(e.FromRule) != 0 || len(e.ToRule) != 2 {
		t.Fatalf("royalty event wrong: %+v", e)
	}
}

func TestClearSeriesRoyalty(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "set1", RoyaltyEntry{Account: "bob", Ratio: 100}))
	// 空规则表示不收版税
	if _, err := r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "set2")); err != nil {
		t.Fatal(err)
	}
	sr, _ := r.GetSeriesRoyalty("s1")
	if len(sr.Entries) != 0 {
		t.Fatalf("rule should be cleared: %+v", sr)
	}
	evs, _ := r.RoyaltyHistory("s1")
	if len(evs) != 2 || len(evs[1].FromRule) != 1 || len(evs[1].ToRule) != 0 {
		t.Fatalf("clear event wrong: %+v", evs)
	}
}

func TestSetSeriesRoyaltyValidation(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	// 重复账户
	_, err := r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "v1",
		RoyaltyEntry{Account: "bob", Ratio: 100}, RoyaltyEntry{Account: "bob", Ratio: 200}))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("duplicate account must be invalid arg, got %v", err)
	}
	// 比例越界
	for _, ratio := range []int{0, -1, 10001} {
		_, err := r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "v2", RoyaltyEntry{Account: "bob", Ratio: ratio}))
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("ratio %d must be invalid arg, got %v", ratio, err)
		}
	}
	// 合计超过 10000
	_, err = r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "v3",
		RoyaltyEntry{Account: "bob", Ratio: 6000}, RoyaltyEntry{Account: "carol", Ratio: 4001}))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("sum over 10000 must be invalid arg, got %v", err)
	}
	// 空账户
	_, err = r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "v4", RoyaltyEntry{Account: "  ", Ratio: 100}))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank account must be invalid arg, got %v", err)
	}
	// 原规则不变
	sr, _ := r.GetSeriesRoyalty("s1")
	if len(sr.Entries) != 0 {
		t.Fatalf("invalid set must not change rule: %+v", sr)
	}
	// 参数错误不占用请求号：同号随后合法设置可成功
	if _, err := r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "v1", RoyaltyEntry{Account: "bob", Ratio: 100})); err != nil {
		t.Fatalf("request id must remain usable after invalid arg: %v", err)
	}
}

func TestSetSeriesRoyaltyAccountsMustExistAndActive(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	// 操作者不存在
	_, err := r.SetSeriesRoyalty(setRoyaltyReq("s1", "ghost", "x1", RoyaltyEntry{Account: "bob", Ratio: 100}))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing operator: %v", err)
	}
	// 收款账户不存在
	_, err = r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "x2", RoyaltyEntry{Account: "ghost", Ratio: 100}))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing payee: %v", err)
	}
	// 操作者停用
	r.DeactivateAccount("alice")
	_, err = r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "x3", RoyaltyEntry{Account: "bob", Ratio: 100}))
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive operator: %v", err)
	}
	// 收款账户停用
	r2 := mustCreate(t, filepath.Join(t.TempDir(), "d"))
	royaltyWorld(t, r2)
	r2.DeactivateAccount("bob")
	_, err = r2.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "x4", RoyaltyEntry{Account: "bob", Ratio: 100}))
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive payee: %v", err)
	}
}

func TestSetSeriesRoyaltyForbiddenAndSealed(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	// 非创建账户操作
	_, err := r.SetSeriesRoyalty(setRoyaltyReq("s1", "bob", "f1", RoyaltyEntry{Account: "carol", Ratio: 100}))
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-creator set must be forbidden, got %v", err)
	}
	// 封存后拒绝设置
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	_, err = r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "f2", RoyaltyEntry{Account: "bob", Ratio: 100}))
	if !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("sealed series set must fail, got %v", err)
	}
}

func TestRoyaltyFixedAfterFirstIssue(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	// 未设置即发行：固定为无版税
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	sr, _ := r.GetSeriesRoyalty("s1")
	if !sr.Fixed || len(sr.Entries) != 0 {
		t.Fatalf("rule should be fixed to no-royalty: %+v", sr)
	}
	// 发行后拒绝设置
	_, err := r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "f1", RoyaltyEntry{Account: "bob", Ratio: 100}))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("set after issue must conflict, got %v", err)
	}
	// 已有规则也不能再改
	r2 := mustCreate(t, filepath.Join(t.TempDir(), "d"))
	royaltyWorld(t, r2)
	r2.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "s1", RoyaltyEntry{Account: "bob", Ratio: 100}))
	r2.Issue(issueReq("i1", "alice"))
	_, err = r2.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "s2", RoyaltyEntry{Account: "carol", Ratio: 200}))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("rule change after issue must conflict, got %v", err)
	}
}

// 设置与首次发行同时发生：存在完整先后次序，先设置则发行适用新规则，
// 先发行则设置被拒。
func TestConcurrentSetRoyaltyVsIssue(t *testing.T) {
	for range 20 {
		r := mustCreate(t, filepath.Join(t.TempDir(), "d"))
		royaltyWorld(t, r)
		var wg sync.WaitGroup
		var setErr, issueErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, setErr = r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "s1", RoyaltyEntry{Account: "bob", Ratio: 100}))
		}()
		go func() {
			defer wg.Done()
			_, issueErr = r.Issue(issueReq("i1", "alice"))
		}()
		wg.Wait()
		sr, _ := r.GetSeriesRoyalty("s1")
		if setErr == nil && issueErr == nil {
			if !sr.Fixed || len(sr.Entries) != 1 {
				t.Fatalf("set-first ordering wrong: %+v", sr)
			}
		} else if setErr == nil && issueErr != nil {
			t.Fatalf("unexpected: set succeeded but issue failed: %v", issueErr)
		} else if setErr != nil && issueErr == nil {
			if !errors.Is(setErr, ErrConflict) || !sr.Fixed || len(sr.Entries) != 0 {
				t.Fatalf("issue-first ordering wrong: set=%v rule=%+v", setErr, sr)
			}
		} else {
			t.Fatalf("both failed: set=%v issue=%v", setErr, issueErr)
		}
	}
}

// ---- 转让价款与应付金额 ----

func TestTransferRoyaltySplit(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "s1",
		RoyaltyEntry{Account: "bob", Ratio: 100},   // 1%
		RoyaltyEntry{Account: "carol", Ratio: 250}, // 2.5%
	))
	r.Issue(issueReq("i1", "alice"))

	// 价款 10000 分：bob 得 100，carol 得 250，余款 9650 归 alice
	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = 10000
	if _, err := r.Transfer(req); err != nil {
		t.Fatal(err)
	}
	tr, err := r.GetTransferRoyalty("i1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Price != 10000 || tr.PayerID != "alice" {
		t.Fatalf("basis wrong: %+v", tr)
	}
	want := map[string]int64{"bob": 100, "carol": 250}
	if len(tr.Payees) != 2 {
		t.Fatalf("payees wrong: %+v", tr.Payees)
	}
	for _, p := range tr.Payees {
		if want[p.Account] != p.Amount {
			t.Fatalf("payee %s amount wrong: %d", p.Account, p.Amount)
		}
	}
}

// 零金额也保留明细；无规则时无明细。
func TestTransferRoyaltyZeroAmountsAndNoRule(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "s1", RoyaltyEntry{Account: "bob", Ratio: 1}))
	r.Issue(issueReq("i1", "alice"))

	// 价款 99 分：99 * 1 / 10000 向下取整为 0，零金额保留明细
	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = 99
	r.Transfer(req)
	tr, _ := r.GetTransferRoyalty("i1", 2)
	if len(tr.Payees) != 1 || tr.Payees[0].Amount != 0 {
		t.Fatalf("zero amount must keep detail: %+v", tr.Payees)
	}

	// 无规则系列的转让：无明细，价款仍记录
	r2 := mustCreate(t, filepath.Join(t.TempDir(), "d"))
	royaltyWorld(t, r2)
	r2.Issue(issueReq("i2", "alice"))
	req2 := xferReq("alice", "i2", "bob", 1, "t2")
	req2.Price = 500
	r2.Transfer(req2)
	tr2, _ := r2.GetTransferRoyalty("i2", 2)
	if tr2.Price != 500 || len(tr2.Payees) != 0 {
		t.Fatalf("no-rule transfer: %+v", tr2)
	}
}

// 价款未填按零；负数拒绝。
func TestTransferPriceDefaultAndNegative(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "s1", RoyaltyEntry{Account: "bob", Ratio: 100}))
	r.Issue(issueReq("i1", "alice"))

	// 未填价款按零
	r.Transfer(xferReq("alice", "i1", "bob", 1, "t1"))
	tr, _ := r.GetTransferRoyalty("i1", 2)
	if tr.Price != 0 || len(tr.Payees) != 1 || tr.Payees[0].Amount != 0 {
		t.Fatalf("unset price must be zero: %+v", tr)
	}

	// 负数拒绝
	req := xferReq("alice", "i1", "carol", 2, "t2")
	req.Price = -1
	if _, err := r.Transfer(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative price must be invalid arg, got %v", err)
	}
}

// 整个价款范围内结果准确（int64 最大值边界）。
func TestRoyaltyAmountFullRange(t *testing.T) {
	const max = math.MaxInt64
	cases := []struct {
		price int64
		ratio int
		want  int64
	}{
		{max, 10000, max},                // 全额
		{max, 9999, max - max/10000 - 1}, // 近似全额，向下取整
		{max, 1, max / 10000},            // 最小比例
		{1, 10000, 1},                    // 价款 1 全额比例
		{9999, 10000, 9999},              // 余数边界
		{10000, 9999, 9999},              // 整除
		{0, 10000, 0},
	}
	for _, c := range cases {
		got := royaltyAmount(c.price, c.ratio)
		if got != c.want {
			t.Fatalf("royaltyAmount(%d, %d) = %d, want %d", c.price, c.ratio, got, c.want)
		}
	}
	// 合计不超过价款
	var price int64 = max
	rule := &royaltyRule{Entries: []royaltyEntry{
		{Account: "a", Ratio: 3333}, {Account: "b", Ratio: 3333}, {Account: "c", Ratio: 3334},
	}}
	var sum int64
	for _, e := range rule.Entries {
		sum += royaltyAmount(price, e.Ratio)
	}
	if sum > price {
		t.Fatalf("sum %d exceeds price %d", sum, price)
	}
}

// 收款人可以同时是转让参与者；收款人后来停用不阻止新转让，也不删除
// 其应付记录。
func TestPayeeIsParticipantAndInactive(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	// carol 是仅作为收款人的账户；bob 既是收款人又是接收人
	r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "s1",
		RoyaltyEntry{Account: "bob", Ratio: 100}, RoyaltyEntry{Account: "carol", Ratio: 200}))
	r.Issue(issueReq("i1", "alice"))

	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = 10000
	r.Transfer(req)
	payables, err := r.RoyaltyPayablesOf("bob")
	if err != nil || len(payables) != 1 || payables[0].Amount != 100 {
		t.Fatalf("payee-as-participant: %+v %v", payables, err)
	}

	// 仅作为收款人的 carol 停用后，新转让不被阻止，旧应付记录保留
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	req2 := xferReq("bob", "i1", "dave", 2, "t2")
	req2.Price = 20000
	if _, err := r.Transfer(req2); err != nil {
		t.Fatalf("transfer with inactive payee must proceed: %v", err)
	}
	payables, _ = r.RoyaltyPayablesOf("carol")
	if len(payables) != 2 {
		t.Fatalf("inactive payee records must remain: %+v", payables)
	}
}

// ---- 代转价款 ----

func TestProxyTransferRoyaltyPriceFromAuth(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "s1", RoyaltyEntry{Account: "dave", Ratio: 100}))
	r.Issue(issueReq("i1", "alice"))

	// 价款在创建授权时确定
	authReq := createAuthReq(r, "a1", time.Hour)
	authReq.Price = 5000
	if _, err := r.CreateAuthorization(authReq); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a1", "p1")); err != nil {
		t.Fatal(err)
	}
	tr, err := r.GetTransferRoyalty("i1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Price != 5000 || tr.PayerID != "alice" {
		t.Fatalf("proxy royalty basis wrong: %+v", tr)
	}
	// 余款归授权人 alice，受托人 bob 不是收款人，不记给 bob
	if len(tr.Payees) != 1 || tr.Payees[0].Account != "dave" || tr.Payees[0].Amount != 50 {
		t.Fatalf("proxy payees wrong: %+v", tr.Payees)
	}
}

// 旧授权（没有价款字段）按零处理。
func TestProxyTransferLegacyAuthPriceZero(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "s1", RoyaltyEntry{Account: "dave", Ratio: 100}))
	r.Issue(issueReq("i1", "alice"))
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	r.ProxyTransfer(proxyReq("bob", "a1", "p1"))
	tr, _ := r.GetTransferRoyalty("i1", 2)
	if tr.Price != 0 || len(tr.Payees) != 1 || tr.Payees[0].Amount != 0 {
		t.Fatalf("legacy auth price must be zero: %+v", tr)
	}
}

// ---- 查询 ----

func TestRoyaltyPayablesOfOrderingAndEmpty(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "s1", RoyaltyEntry{Account: "bob", Ratio: 100}))
	r.Issue(issueReq("i1", "alice"))
	r.Issue(issueReq("i2", "alice"))

	for i, item := range []string{"i1", "i2"} {
		req := xferReq("alice", item, "bob", 1, "t"+item)
		req.Price = int64((i + 1) * 1000)
		r.Transfer(req)
	}
	// 按转让序号排列（发行也占序号：i1 发行=1、i2 发行=2、i1 转让=3、i2 转让=4）
	payables, err := r.RoyaltyPayablesOf("bob")
	if err != nil || len(payables) != 2 {
		t.Fatalf("payables: %+v %v", payables, err)
	}
	if payables[0].TxSeq != 3 || payables[1].TxSeq != 4 ||
		payables[0].ItemID != "i1" || payables[1].ItemID != "i2" {
		t.Fatalf("ordering wrong: %+v", payables)
	}
	// 无明细返回空列表
	empty, err := r.RoyaltyPayablesOf("carol")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty payables must be empty list: %+v %v", empty, err)
	}
	// 账户不存在报错
	if _, err := r.RoyaltyPayablesOf("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	// 转让记录不存在报错
	if _, err := r.GetTransferRoyalty("i1", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing transfer: %v", err)
	}
	// 发行记录不是转让，报错
	if _, err := r.GetTransferRoyalty("i1", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("issue seq must not be a transfer: %v", err)
	}
}

// ---- 幂等 ----

func TestRoyaltyIdempotency(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	// 规则设置：相同参数重放
	req := setRoyaltyReq("s1", "alice", "s1", RoyaltyEntry{Account: "bob", Ratio: 100})
	first, err := r.SetSeriesRoyalty(req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.SetSeriesRoyalty(req)
	if err != nil || !again.Replayed || again.SeriesID != first.SeriesID {
		t.Fatalf("set replay: %+v %v", again, err)
	}
	evs, _ := r.RoyaltyHistory("s1")
	if len(evs) != 1 {
		t.Fatalf("replay must not add event: %d", len(evs))
	}
	// 改规则 -> 请求号冲突
	changed := setRoyaltyReq("s1", "alice", "s1", RoyaltyEntry{Account: "carol", Ratio: 100})
	if _, err := r.SetSeriesRoyalty(changed); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("rule change must conflict, got %v", err)
	}

	// 转让改价 -> 请求号冲突
	r.Issue(issueReq("i1", "alice"))
	treq := xferReq("alice", "i1", "bob", 1, "t1")
	treq.Price = 1000
	r.Transfer(treq)
	treq.Price = 2000
	if _, err := r.Transfer(treq); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("price change must conflict, got %v", err)
	}
	// 原请求重放返回原金额
	treq.Price = 1000
	res, err := r.Transfer(treq)
	if err != nil || !res.Replayed {
		t.Fatalf("replay: %+v %v", res, err)
	}
	tr, _ := r.GetTransferRoyalty("i1", 2)
	if tr.Price != 1000 {
		t.Fatalf("replay must keep original price: %d", tr.Price)
	}

	// 代转改价：价款以授权创建时为准，执行时不得改价；重放仍为原价款
	r.Issue(issueReq("i2", "alice"))
	authReq := createAuthReq(r, "a1", time.Hour)
	authReq.ItemID = "i2"
	authReq.Price = 3000
	if _, err := r.CreateAuthorization(authReq); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a1", "p1")); err != nil {
		t.Fatal(err)
	}
	// 序号：i1 发行=1、i1 转让=2、i2 发行=3、i2 代转=4
	tr2, _ := r.GetTransferRoyalty("i2", 4)
	if tr2.Price != 3000 {
		t.Fatalf("proxy price must be fixed at auth creation: %d", tr2.Price)
	}
}

// 规则设置与现有操作共用操作者请求号。
func TestRoyaltyRequestIDShared(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "shared", RoyaltyEntry{Account: "bob", Ratio: 100}))
	// 同号用于发行 -> 冲突
	req := issueReq("i1", "bob")
	req.RequestID = "shared"
	if _, err := r.Issue(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("shared request id must conflict, got %v", err)
	}
}

// 重开或藏品再次易手后，原转让请求仍返回原金额而不重复计入。
func TestRoyaltyReplayAfterReopenAndResale(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	royaltyWorld(t, r)
	r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "s1", RoyaltyEntry{Account: "bob", Ratio: 100}))
	r.Issue(issueReq("i1", "alice"))
	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = 10000
	r.Transfer(req)
	// 藏品再次易手
	r.Transfer(xferReq("bob", "i1", "carol", 2, "t2"))
	r.Close()

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	again, err := r2.Transfer(req)
	if err != nil || !again.Replayed {
		t.Fatalf("replay after reopen: %+v %v", again, err)
	}
	tr, _ := r2.GetTransferRoyalty("i1", 2)
	if tr.Price != 10000 {
		t.Fatalf("replay must keep original price: %d", tr.Price)
	}
	// 再次易手的转让价款为零（未填），bob 作为收款人有两笔明细；
	// 重放不新增、不重复计入。
	payables, _ := r2.RoyaltyPayablesOf("bob")
	if len(payables) != 2 || payables[0].Amount != 100 || payables[1].Amount != 0 {
		t.Fatalf("replay must not double-count: %+v", payables)
	}
}

// ---- 持久化 ----

func TestRoyaltyPersistenceAcrossReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	royaltyWorld(t, r)
	r.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "s1", RoyaltyEntry{Account: "bob", Ratio: 100}))
	r.Issue(issueReq("i1", "alice"))
	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = 10000
	r.Transfer(req)
	r.Close()

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	sr, _ := r2.GetSeriesRoyalty("s1")
	if !sr.Fixed || len(sr.Entries) != 1 {
		t.Fatalf("rule must persist: %+v", sr)
	}
	tr, _ := r2.GetTransferRoyalty("i1", 2)
	if tr.Price != 10000 || len(tr.Payees) != 1 || tr.Payees[0].Amount != 100 {
		t.Fatalf("royalty record must persist: %+v", tr)
	}
	evs, _ := r2.RoyaltyHistory("s1")
	if len(evs) != 1 {
		t.Fatalf("royalty events must persist: %d", len(evs))
	}
}

// 旧登记册直接打开：旧历史保留、旧请求可回放、旧成交按零查询、不补造
// 应付明细。
func TestRoyaltyLegacySnapshot(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupWorld(t, r)
	r.Issue(issueReq("i1", "alice"))
	r.Transfer(xferReq("alice", "i1", "bob", 1, "t1"))
	r.Close()

	// 旧格式快照没有版税字段，直接打开
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("old registry must open: %v", err)
	}
	defer r2.Close()

	hist, _ := r2.History("i1")
	if len(hist) != 2 {
		t.Fatalf("old history must be preserved: %d", len(hist))
	}
	// 旧成交按零查询，无补造明细
	tr, err := r2.GetTransferRoyalty("i1", 2)
	if err != nil || tr.Price != 0 || len(tr.Payees) != 0 {
		t.Fatalf("legacy deal must query as zero: %+v %v", tr, err)
	}
	// 旧请求仍可回放
	again, err := r2.Transfer(xferReq("alice", "i1", "bob", 1, "t1"))
	if err != nil || !again.Replayed {
		t.Fatalf("legacy request must replay: %+v %v", again, err)
	}
	// 旧系列规则已固定（有藏品发行），不能再设置
	if err := r2.RegisterAccount("carol", ""); err != nil {
		t.Fatal(err)
	}
	_, err = r2.SetSeriesRoyalty(setRoyaltyReq("s1", "alice", "s2", RoyaltyEntry{Account: "bob", Ratio: 100}))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy series rule must be fixed: %v", err)
	}
}
