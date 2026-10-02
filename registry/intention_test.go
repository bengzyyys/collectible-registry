package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// intentionWorld 建立 alice（持有人/创作者）、bob、carol、dave 四个可用
// 账户与系列 s1，并向 alice 发行藏品 i1（持有版本 1）。
func intentionWorld(t *testing.T, r *Registry) {
	t.Helper()
	setupWorld(t, r) // alice, bob, s1
	if err := r.RegisterAccount("carol", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterAccount("dave", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
}

func createIntentionReq(r *Registry, id string, expiry time.Duration, shares []IntentionShare) CreateIntentionRequest {
	return CreateIntentionRequest{
		Operator: "alice", Reason: "拆分意向", RequestID: "create-" + id,
		IntentionID: id, ItemID: "i1", ExpectedOwner: "alice", ExpectedVer: 1,
		ExpiresAt: r.now().Add(expiry), Shares: shares,
	}
}

func defaultShares() []IntentionShare {
	return []IntentionShare{
		{AccountID: "alice", Rate: 5000},
		{AccountID: "bob", Rate: 3000},
		{AccountID: "carol", Rate: 2000},
	}
}

func respondReq(op, id, rid, answer string) RespondIntentionRequest {
	return RespondIntentionRequest{
		Operator: op, Reason: "答复", RequestID: rid,
		IntentionID: id, Answer: answer,
	}
}

// ---- 创建 ----

func TestCreateIntention(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	res, err := r.CreateIntention(req)
	if err != nil {
		t.Fatalf("CreateIntention: %v", err)
	}
	if res.Status != IntentionPending || res.Replayed {
		t.Fatalf("unexpected create result: %+v", res)
	}

	it, err := r.GetIntention("i1-a")
	if err != nil {
		t.Fatal(err)
	}
	if it.OperatorID != "alice" || it.ItemID != "i1" ||
		it.ExpectedOwner != "alice" || it.ExpectedVer != 1 ||
		it.Status != IntentionPending {
		t.Fatalf("intention content wrong: %+v", it)
	}
	// 份额按账户排序
	if len(it.Shares) != 3 || it.Shares[0].AccountID != "alice" ||
		it.Shares[1].AccountID != "bob" || it.Shares[2].AccountID != "carol" ||
		it.Shares[0].Rate != 5000 || it.Shares[1].Rate != 3000 || it.Shares[2].Rate != 2000 {
		t.Fatalf("shares wrong: %+v", it.Shares)
	}
	// 发起人创建时视为同意，其余参与账户尚未答复
	if len(it.Responses) != 3 {
		t.Fatalf("responses wrong: %+v", it.Responses)
	}
	respMap := map[string]string{}
	for _, rp := range it.Responses {
		respMap[rp.AccountID] = rp.Answer
	}
	if respMap["alice"] != IntentionAnswerAgree || respMap["bob"] != "" || respMap["carol"] != "" {
		t.Fatalf("responses wrong: %+v", respMap)
	}
	// 创建事件
	evs, err := r.IntentionHistory("i1")
	if err != nil || len(evs) != 1 {
		t.Fatalf("intention history: %v %v", evs, err)
	}
	if e := evs[0]; e.Kind != "create" || e.Operator != "alice" ||
		e.Reason != "拆分意向" || e.RequestID != "create-i1-a" ||
		e.FromStatus != "" || e.ToStatus != IntentionPending {
		t.Fatalf("create event wrong: %+v", e)
	}
	// 意向不改变持有关系
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("creating intention must not change holding: %+v", h)
	}
	// 藏品历史中不出现意向创建
	hist, _ := r.History("i1")
	if len(hist) != 1 {
		t.Fatalf("create intention must not add item history: %d", len(hist))
	}
}

func TestCreateIntentionValidation(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	base := createIntentionReq(r, "i1-a", time.Hour, defaultShares())

	// 必填缺失
	req := base
	req.Operator = ""
	if _, err := r.CreateIntention(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	// 份额不足两个账户
	req = base
	req.IntentionID = "i1-b"
	req.Shares = []IntentionShare{{AccountID: "alice", Rate: 10000}}
	if _, err := r.CreateIntention(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for one share, got %v", err)
	}
	// 份额超出范围
	req = base
	req.IntentionID = "i1-c"
	req.Shares = []IntentionShare{
		{AccountID: "alice", Rate: 10001},
		{AccountID: "bob", Rate: 1},
	}
	if _, err := r.CreateIntention(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for rate out of range, got %v", err)
	}
	// 份额为 0
	req = base
	req.IntentionID = "i1-d"
	req.Shares = []IntentionShare{
		{AccountID: "alice", Rate: 0},
		{AccountID: "bob", Rate: 10000},
	}
	if _, err := r.CreateIntention(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for zero rate, got %v", err)
	}
	// 份额合计不为 10000
	req = base
	req.IntentionID = "i1-e"
	req.Shares = []IntentionShare{
		{AccountID: "alice", Rate: 5000},
		{AccountID: "bob", Rate: 4000},
	}
	if _, err := r.CreateIntention(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for bad total, got %v", err)
	}
	// 账户重复
	req = base
	req.IntentionID = "i1-f"
	req.Shares = []IntentionShare{
		{AccountID: "alice", Rate: 5000},
		{AccountID: "alice", Rate: 5000},
	}
	if _, err := r.CreateIntention(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for duplicate account, got %v", err)
	}
	// 到期时间未晚于当前
	req = base
	req.IntentionID = "i1-g"
	req.ExpiresAt = r.now()
	if _, err := r.CreateIntention(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for expiry not in future, got %v", err)
	}
	// 份额账户不存在
	req = base
	req.IntentionID = "i1-h"
	req.Shares = []IntentionShare{
		{AccountID: "alice", Rate: 5000},
		{AccountID: "ghost", Rate: 5000},
	}
	if _, err := r.CreateIntention(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// 发起人不是当前持有人
	req = base
	req.IntentionID = "i1-i"
	req.Operator = "bob"
	req.RequestID = "create-i1-i"
	if _, err := r.CreateIntention(req); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict for non-holder, got %v", err)
	}
	// 期望版本不符
	req = base
	req.IntentionID = "i1-j"
	req.ExpectedVer = 2
	if _, err := r.CreateIntention(req); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict for version mismatch, got %v", err)
	}
}

func TestCreateIntentionAccountInactive(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("want ErrAccountInactive, got %v", err)
	}
}

func TestCreateIntentionIDTaken(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 同编号再次创建 -> 编号已用
	req2 := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	req2.RequestID = "create-i1-a-2"
	if _, err := r.CreateIntention(req2); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists, got %v", err)
	}
}

func TestCreateIntentionSealedSeriesAllowed(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatalf("sealed series must not block intention: %v", err)
	}
}

func TestCreateIntentionStatusConflict(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	// 第一份意向有效（待确认）
	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 同藏品再建 -> 状态冲突
	req2 := createIntentionReq(r, "i1-b", time.Hour, defaultShares())
	req2.RequestID = "create-i1-b"
	if _, err := r.CreateIntention(req2); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict for active intention, got %v", err)
	}

	// 拒绝后可另建
	if _, err := r.RespondIntention(respondReq("bob", "i1-a", "resp-1", IntentionAnswerReject)); err != nil {
		t.Fatal(err)
	}
	req3 := createIntentionReq(r, "i1-c", time.Hour, defaultShares())
	req3.RequestID = "create-i1-c"
	if _, err := r.CreateIntention(req3); err != nil {
		t.Fatalf("rejected intention should allow new: %v", err)
	}

	// 撤回后可另建
	if _, err := r.WithdrawIntention(WithdrawIntentionRequest{
		Operator: "alice", Reason: "撤回", RequestID: "wd-1", IntentionID: "i1-c",
	}); err != nil {
		t.Fatal(err)
	}
	req4 := createIntentionReq(r, "i1-d", time.Hour, defaultShares())
	req4.RequestID = "create-i1-d"
	if _, err := r.CreateIntention(req4); err != nil {
		t.Fatalf("withdrawn intention should allow new: %v", err)
	}
}

func TestCreateIntentionAfterInvalidation(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 藏品易手 -> 意向失效
	if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "t1")); err != nil {
		t.Fatal(err)
	}
	it, _ := r.GetIntention("i1-a")
	if it.Status != IntentionInvalid {
		t.Fatalf("want invalid, got %s", it.Status)
	}
	// 当前持有人 bob 可用新编号另建
	newShares := []IntentionShare{
		{AccountID: "bob", Rate: 5000},
		{AccountID: "carol", Rate: 5000},
	}
	newReq := CreateIntentionRequest{
		Operator: "bob", Reason: "新意向", RequestID: "create-i1-b",
		IntentionID: "i1-b", ItemID: "i1", ExpectedOwner: "bob", ExpectedVer: 2,
		ExpiresAt: r.now().Add(time.Hour), Shares: newShares,
	}
	if _, err := r.CreateIntention(newReq); err != nil {
		t.Fatalf("current holder should create new after invalidation: %v", err)
	}
}

// ---- 答复 ----

func TestIntentionRespondAgree(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}

	// bob 同意，carol 尚未答复 -> 仍待确认
	res, err := r.RespondIntention(respondReq("bob", "i1-a", "resp-bob", IntentionAnswerAgree))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != IntentionPending || res.Answer != IntentionAnswerAgree {
		t.Fatalf("unexpected respond result: %+v", res)
	}
	// carol 同意 -> 已达成
	res, err = r.RespondIntention(respondReq("carol", "i1-a", "resp-carol", IntentionAnswerAgree))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != IntentionAgreed {
		t.Fatalf("want agreed, got %s", res.Status)
	}
	it, _ := r.GetIntention("i1-a")
	if it.Status != IntentionAgreed {
		t.Fatalf("want agreed, got %s", it.Status)
	}
	// 历史：create + 两条 respond
	evs, _ := r.IntentionHistory("i1")
	if len(evs) != 3 {
		t.Fatalf("want 3 events, got %d", len(evs))
	}
	if e := evs[1]; e.Kind != "respond" || e.Operator != "bob" ||
		e.Answer != IntentionAnswerAgree || e.FromStatus != IntentionPending ||
		e.ToStatus != IntentionPending {
		t.Fatalf("bob respond event wrong: %+v", e)
	}
	if e := evs[2]; e.Operator != "carol" || e.ToStatus != IntentionAgreed {
		t.Fatalf("carol respond event wrong: %+v", e)
	}
}

func TestIntentionRespondReject(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	res, err := r.RespondIntention(respondReq("bob", "i1-a", "resp-bob", IntentionAnswerReject))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != IntentionRejected || res.Answer != IntentionAnswerReject {
		t.Fatalf("unexpected respond result: %+v", res)
	}
	it, _ := r.GetIntention("i1-a")
	if it.Status != IntentionRejected {
		t.Fatalf("want rejected, got %s", it.Status)
	}
	// 历史中答复事件记录前后状态
	evs, _ := r.IntentionHistory("i1")
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	if e := evs[1]; e.FromStatus != IntentionPending || e.ToStatus != IntentionRejected {
		t.Fatalf("reject event wrong: %+v", e)
	}
}

func TestIntentionRespondIdempotent(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// bob 首次同意
	if _, err := r.RespondIntention(respondReq("bob", "i1-a", "resp-bob", IntentionAnswerAgree)); err != nil {
		t.Fatal(err)
	}
	// bob 重复相同答复（同请求号）-> 成功，不新增记录
	res, err := r.RespondIntention(respondReq("bob", "i1-a", "resp-bob", IntentionAnswerAgree))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replayed {
		t.Fatalf("want replayed, got %+v", res)
	}
	// bob 用不同请求号重复相同答复 -> 成功，不新增记录
	res, err = r.RespondIntention(respondReq("bob", "i1-a", "resp-bob-2", IntentionAnswerAgree))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replayed {
		t.Fatalf("want replayed for duplicate with new request id, got %+v", res)
	}
	evs, _ := r.IntentionHistory("i1")
	if len(evs) != 2 {
		t.Fatalf("duplicate respond must not add history: %d", len(evs))
	}
}

func TestIntentionChangeAnswerToReject(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// bob 同意、carol 同意 -> 已达成
	if _, err := r.RespondIntention(respondReq("bob", "i1-a", "resp-bob", IntentionAnswerAgree)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RespondIntention(respondReq("carol", "i1-a", "resp-carol", IntentionAnswerAgree)); err != nil {
		t.Fatal(err)
	}
	it, _ := r.GetIntention("i1-a")
	if it.Status != IntentionAgreed {
		t.Fatalf("want agreed, got %s", it.Status)
	}
	// bob 改答拒绝 -> 已拒绝
	res, err := r.RespondIntention(respondReq("bob", "i1-a", "resp-bob-2", IntentionAnswerReject))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != IntentionRejected {
		t.Fatalf("change to reject must reject intention, got %s", res.Status)
	}
	it, _ = r.GetIntention("i1-a")
	if it.Status != IntentionRejected {
		t.Fatalf("want rejected, got %s", it.Status)
	}
	// 历史包含改答事件
	evs, _ := r.IntentionHistory("i1")
	if len(evs) != 4 {
		t.Fatalf("want 4 events (create + 3 responds), got %d", len(evs))
	}
	if e := evs[3]; e.Answer != IntentionAnswerReject ||
		e.FromStatus != IntentionAgreed || e.ToStatus != IntentionRejected {
		t.Fatalf("change-answer event wrong: %+v", e)
	}
}

func TestIntentionRespondForbidden(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 名单外账户答复
	if _, err := r.RespondIntention(respondReq("dave", "i1-a", "resp-dave", IntentionAnswerAgree)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden for non-participant, got %v", err)
	}
	// 发起人拒绝自己的方案
	if _, err := r.RespondIntention(respondReq("alice", "i1-a", "resp-alice", IntentionAnswerReject)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden for founder reject, got %v", err)
	}
}

func TestIntentionRespondAfterTerminal(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 已拒绝后不再接受答复
	if _, err := r.RespondIntention(respondReq("bob", "i1-a", "resp-bob", IntentionAnswerReject)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RespondIntention(respondReq("carol", "i1-a", "resp-carol", IntentionAnswerAgree)); !errors.Is(err, ErrIntentionRejected) {
		t.Fatalf("want ErrIntentionRejected, got %v", err)
	}

	// 已撤回后不再接受答复
	req2 := createIntentionReq(r, "i1-b", time.Hour, defaultShares())
	req2.RequestID = "create-i1-b"
	if _, err := r.CreateIntention(req2); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WithdrawIntention(WithdrawIntentionRequest{
		Operator: "alice", Reason: "撤回", RequestID: "wd-1", IntentionID: "i1-b",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RespondIntention(respondReq("bob", "i1-b", "resp-bob-2", IntentionAnswerAgree)); !errors.Is(err, ErrIntentionWithdrawn) {
		t.Fatalf("want ErrIntentionWithdrawn, got %v", err)
	}
}

// ---- 撤回 ----

func TestIntentionWithdraw(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 发起人撤回
	res, err := r.WithdrawIntention(WithdrawIntentionRequest{
		Operator: "alice", Reason: "撤回", RequestID: "wd-1", IntentionID: "i1-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != IntentionWithdrawn || res.Replayed {
		t.Fatalf("unexpected withdraw result: %+v", res)
	}
	it, _ := r.GetIntention("i1-a")
	if it.Status != IntentionWithdrawn || it.WithdrawnAt.IsZero() {
		t.Fatalf("want withdrawn with time, got %+v", it)
	}
	// 再次撤回幂等，不新增记录
	res, err = r.WithdrawIntention(WithdrawIntentionRequest{
		Operator: "alice", Reason: "撤回", RequestID: "wd-1", IntentionID: "i1-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replayed {
		t.Fatalf("want replayed, got %+v", res)
	}
	res, err = r.WithdrawIntention(WithdrawIntentionRequest{
		Operator: "alice", Reason: "再次撤回", RequestID: "wd-2", IntentionID: "i1-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replayed {
		t.Fatalf("want replayed for duplicate withdraw, got %+v", res)
	}
	evs, _ := r.IntentionHistory("i1")
	if len(evs) != 2 {
		t.Fatalf("want 2 events (create + withdraw), got %d", len(evs))
	}
	if e := evs[1]; e.Kind != "withdraw" || e.FromStatus != IntentionPending ||
		e.ToStatus != IntentionWithdrawn {
		t.Fatalf("withdraw event wrong: %+v", e)
	}
}

func TestIntentionWithdrawForbidden(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 非发起人撤回
	if _, err := r.WithdrawIntention(WithdrawIntentionRequest{
		Operator: "bob", Reason: "撤回", RequestID: "wd-bob", IntentionID: "i1-a",
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

func TestIntentionWithdrawRejected(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 拒绝后不能撤回
	if _, err := r.RespondIntention(respondReq("bob", "i1-a", "resp-bob", IntentionAnswerReject)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WithdrawIntention(WithdrawIntentionRequest{
		Operator: "alice", Reason: "撤回", RequestID: "wd-1", IntentionID: "i1-a",
	}); !errors.Is(err, ErrIntentionRejected) {
		t.Fatalf("want ErrIntentionRejected, got %v", err)
	}
}

func TestIntentionWithdrawAgreed(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 全部同意 -> 已达成，仍可撤回
	if _, err := r.RespondIntention(respondReq("bob", "i1-a", "resp-bob", IntentionAnswerAgree)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RespondIntention(respondReq("carol", "i1-a", "resp-carol", IntentionAnswerAgree)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WithdrawIntention(WithdrawIntentionRequest{
		Operator: "alice", Reason: "撤回", RequestID: "wd-1", IntentionID: "i1-a",
	}); err != nil {
		t.Fatalf("agreed intention should be withdrawable: %v", err)
	}
}

// ---- 失效 ----

func TestIntentionInvalidOnVersionChange(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 藏品易手 -> 失效
	if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "t1")); err != nil {
		t.Fatal(err)
	}
	it, _ := r.GetIntention("i1-a")
	if it.Status != IntentionInvalid {
		t.Fatalf("want invalid after version change, got %s", it.Status)
	}
	// 答复被拒绝并说明原因
	if _, err := r.RespondIntention(respondReq("carol", "i1-a", "resp-carol", IntentionAnswerAgree)); !errors.Is(err, ErrIntentionInvalid) {
		t.Fatalf("want ErrIntentionInvalid, got %v", err)
	}
	// 撤回被拒绝并说明原因
	if _, err := r.WithdrawIntention(WithdrawIntentionRequest{
		Operator: "alice", Reason: "撤回", RequestID: "wd-1", IntentionID: "i1-a",
	}); !errors.Is(err, ErrIntentionInvalid) {
		t.Fatalf("want ErrIntentionInvalid, got %v", err)
	}
	// 藏品转回原持有人也不能恢复
	if _, err := r.Transfer(xferReq("bob", "i1", "alice", 2, "t2")); err != nil {
		t.Fatal(err)
	}
	it, _ = r.GetIntention("i1-a")
	if it.Status != IntentionInvalid {
		t.Fatalf("want still invalid after transfer back, got %s", it.Status)
	}
}

func TestIntentionInvalidOnAccountDeactivate(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 参与账户停用 -> 失效
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	it, _ := r.GetIntention("i1-a")
	if it.Status != IntentionInvalid {
		t.Fatalf("want invalid after participant deactivate, got %s", it.Status)
	}

	// 发起人停用 -> 失效（bob 已停用，第二份意向不再列入 bob）
	req2 := createIntentionReq(r, "i1-b", time.Hour, []IntentionShare{
		{AccountID: "alice", Rate: 5000},
		{AccountID: "carol", Rate: 5000},
	})
	req2.RequestID = "create-i1-b"
	if _, err := r.CreateIntention(req2); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	it, _ = r.GetIntention("i1-b")
	if it.Status != IntentionInvalid {
		t.Fatalf("want invalid after founder deactivate, got %s", it.Status)
	}
}

// ---- 过期 ----

func TestIntentionExpired(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	base := time.Unix(2_000_000_000, 0)
	r.now = func() time.Time { return base }
	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 到期时间点起 -> 过期
	r.now = func() time.Time { return base.Add(time.Hour) }
	it, _ := r.GetIntention("i1-a")
	if it.Status != IntentionExpired {
		t.Fatalf("want expired at expiry point, got %s", it.Status)
	}
	// 答复被拒绝并说明原因
	if _, err := r.RespondIntention(respondReq("bob", "i1-a", "resp-bob", IntentionAnswerAgree)); !errors.Is(err, ErrIntentionExpired) {
		t.Fatalf("want ErrIntentionExpired, got %v", err)
	}
	// 撤回被拒绝并说明原因
	if _, err := r.WithdrawIntention(WithdrawIntentionRequest{
		Operator: "alice", Reason: "撤回", RequestID: "wd-1", IntentionID: "i1-a",
	}); !errors.Is(err, ErrIntentionExpired) {
		t.Fatalf("want ErrIntentionExpired, got %v", err)
	}
}

func TestIntentionInvalidPriorityOverExpired(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	base := time.Unix(2_000_000_000, 0)
	r.now = func() time.Time { return base }
	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 版本变化 + 已过期 -> 失效优先
	r.now = func() time.Time { return base.Add(2 * time.Hour) }
	if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "t1")); err != nil {
		t.Fatal(err)
	}
	it, _ := r.GetIntention("i1-a")
	if it.Status != IntentionInvalid {
		t.Fatalf("want invalid (priority over expired), got %s", it.Status)
	}
}

// ---- 查询 ----

func TestGetIntentionNotFound(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	if _, err := r.GetIntention("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestIntentionHistoryNotFound(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	if _, err := r.IntentionHistory("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestIntentionHistoryOrder(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RespondIntention(respondReq("bob", "i1-a", "resp-bob", IntentionAnswerAgree)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RespondIntention(respondReq("carol", "i1-a", "resp-carol", IntentionAnswerReject)); err != nil {
		t.Fatal(err)
	}
	evs, err := r.IntentionHistory("i1")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("want 3 events, got %d", len(evs))
	}
	// 序号严格递增
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d seq wrong: %d", i, e.Seq)
		}
	}
	if evs[0].Kind != "create" || evs[1].Kind != "respond" || evs[2].Kind != "respond" {
		t.Fatalf("event kinds wrong: %s %s %s", evs[0].Kind, evs[1].Kind, evs[2].Kind)
	}
	if evs[1].Operator != "bob" || evs[2].Operator != "carol" {
		t.Fatalf("event operators wrong: %s %s", evs[1].Operator, evs[2].Operator)
	}
}

// ---- 幂等 ----

func TestIntentionCreateIdempotent(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	res1, err := r.CreateIntention(req)
	if err != nil {
		t.Fatal(err)
	}
	// 相同内容重提 -> 回放首次成功
	res2, err := r.CreateIntention(req)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Replayed {
		t.Fatalf("want replayed, got %+v", res2)
	}
	if res2.IntentionID != res1.IntentionID {
		t.Fatalf("replay result mismatch: %+v vs %+v", res1, res2)
	}
	// 仅调整份额名单排列 -> 相同内容，回放成功
	reqReorder := req
	reqReorder.Shares = []IntentionShare{
		{AccountID: "carol", Rate: 2000},
		{AccountID: "alice", Rate: 5000},
		{AccountID: "bob", Rate: 3000},
	}
	res3, err := r.CreateIntention(reqReorder)
	if err != nil {
		t.Fatal(err)
	}
	if !res3.Replayed {
		t.Fatalf("want replayed for reordered shares, got %+v", res3)
	}
	// 改内容 -> 请求号冲突
	reqChange := req
	reqChange.Shares = []IntentionShare{
		{AccountID: "alice", Rate: 4000},
		{AccountID: "bob", Rate: 4000},
		{AccountID: "carol", Rate: 2000},
	}
	if _, err := r.CreateIntention(reqChange); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("want ErrRequestConflict, got %v", err)
	}
}

func TestIntentionValidationDoesNotConsumeRequestID(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	// 参数错误不占用请求号：用同一请求号先提交非法参数，再提交合法参数
	bad := createIntentionReq(r, "i1-a", time.Hour, []IntentionShare{
		{AccountID: "alice", Rate: 10000},
	})
	if _, err := r.CreateIntention(bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	good := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(good); err != nil {
		t.Fatalf("validation error must not consume request id: %v", err)
	}
}

func TestIntentionConcurrentRespond(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	// 多个 goroutine 用同一请求号并发答复，只有一次生效
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.RespondIntention(respondReq("bob", "i1-a", "resp-bob", IntentionAnswerAgree))
		}()
	}
	wg.Wait()
	evs, _ := r.IntentionHistory("i1")
	// create + 一条 respond（其余回放不新增）
	if len(evs) != 2 {
		t.Fatalf("concurrent respond must add only one event: %d", len(evs))
	}
}

// ---- 持久化 ----

func TestIntentionPersistence(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	intentionWorld(t, r)

	req := createIntentionReq(r, "i1-a", time.Hour, defaultShares())
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RespondIntention(respondReq("bob", "i1-a", "resp-bob", IntentionAnswerAgree)); err != nil {
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

	// 方案、答复与状态完整保留
	it, err := r2.GetIntention("i1-a")
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != IntentionPending {
		t.Fatalf("status after reopen: %s", it.Status)
	}
	respMap := map[string]string{}
	for _, rp := range it.Responses {
		respMap[rp.AccountID] = rp.Answer
	}
	if respMap["bob"] != IntentionAnswerAgree || respMap["carol"] != "" {
		t.Fatalf("responses after reopen: %+v", respMap)
	}
	// 历史完整保留
	evs, err := r2.IntentionHistory("i1")
	if err != nil || len(evs) != 2 {
		t.Fatalf("history after reopen: %v %v", evs, err)
	}
	// 原请求重放
	res, err := r2.RespondIntention(respondReq("bob", "i1-a", "resp-bob", IntentionAnswerAgree))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replayed {
		t.Fatalf("want replay after reopen, got %+v", res)
	}
	// 撤回后重开仍保留
	if _, err := r2.WithdrawIntention(WithdrawIntentionRequest{
		Operator: "alice", Reason: "撤回", RequestID: "wd-1", IntentionID: "i1-a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	r3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Close()
	it, _ = r3.GetIntention("i1-a")
	if it.Status != IntentionWithdrawn {
		t.Fatalf("status after second reopen: %s", it.Status)
	}
}

func TestIntentionOldRegistry(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	intentionWorld(t, r)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// 模拟旧版本登记册：移除意向相关字段
	data, err := os.ReadFile(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "intentions")
	delete(raw, "intention_events")
	delete(raw, "next_intention_seq")
	newData, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry.json"), newData, 0o644); err != nil {
		t.Fatal(err)
	}

	// 旧登记册可直接打开
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("old registry must open: %v", err)
	}
	defer r2.Close()

	// 原有数据不变
	h, _ := r2.GetHolding("i1")
	if h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("holding after old open: %+v", h)
	}
	hist, _ := r2.History("i1")
	if len(hist) != 1 {
		t.Fatalf("history after old open: %d", len(hist))
	}
	// 旧请求仍可回放（发行）
	res, err := r2.Issue(issueReq("i1", "alice"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replayed {
		t.Fatalf("want replay of old issue request, got %+v", res)
	}
	// 旧登记册上可直接创建意向
	req := createIntentionReq(r2, "i1-a", time.Hour, defaultShares())
	if _, err := r2.CreateIntention(req); err != nil {
		t.Fatalf("create intention on old registry: %v", err)
	}
	it, _ := r2.GetIntention("i1-a")
	if it.Status != IntentionPending {
		t.Fatalf("intention on old registry: %+v", it)
	}
}

// ---- 发起人不列入份额名单 ----

func TestIntentionFounderNotInShares(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentionWorld(t, r)

	// 发起人不列入名单：份额由 bob、carol、dave 分
	shares := []IntentionShare{
		{AccountID: "bob", Rate: 4000},
		{AccountID: "carol", Rate: 3000},
		{AccountID: "dave", Rate: 3000},
	}
	req := createIntentionReq(r, "i1-a", time.Hour, shares)
	if _, err := r.CreateIntention(req); err != nil {
		t.Fatal(err)
	}
	it, _ := r.GetIntention("i1-a")
	// 没有发起人答复行
	if len(it.Responses) != 3 {
		t.Fatalf("responses: %+v", it.Responses)
	}
	// 全部同意后才达成
	for _, op := range []string{"bob", "carol", "dave"} {
		if _, err := r.RespondIntention(respondReq(op, "i1-a", "resp-"+op, IntentionAnswerAgree)); err != nil {
			t.Fatal(err)
		}
	}
	it, _ = r.GetIntention("i1-a")
	if it.Status != IntentionAgreed {
		t.Fatalf("want agreed, got %s", it.Status)
	}
}
