package registry

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// royaltyWorld 建立 alice（创作者）、bob、carol、dave 四个可用账户与
// alice 创建的系列 s1（尚未发行，版税规则可设置）。
func royaltyWorld(t *testing.T, r *Registry) {
	t.Helper()
	setupWorld(t, r)
	for _, id := range []string{"carol", "dave"} {
		if err := r.RegisterAccount(id, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func setRoyaltyReq(rid string, shares ...RoyaltyShare) SetRoyaltyRequest {
	return SetRoyaltyRequest{
		Operator: "alice", Reason: "设定版税", RequestID: rid,
		SeriesID: "s1", Shares: shares,
	}
}

func mustSetRoyalty(t *testing.T, r *Registry, rid string, shares ...RoyaltyShare) {
	t.Helper()
	if _, err := r.SetRoyalty(setRoyaltyReq(rid, shares...)); err != nil {
		t.Fatalf("SetRoyalty: %v", err)
	}
}

// ---- 规则设置与查询 ----

func TestSetAndQueryRoyalty(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	res, err := r.SetRoyalty(setRoyaltyReq("rs1",
		RoyaltyShare{AccountID: "carol", Rate: 2500},
		RoyaltyShare{AccountID: "bob", Rate: 1000},
	))
	if err != nil {
		t.Fatalf("SetRoyalty: %v", err)
	}
	if res.Replayed || len(res.Shares) != 2 {
		t.Fatalf("unexpected set result: %+v", res)
	}
	// 规则按收款账户排序规范化
	rule, err := r.GetSeriesRoyalty("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rule) != 2 || rule[0].AccountID != "bob" || rule[0].Rate != 1000 ||
		rule[1].AccountID != "carol" || rule[1].Rate != 2500 {
		t.Fatalf("rule wrong: %+v", rule)
	}
	// 变更记录包含操作者、原因、请求号与前后内容
	evs, err := r.RoyaltyHistory("s1")
	if err != nil || len(evs) != 1 {
		t.Fatalf("royalty history: %v %v", evs, err)
	}
	if e := evs[0]; e.Operator != "alice" || e.Reason != "设定版税" ||
		e.RequestID != "rs1" || len(e.Before) != 0 || len(e.After) != 2 {
		t.Fatalf("royalty event wrong: %+v", e)
	}
	// 再次设置（改为只给 dave），前后内容都记录
	mustSetRoyalty(t, r, "rs2", RoyaltyShare{AccountID: "dave", Rate: 500})
	evs, _ = r.RoyaltyHistory("s1")
	if len(evs) != 2 || len(evs[1].Before) != 2 ||
		len(evs[1].After) != 1 || evs[1].After[0].AccountID != "dave" {
		t.Fatalf("second event wrong: %+v", evs)
	}
	// 清空规则：空份额表示不收版税
	mustSetRoyalty(t, r, "rs3")
	rule, _ = r.GetSeriesRoyalty("s1")
	if len(rule) != 0 {
		t.Fatalf("cleared rule should be empty: %+v", rule)
	}
	evs, _ = r.RoyaltyHistory("s1")
	if len(evs) != 3 || len(evs[2].After) != 0 || len(evs[2].Before) != 1 {
		t.Fatalf("clear event wrong: %+v", evs[2])
	}
}

func TestSetRoyaltyValidation(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	cases := []struct {
		name   string
		shares []RoyaltyShare
	}{
		{"重复账户", []RoyaltyShare{{AccountID: "bob", Rate: 1}, {AccountID: "bob", Rate: 2}}},
		{"比例为零", []RoyaltyShare{{AccountID: "bob", Rate: 0}}},
		{"比例超上限", []RoyaltyShare{{AccountID: "bob", Rate: 10001}}},
		{"比例为负", []RoyaltyShare{{AccountID: "bob", Rate: -5}}},
		{"合计超限", []RoyaltyShare{{AccountID: "bob", Rate: 6000}, {AccountID: "carol", Rate: 4001}}},
		{"空账户", []RoyaltyShare{{AccountID: " ", Rate: 1}}},
	}
	for i, tc := range cases {
		rid := fmt.Sprintf("bad-%d", i)
		if _, err := r.SetRoyalty(setRoyaltyReq(rid, tc.shares...)); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: want ErrInvalidArgument, got %v", tc.name, err)
		}
		// 参数错误不占用请求号：同一请求号修正参数后可用
		if _, err := r.SetRoyalty(setRoyaltyReq(rid, RoyaltyShare{AccountID: "dave", Rate: 10})); err != nil {
			t.Fatalf("%s: request id should not be consumed: %v", tc.name, err)
		}
	}
	// 参数不合法时原规则不变
	r2 := mustCreate(t, filepath.Join(t.TempDir(), "d2"))
	royaltyWorld(t, r2)
	mustSetRoyalty(t, r2, "keep", RoyaltyShare{AccountID: "bob", Rate: 777})
	if _, err := r2.SetRoyalty(setRoyaltyReq("bad",
		RoyaltyShare{AccountID: "bob", Rate: 1}, RoyaltyShare{AccountID: "bob", Rate: 2})); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	rule, _ := r2.GetSeriesRoyalty("s1")
	if len(rule) != 1 || rule[0].Rate != 777 {
		t.Fatalf("invalid set must keep original rule: %+v", rule)
	}
}

func TestSetRoyaltyPermissionAndState(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	// 其他账户操作明确拒绝
	req := setRoyaltyReq("x1", RoyaltyShare{AccountID: "bob", Rate: 1})
	req.Operator = "bob"
	if _, err := r.SetRoyalty(req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-creator must be forbidden, got %v", err)
	}
	// 操作者不存在 -> ErrNotFound
	req = setRoyaltyReq("x2", RoyaltyShare{AccountID: "bob", Rate: 1})
	req.Operator = "ghost"
	if _, err := r.SetRoyalty(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown operator: %v", err)
	}
	// 系列不存在 -> ErrNotFound
	req = setRoyaltyReq("x3", RoyaltyShare{AccountID: "bob", Rate: 1})
	req.SeriesID = "nope"
	if _, err := r.SetRoyalty(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown series: %v", err)
	}
	// 收款账户不存在 -> ErrNotFound，不占用请求号
	if _, err := r.SetRoyalty(setRoyaltyReq("x4", RoyaltyShare{AccountID: "ghost", Rate: 1})); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown payee: %v", err)
	}
	if _, err := r.SetRoyalty(setRoyaltyReq("x4", RoyaltyShare{AccountID: "bob", Rate: 1})); err != nil {
		t.Fatalf("request id x4 should not be consumed: %v", err)
	}
	// 收款账户已停用 -> ErrAccountInactive
	if err := r.DeactivateAccount("dave"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetRoyalty(setRoyaltyReq("x5", RoyaltyShare{AccountID: "dave", Rate: 1})); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive payee: %v", err)
	}
	// 操作者（创建账户）停用 -> ErrAccountInactive
	if err := r.DeactivateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetRoyalty(setRoyaltyReq("x6", RoyaltyShare{AccountID: "bob", Rate: 1})); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive operator: %v", err)
	}
}

func TestRoyaltyFrozenAfterFirstIssue(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	mustSetRoyalty(t, r, "rs1", RoyaltyShare{AccountID: "carol", Rate: 500})

	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	// 首次成功发行后规则固定
	if _, err := r.SetRoyalty(setRoyaltyReq("rs2", RoyaltyShare{AccountID: "bob", Rate: 1})); !errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("frozen rule must reject, got %v", err)
	}
	// 清空同样被拒绝
	if _, err := r.SetRoyalty(setRoyaltyReq("rs3")); !errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("clearing frozen rule must reject, got %v", err)
	}
	rule, _ := r.GetSeriesRoyalty("s1")
	if len(rule) != 1 || rule[0].AccountID != "carol" || rule[0].Rate != 500 {
		t.Fatalf("frozen rule unchanged: %+v", rule)
	}

	// 未设置过规则的系列，首次发行后固定为无版税
	if err := r.CreateSeries("s2", "alice", ""); err != nil {
		t.Fatal(err)
	}
	req := issueReq("i2", "bob")
	req.SeriesID = "s2"
	if _, err := r.Issue(req); err != nil {
		t.Fatal(err)
	}
	set2 := setRoyaltyReq("rs4", RoyaltyShare{AccountID: "bob", Rate: 1})
	set2.SeriesID = "s2"
	if _, err := r.SetRoyalty(set2); !errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("never-set series must freeze as no-royalty, got %v", err)
	}
	rule, _ = r.GetSeriesRoyalty("s2")
	if len(rule) != 0 {
		t.Fatalf("never-set series rule should be empty: %+v", rule)
	}
}

func TestSetRoyaltyRejectedWhenSealed(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetRoyalty(setRoyaltyReq("rs1", RoyaltyShare{AccountID: "bob", Rate: 1})); !errors.Is(err, ErrSeriesSealed) {
		t.Fatalf("sealed series must reject royalty set, got %v", err)
	}
}

func TestSetRoyaltyIdempotency(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)

	req := setRoyaltyReq("rs1",
		RoyaltyShare{AccountID: "carol", Rate: 2500},
		RoyaltyShare{AccountID: "bob", Rate: 1000},
	)
	first, err := r.SetRoyalty(req)
	if err != nil {
		t.Fatal(err)
	}
	// 相同参数重提（即使书写顺序不同）回放首次结果
	again, err := r.SetRoyalty(setRoyaltyReq("rs1",
		RoyaltyShare{AccountID: "bob", Rate: 1000},
		RoyaltyShare{AccountID: "carol", Rate: 2500},
	))
	if err != nil || !again.Replayed {
		t.Fatalf("same rule in different order must replay: %+v %v", again, err)
	}
	if len(again.Shares) != len(first.Shares) {
		t.Fatalf("replayed shares mismatch: %+v vs %+v", again, first)
	}
	evs, _ := r.RoyaltyHistory("s1")
	if len(evs) != 1 {
		t.Fatalf("replay must not add royalty events: %d", len(evs))
	}
	// 改规则 -> 请求号冲突
	if _, err := r.SetRoyalty(setRoyaltyReq("rs1", RoyaltyShare{AccountID: "bob", Rate: 1000})); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed rule must conflict, got %v", err)
	}
	// 改原因 -> 请求号冲突
	bad := setRoyaltyReq("rs1",
		RoyaltyShare{AccountID: "carol", Rate: 2500},
		RoyaltyShare{AccountID: "bob", Rate: 1000},
	)
	bad.Reason = "换个说法"
	if _, err := r.SetRoyalty(bad); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed reason must conflict, got %v", err)
	}
	// 规则设置与发行共用操作者的请求号
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	ireq := issueReq("i2", "alice")
	ireq.RequestID = "rs1"
	if _, err := r.Issue(ireq); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request id shared with royalty set must conflict, got %v", err)
	}
}

func TestSetRoyaltyRejectedReplays(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	req := setRoyaltyReq("rs1", RoyaltyShare{AccountID: "bob", Rate: 1})
	req.Operator = "bob" // 非创建账户：状态类拒绝，占用请求号
	if _, err := r.SetRoyalty(req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v", err)
	}
	res, err := r.SetRoyalty(req)
	if !errors.Is(err, ErrForbidden) || !res.Replayed {
		t.Fatalf("rejection must replay: %+v %v", res, err)
	}
}

// 设置与首次发行同时发生：存在一个完整先后次序——先完成的设置被发行
// 采用，或发行先完成使设置被拒绝，恰有一个结果。
func TestConcurrentSetRoyaltyVsFirstIssue(t *testing.T) {
	for range 20 {
		r := mustCreate(t, filepath.Join(t.TempDir(), "d"))
		royaltyWorld(t, r)
		var wg sync.WaitGroup
		var setErr, issueErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, setErr = r.SetRoyalty(setRoyaltyReq("rs1", RoyaltyShare{AccountID: "carol", Rate: 5000}))
		}()
		go func() {
			defer wg.Done()
			_, issueErr = r.Issue(issueReq("i1", "alice"))
		}()
		wg.Wait()
		if issueErr != nil {
			t.Fatalf("first issue should always succeed: %v", issueErr)
		}
		if setErr != nil && !errors.Is(setErr, ErrRoyaltyFrozen) {
			t.Fatalf("set either applies or is frozen-rejected, got %v", setErr)
		}
		rule, _ := r.GetSeriesRoyalty("s1")
		if setErr == nil {
			// 设置先完成：发行采用新规则，之后的转让按新规则记录应付
			if len(rule) != 1 || rule[0].Rate != 5000 {
				t.Fatalf("set-first rule wrong: %+v", rule)
			}
			req := xferReq("alice", "i1", "bob", 1, "t1")
			req.Price = 100
			res, err := r.Transfer(req)
			if err != nil || len(res.Payables) != 1 || res.Payables[0].Amount != 50 {
				t.Fatalf("transfer must use the new rule: %+v %v", res, err)
			}
		} else {
			// 发行先完成：规则固定为无版税
			if len(rule) != 0 {
				t.Fatalf("issue-first rule must be empty: %+v", rule)
			}
		}
	}
}

// ---- 转让价款与应付计算 ----

func TestTransferRoyaltyCalculation(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	mustSetRoyalty(t, r, "rs1",
		RoyaltyShare{AccountID: "carol", Rate: 2500}, // 25%
		RoyaltyShare{AccountID: "dave", Rate: 1000},  // 10%
	)
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}

	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = 12345
	res, err := r.Transfer(req)
	if err != nil {
		t.Fatal(err)
	}
	// 2500/10000 * 12345 = 3086.25 -> 3086；1000/10000 * 12345 = 1234.5 -> 1234
	if res.Price != 12345 || len(res.Payables) != 2 {
		t.Fatalf("transfer result wrong: %+v", res)
	}
	if res.Payables[0].AccountID != "carol" || res.Payables[0].Amount != 3086 ||
		res.Payables[1].AccountID != "dave" || res.Payables[1].Amount != 1234 {
		t.Fatalf("payables wrong: %+v", res.Payables)
	}
	if res.Remainder != 12345-3086-1234 {
		t.Fatalf("remainder wrong: %d", res.Remainder)
	}

	// 按转让记录查询计算依据与全部金额
	ro, err := r.TransferRoyalty(res.TxSeq)
	if err != nil {
		t.Fatal(err)
	}
	if ro.Price != 12345 || ro.OwnerID != "alice" || ro.Remainder != res.Remainder ||
		len(ro.Payables) != 2 || ro.ItemID != "i1" {
		t.Fatalf("TransferRoyalty wrong: %+v", ro)
	}
}

func TestTransferRoyaltyZeroAmountKept(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	// 比例 1/10000，价款 100 分：应付向下取整为 0，明细仍保留
	mustSetRoyalty(t, r, "rs1", RoyaltyShare{AccountID: "carol", Rate: 1})
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = 100
	res, err := r.Transfer(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Payables) != 1 || res.Payables[0].AccountID != "carol" ||
		res.Payables[0].Amount != 0 {
		t.Fatalf("zero amount must be kept: %+v", res.Payables)
	}
	if res.Remainder != 100 {
		t.Fatalf("remainder should be full price: %d", res.Remainder)
	}
	ro, err := r.TransferRoyalty(res.TxSeq)
	if err != nil || len(ro.Payables) != 1 || ro.Payables[0].Amount != 0 {
		t.Fatalf("zero amount must persist in record: %+v %v", ro, err)
	}
}

func TestTransferRoyaltyNoRule(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = 999
	res, err := r.Transfer(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Payables) != 0 || res.Remainder != 999 {
		t.Fatalf("no rule: all price goes to holder: %+v", res)
	}
	ro, err := r.TransferRoyalty(res.TxSeq)
	if err != nil || ro.Price != 999 || ro.Remainder != 999 || len(ro.Payables) != 0 {
		t.Fatalf("TransferRoyalty wrong: %+v %v", ro, err)
	}
}

func TestTransferNegativePrice(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = -1
	if _, err := r.Transfer(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative price must be invalid argument, got %v", err)
	}
	// 参数错误不占用请求号
	req.Price = 0
	if _, err := r.Transfer(req); err != nil {
		t.Fatalf("request id must not be consumed by invalid price: %v", err)
	}
}

func TestTransferRoyaltyFullRange(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	mustSetRoyalty(t, r, "rs1", RoyaltyShare{AccountID: "carol", Rate: 10000}) // 100%
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = math.MaxInt64
	res, err := r.Transfer(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Payables[0].Amount != math.MaxInt64 || res.Remainder != 0 {
		t.Fatalf("full-range amount wrong: %+v", res)
	}

	// 大价款下的向下取整：MaxInt64 * 9999 / 10000，乘积超出 int64
	r2 := mustCreate(t, filepath.Join(t.TempDir(), "d2"))
	royaltyWorld(t, r2)
	mustSetRoyalty(t, r2, "rs1", RoyaltyShare{AccountID: "carol", Rate: 9999})
	if _, err := r2.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	req = xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = math.MaxInt64
	res2, err := r2.Transfer(req)
	if err != nil {
		t.Fatal(err)
	}
	bi := big.NewInt(math.MaxInt64)
	bi.Mul(bi, big.NewInt(9999))
	bi.Div(bi, big.NewInt(10000))
	want := bi.Int64()
	if res2.Payables[0].Amount != want || res2.Remainder != math.MaxInt64-want {
		t.Fatalf("floor division wrong: got %d want %d", res2.Payables[0].Amount, want)
	}
}

func TestRoyaltyPayeeCanBeParticipant(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	// 收款人同时是转让接收人
	mustSetRoyalty(t, r, "rs1", RoyaltyShare{AccountID: "bob", Rate: 2000})
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = 1000
	res, err := r.Transfer(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Payables) != 1 || res.Payables[0].AccountID != "bob" || res.Payables[0].Amount != 200 {
		t.Fatalf("participant payee wrong: %+v", res.Payables)
	}
}

func TestDeactivatedPayeeKeepsPayables(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	mustSetRoyalty(t, r, "rs1", RoyaltyShare{AccountID: "carol", Rate: 5000})
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	// 仅作为版税收款人的账户停用，不阻止新转让
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = 1000
	res, err := r.Transfer(req)
	if err != nil {
		t.Fatalf("transfer with deactivated payee must succeed: %v", err)
	}
	if len(res.Payables) != 1 || res.Payables[0].Amount != 500 {
		t.Fatalf("payable for deactivated payee must be recorded: %+v", res.Payables)
	}
	// 停用账户的应付明细仍可查询
	entries, err := r.PayablesOf("carol")
	if err != nil || len(entries) != 1 || entries[0].Amount != 500 {
		t.Fatalf("PayablesOf for deactivated payee: %+v %v", entries, err)
	}
}

// ---- 代转价款 ----

func TestProxyTransferPriceFromAuthorization(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	mustSetRoyalty(t, r, "rs1", RoyaltyShare{AccountID: "dave", Rate: 1000})
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}

	creq := CreateAuthorizationRequest{
		Operator: "alice", Reason: "委托代转", RequestID: "create-a1",
		AuthID: "a1", ItemID: "i1", TrusteeID: "bob", ToID: "carol",
		ExpectedOwner: "alice", ExpectedVer: 1,
		ExpiresAt: r.now().Add(time.Hour), Price: 20000,
	}
	if _, err := r.CreateAuthorization(creq); err != nil {
		t.Fatal(err)
	}
	a, _ := r.GetAuthorization("a1")
	if a.Price != 20000 {
		t.Fatalf("authorization must carry price: %+v", a)
	}
	// 执行时不得另行改价：代转请求没有价款参数，金额以授权记载为准
	res, err := r.ProxyTransfer(proxyReq("bob", "a1", "p1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Price != 20000 || len(res.Payables) != 1 || res.Payables[0].Amount != 2000 {
		t.Fatalf("proxy royalty wrong: %+v", res)
	}
	// 余款归转让前持有人（授权人 alice），不是受托人 bob
	if res.Remainder != 18000 {
		t.Fatalf("remainder wrong: %d", res.Remainder)
	}
	ro, err := r.TransferRoyalty(res.TxSeq)
	if err != nil || ro.OwnerID != "alice" || ro.Remainder != 18000 {
		t.Fatalf("proxy remainder must go to granter: %+v %v", ro, err)
	}
	// 重放仍返回原金额
	again, err := r.ProxyTransfer(proxyReq("bob", "a1", "p1"))
	if err != nil || !again.Replayed || again.Price != 20000 || again.Remainder != 18000 ||
		len(again.Payables) != 1 {
		t.Fatalf("proxy replay must return original amounts: %+v %v", again, err)
	}
}

func TestCreateAuthorizationNegativePrice(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	creq := CreateAuthorizationRequest{
		Operator: "alice", Reason: "委托", RequestID: "create-a1",
		AuthID: "a1", ItemID: "i1", TrusteeID: "bob", ToID: "carol",
		ExpectedOwner: "alice", ExpectedVer: 1,
		ExpiresAt: r.now().Add(time.Hour), Price: -1,
	}
	if _, err := r.CreateAuthorization(creq); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative price must be invalid, got %v", err)
	}
	creq.Price = 100
	if _, err := r.CreateAuthorization(creq); err != nil {
		t.Fatalf("request id must not be consumed: %v", err)
	}
	// 改价 -> 请求号冲突
	creq.Price = 200
	if _, err := r.CreateAuthorization(creq); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed price must conflict, got %v", err)
	}
}

func TestFailedProxyTransferNoPayable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	mustSetRoyalty(t, r, "rs1", RoyaltyShare{AccountID: "dave", Rate: 1000})
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	creq := CreateAuthorizationRequest{
		Operator: "alice", Reason: "委托", RequestID: "create-a1",
		AuthID: "a1", ItemID: "i1", TrusteeID: "bob", ToID: "carol",
		ExpectedOwner: "alice", ExpectedVer: 1,
		ExpiresAt: r.now().Add(time.Hour), Price: 5000,
	}
	if _, err := r.CreateAuthorization(creq); err != nil {
		t.Fatal(err)
	}
	// 非受托人代转失败：不新增应付，也不消耗授权
	if _, err := r.ProxyTransfer(proxyReq("carol", "a1", "p1")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v", err)
	}
	entries, err := r.PayablesOf("dave")
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed proxy must not add payables: %+v %v", entries, err)
	}
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthActive {
		t.Fatalf("failed proxy must not consume authorization: %s", a.Status)
	}
}

// ---- 应付明细查询 ----

func TestPayablesOfOrderingAndEmpty(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	mustSetRoyalty(t, r, "rs1", RoyaltyShare{AccountID: "carol", Rate: 1000})
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Issue(issueReq("i2", "bob")); err != nil {
		t.Fatal(err)
	}
	// 先转 i2（序号更大），再转 i1：查询结果必须按转让序号排列
	q1 := xferReq("bob", "i2", "alice", 1, "t-a")
	q1.Price = 1000
	if _, err := r.Transfer(q1); err != nil {
		t.Fatal(err)
	}
	q2 := xferReq("alice", "i1", "bob", 1, "t-b")
	q2.Price = 2000
	res2, err := r.Transfer(q2)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := r.PayablesOf("carol")
	if err != nil || len(entries) != 2 {
		t.Fatalf("PayablesOf: %+v %v", entries, err)
	}
	if entries[0].TxSeq > entries[1].TxSeq || entries[1].TxSeq != res2.TxSeq {
		t.Fatalf("entries must be ordered by tx seq: %+v", entries)
	}
	if entries[0].Amount != 100 || entries[1].Amount != 200 {
		t.Fatalf("amounts wrong: %+v", entries)
	}
	if entries[0].Price != 1000 || entries[0].Rate != 1000 || entries[0].ItemID != "i2" {
		t.Fatalf("entry must carry calculation basis: %+v", entries[0])
	}
	// 无明细的账户返回空列表
	entries, err = r.PayablesOf("bob")
	if err != nil || len(entries) != 0 {
		t.Fatalf("account without payables must get empty list: %+v %v", entries, err)
	}
	// 不存在的账户明确报错
	if _, err := r.PayablesOf("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
}

func TestTransferRoyaltyNotFound(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	// 发行记录不是转让记录
	if _, err := r.TransferRoyalty(1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("issue seq must be not found, got %v", err)
	}
	// 不存在的序号
	if _, err := r.TransferRoyalty(99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown seq: %v", err)
	}
	if _, err := r.TransferRoyalty(0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid seq: %v", err)
	}
}

// ---- 幂等与重放 ----

func TestTransferPriceIdempotency(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	royaltyWorld(t, r)
	mustSetRoyalty(t, r, "rs1", RoyaltyShare{AccountID: "carol", Rate: 5000})
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = 1000
	first, err := r.Transfer(req)
	if err != nil {
		t.Fatal(err)
	}
	// 藏品再次易手后，原请求仍返回原金额，不重复计入
	if _, err := r.Transfer(xferReq("bob", "i1", "dave", 2, "t2")); err != nil {
		t.Fatal(err)
	}
	before, _ := r.PayablesOf("carol")
	again, err := r.Transfer(req)
	if err != nil || !again.Replayed {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if again.Price != 1000 || again.Remainder != 500 || len(again.Payables) != 1 ||
		again.Payables[0].Amount != 500 {
		t.Fatalf("replayed amounts wrong: %+v (first %+v)", again, first)
	}
	after, _ := r.PayablesOf("carol")
	if len(after) != len(before) {
		t.Fatalf("replay must not double-count payables: before=%+v after=%+v", before, after)
	}
	// 改价 -> 请求号冲突
	req.Price = 1001
	if _, err := r.Transfer(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed price must conflict, got %v", err)
	}
}

// ---- 持久化 ----

func TestRoyaltyPersistenceAcrossReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	royaltyWorld(t, r)
	mustSetRoyalty(t, r, "rs1", RoyaltyShare{AccountID: "carol", Rate: 2500})
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	req := xferReq("alice", "i1", "bob", 1, "t1")
	req.Price = 10000
	res, err := r.Transfer(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	rule, _ := r2.GetSeriesRoyalty("s1")
	if len(rule) != 1 || rule[0].AccountID != "carol" || rule[0].Rate != 2500 {
		t.Fatalf("rule must persist: %+v", rule)
	}
	evs, _ := r2.RoyaltyHistory("s1")
	if len(evs) != 1 || evs[0].RequestID != "rs1" {
		t.Fatalf("royalty events must persist: %+v", evs)
	}
	ro, err := r2.TransferRoyalty(res.TxSeq)
	if err != nil || ro.Price != 10000 || ro.Remainder != 7500 ||
		len(ro.Payables) != 1 || ro.Payables[0].Amount != 2500 {
		t.Fatalf("royalty record must persist: %+v %v", ro, err)
	}
	// 重开后原转让请求仍返回原金额
	again, err := r2.Transfer(req)
	if err != nil || !again.Replayed || again.Price != 10000 || again.Remainder != 7500 {
		t.Fatalf("replayed amounts must persist: %+v %v", again, err)
	}
	entries, _ := r2.PayablesOf("carol")
	if len(entries) != 1 || entries[0].Amount != 2500 {
		t.Fatalf("payables must persist: %+v", entries)
	}
	// 规则已固定状态也要保留
	if _, err := r2.SetRoyalty(setRoyaltyReq("rs2", RoyaltyShare{AccountID: "bob", Rate: 1})); !errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("frozen state must persist, got %v", err)
	}
}

// ---- 旧登记册兼容 ----

// 手写一份引入版税功能之前的旧格式快照：没有 authzs/royalties 等新字段，
// 含一次发行与一次转让及其请求结果。旧登记册必须能直接打开：历史保留、
// 旧请求可回放、旧成交按价款 0 查询且不补造应付明细。
func TestOpenLegacyRegistry(t *testing.T) {
	dir := tempDir(t)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	// 请求结果的键是 操作者 + "\x00" + 请求号；JSON 文本中以 ‌ 转义。
	legacy := `{
  "version": 1,
  "accounts": {
    "alice": {"id": "alice", "metadata": "", "active": true},
    "bob": {"id": "bob", "metadata": "", "active": true},
    "carol": {"id": "carol", "metadata": "", "active": true}
  },
  "series": {
    "s1": {"id": "s1", "creator_id": "alice", "metadata": "", "sealed": false},
    "s2": {"id": "s2", "creator_id": "alice", "metadata": "", "sealed": false}
  },
  "items": {
    "i1": {"id": "i1", "series_id": "s1", "batch_no": "b1", "metadata": "", "issued_tx_id": 1}
  },
  "holdings": {
    "i1": {"item_id": "i1", "owner_id": "bob", "version": 2}
  },
  "history": [
    {"seq": 1, "kind": "issue", "item_id": "i1", "operator": "alice", "reason": "首发",
     "request_id": "req-i1", "from_id": "", "to_id": "alice", "from_version": 0, "to_version": 1},
    {"seq": 2, "kind": "transfer", "item_id": "i1", "operator": "alice", "reason": "卖出",
     "request_id": "t1", "from_id": "alice", "to_id": "bob", "from_version": 1, "to_version": 2}
  ],
  "requests": {
    "alice@NUL@req-i1": {"operator": "alice", "request_id": "req-i1", "kind": "issue",
      "params": "{\"kind\":\"issue\",\"item_id\":\"i1\",\"series_id\":\"s1\",\"batch_no\":\"b1\",\"metadata\":\"\",\"holder_id\":\"alice\",\"reason\":\"首发\"}",
      "rejected": false, "reject_reason": "", "item_id": "i1", "to_id": "alice", "version": 1, "tx_seq": 1},
    "alice@NUL@t1": {"operator": "alice", "request_id": "t1", "kind": "transfer",
      "params": "{\"kind\":\"transfer\",\"item_id\":\"i1\",\"expected_owner\":\"alice\",\"expected_version\":1,\"to_id\":\"bob\",\"reason\":\"卖出\"}",
      "rejected": false, "reject_reason": "", "item_id": "i1", "from_id": "alice", "to_id": "bob", "version": 2, "tx_seq": 2}
  },
  "next_seq": 2
}`
	legacy = strings.ReplaceAll(legacy, "@NUL@", "\\u0000")
	if err := os.WriteFile(dataFile(dir), []byte(legacy), fileMode); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("legacy registry must open: %v", err)
	}
	defer r.Close()

	// 旧历史保留
	hist, err := r.History("i1")
	if err != nil || len(hist) != 2 {
		t.Fatalf("legacy history must be kept: %+v %v", hist, err)
	}
	// 旧成交按价款 0 查询，不补造应付明细
	ro, err := r.TransferRoyalty(2)
	if err != nil {
		t.Fatal(err)
	}
	if ro.Price != 0 || ro.Remainder != 0 || len(ro.Payables) != 0 || ro.OwnerID != "alice" {
		t.Fatalf("legacy transfer must read as zero price: %+v", ro)
	}
	// 旧请求仍可回放（旧签名不含价款，等价于价款 0）
	ireq := IssueRequest{Operator: "alice", Reason: "首发", RequestID: "req-i1",
		ItemID: "i1", SeriesID: "s1", BatchNo: "b1", HolderID: "alice"}
	ires, err := r.Issue(ireq)
	if err != nil || !ires.Replayed || ires.TxSeq != 1 {
		t.Fatalf("legacy issue request must replay: %+v %v", ires, err)
	}
	treq := TransferRequest{Operator: "alice", Reason: "卖出", RequestID: "t1",
		ItemID: "i1", ExpectedOwner: "alice", ExpectedVer: 1, ToID: "bob"}
	tres, err := r.Transfer(treq)
	if err != nil || !tres.Replayed || tres.TxSeq != 2 || tres.Price != 0 {
		t.Fatalf("legacy transfer request must replay: %+v %v", tres, err)
	}
	// 旧请求改价重提 -> 请求号冲突
	treq.Price = 5
	if _, err := r.Transfer(treq); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("legacy request with new price must conflict, got %v", err)
	}
	// 已有藏品的旧系列规则已固定；未发行的旧系列仍可设置规则
	if _, err := r.SetRoyalty(setRoyaltyReq("n1", RoyaltyShare{AccountID: "carol", Rate: 1})); !errors.Is(err, ErrRoyaltyFrozen) {
		t.Fatalf("legacy series with items must be frozen, got %v", err)
	}
	set2 := setRoyaltyReq("n2", RoyaltyShare{AccountID: "carol", Rate: 100})
	set2.SeriesID = "s2"
	if _, err := r.SetRoyalty(set2); err != nil {
		t.Fatalf("legacy series without items must accept rule: %v", err)
	}
	// 新发行 + 新转让在旧登记册上正常记录版税（s2 规则 carol 1%）
	ireq2 := issueReq("i2", "bob")
	ireq2.SeriesID = "s2"
	if _, err := r.Issue(ireq2); err != nil {
		t.Fatal(err)
	}
	nreq := xferReq("bob", "i2", "carol", 1, "t2")
	nreq.Price = 400
	nres, err := r.Transfer(nreq)
	if err != nil {
		t.Fatal(err)
	}
	if nres.Price != 400 || len(nres.Payables) != 1 || nres.Payables[0].Amount != 4 ||
		nres.Remainder != 396 {
		t.Fatalf("new transfer on legacy registry wrong: %+v", nres)
	}
}
