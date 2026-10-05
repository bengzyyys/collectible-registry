package registry

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件为版税规则变更历史补充回归保障，重点钉住一条契约：
// RoyaltyHistory（以及 GetSeriesRoyalty）返回的是登记册内容的独立副本，
// 调用者整理展示内容时对返回记录或份额列表的任何修改，都不是一次规则
// 设置，不能改变登记册里保存的当前规则与历史，也不能影响其他查询。

// ---- 测试辅助 ----

func mustRoyaltyHistory(t *testing.T, r *Registry, seriesID string) []RoyaltyEvent {
	t.Helper()
	evs, err := r.RoyaltyHistory(seriesID)
	if err != nil {
		t.Fatalf("RoyaltyHistory(%s): %v", seriesID, err)
	}
	return evs
}

// cloneRoyaltyEvents 深拷贝一份历史结果（含 Before/After 份额列表），
// 用作篡改后期望保留的原始内容基线。
func cloneRoyaltyEvents(evs []RoyaltyEvent) []RoyaltyEvent {
	out := make([]RoyaltyEvent, len(evs))
	for i, e := range evs {
		// 用 make(…,0,len)+append 保留"非 nil 空列表"语义，与查询返回一致，
		// 便于直接 reflect.DeepEqual。
		before := make([]RoyaltyShare, 0, len(e.Before))
		before = append(before, e.Before...)
		after := make([]RoyaltyShare, 0, len(e.After))
		after = append(after, e.After...)
		e.Before, e.After = before, after
		out[i] = e
	}
	return out
}

func assertEventsDeepEqual(t *testing.T, got, want []RoyaltyEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("历史条数 = %d，期望 %d\ngot:  %+v\nwant: %+v", len(got), len(want), got, want)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("第 %d 条记录被改变:\ngot:  %+v\nwant: %+v", i+1, got[i], want[i])
		}
	}
}

func assertSharesDeepEqual(t *testing.T, got, want []RoyaltyShare) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("规则内容被改变:\ngot:  %+v\nwant: %+v", got, want)
	}
}

// storedRoyaltyState 直接（白盒）读取登记册内部保存的当前规则与历史，
// 验证调用者对返回副本的篡改连内部状态都没有碰到——仅再次查询一致还
// 不够，因为再次查询可能从已被污染的内部状态拷贝。
type storedRoyaltyCore struct {
	Seq           int64
	Operator      string
	Reason        string
	RequestID     string
	Before, After []RoyaltyShare
	OccurredAt    time.Time
}

func readStoredRoyaltyState(t *testing.T, r *Registry, seriesID string) ([]RoyaltyShare, []storedRoyaltyCore) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	toPub := func(shs []royaltyShare) []RoyaltyShare {
		out := make([]RoyaltyShare, len(shs))
		for i, sh := range shs {
			out[i] = RoyaltyShare{AccountID: sh.AccountID, Rate: sh.Rate}
		}
		return out
	}
	var cur []RoyaltyShare
	if s, ok := r.state.Series[seriesID]; ok {
		cur = toPub(s.Royalty)
	}
	var evs []storedRoyaltyCore
	for _, e := range r.state.RoyaltyEvents {
		if e.SeriesID != seriesID {
			continue
		}
		evs = append(evs, storedRoyaltyCore{
			Seq: e.Seq, Operator: e.Operator, Reason: e.Reason,
			RequestID: e.RequestID, Before: toPub(e.Before), After: toPub(e.After),
			OccurredAt: e.OccurredAt,
		})
	}
	return cur, evs
}

func assertStoredEventsMatch(t *testing.T, stored []storedRoyaltyCore, want []RoyaltyEvent) {
	t.Helper()
	if len(stored) != len(want) {
		t.Fatalf("内部历史条数 = %d，期望 %d: %+v", len(stored), len(want), stored)
	}
	for i := range want {
		g := stored[i]
		w := want[i]
		if g.Seq != w.Seq || g.Operator != w.Operator || g.Reason != w.Reason ||
			g.RequestID != w.RequestID || !g.OccurredAt.Equal(w.OccurredAt) ||
			!reflect.DeepEqual(g.Before, w.Before) || !reflect.DeepEqual(g.After, w.After) {
			t.Fatalf("内部第 %d 条记录被改变:\ngot:  %+v\nwant: %+v", i+1, g, w)
		}
	}
}

// stagedClock 让每次设置带上可预测、严格递增的发生时间，便于断言历史里
// 的时间字段被原样保存。
type stagedClock struct{ cur time.Time }

func newStagedClock() *stagedClock {
	return &stagedClock{cur: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)}
}

func (c *stagedClock) read() time.Time { return c.cur }

func (c *stagedClock) advance() time.Time {
	c.cur = c.cur.Add(time.Second)
	return c.cur
}

// mutateHistoryCopy 以调用者整理展示内容时可能采用的各种方式就地篡改一份
// 历史结果的各条记录：改操作者/原因/请求号/序号/时间，改前后份额里的账户
// 与比例，并向空的前后列表追加份额。只就地改写记录与份额元素——通过返回
// 切片头追加伪造记录无法跨函数传播，因此由各测试自行演示。
func mutateHistoryCopy(evs []RoyaltyEvent) {
	evil := func() RoyaltyShare { return RoyaltyShare{AccountID: "mallory", Rate: 9999} }
	if len(evs) > 0 {
		e := &evs[0]
		e.Operator = "mallory"
		e.Reason = "被篡改的原因"
		e.RequestID = "fake-request"
		e.Seq = 999
		e.OccurredAt = e.OccurredAt.Add(99 * time.Hour)
		if len(e.After) > 0 {
			e.After[0] = evil()
		} else {
			e.After = append(e.After, evil())
		}
		if len(e.Before) > 0 {
			e.Before[0] = RoyaltyShare{AccountID: "mallory", Rate: 1}
		} else {
			e.Before = append(e.Before, evil())
		}
	}
	if len(evs) > 1 {
		e := &evs[1]
		if len(e.Before) > 0 {
			e.Before[len(e.Before)-1].Rate = 4242
		}
		if len(e.After) > 0 {
			e.After[0].AccountID = "mallory"
		}
	}
	if len(evs) > 2 {
		e := &evs[2]
		if len(e.Before) > 0 {
			e.Before[0] = evil()
		}
		// 向变更后的空列表追加：不能借助共享底层数组污染别处。
		e.After = append(e.After, evil())
	}
}

// ---- 正常使用过程：首次发行前连续两份不同规则，再清空 ----

// 同一系列在首次发行前连续设置两份不同规则、再清空；历史按发生顺序保留
// 三次真实变更，前后内容正确链接：第一次变更前为空，第二次变更前等于第
// 一次设置的内容，清空记录变更后为空。操作者、原因、请求号、时间与记录
// 序号都原样保留。
func TestRoyaltyHistorySetTwiceThenClear(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	clock := newStagedClock()
	r.now = clock.read

	ruleA := []RoyaltyShare{{AccountID: "carol", Rate: 2500}, {AccountID: "dave", Rate: 1000}}
	ruleB := []RoyaltyShare{{AccountID: "bob", Rate: 400}, {AccountID: "carol", Rate: 600}}

	t1 := clock.advance()
	if _, err := r.SetRoyalty(SetRoyaltyRequest{
		Operator: "alice", Reason: "第一版规则", RequestID: "rs1", SeriesID: "s1", Shares: ruleA,
	}); err != nil {
		t.Fatal(err)
	}
	t2 := clock.advance()
	if _, err := r.SetRoyalty(SetRoyaltyRequest{
		Operator: "alice", Reason: "第二版规则", RequestID: "rs2", SeriesID: "s1", Shares: ruleB,
	}); err != nil {
		t.Fatal(err)
	}
	t3 := clock.advance()
	if _, err := r.SetRoyalty(SetRoyaltyRequest{
		Operator: "alice", Reason: "清空规则", RequestID: "rs3", SeriesID: "s1",
	}); err != nil {
		t.Fatal(err)
	}

	want := []RoyaltyEvent{
		{Seq: 1, SeriesID: "s1", Operator: "alice", Reason: "第一版规则", RequestID: "rs1",
			Before: []RoyaltyShare{}, After: ruleA, OccurredAt: t1},
		{Seq: 2, SeriesID: "s1", Operator: "alice", Reason: "第二版规则", RequestID: "rs2",
			Before: ruleA, After: ruleB, OccurredAt: t2},
		{Seq: 3, SeriesID: "s1", Operator: "alice", Reason: "清空规则", RequestID: "rs3",
			Before: ruleB, After: []RoyaltyShare{}, OccurredAt: t3},
	}
	evs := mustRoyaltyHistory(t, r, "s1")
	assertEventsDeepEqual(t, evs, want)

	// 清空后当前规则为空。
	cur, err := r.GetSeriesRoyalty("s1")
	if err != nil || len(cur) != 0 {
		t.Fatalf("清空后当前规则应为空: %+v %v", cur, err)
	}
	// 白盒：三次真实变更，序号计数停在 3（查询与篡改都不会增加设置次数）。
	if r.state.NextRoyaltySeq != 3 {
		t.Fatalf("NextRoyaltySeq = %d，期望 3", r.state.NextRoyaltySeq)
	}
}

// ---- 篡改返回记录不影响登记册与其他查询 ----

// 调用者修改自己拿到的历史记录（账户、比例、前后列表、操作者、原因、
// 请求号、时间、序号）后，再次查询得到原先保存的完整内容，登记册内部
// 的规则与历史同样原封不动；本地修改不会成为一次规则设置。
func TestRoyaltyHistoryMutatingReturnedRecordsIsLocal(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	clock := newStagedClock()
	r.now = clock.read

	ruleA := []RoyaltyShare{{AccountID: "carol", Rate: 2500}, {AccountID: "dave", Rate: 1000}}
	ruleB := []RoyaltyShare{{AccountID: "bob", Rate: 400}, {AccountID: "carol", Rate: 600}}
	mkSet := func(rid, reason string, shares []RoyaltyShare) {
		t.Helper()
		clock.advance()
		if _, err := r.SetRoyalty(SetRoyaltyRequest{
			Operator: "alice", Reason: reason, RequestID: rid, SeriesID: "s1", Shares: shares,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mkSet("rs1", "第一版规则", ruleA)
	mkSet("rs2", "第二版规则", ruleB)
	mkSet("rs3", "清空规则", nil)

	want := cloneRoyaltyEvents(mustRoyaltyHistory(t, r, "s1"))

	// 第一次查询的返回结果被调用者按自己的展示需要改写。
	got := mustRoyaltyHistory(t, r, "s1")
	mutateHistoryCopy(got)

	// 再次查询：仍是原先保存的完整内容。
	assertEventsDeepEqual(t, mustRoyaltyHistory(t, r, "s1"), want)
	// 当前规则不受影响（清空后的空规则）。
	cur, err := r.GetSeriesRoyalty("s1")
	if err != nil || len(cur) != 0 {
		t.Fatalf("篡改历史不能改变当前规则: %+v %v", cur, err)
	}
	// 白盒：登记册内部保存的规则、历史与序号计数都保持原样。
	storedCur, storedEvs := readStoredRoyaltyState(t, r, "s1")
	if len(storedCur) != 0 {
		t.Fatalf("内部当前规则被污染: %+v", storedCur)
	}
	assertStoredEventsMatch(t, storedEvs, want)
	if r.state.NextRoyaltySeq != 3 {
		t.Fatalf("篡改返回记录不能成为一次设置，NextRoyaltySeq = %d", r.state.NextRoyaltySeq)
	}

	// 再取一份结果继续改：多次查询之间也互不连带。
	other := mustRoyaltyHistory(t, r, "s1")
	other[0].After[0].Rate = 1
	assertEventsDeepEqual(t, mustRoyaltyHistory(t, r, "s1"), want)
}

// GetSeriesRoyalty 返回的份额列表同样是独立副本：改账户、改比例、追加
// 份额都不能改变登记册保存的当前规则，也不能改变历史中的同名列表。
func TestGetSeriesRoyaltyReturnedSharesAreDetached(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	mustSetRoyalty(t, r, "rs1",
		RoyaltyShare{AccountID: "carol", Rate: 2500},
		RoyaltyShare{AccountID: "dave", Rate: 1000},
	)
	want := []RoyaltyShare{{AccountID: "carol", Rate: 2500}, {AccountID: "dave", Rate: 1000}}

	got, err := r.GetSeriesRoyalty("s1")
	if err != nil {
		t.Fatal(err)
	}
	got[0].AccountID = "mallory"
	got[0].Rate = 9999
	got = append(got, RoyaltyShare{AccountID: "mallory", Rate: 1})

	again, err := r.GetSeriesRoyalty("s1")
	if err != nil {
		t.Fatal(err)
	}
	assertSharesDeepEqual(t, again, want)

	hist := mustRoyaltyHistory(t, r, "s1")
	if len(hist) != 1 || !reflect.DeepEqual(hist[0].After, want) {
		t.Fatalf("篡改当前规则查询结果不能波及历史: %+v", hist)
	}
	storedCur, storedEvs := readStoredRoyaltyState(t, r, "s1")
	assertSharesDeepEqual(t, storedCur, want)
	assertStoredEventsMatch(t, storedEvs, hist)
}

// ---- 同一结果内不同记录相互独立 ----

// 即使一条记录的变更后内容与下一条的变更前内容（甚至变更后内容）完全
// 相同，它们也是各自独立的副本：改其中一处不能连带改掉另一处。
func TestRoyaltyHistoryEqualRulesAcrossRecordsStayIndependent(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	ruleX := []RoyaltyShare{{AccountID: "carol", Rate: 3000}, {AccountID: "dave", Rate: 2000}}
	// 连续两次设置内容完全相同的规则（请求号、原因不同）：第二次的变更
	// 前与变更后都等于第一次的变更后。
	mustSetRoyaltyReason(t, r, "rs1", "首次设置", ruleX)
	mustSetRoyaltyReason(t, r, "rs2", "重申同一规则", ruleX)

	want := cloneRoyaltyEvents(mustRoyaltyHistory(t, r, "s1"))
	got := mustRoyaltyHistory(t, r, "s1")
	if !reflect.DeepEqual(got[0].After, got[1].Before) ||
		!reflect.DeepEqual(got[0].After, got[1].After) {
		t.Fatalf("前置条件不成立：两处规则内容应相同: %+v", got)
	}

	// 改第一条的变更后：第二条的变更前/变更后在本地副本中都不能被带动。
	got[0].After[0].AccountID = "mallory"
	got[0].After[0].Rate = 9999
	// afterOwnEdit0 是第一条变更后被"我自己"改写后应稳定保持的样子。
	afterOwnEdit0 := append([]RoyaltyShare(nil), got[0].After...)
	if !reflect.DeepEqual(got[1].Before, want[1].Before) {
		t.Fatalf("改上一条变更后连带改了下一条变更前: %+v", got[1].Before)
	}
	if !reflect.DeepEqual(got[1].After, want[1].After) {
		t.Fatalf("改上一条变更后连带改了下一条变更后: %+v", got[1].After)
	}
	// 反向：改第二条的变更前，第一条变更后保持它自己被改写后的样子，
	// 不会因为触碰下一条而进一步变化。
	got[1].Before[1].Rate = 1
	if !reflect.DeepEqual(got[0].After, afterOwnEdit0) {
		t.Fatalf("改下一条变更前连带改了上一条变更后: %+v", got[0].After)
	}
	// 向第二条变更前追加份额，不能影响任何其他列表。
	got[1].Before = append(got[1].Before, RoyaltyShare{AccountID: "mallory", Rate: 1})
	if !reflect.DeepEqual(got[0].After, afterOwnEdit0) ||
		!reflect.DeepEqual(got[1].After, want[1].After) {
		t.Fatalf("向前列表追加份额连带改了别处: %+v", got)
	}

	// 登记册与新查询仍保存原始内容。
	assertEventsDeepEqual(t, mustRoyaltyHistory(t, r, "s1"), want)
	_, storedEvs := readStoredRoyaltyState(t, r, "s1")
	assertStoredEventsMatch(t, storedEvs, want)
}

func mustSetRoyaltyReason(t *testing.T, r *Registry, rid, reason string, shares []RoyaltyShare) {
	t.Helper()
	if _, err := r.SetRoyalty(SetRoyaltyRequest{
		Operator: "alice", Reason: reason, RequestID: rid, SeriesID: "s1", Shares: shares,
	}); err != nil {
		t.Fatalf("SetRoyalty: %v", err)
	}
}

// ---- 旧查询快照与后续真实设置、发行、转让相互隔离 ----

// 调用者先保留一份历史结果，系列创建账户随后用新请求设置另一份规则：
// 旧结果继续表示取得时的历史，新查询才出现新增记录。篡改旧结果后，当前
// 规则仍是最后一次真正设置的规则；发行藏品并成功转让时，版税按真实规则
// 计算，应付归原规则账户，而不是历史副本里被改出的账户或比例。
func TestRoyaltyHistoryOldSnapshotVsLaterSetIssueTransfer(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	rule1 := []RoyaltyShare{{AccountID: "carol", Rate: 2000}}
	rule2 := []RoyaltyShare{{AccountID: "bob", Rate: 1000}, {AccountID: "carol", Rate: 500}}
	mustSetRoyaltyReason(t, r, "rs1", "第一版规则", rule1)

	// 先保留一份历史结果（取得时只有一次变更）。
	old := mustRoyaltyHistory(t, r, "s1")
	if len(old) != 1 || !reflect.DeepEqual(old[0].After, rule1) || len(old[0].Before) != 0 {
		t.Fatalf("旧快照应表示取得时的历史: %+v", old)
	}

	// 系列创建账户用新请求设置另一份规则。
	mustSetRoyaltyReason(t, r, "rs2", "第二版规则", rule2)

	// 不做任何修改时，旧结果仍表示取得时的历史（长度、内容都不变）。
	if len(old) != 1 || !reflect.DeepEqual(old[0].After, rule1) {
		t.Fatalf("后续设置不能改变调用者已持有的旧快照: %+v", old)
	}
	fresh := mustRoyaltyHistory(t, r, "s1")
	if len(fresh) != 2 || !reflect.DeepEqual(fresh[0].After, rule1) ||
		!reflect.DeepEqual(fresh[1].Before, rule1) || !reflect.DeepEqual(fresh[1].After, rule2) {
		t.Fatalf("新查询应出现新增记录与新的前后内容: %+v", fresh)
	}
	// 保存新查询的完整基线，篡改旧快照后用于再次核对登记册未被污染。
	wantHist := cloneRoyaltyEvents(fresh)

	// 调用者任意篡改旧结果。
	old[0].Operator = "mallory"
	old[0].Reason = "被篡改"
	old[0].RequestID = "fake"
	old[0].Seq = 77
	old[0].After[0] = RoyaltyShare{AccountID: "mallory", Rate: 9999}
	old[0].Before = append(old[0].Before, RoyaltyShare{AccountID: "mallory", Rate: 9999})
	old = append(old, RoyaltyEvent{Seq: 78, Operator: "mallory"})

	// 篡改旧快照后再查历史：仍是两次真实变更的完整原始内容。
	assertEventsDeepEqual(t, mustRoyaltyHistory(t, r, "s1"), wantHist)

	// 当前规则查询仍返回最后一次真正设置的规则 rule2。
	cur, err := r.GetSeriesRoyalty("s1")
	if err != nil {
		t.Fatal(err)
	}
	assertSharesDeepEqual(t, cur, rule2)

	// 首次发行后规则固定：仍不能修改规则（公开入口与含义不变）。
	if _, err := r.Issue(issueReq("i1", "bob")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetRoyalty(setRoyaltyReq("rs3", RoyaltyShare{AccountID: "dave", Rate: 1})); !errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("首次发行后设置规则必须返回 ErrRoyaltyFrozen, got %v", err)
	}

	// 成功转让：版税按真实规则 rule2 计算（价款 10000 分）。
	// bob 1000/10000 -> 1000；carol 500/10000 -> 500；余款 8500 归 bob。
	req := xferReq("bob", "i1", "dave", 1, "t1")
	req.Price = 10000
	res, err := r.Transfer(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Price != 10000 || len(res.Payables) != 2 || res.Remainder != 8500 {
		t.Fatalf("转让结果异常: %+v", res)
	}
	if res.Payables[0].AccountID != "bob" || res.Payables[0].Rate != 1000 || res.Payables[0].Amount != 1000 ||
		res.Payables[1].AccountID != "carol" || res.Payables[1].Rate != 500 || res.Payables[1].Amount != 500 {
		t.Fatalf("应付必须按真实规则计算并归原规则账户，不能落入 mallory: %+v", res.Payables)
	}
	// 按账户核对应付：原规则账户各有一笔，受让人与被篡改账户都没有应付。
	bobPay, _ := r.PayablesOf("bob")
	carolPay, _ := r.PayablesOf("carol")
	davePay, _ := r.PayablesOf("dave")
	if len(bobPay) != 1 || bobPay[0].Amount != 1000 ||
		len(carolPay) != 1 || carolPay[0].Amount != 500 || len(davePay) != 0 {
		t.Fatalf("应付归属错误: bob=%+v carol=%+v dave=%+v", bobPay, carolPay, davePay)
	}

	// 发行与转让不改变版税规则历史；当前规则与新查询都还是真实内容。
	cur, _ = r.GetSeriesRoyalty("s1")
	assertSharesDeepEqual(t, cur, rule2)
	fresh = mustRoyaltyHistory(t, r, "s1")
	if len(fresh) != 2 || !reflect.DeepEqual(fresh[1].After, rule2) {
		t.Fatalf("发行与转让后历史仍应只有两次真实变更: %+v", fresh)
	}
	if r.state.NextRoyaltySeq != 2 {
		t.Fatalf("篡改与转让都不能增加规则设置次数, NextRoyaltySeq=%d", r.state.NextRoyaltySeq)
	}
}

// ---- 空历史与空前后列表互不影响 ----

// 未设置过版税规则的已登记系列返回空历史；在别的系列上设置或清空产生的
// 空前后列表彼此独立，追加内容也不能污染其他记录或其他系列。
func TestRoyaltyHistoryEmptySeriesAndEmptyListsIsolation(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	if err := r.CreateSeries("s2", "alice", ""); err != nil {
		t.Fatal(err)
	}

	// s1 从未设置：空历史、空当前规则。
	evs := mustRoyaltyHistory(t, r, "s1")
	if len(evs) != 0 {
		t.Fatalf("未设置过规则的系列应返回空历史: %+v", evs)
	}
	cur, err := r.GetSeriesRoyalty("s1")
	if err != nil || len(cur) != 0 {
		t.Fatalf("未设置过规则的系列当前规则应为空: %+v %v", cur, err)
	}

	ruleX := []RoyaltyShare{{AccountID: "carol", Rate: 3000}}
	// s2：先清空（前后皆空），再设置（前空后 X），再清空（前 X 后空）。
	mustSetRoyaltyReasonSeries(t, r, "s2", "c1", "先清空", nil)
	mustSetRoyaltyReasonSeries(t, r, "s2", "c2", "设置 X", ruleX)
	mustSetRoyaltyReasonSeries(t, r, "s2", "c3", "再清空", nil)

	want := []RoyaltyEvent{
		{Seq: 1, SeriesID: "s2", Operator: "alice", Reason: "先清空", RequestID: "c1",
			Before: []RoyaltyShare{}, After: []RoyaltyShare{}},
		{Seq: 2, SeriesID: "s2", Operator: "alice", Reason: "设置 X", RequestID: "c2",
			Before: []RoyaltyShare{}, After: ruleX},
		{Seq: 3, SeriesID: "s2", Operator: "alice", Reason: "再清空", RequestID: "c3",
			Before: ruleX, After: []RoyaltyShare{}},
	}
	got := mustRoyaltyHistory(t, r, "s2")
	// 时间字段单独按非零与递增核对，基线里补上实际时间。
	for i := range want {
		want[i].OccurredAt = got[i].OccurredAt
	}
	assertEventsDeepEqual(t, got, want)

	// 向第一条（前后皆空）和第三条（后为空）的空列表追加份额：不能污染
	// 第二条的空前列表、ruleX，或任何其他记录。
	got[0].Before = append(got[0].Before, RoyaltyShare{AccountID: "mallory", Rate: 1})
	got[0].After = append(got[0].After, RoyaltyShare{AccountID: "mallory", Rate: 1})
	got[2].After = append(got[2].After, RoyaltyShare{AccountID: "mallory", Rate: 1})
	got[1].After[0].Rate = 9999

	assertEventsDeepEqual(t, mustRoyaltyHistory(t, r, "s2"), want)
	// s1 仍为空历史，s2 的设置/清空没有串到别的系列。
	if evs := mustRoyaltyHistory(t, r, "s1"); len(evs) != 0 {
		t.Fatalf("其他系列的变更不能产生 s1 的历史: %+v", evs)
	}
	// 当前规则为空，且白盒内部历史不被污染。
	cur, _ = r.GetSeriesRoyalty("s2")
	if len(cur) != 0 {
		t.Fatalf("清空后当前规则应为空: %+v", cur)
	}
	storedCur, storedEvs := readStoredRoyaltyState(t, r, "s2")
	if len(storedCur) != 0 {
		t.Fatalf("内部当前规则被污染: %+v", storedCur)
	}
	assertStoredEventsMatch(t, storedEvs, want)
}

func mustSetRoyaltyReasonSeries(t *testing.T, r *Registry, seriesID, rid, reason string, shares []RoyaltyShare) {
	t.Helper()
	if _, err := r.SetRoyalty(SetRoyaltyRequest{
		Operator: "alice", Reason: reason, RequestID: rid, SeriesID: seriesID, Shares: shares,
	}); err != nil {
		t.Fatalf("SetRoyalty(%s): %v", seriesID, err)
	}
}
