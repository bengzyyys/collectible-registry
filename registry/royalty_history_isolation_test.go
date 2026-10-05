package registry

import (
	"errors"
	"testing"
	"time"
)

// 本文件为版税规则变更历史（RoyaltyHistory）补充回归保障：调用者可以
// 任意改写自己拿到的历史记录与份额列表来整理展示内容，但这些本地修改
// 既不构成一次规则设置，也不能改变登记册中保存的当前规则、历史以及
// 后续发行/转让时实际使用的版税规则。

// wantSharesEqual 断言一份规则份额与期望完全一致（含条数、账户与比例）。
func wantSharesEqual(t *testing.T, got []RoyaltyShare, want ...RoyaltyShare) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("份额条数 = %d，期望 %d（实际 %+v）", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条份额 = %+v，期望 %+v（实际全部 %+v）", i, got[i], want[i], got)
		}
	}
}

// payableMap 把转让结果中的应付明细按收款账户索引，重复账户直接失败。
func payableMap(t *testing.T, ps []RoyaltyPayable) map[string]RoyaltyPayable {
	t.Helper()
	m := make(map[string]RoyaltyPayable, len(ps))
	for _, p := range ps {
		if _, dup := m[p.AccountID]; dup {
			t.Fatalf("应付明细中收款账户 %s 重复: %+v", p.AccountID, ps)
		}
		m[p.AccountID] = p
	}
	return m
}

// TestRoyaltyHistoryReturnsIndependentCopies 覆盖首次发行前"设置两份不同
// 规则、再清空"的正常过程：历史按顺序保留三次真实变更，且每条记录、
// 每个份额列表都是独立副本，调用者的本地修改在重新查询时全部消失。
func TestRoyaltyHistoryReturnsIndependentCopies(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	rule1 := []RoyaltyShare{{AccountID: "carol", Rate: 2500}}
	rule2 := []RoyaltyShare{{AccountID: "bob", Rate: 1000}, {AccountID: "dave", Rate: 500}}

	// 设置结果本身也是独立副本：改写返回的份额不能影响登记册里的规则。
	res1, err := r.SetRoyalty(setRoyaltyReq("rs-1", rule1...))
	if err != nil {
		t.Fatalf("SetRoyalty rs-1: %v", err)
	}
	res1.Shares[0].AccountID = "hacker"
	res1.Shares[0].Rate = 8888
	cur, err := r.GetSeriesRoyalty("s1")
	if err != nil {
		t.Fatal(err)
	}
	wantSharesEqual(t, cur, rule1...)

	// 首次发行前连续设置第二份不同规则，再清空规则。
	mustSetRoyalty(t, r, "rs-2", rule2...)
	mustSetRoyalty(t, r, "rs-3")

	evs, err := r.RoyaltyHistory("s1")
	if err != nil {
		t.Fatalf("RoyaltyHistory: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("三次真实变更应保留三条历史，得到 %d 条: %+v", len(evs), evs)
	}

	// 三次变更的期望内容：第一次变更前为空；第二次变更前等于第一次
	// 设置的内容；清空记录变更后为空。
	type meta struct {
		seq             int64
		op, reason, rid string
		at              time.Time
		before, after   []RoyaltyShare
	}
	wantEvents := []struct {
		seq           int64
		rid           string
		before, after []RoyaltyShare
	}{
		{seq: 1, rid: "rs-1", before: nil, after: rule1},
		{seq: 2, rid: "rs-2", before: rule1, after: rule2},
		{seq: 3, rid: "rs-3", before: rule2, after: nil},
	}
	saved := make([]meta, len(evs))
	for i, w := range wantEvents {
		e := evs[i]
		if e.Seq != w.seq || e.SeriesID != "s1" || e.Operator != "alice" ||
			e.Reason != "设定版税" || e.RequestID != w.rid {
			t.Fatalf("第 %d 条记录元数据错误: %+v", i+1, e)
		}
		if e.OccurredAt.IsZero() {
			t.Fatalf("第 %d 条记录缺少发生时间", i+1)
		}
		wantSharesEqual(t, e.Before, w.before...)
		wantSharesEqual(t, e.After, w.after...)
		saved[i] = meta{
			seq: e.Seq, op: e.Operator, reason: e.Reason, rid: e.RequestID,
			at:     e.OccurredAt,
			before: append([]RoyaltyShare(nil), e.Before...),
			after:  append([]RoyaltyShare(nil), e.After...),
		}
	}

	// 同一份查询结果中，相邻记录内容相同的 After/Before 也必须彼此独立：
	// event1 的 After 与 event2 的 Before 都是 rule1，改一处不能连带另一处。
	evs[0].After[0].AccountID = "hacker"
	evs[0].After[0].Rate = 1111
	if evs[1].Before[0].AccountID != "carol" || evs[1].Before[0].Rate != 2500 {
		t.Fatalf("修改上一条的 After 连带改掉了下一条的 Before: %+v", evs[1].Before)
	}
	// event2 的 After 与 event3 的 Before 都是 rule2（两条份额各自独立）。
	evs[1].After[0].Rate = 2222
	evs[1].After[1].AccountID = "mallory"
	if evs[2].Before[0].AccountID != "bob" || evs[2].Before[0].Rate != 1000 ||
		evs[2].Before[1].AccountID != "dave" || evs[2].Before[1].Rate != 500 {
		t.Fatalf("修改第二条的 After 连带改掉了清空记录的 Before: %+v", evs[2].Before)
	}
	// 首次设置的空 Before 与清空记录的空 After 也是互不影响的独立切片。
	evs[0].Before = append(evs[0].Before, RoyaltyShare{AccountID: "ghost", Rate: 1})
	if len(evs[2].After) != 0 {
		t.Fatalf("向第一条的空 Before 追加不应影响清空记录的 After: %+v", evs[2].After)
	}
	evs[2].After = append(evs[2].After, RoyaltyShare{AccountID: "ghost", Rate: 2})
	if len(evs[0].Before) != 1 || evs[0].Before[0].AccountID != "ghost" {
		t.Fatalf("调用者自己的追加应保留在本地副本中: %+v", evs[0].Before)
	}

	// 大幅改写调用者自己拿到的记录字段与前后列表，甚至整条替换为零值。
	evs[1].Seq = 999
	evs[1].SeriesID = "sX"
	evs[1].Operator = "mallory"
	evs[1].Reason = "被篡改的原因"
	evs[1].RequestID = "fake-req"
	evs[1].OccurredAt = evs[1].OccurredAt.Add(365 * 24 * time.Hour)
	evs[1].Before = []RoyaltyShare{{AccountID: "zzz", Rate: 1}}
	evs[1].After = evs[1].After[:0]
	evs[2] = RoyaltyEvent{}

	// 本地修改不构成一次规则设置：当前规则仍是清空状态，历史条数不变。
	cur, err = r.GetSeriesRoyalty("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cur) != 0 {
		t.Fatalf("篡改历史副本不得改变当前规则: %+v", cur)
	}

	// 重新查询应得到原先保存的完整内容：账户、比例、前后列表、操作者、
	// 原因、请求号、时间与记录序号全部保持原值。
	fresh, err := r.RoyaltyHistory("s1")
	if err != nil {
		t.Fatalf("RoyaltyHistory: %v", err)
	}
	if len(fresh) != 3 {
		t.Fatalf("篡改本地副本不得增减历史记录，得到 %d 条", len(fresh))
	}
	for i, w := range wantEvents {
		e := fresh[i]
		s := saved[i]
		if e.Seq != s.seq || e.Operator != s.op || e.Reason != s.reason ||
			e.RequestID != s.rid || e.SeriesID != "s1" || !e.OccurredAt.Equal(s.at) {
			t.Fatalf("第 %d 条记录元数据被本地修改污染: %+v（原值 %+v）", i+1, e, s)
		}
		wantSharesEqual(t, e.Before, w.before...)
		wantSharesEqual(t, e.After, w.after...)
	}

	// 两次查询得到的结果互不共享底层数据。
	a, _ := r.RoyaltyHistory("s1")
	b, _ := r.RoyaltyHistory("s1")
	a[0].After[0].Rate = 4242
	a[0].After = a[0].After[:0]
	a = append(a, RoyaltyEvent{Seq: 1})
	if len(b) != 3 || b[0].After[0].AccountID != "carol" || b[0].After[0].Rate != 2500 {
		t.Fatalf("一份查询结果被改写后，另一份查询结果必须保持原样: %+v", b)
	}
	b[1].Before[0].AccountID = "zzz"
	if fresh[1].Before[0].AccountID != "carol" || fresh[1].Before[0].Rate != 2500 {
		t.Fatalf("查询结果的改动不得影响其他查询已拿到的内容: %+v", fresh[1].Before)
	}

	// 本地修改不消耗真实的变更记录序号：随后真正的设置拿到序号 4，
	// 其变更前仍是清空状态。
	mustSetRoyalty(t, r, "rs-4", RoyaltyShare{AccountID: "dave", Rate: 7})
	again, err := r.RoyaltyHistory("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 4 || again[3].Seq != 4 || again[3].RequestID != "rs-4" {
		t.Fatalf("本地篡改不应产生或消耗变更序号: %+v", again)
	}
	wantSharesEqual(t, again[3].Before)
	wantSharesEqual(t, again[3].After, RoyaltyShare{AccountID: "dave", Rate: 7})
}

// TestRoyaltyHistorySnapshotVsLaterSetIssueTransfer 覆盖历史查询结果与
// 后续正常设置、发行、转让之间的关系：保留的旧结果只代表取得时的历史，
// 新查询才出现新记录；无论怎样修改旧副本，当前规则查询、首次发行后的
// 固定规则以及转让时的版税计算都只以登记册中真正设置过的规则为准。
func TestRoyaltyHistorySnapshotVsLaterSetIssueTransfer(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	ruleA := []RoyaltyShare{{AccountID: "carol", Rate: 2000}}
	ruleB := []RoyaltyShare{{AccountID: "bob", Rate: 1000}, {AccountID: "dave", Rate: 3333}}

	// 第一份真实规则，调用者随即保留一份历史结果。
	mustSetRoyalty(t, r, "rs-1", ruleA...)
	old, err := r.RoyaltyHistory("s1")
	if err != nil {
		t.Fatalf("RoyaltyHistory: %v", err)
	}
	if len(old) != 1 {
		t.Fatalf("首次设置后应只有一条历史，得到 %+v", old)
	}
	firstAt := old[0].OccurredAt

	// 调用者为整理展示内容大幅改写自己保留的这份历史（账户、比例、
	// 前后列表、操作者、原因、请求号、序号、时间全部改）。
	old[0].Seq = 77
	old[0].SeriesID = "sX"
	old[0].Operator = "mallory"
	old[0].Reason = "伪造的原因"
	old[0].RequestID = "fake-req"
	old[0].OccurredAt = firstAt.Add(100 * time.Hour)
	old[0].Before = append(old[0].Before, RoyaltyShare{AccountID: "ghost", Rate: 1})
	old[0].After[0].AccountID = "hacker"
	old[0].After[0].Rate = 9999

	// 系列创建账户使用新的请求号设置另一份规则：以登记册真实状态成功，
	// 历史副本里的 hacker/9999 不构成任何规则。
	mustSetRoyalty(t, r, "rs-2", ruleB...)

	// 旧查询结果继续表示取得时的历史（仍只有一条，本地改写原样保留）。
	if len(old) != 1 || old[0].After[0].AccountID != "hacker" || old[0].After[0].Rate != 9999 {
		t.Fatalf("保留的旧结果应仍是调用者取得时的那份副本: %+v", old)
	}

	// 当前规则查询返回最后一次真正设置的规则 ruleB。
	cur, err := r.GetSeriesRoyalty("s1")
	if err != nil {
		t.Fatal(err)
	}
	wantSharesEqual(t, cur, ruleB...)

	// 新查询才出现新增记录和新的前后内容，全部是真实保存过的内容。
	fresh, err := r.RoyaltyHistory("s1")
	if err != nil {
		t.Fatalf("RoyaltyHistory: %v", err)
	}
	if len(fresh) != 2 {
		t.Fatalf("新查询应出现两条历史，得到 %d 条", len(fresh))
	}
	if fresh[0].Seq != 1 || fresh[0].RequestID != "rs-1" || fresh[0].Operator != "alice" ||
		fresh[0].Reason != "设定版税" || !fresh[0].OccurredAt.Equal(firstAt) {
		t.Fatalf("第一条记录元数据错误: %+v", fresh[0])
	}
	wantSharesEqual(t, fresh[0].Before)
	wantSharesEqual(t, fresh[0].After, ruleA...)
	wantSharesEqual(t, fresh[1].Before, ruleA...)
	wantSharesEqual(t, fresh[1].After, ruleB...)

	// 首次发行后规则固定：篡改历史副本不可能打开设置入口，冻结拒绝也
	// 不新增变更记录。
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := r.SetRoyalty(setRoyaltyReq("rs-3",
		RoyaltyShare{AccountID: "hacker", Rate: 9999})); !errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("首次发行后必须按 ErrRoyaltyFrozen 拒绝，得到 %v", err)
	}
	frozen, err := r.RoyaltyHistory("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen) != 2 {
		t.Fatalf("冻结拒绝不得新增变更记录，历史条数 = %d", len(frozen))
	}

	// 成功转让：成交金额以分计，版税按真实规则 ruleB 计算，各账户分别
	// 按万分之一比例向下取整，而不是按历史副本里改出的账户或比例。
	req := xferReq("alice", "i1", "bob", 1, "t-1")
	req.Price = 12345
	res, err := r.Transfer(req)
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if res.Price != 12345 {
		t.Fatalf("成交金额应以分记录: %d", res.Price)
	}
	pm := payableMap(t, res.Payables)
	if len(pm) != 2 {
		t.Fatalf("只应为真实规则中的两个账户记账: %+v", res.Payables)
	}
	// 12345 * 1000 / 10000 = 1234.5 -> 向下取整 1234
	// 12345 * 3333 / 10000 = 4114.5885 -> 向下取整 4114
	if p := pm["bob"]; p.Rate != 1000 || p.Amount != 1234 {
		t.Fatalf("bob 的应付应为比例 1000、金额 1234，得到 %+v", p)
	}
	if p := pm["dave"]; p.Rate != 3333 || p.Amount != 4114 {
		t.Fatalf("dave 的应付应为比例 3333、金额 4114，得到 %+v", p)
	}
	if _, ok := pm["hacker"]; ok {
		t.Fatalf("历史副本中改出的 hacker 账户不应产生应付: %+v", res.Payables)
	}
	if _, ok := pm["carol"]; ok {
		t.Fatalf("已被替换的旧规则账户 carol 不应再参与计算: %+v", res.Payables)
	}
	// 余款归转让前持有人 alice。
	if res.Remainder != 12345-1234-4114 {
		t.Fatalf("余款金额错误: %d", res.Remainder)
	}
	ro, err := r.TransferRoyalty(res.TxSeq)
	if err != nil {
		t.Fatalf("TransferRoyalty: %v", err)
	}
	if ro.Price != 12345 || ro.OwnerID != "alice" || ro.Remainder != 12345-1234-4114 {
		t.Fatalf("转让版税记录错误: %+v", ro)
	}

	// 由此产生的应付只归原（真实）规则的账户。
	for acct, wantAmt := range map[string]int64{"bob": 1234, "dave": 4114} {
		entries, err := r.PayablesOf(acct)
		if err != nil {
			t.Fatalf("PayablesOf %s: %v", acct, err)
		}
		if len(entries) != 1 || entries[0].Amount != wantAmt {
			t.Fatalf("账户 %s 的应付应为一笔 %d，得到 %+v", acct, wantAmt, entries)
		}
	}
	entries, err := r.PayablesOf("carol")
	if err != nil {
		t.Fatalf("PayablesOf carol: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("旧规则账户 carol 不应有应付明细: %+v", entries)
	}

	// 全部操作结束后，旧副本仍只代表取得时的那一条本地内容。
	if len(old) != 1 || old[0].RequestID != "fake-req" {
		t.Fatalf("旧副本不应被后续真实操作覆盖: %+v", old)
	}
}

// TestRoyaltyHistoryEmptyForSeriesWithoutRule 覆盖从未设置过版税的已登记
// 系列始终返回空历史；设置与清空产生的空前后列表彼此独立，也不影响
// 其他系列的查询结果。
func TestRoyaltyHistoryEmptyForSeriesWithoutRule(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	// s1 已登记但从未设置版税：空历史、空当前规则。
	evs, err := r.RoyaltyHistory("s1")
	if err != nil {
		t.Fatalf("RoyaltyHistory: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("未设置过规则的系列应返回空历史，得到 %+v", evs)
	}
	rule, err := r.GetSeriesRoyalty("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rule) != 0 {
		t.Fatalf("未设置过规则的系列当前规则应为空: %+v", rule)
	}

	// 在空结果上追加、改写本地内容，不产生变更也不改变再次查询。
	evs = append(evs, RoyaltyEvent{
		Seq: 1, Operator: "mallory",
		After: []RoyaltyShare{{AccountID: "bob", Rate: 1}},
	})
	evs[0].After[0].Rate = 9999
	again, err := r.RoyaltyHistory("s1")
	if err != nil || len(again) != 0 {
		t.Fatalf("篡改空历史本地副本不应产生记录: %+v %v", again, err)
	}
	rule, _ = r.GetSeriesRoyalty("s1")
	if len(rule) != 0 {
		t.Fatalf("篡改空历史本地副本不应设置规则: %+v", rule)
	}

	// 另一个已登记系列 s2 独立返回空历史。
	if err := r.CreateSeries("s2", "alice", ""); err != nil {
		t.Fatal(err)
	}
	s2hist, err := r.RoyaltyHistory("s2")
	if err != nil || len(s2hist) != 0 {
		t.Fatalf("s2 应返回空历史: %+v %v", s2hist, err)
	}

	// 在 s1 上设置再清空：产生的空 Before/空 After 互不影响，也不影响 s2。
	mustSetRoyalty(t, r, "rs-1", RoyaltyShare{AccountID: "carol", Rate: 100})
	h1, err := r.RoyaltyHistory("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(h1) != 1 || len(h1[0].Before) != 0 {
		t.Fatalf("首次设置的变更前应为空列表: %+v", h1)
	}
	h1[0].Before = append(h1[0].Before, RoyaltyShare{AccountID: "zzz", Rate: 1})
	h1[0].After[0].Rate = 4242
	h1[0].After[0].AccountID = "xxx"

	mustSetRoyalty(t, r, "rs-2") // 清空
	cl, err := r.RoyaltyHistory("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cl) != 2 || len(cl[0].Before) != 0 || len(cl[1].After) != 0 {
		t.Fatalf("设置/清空两条记录的空前后列表形状错误: %+v", cl)
	}
	cl[0].Before = append(cl[0].Before, RoyaltyShare{AccountID: "a", Rate: 1})
	cl[0].After[0].AccountID = "xxx"
	cl[1].After = append(cl[1].After, RoyaltyShare{AccountID: "b", Rate: 2})
	cl[1].Before[0].Rate = 7

	final1, err := r.RoyaltyHistory("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(final1) != 2 {
		t.Fatalf("s1 历史应仍为两条，得到 %d 条", len(final1))
	}
	if len(final1[0].Before) != 0 {
		t.Fatalf("第一条的空 Before 被污染: %+v", final1[0].Before)
	}
	wantSharesEqual(t, final1[0].After, RoyaltyShare{AccountID: "carol", Rate: 100})
	wantSharesEqual(t, final1[1].Before, RoyaltyShare{AccountID: "carol", Rate: 100})
	if len(final1[1].After) != 0 {
		t.Fatalf("清空记录的 After 应仍为空: %+v", final1[1].After)
	}
	final2, err := r.RoyaltyHistory("s2")
	if err != nil || len(final2) != 0 {
		t.Fatalf("s1 的本地改写不应影响 s2 的空历史: %+v %v", final2, err)
	}

	// 未设置过规则的系列首次发行后固定为无版税，历史仍为空。
	ireq := issueReq("i-s2", "bob")
	ireq.SeriesID = "s2"
	if _, err := r.Issue(ireq); err != nil {
		t.Fatalf("Issue on s2: %v", err)
	}
	post, err := r.RoyaltyHistory("s2")
	if err != nil || len(post) != 0 {
		t.Fatalf("未设置过规则的系列发行后历史仍应为空: %+v %v", post, err)
	}
	rule2, err := r.GetSeriesRoyalty("s2")
	if err != nil || len(rule2) != 0 {
		t.Fatalf("未设置过规则的系列应固定为空规则: %+v %v", rule2, err)
	}
}
