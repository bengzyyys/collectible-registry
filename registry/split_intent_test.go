package registry

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// intentWorld 建立 alice（持有人/创作者）、bob、carol、dave 四个可用账户，
// 并向 alice 发行藏品 i1（持有版本 1）。
func intentWorld(t *testing.T, r *Registry) {
	t.Helper()
	setupWorld(t, r)
	for _, id := range []string{"carol", "dave"} {
		if err := r.RegisterAccount(id, "参与人"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
}

func intentShares() []SplitShare {
	return []SplitShare{
		{AccountID: "alice", Share: 5000},
		{AccountID: "bob", Share: 3000},
		{AccountID: "carol", Share: 2000},
	}
}

func createIntentReq(r *Registry, id string) CreateSplitIntentRequest {
	return CreateSplitIntentRequest{
		Operator: "alice", Reason: "协商拆分", RequestID: "create-" + id,
		IntentID: id, ItemID: "i1", ExpectedOwner: "alice", ExpectedVer: 1,
		ExpiresAt: r.now().Add(time.Hour), Shares: intentShares(),
	}
}

func answerReq(op, intentID, rid string, agree bool) AnswerSplitIntentRequest {
	return AnswerSplitIntentRequest{
		Operator: op, Reason: "答复方案", RequestID: rid,
		IntentID: intentID, Agree: agree,
	}
}

func withdrawReq(op, intentID, rid string) WithdrawSplitIntentRequest {
	return WithdrawSplitIntentRequest{
		Operator: op, Reason: "撤回意向", RequestID: rid, IntentID: intentID,
	}
}

// ---- 创建 ----

func TestCreateSplitIntent(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)

	res, err := r.CreateSplitIntent(createIntentReq(r, "sp1"))
	if err != nil {
		t.Fatalf("CreateSplitIntent: %v", err)
	}
	if res.Status != SplitPending || res.GrantVer != 1 || res.Replayed {
		t.Fatalf("unexpected create result: %+v", res)
	}
	in, err := r.GetSplitIntent("sp1")
	if err != nil {
		t.Fatal(err)
	}
	if in.ItemID != "i1" || in.InitiatorID != "alice" || in.GrantVer != 1 ||
		in.Status != SplitPending || len(in.Shares) != 3 {
		t.Fatalf("intent content wrong: %+v", in)
	}
	// 份额按账户排序保存；发起人列入方案，创建时视为已同意
	want := []SplitParty{
		{AccountID: "alice", Share: 5000, Answer: SplitAnswerAgree},
		{AccountID: "bob", Share: 3000},
		{AccountID: "carol", Share: 2000},
	}
	for i, p := range in.Shares {
		if p != want[i] {
			t.Fatalf("party %d wrong: %+v want %+v", i, p, want[i])
		}
	}
	// 意向不改变持有关系与藏品历史
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("creating intent must not change holding: %+v", h)
	}
	hist, _ := r.History("i1")
	if len(hist) != 1 {
		t.Fatalf("create intent must not add item history: %d", len(hist))
	}
	// 创建事件
	evs, err := r.SplitIntentHistory("i1")
	if err != nil || len(evs) != 1 {
		t.Fatalf("intent history: %v %v", evs, err)
	}
	if e := evs[0]; e.Kind != "create" || e.IntentID != "sp1" || e.Operator != "alice" ||
		e.Reason != "协商拆分" || e.RequestID != "create-sp1" ||
		e.FromStatus != "" || e.ToStatus != SplitPending || e.OccurredAt.IsZero() {
		t.Fatalf("create event wrong: %+v", e)
	}
}

func TestCreateSplitIntentValidation(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)

	// 必填缺失
	if _, err := r.CreateSplitIntent(CreateSplitIntentRequest{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty request: %v", err)
	}
	base := createIntentReq(r, "sp1")
	// 账户少于两个
	bad := base
	bad.Shares = []SplitShare{{AccountID: "alice", Share: 10000}}
	if _, err := r.CreateSplitIntent(bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("single party: %v", err)
	}
	// 账户重复
	bad = base
	bad.Shares = []SplitShare{{AccountID: "bob", Share: 5000}, {AccountID: "bob", Share: 5000}}
	if _, err := r.CreateSplitIntent(bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("dup account: %v", err)
	}
	// 份额越界
	for _, share := range []int64{0, -1, 10001} {
		bad = base
		bad.Shares = []SplitShare{{AccountID: "bob", Share: share}, {AccountID: "carol", Share: 10000 - share}}
		if _, err := r.CreateSplitIntent(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("share %d: %v", share, err)
		}
	}
	// 合计不为 10000
	bad = base
	bad.Shares = []SplitShare{{AccountID: "bob", Share: 5000}, {AccountID: "carol", Share: 4999}}
	if _, err := r.CreateSplitIntent(bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("total 9999: %v", err)
	}
	// 到期时间不晚于当前时间
	bad = base
	bad.ExpiresAt = r.now().Add(-time.Minute)
	if _, err := r.CreateSplitIntent(bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("past expiry: %v", err)
	}
	// 参数错误不占用请求号：同一请求号修正后可正常执行
	good := base
	good.RequestID = "create-sp1"
	if _, err := r.CreateSplitIntent(good); err != nil {
		t.Fatalf("request id must not be consumed by invalid params: %v", err)
	}
}

func TestCreateSplitIntentNotFound(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)

	// 藏品不存在
	req := createIntentReq(r, "sp1")
	req.ItemID = "nope"
	if _, err := r.CreateSplitIntent(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown item: %v", err)
	}
	// 发起人未登记
	req = createIntentReq(r, "sp1")
	req.Operator = "ghost"
	if _, err := r.CreateSplitIntent(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown operator: %v", err)
	}
	// 参与账户未登记
	req = createIntentReq(r, "sp1")
	req.Shares = []SplitShare{{AccountID: "bob", Share: 5000}, {AccountID: "ghost", Share: 5000}}
	if _, err := r.CreateSplitIntent(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown party: %v", err)
	}
	// 引用不存在不占用请求号
	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp1")); err != nil {
		t.Fatalf("request id must not be consumed by not-found: %v", err)
	}
}

func TestCreateSplitIntentStateRejections(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)

	// 参与账户停用
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	req := createIntentReq(r, "sp1")
	if _, err := r.CreateSplitIntent(req); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive party: %v", err)
	}
	// 状态类拒绝占用请求号：相同参数重提回放首次拒绝
	res, err := r.CreateSplitIntent(req)
	if !errors.Is(err, ErrAccountInactive) || !res.Replayed {
		t.Fatalf("replayed rejection: %+v %v", res, err)
	}
	// 账户恢复可用不可能（停用是终态），换请求号与方案继续
	if err := r.RegisterAccount("erin", "替补"); err != nil {
		t.Fatal(err)
	}
	req = createIntentReq(r, "sp1")
	req.RequestID = "create-sp1b"
	req.Shares = []SplitShare{{AccountID: "bob", Share: 5000}, {AccountID: "erin", Share: 5000}}
	if _, err := r.CreateSplitIntent(req); err != nil {
		t.Fatalf("create with active parties: %v", err)
	}
	// 意向编号已用
	activeShares := []SplitShare{{AccountID: "bob", Share: 5000}, {AccountID: "erin", Share: 5000}}
	req = createIntentReq(r, "sp1")
	req.RequestID = "create-dup"
	req.Shares = activeShares
	if _, err := r.CreateSplitIntent(req); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate intent id: %v", err)
	}
	// 持有版本不符（状态类拒绝占用请求号，后续各例换新请求号）
	req = createIntentReq(r, "sp2")
	req.Shares = activeShares
	req.ExpectedVer = 7
	if _, err := r.CreateSplitIntent(req); !errors.Is(err, ErrConflict) {
		t.Fatalf("version mismatch: %v", err)
	}
	// 期望持有人不符
	req = createIntentReq(r, "sp2")
	req.RequestID = "create-sp2b"
	req.Shares = activeShares
	req.ExpectedOwner = "bob"
	if _, err := r.CreateSplitIntent(req); !errors.Is(err, ErrConflict) {
		t.Fatalf("owner mismatch: %v", err)
	}
	// 非持有人发起
	req = createIntentReq(r, "sp2")
	req.RequestID = "create-sp2c"
	req.Operator = "bob"
	req.Shares = activeShares
	if _, err := r.CreateSplitIntent(req); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-holder operator: %v", err)
	}
}

func TestCreateSplitIntentSealedSeries(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if err := r.SealSeries("s1", "alice"); err != nil {
		t.Fatal(err)
	}
	// 系列封存不妨碍登记意向
	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp1")); err != nil {
		t.Fatalf("sealed series must not block intent: %v", err)
	}
}

func TestCreateSplitIntentConflictLive(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)

	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp1")); err != nil {
		t.Fatal(err)
	}
	// 已有仍有效的待确认意向：新建按状态冲突拒绝
	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp2")); !errors.Is(err, ErrConflict) {
		t.Fatalf("live intent must conflict: %v", err)
	}
	// 撤回后可用新编号另建（状态类拒绝已占用原请求号，换新请求号）
	if _, err := r.WithdrawSplitIntent(withdrawReq("alice", "sp1", "wd-1")); err != nil {
		t.Fatal(err)
	}
	req := createIntentReq(r, "sp2")
	req.RequestID = "create-sp2b"
	if _, err := r.CreateSplitIntent(req); err != nil {
		t.Fatalf("new intent after withdraw: %v", err)
	}
}

// ---- 答复 ----

func TestAnswerSplitIntent(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp1")); err != nil {
		t.Fatal(err)
	}

	// bob 同意：仍为待确认（carol 未答复）
	res, err := r.AnswerSplitIntent(answerReq("bob", "sp1", "ans-bob", true))
	if err != nil || res.Status != SplitPending {
		t.Fatalf("bob agree: %+v %v", res, err)
	}
	// 重复相同答复（新请求号）：成功但不新增记录
	res, err = r.AnswerSplitIntent(answerReq("bob", "sp1", "ans-bob-2", true))
	if err != nil || res.Status != SplitPending || res.Replayed {
		t.Fatalf("repeat same answer: %+v %v", res, err)
	}
	evs, _ := r.SplitIntentHistory("i1")
	if len(evs) != 2 { // create + 首次答复
		t.Fatalf("repeat answer must not add event: %d", len(evs))
	}
	// 改答拒绝
	if _, err := r.AnswerSplitIntent(answerReq("bob", "sp1", "ans-bob-3", false)); !errors.Is(err, ErrSplitAnswered) {
		t.Fatalf("change answer: %v", err)
	}
	// 名单外账户无权答复
	if _, err := r.AnswerSplitIntent(answerReq("dave", "sp1", "ans-dave", true)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("outsider answer: %v", err)
	}
	// 发起人创建时已视为同意，改答拒绝
	if _, err := r.AnswerSplitIntent(answerReq("alice", "sp1", "ans-alice", false)); !errors.Is(err, ErrSplitAnswered) {
		t.Fatalf("initiator change answer: %v", err)
	}
	// carol 同意：全部同意，已达成
	res, err = r.AnswerSplitIntent(answerReq("carol", "sp1", "ans-carol", true))
	if err != nil || res.Status != SplitAgreed {
		t.Fatalf("carol agree: %+v %v", res, err)
	}
	in, _ := r.GetSplitIntent("sp1")
	if in.Status != SplitAgreed {
		t.Fatalf("status must be agreed: %+v", in)
	}
	// 答复事件：首次答复记录操作者、请求号与前后状态
	evs, _ = r.SplitIntentHistory("i1")
	if len(evs) != 3 {
		t.Fatalf("want 3 events, got %d", len(evs))
	}
	if e := evs[1]; e.Kind != "answer" || e.Operator != "bob" || e.Answer != SplitAnswerAgree ||
		e.RequestID != "ans-bob" || e.FromStatus != SplitPending || e.ToStatus != SplitPending {
		t.Fatalf("bob answer event wrong: %+v", e)
	}
	if e := evs[2]; e.Kind != "answer" || e.Operator != "carol" ||
		e.FromStatus != SplitPending || e.ToStatus != SplitAgreed {
		t.Fatalf("carol answer event wrong: %+v", e)
	}
	// 已达成且有效时重复相同答复仍成功
	res, err = r.AnswerSplitIntent(answerReq("carol", "sp1", "ans-carol-2", true))
	if err != nil || res.Status != SplitAgreed {
		t.Fatalf("repeat after agreed: %+v %v", res, err)
	}
	// 已达成仍阻止另建意向
	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp2")); !errors.Is(err, ErrConflict) {
		t.Fatalf("agreed intent must block new intent: %v", err)
	}
}

func TestAnswerSplitIntentReject(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp1")); err != nil {
		t.Fatal(err)
	}

	res, err := r.AnswerSplitIntent(answerReq("bob", "sp1", "ans-bob", false))
	if err != nil || res.Status != SplitRejected {
		t.Fatalf("bob reject: %+v %v", res, err)
	}
	in, _ := r.GetSplitIntent("sp1")
	if in.Status != SplitRejected || in.EndedAt.IsZero() {
		t.Fatalf("intent must be rejected: %+v", in)
	}
	// 已拒绝不再接受答复
	if _, err := r.AnswerSplitIntent(answerReq("carol", "sp1", "ans-carol", true)); !errors.Is(err, ErrSplitIntentRejected) {
		t.Fatalf("answer after reject: %v", err)
	}
	// 已拒绝不能撤回
	if _, err := r.WithdrawSplitIntent(withdrawReq("alice", "sp1", "wd-1")); !errors.Is(err, ErrSplitIntentRejected) {
		t.Fatalf("withdraw rejected: %v", err)
	}
	// 已结束的意向仍可查询，且可用新编号另建方案
	evs, _ := r.SplitIntentHistory("i1")
	if len(evs) != 2 || evs[1].ToStatus != SplitRejected {
		t.Fatalf("reject event wrong: %+v", evs)
	}
	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp2")); err != nil {
		t.Fatalf("new intent after reject: %v", err)
	}
}

// ---- 撤回 ----

func TestWithdrawSplitIntent(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp1")); err != nil {
		t.Fatal(err)
	}

	// 仅发起人可撤回
	if _, err := r.WithdrawSplitIntent(withdrawReq("bob", "sp1", "wd-bob")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-initiator withdraw: %v", err)
	}
	res, err := r.WithdrawSplitIntent(withdrawReq("alice", "sp1", "wd-1"))
	if err != nil || res.Status != SplitWithdrawn {
		t.Fatalf("withdraw: %+v %v", res, err)
	}
	in, _ := r.GetSplitIntent("sp1")
	if in.Status != SplitWithdrawn || in.EndedAt.IsZero() {
		t.Fatalf("intent must be withdrawn: %+v", in)
	}
	evs, _ := r.SplitIntentHistory("i1")
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	if e := evs[1]; e.Kind != "withdraw" || e.Operator != "alice" ||
		e.RequestID != "wd-1" || e.FromStatus != SplitPending || e.ToStatus != SplitWithdrawn {
		t.Fatalf("withdraw event wrong: %+v", e)
	}
	// 已撤回再次撤回成功但不增加记录
	res, err = r.WithdrawSplitIntent(withdrawReq("alice", "sp1", "wd-2"))
	if err != nil || res.Status != SplitWithdrawn {
		t.Fatalf("re-withdraw: %+v %v", res, err)
	}
	evs, _ = r.SplitIntentHistory("i1")
	if len(evs) != 2 {
		t.Fatalf("re-withdraw must not add event: %d", len(evs))
	}
	// 已撤回不再接受答复
	if _, err := r.AnswerSplitIntent(answerReq("bob", "sp1", "ans-bob", true)); !errors.Is(err, ErrSplitIntentWithdrawn) {
		t.Fatalf("answer after withdraw: %v", err)
	}
}

// ---- 失效与过期 ----

func TestSplitIntentInvalidOnTransfer(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp1")); err != nil {
		t.Fatal(err)
	}

	// 持有版本变化后意向失效
	if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "t-1")); err != nil {
		t.Fatal(err)
	}
	in, _ := r.GetSplitIntent("sp1")
	if in.Status != SplitInvalid {
		t.Fatalf("intent must be invalid after transfer: %+v", in)
	}
	if _, err := r.AnswerSplitIntent(answerReq("bob", "sp1", "ans-bob", true)); !errors.Is(err, ErrSplitIntentInvalid) {
		t.Fatalf("answer on invalid: %v", err)
	}
	if _, err := r.WithdrawSplitIntent(withdrawReq("alice", "sp1", "wd-1")); !errors.Is(err, ErrSplitIntentInvalid) {
		t.Fatalf("withdraw on invalid: %v", err)
	}
	// 藏品转回原持有人也不能恢复
	if _, err := r.Transfer(xferReq("bob", "i1", "alice", 2, "t-2")); err != nil {
		t.Fatal(err)
	}
	in, _ = r.GetSplitIntent("sp1")
	if in.Status != SplitInvalid {
		t.Fatalf("transfer back must not restore intent: %+v", in)
	}
	// 失效后可用新编号另建方案
	req := createIntentReq(r, "sp2")
	req.ExpectedVer = 3
	if _, err := r.CreateSplitIntent(req); err != nil {
		t.Fatalf("new intent after invalid: %v", err)
	}
}

func TestSplitIntentInvalidOnDeactivate(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp1")); err != nil {
		t.Fatal(err)
	}
	// 参与账户停用后意向失效
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	in, _ := r.GetSplitIntent("sp1")
	if in.Status != SplitInvalid {
		t.Fatalf("intent must be invalid after party deactivated: %+v", in)
	}
	// 发起人停用同样失效
	req := createIntentReq(r, "sp2")
	req.Shares = []SplitShare{
		{AccountID: "alice", Share: 5000},
		{AccountID: "bob", Share: 3000},
		{AccountID: "dave", Share: 2000},
	}
	if _, err := r.CreateSplitIntent(req); err != nil {
		t.Fatalf("new intent after invalid: %v", err)
	}
	if err := r.DeactivateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	in, _ = r.GetSplitIntent("sp2")
	if in.Status != SplitInvalid {
		t.Fatalf("intent must be invalid after initiator deactivated: %+v", in)
	}
}

func TestSplitIntentExpiry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	base := time.Unix(2_000_000_000, 0)
	r.now = func() time.Time { return base }
	defer func() { r.now = time.Now }()

	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp1")); err != nil {
		t.Fatal(err)
	}
	// 到期前一刻仍有效
	r.now = func() time.Time { return base.Add(time.Hour - time.Nanosecond) }
	in, _ := r.GetSplitIntent("sp1")
	if in.Status != SplitPending {
		t.Fatalf("before expiry must be pending: %+v", in)
	}
	// 从到期时间点起过期
	r.now = func() time.Time { return base.Add(time.Hour) }
	in, _ = r.GetSplitIntent("sp1")
	if in.Status != SplitExpired {
		t.Fatalf("at expiry must be expired: %+v", in)
	}
	if _, err := r.AnswerSplitIntent(answerReq("bob", "sp1", "ans-bob", true)); !errors.Is(err, ErrSplitIntentExpired) {
		t.Fatalf("answer on expired: %v", err)
	}
	if _, err := r.WithdrawSplitIntent(withdrawReq("alice", "sp1", "wd-1")); !errors.Is(err, ErrSplitIntentExpired) {
		t.Fatalf("withdraw on expired: %v", err)
	}
	// 过期后可用新编号另建方案
	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp2")); err != nil {
		t.Fatalf("new intent after expiry: %v", err)
	}
}

// ---- 幂等 ----

func TestSplitIntentIdempotency(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)

	req := createIntentReq(r, "sp1")
	res, err := r.CreateSplitIntent(req)
	if err != nil || res.Replayed {
		t.Fatalf("first create: %+v %v", res, err)
	}
	// 相同内容重提回放首次成功；仅调整份额名单排列视为相同内容
	dup := req
	dup.Shares = []SplitShare{
		{AccountID: "carol", Share: 2000},
		{AccountID: "alice", Share: 5000},
		{AccountID: "bob", Share: 3000},
	}
	res, err = r.CreateSplitIntent(dup)
	if err != nil || !res.Replayed || res.IntentID != "sp1" || res.Status != SplitPending {
		t.Fatalf("reordered replay: %+v %v", res, err)
	}
	// 改内容报请求号冲突
	conflict := req
	conflict.Reason = "换个原因"
	if _, err := r.CreateSplitIntent(conflict); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed reason: %v", err)
	}
	// 答复幂等
	ans := answerReq("bob", "sp1", "ans-bob", true)
	if _, err := r.AnswerSplitIntent(ans); err != nil {
		t.Fatal(err)
	}
	res2, err := r.AnswerSplitIntent(ans)
	if err != nil || !res2.Replayed || res2.Status != SplitPending {
		t.Fatalf("answer replay: %+v %v", res2, err)
	}
	ans.Agree = false
	if _, err := r.AnswerSplitIntent(ans); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed answer: %v", err)
	}
	// 撤回幂等
	wd := withdrawReq("alice", "sp1", "wd-1")
	if _, err := r.WithdrawSplitIntent(wd); err != nil {
		t.Fatal(err)
	}
	res3, err := r.WithdrawSplitIntent(wd)
	if err != nil || !res3.Replayed || res3.Status != SplitWithdrawn {
		t.Fatalf("withdraw replay: %+v %v", res3, err)
	}
	wd.Reason = "换个理由"
	if _, err := r.WithdrawSplitIntent(wd); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed withdraw reason: %v", err)
	}
	// 意向操作与既有操作共用请求号范围
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "首发", RequestID: "create-sp1",
		ItemID: "i9", SeriesID: "s1", BatchNo: "b1", HolderID: "alice",
	}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request id shared with issue: %v", err)
	}
}

func TestSplitIntentConcurrentDuplicate(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)

	// 并发重复创建同一请求只生效一次
	const n = 8
	var wg sync.WaitGroup
	results := make([]CreateSplitIntentResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, _ := r.CreateSplitIntent(createIntentReq(r, "sp1"))
			results[i] = res
		}(i)
	}
	wg.Wait()
	succeeded := 0
	for _, res := range results {
		if res.Err == nil {
			succeeded++
		}
	}
	if succeeded != n {
		t.Fatalf("all duplicates must return first success, got %d/%d", succeeded, n)
	}
	evs, _ := r.SplitIntentHistory("i1")
	if len(evs) != 1 {
		t.Fatalf("concurrent create must produce exactly one event: %d", len(evs))
	}
	// 并发重复答复只记录一次
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.AnswerSplitIntent(answerReq("bob", "sp1", "ans-bob", true))
		}()
	}
	wg.Wait()
	evs, _ = r.SplitIntentHistory("i1")
	if len(evs) != 2 {
		t.Fatalf("concurrent answer must produce exactly one answer event: %d", len(evs))
	}
}

// ---- 持久化 ----

func TestSplitIntentPersistence(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	intentWorld(t, r)
	createReq := createIntentReq(r, "sp1")
	if _, err := r.CreateSplitIntent(createReq); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AnswerSplitIntent(answerReq("bob", "sp1", "ans-bob", true)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WithdrawSplitIntent(withdrawReq("alice", "sp1", "wd-1")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// 关闭重开后方案、答复、历史与请求结果完整保留
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()
	in, err := r2.GetSplitIntent("sp1")
	if err != nil || in.Status != SplitWithdrawn || len(in.Shares) != 3 {
		t.Fatalf("intent after reopen: %+v %v", in, err)
	}
	if in.Shares[0].Answer != SplitAnswerAgree || in.Shares[1].Answer != SplitAnswerAgree ||
		in.Shares[2].Answer != "" {
		t.Fatalf("answers after reopen: %+v", in.Shares)
	}
	evs, err := r2.SplitIntentHistory("i1")
	if err != nil || len(evs) != 3 {
		t.Fatalf("history after reopen: %+v %v", evs, err)
	}
	if evs[0].Kind != "create" || evs[1].Kind != "answer" || evs[2].Kind != "withdraw" {
		t.Fatalf("event order after reopen: %+v", evs)
	}
	// 旧请求重放（同一请求内容）
	res, err := r2.CreateSplitIntent(createReq)
	if err != nil || !res.Replayed || res.Status != SplitPending {
		t.Fatalf("create replay after reopen: %+v %v", res, err)
	}
	res2, err := r2.WithdrawSplitIntent(withdrawReq("alice", "sp1", "wd-1"))
	if err != nil || !res2.Replayed || res2.Status != SplitWithdrawn {
		t.Fatalf("withdraw replay after reopen: %+v %v", res2, err)
	}
}

// ---- 查询结果隔离 ----

// wantInitialParties 返回该方案登记时的原始参与名单：按账户编号排列，
// 发起人 alice 创建即视为同意，bob、carol 尚未答复。
func wantInitialParties() []SplitParty {
	return []SplitParty{
		{AccountID: "alice", Share: 5000, Answer: SplitAnswerAgree},
		{AccountID: "bob", Share: 3000},
		{AccountID: "carol", Share: 2000},
	}
}

func cloneParties(in SplitIntent) []SplitParty {
	out := make([]SplitParty, len(in.Shares))
	copy(out, in.Shares)
	return out
}

func assertParties(t *testing.T, got []SplitParty, want []SplitParty, ctx string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: party count %d, want %d: %+v", ctx, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: party %d = %+v, want %+v (full: %+v)", ctx, i, got[i], want[i], got)
		}
	}
}

// TestGetSplitIntentResultIsolation 锁定一条数据隔离规则：每次查询返回的
// 都是独立于登记册、也独立于其他查询结果的副本。调用者为展示而本地改动
// 返回名单（账户、份额、答复、顺序）只影响自己手里那份数据——既不能成为
// 有效答复，也不能在后来的查询或他人保留的旧结果中改写登记的方案；后来
// 的真实答复同样不会回写调用者此前保留的快照。
func TestGetSplitIntentResultIsolation(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if _, err := r.CreateSplitIntent(createIntentReq(r, "sp1")); err != nil {
		t.Fatal(err)
	}

	// 第一次查询并原样保留（模拟另一位调用者此前取得后未作改动的结果）。
	first, err := r.GetSplitIntent("sp1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != SplitPending {
		t.Fatalf("initial intent must be pending: %+v", first)
	}
	wantInitial := wantInitialParties()
	assertParties(t, first.Shares, wantInitial, "首次查询")
	kept := cloneParties(first)

	// 第二次查询，调用者在本地任意篡改这份返回内容：
	//  - 把 bob 伪造成"同意"，并把 carol 伪造成"拒绝"；
	//  - 用名单外、但已登记可用的 dave 替换原参与账户 carol；
	//  - 改动份额使合计不再是 10000；
	//  - 重排名单顺序。
	local, err := r.GetSplitIntent("sp1")
	if err != nil {
		t.Fatal(err)
	}
	local.Shares[0].Answer = SplitAnswerReject                          // 篡改发起人答复
	local.Shares[1].Answer = SplitAnswerAgree                           // 本地给 bob 填"同意"
	local.Shares[2].AccountID = "dave"                                  // 用名单外账户替换 carol
	local.Shares[2].Answer = SplitAnswerReject                          // 本地给 dave 填"拒绝"
	local.Shares[1].Share = 9000                                        // 破坏份额合计
	local.Shares[0], local.Shares[1] = local.Shares[1], local.Shares[0] // 打乱顺序

	// 无论本地改动是否仍符合原方案要求，下一次查询都返回原先登记的账户、
	// 份额、答复与按账户编号排列的顺序，且仍为待确认。
	again, err := r.GetSplitIntent("sp1")
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != SplitPending {
		t.Fatalf("本地改动后重查必须仍为待确认: %+v", again)
	}
	assertParties(t, again.Shares, wantInitial, "本地篡改后重查")

	// 先前另一次查询保留的结果不能跟着本地改动变化。
	assertParties(t, first.Shares, wantInitial, "先前保留的查询结果")
	assertParties(t, kept, wantInitial, "先前保留的名单副本")

	// 本地伪造的答复不是有效答复：被本地加入、实际不在登记名单的 dave
	// 提交答复，仍按既有无权答复拒绝。
	if _, err := r.AnswerSplitIntent(answerReq("dave", "sp1", "ans-dave", true)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("名单外账户答复必须按无权拒绝: %v", err)
	}

	// 原名单中尚未答复的 bob 能按原份额正常同意，不应被本地伪造的"同意"
	// 当作已答复过（否则会落到重复答复分支，行为与事件均不同）。
	res, err := r.AnswerSplitIntent(answerReq("bob", "sp1", "ans-bob", true))
	if err != nil || res.Status != SplitPending || res.Replayed {
		t.Fatalf("bob 首次真实同意应生效且仍待确认: %+v %v", res, err)
	}

	// 真实同意后重查：只显示 bob 这一名的新答复，carol 仍为空，整体待确认，
	// 账户、份额与排序维持原登记方案（carol 没有被本地替换成 dave）。
	after, err := r.GetSplitIntent("sp1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != SplitPending {
		t.Fatalf("仅一方同意后必须仍为待确认，本地伪造不能让全员视为已同意: %+v", after)
	}
	wantAfter := []SplitParty{
		{AccountID: "alice", Share: 5000, Answer: SplitAnswerAgree},
		{AccountID: "bob", Share: 3000, Answer: SplitAnswerAgree},
		{AccountID: "carol", Share: 2000},
	}
	assertParties(t, after.Shares, wantAfter, "真实同意后重查")

	// 后来的真实答复不能回写此前保留、且未自行改写的查询结果：它仍呈现
	// 查询当时 bob、carol 均未答复的内容。
	assertParties(t, first.Shares, wantInitial, "真实答复后旧快照必须保持当时内容")
	assertParties(t, kept, wantInitial, "真实答复后旧名单副本必须保持当时内容")

	// 整个过程只记录拆分约定：藏品持有人与持有版本维持原值，藏品历史不变。
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("查询与本地编辑不能改变持有: %+v", h)
	}
	if hist, _ := r.History("i1"); len(hist) != 1 {
		t.Fatalf("查询与本地编辑不能增加藏品历史: %d", len(hist))
	}

	// 查询和本地编辑不产生任何意向事件；只有 bob 刚才真实提交的首次答复
	// 在创建事件之外新增一条 answer 记录（dave 的无权答复也不落事件）。
	evs, err := r.SplitIntentHistory("i1")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("意向历史应只有创建+一次真实答复，got %d: %+v", len(evs), evs)
	}
	if evs[0].Kind != "create" {
		t.Fatalf("首条事件应为创建: %+v", evs[0])
	}
	if e := evs[1]; e.Kind != "answer" || e.Operator != "bob" ||
		e.Answer != SplitAnswerAgree || e.RequestID != "ans-bob" ||
		e.FromStatus != SplitPending || e.ToStatus != SplitPending {
		t.Fatalf("第二条事件应为 bob 的首次同意: %+v", e)
	}
}

// 手写一份引入拆分意向之前的旧格式快照（没有 intents 等新字段）：旧登记册
// 必须能直接打开，意向查询按不存在处理，新意向可正常创建。
func TestSplitIntentLegacyRegistry(t *testing.T) {
	dir := tempDir(t)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	legacy := `{
  "version": 1,
  "accounts": {
    "alice": {"id": "alice", "metadata": "", "active": true},
    "bob": {"id": "bob", "metadata": "", "active": true}
  },
  "series": {
    "s1": {"id": "s1", "creator_id": "alice", "metadata": "", "sealed": false}
  },
  "items": {
    "i1": {"id": "i1", "series_id": "s1", "batch_no": "b1", "metadata": "", "issued_tx_id": 1}
  },
  "holdings": {
    "i1": {"item_id": "i1", "owner_id": "alice", "version": 1}
  },
  "history": [
    {"seq": 1, "kind": "issue", "item_id": "i1", "operator": "alice", "reason": "首发",
     "request_id": "req-i1", "from_id": "", "to_id": "alice", "from_version": 0, "to_version": 1}
  ],
  "requests": {},
  "next_seq": 1
}`
	if err := os.WriteFile(dataFile(dir), []byte(legacy), fileMode); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("legacy registry must open: %v", err)
	}
	defer r.Close()

	if _, err := r.GetSplitIntent("sp1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy intent lookup: %v", err)
	}
	if _, err := r.CreateSplitIntent(CreateSplitIntentRequest{
		Operator: "alice", Reason: "协商拆分", RequestID: "create-sp1",
		IntentID: "sp1", ItemID: "i1", ExpectedOwner: "alice", ExpectedVer: 1,
		ExpiresAt: r.now().Add(time.Hour),
		Shares: []SplitShare{
			{AccountID: "alice", Share: 6000},
			{AccountID: "bob", Share: 4000},
		},
	}); err != nil {
		t.Fatalf("create intent on legacy registry: %v", err)
	}
	in, err := r.GetSplitIntent("sp1")
	if err != nil || in.Status != SplitPending {
		t.Fatalf("legacy intent after create: %+v %v", in, err)
	}
	// 旧藏品历史不受意向影响
	hist, _ := r.History("i1")
	if len(hist) != 1 {
		t.Fatalf("legacy item history must be unchanged: %d", len(hist))
	}
}
