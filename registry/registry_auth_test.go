package registry

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// authWorld 建立 alice、bob、carol、dave 四个可用账户，系列 s1，
// 并发行一件藏品 i1 给 alice（版本 1）。
func authWorld(t *testing.T, r *Registry) {
	t.Helper()
	for _, a := range []string{"alice", "bob", "carol", "dave"} {
		if err := r.RegisterAccount(a, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.CreateSeries("s1", "alice", "首批系列"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
}

func createAuthReq(authID, item, trustee, receiver string, exp time.Time, ver int64, rid string) CreateAuthRequest {
	return CreateAuthRequest{
		Operator: "alice", Reason: "代转授权", RequestID: rid,
		AuthID: authID, ItemID: item, Trustee: trustee, Receiver: receiver,
		ExpiresAt: exp, ExpectedOwner: "alice", ExpectedVer: ver,
	}
}

func delegateReq(operator, authID, rid string) DelegateTransferRequest {
	return DelegateTransferRequest{
		Operator: operator, Reason: "凭授权代转", RequestID: rid, AuthID: authID,
	}
}

func revokeReq(operator, authID, rid string) RevokeAuthRequest {
	return RevokeAuthRequest{
		Operator: operator, Reason: "撤销授权", RequestID: rid, AuthID: authID,
	}
}

// ---- 创建授权 ----

func TestCreateAuthBasic(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)

	exp := time.Now().Add(time.Hour)
	res, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1"))
	if err != nil {
		t.Fatalf("CreateAuth: %v", err)
	}
	if res.AuthID != "a1" || res.ItemID != "i1" || res.Version != 1 || res.TxSeq != 2 {
		t.Fatalf("unexpected create result: %+v", res)
	}
	a, err := r.GetAuth("a1")
	if err != nil {
		t.Fatal(err)
	}
	if a.ItemID != "i1" || a.Authorizer != "alice" || a.Trustee != "bob" ||
		a.Receiver != "carol" || a.HolderID != "alice" || a.Version != 1 ||
		a.Revoked || a.Used || a.UsedTxID != 0 {
		t.Fatalf("auth wrong: %+v", a)
	}
	if !a.ExpiresAt.Equal(exp) {
		t.Fatalf("expiry mismatch: %v vs %v", a.ExpiresAt, exp)
	}
	// 授权不改变持有关系
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("holding must be unchanged after auth: %+v", h)
	}
	// 授权变更记录
	ah, err := r.AuthHistory("i1")
	if err != nil || len(ah) != 1 {
		t.Fatalf("auth history wrong: %v %v", ah, err)
	}
	e := ah[0]
	if e.Kind != "create" || e.AuthID != "a1" || e.Operator != "alice" ||
		e.RequestID != "c1" || !e.After.Exists || e.After.Revoked || e.After.Used ||
		e.Before.Exists {
		t.Fatalf("create auth history entry wrong: %+v", e)
	}
}

func TestCreateAuthValidation(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)

	// 缺少必填字段
	req := createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")
	req.Trustee = "  "
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing trustee: %v", err)
	}
	// 到期时间不晚于当前时间
	req = createAuthReq("a1", "i1", "bob", "carol", time.Now(), 1, "c2")
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("expiry not after now: %v", err)
	}
	req = createAuthReq("a1", "i1", "bob", "carol", time.Now().Add(-time.Second), 1, "c3")
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("past expiry: %v", err)
	}
	// 受托人是授权人
	req = createAuthReq("a1", "i1", "alice", "carol", exp, 1, "c4")
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrSameAccount) {
		t.Fatalf("trustee == authorizer: %v", err)
	}
	// 接收人是授权人
	req = createAuthReq("a1", "i1", "bob", "alice", exp, 1, "c5")
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrSameAccount) {
		t.Fatalf("receiver == authorizer: %v", err)
	}
	// 受托人与接收人相同是允许的
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "bob", exp, 1, "c6")); err != nil {
		t.Fatalf("trustee == receiver should be allowed: %v", err)
	}
	// 引用不存在的藏品
	req = createAuthReq("a2", "nope", "bob", "carol", exp, 1, "c7")
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown item: %v", err)
	}
	// 引用不存在的受托人
	req = createAuthReq("a2", "i1", "ghost", "carol", exp, 1, "c8")
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown trustee: %v", err)
	}
	// 引用不存在的接收人
	req = createAuthReq("a2", "i1", "bob", "ghost", exp, 1, "c9")
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown receiver: %v", err)
	}
	// 授权编号已占用
	req = createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c10")
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate auth id: %v", err)
	}
	// 持有人或版本不符
	req = createAuthReq("a3", "i1", "bob", "carol", exp, 2, "c11")
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong version: %v", err)
	}
	req = createAuthReq("a3", "i1", "bob", "carol", exp, 1, "c12")
	req.ExpectedOwner = "bob"
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong expected owner: %v", err)
	}
	// 非当前持有人创建授权（受托人不能是操作者本人，否则先触发同账户错误）
	req = createAuthReq("a4", "i1", "dave", "carol", exp, 1, "c13")
	req.Operator = "bob"
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-holder create: %v", err)
	}
	// 拒绝不生成授权
	if _, err := r.GetAuth("a2"); !errors.Is(err, ErrNotFound) {
		t.Fatal("rejected create must not generate auth")
	}
	ah, _ := r.AuthHistory("i1")
	if len(ah) != 1 {
		t.Fatalf("rejected creates must not add history: %d", len(ah))
	}
}

func TestCreateAuthInactiveAccounts(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)

	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive trustee: %v", err)
	}
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "carol", "bob", exp, 1, "c2")); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive receiver: %v", err)
	}
	if err := r.DeactivateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "carol", "dave", exp, 1, "c3")); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive authorizer: %v", err)
	}
}

func TestMultipleAuthsPerItem(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)

	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateAuth(createAuthReq("a2", "i1", "carol", "dave", exp, 1, "c2")); err != nil {
		t.Fatal(err)
	}
	ah, _ := r.AuthHistory("i1")
	if len(ah) != 2 {
		t.Fatalf("want 2 auths, got %d", len(ah))
	}
}

// ---- 代转 ----

func TestDelegateTransferBasic(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}

	res, err := r.DelegateTransfer(delegateReq("bob", "a1", "d1"))
	if err != nil {
		t.Fatalf("DelegateTransfer: %v", err)
	}
	if res.AuthID != "a1" || res.ItemID != "i1" || res.FromID != "alice" ||
		res.ToID != "carol" || res.Version != 2 || res.TxSeq != 3 {
		t.Fatalf("unexpected delegate result: %+v", res)
	}
	// 持有人变为接收人，版本加一
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("holding wrong: %+v", h)
	}
	// 授权记为已使用并关联转让
	a, _ := r.GetAuth("a1")
	if !a.Used || a.UsedTxID != res.TxSeq {
		t.Fatalf("auth should be used with tx seq: %+v", a)
	}
	// 代转出现在原藏品历史中，操作者为实际受托人，可追溯授权
	hist, _ := r.History("i1")
	if len(hist) != 2 {
		t.Fatalf("want 2 history entries, got %d", len(hist))
	}
	e := hist[1]
	if e.Kind != "transfer" || e.Operator != "bob" || e.FromID != "alice" ||
		e.ToID != "carol" || e.FromVersion != 1 || e.ToVersion != 2 || e.AuthID != "a1" {
		t.Fatalf("delegate history entry wrong: %+v", e)
	}
	// 授权变更记录
	ah, _ := r.AuthHistory("i1")
	if len(ah) != 2 || ah[1].Kind != "use" || ah[1].Operator != "bob" ||
		!ah[1].Before.Exists || ah[1].Before.Used ||
		!ah[1].After.Exists || !ah[1].After.Used || ah[1].After.Revoked {
		t.Fatalf("use auth history entry wrong: %+v", ah)
	}
}

func TestDelegateTransferRejections(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}

	// 非受托人操作
	if _, err := r.DelegateTransfer(delegateReq("carol", "a1", "x1")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-trustee: %v", err)
	}
	// 授权不存在
	if _, err := r.DelegateTransfer(delegateReq("bob", "nope", "x2")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown auth: %v", err)
	}
	// 授权已撤销
	if _, err := r.RevokeAuth(revokeReq("alice", "a1", "r1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DelegateTransfer(delegateReq("bob", "a1", "x3")); !errors.Is(err, ErrAuthRevoked) {
		t.Fatalf("revoked: %v", err)
	}
	// 撤销后状态不变
	a, _ := r.GetAuth("a1")
	if a.Used {
		t.Fatal("revoked auth must not be used")
	}
}

func TestDelegateTransferExpired(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	// 极短到期时间
	exp := time.Now().Add(100 * time.Millisecond)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := r.DelegateTransfer(delegateReq("bob", "a1", "d1")); !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("expired: %v", err)
	}
	// 到期拒绝不改变状态
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("expired delegate must not change holding: %+v", h)
	}
	a, _ := r.GetAuth("a1")
	if a.Used {
		t.Fatal("expired delegate must not use auth")
	}
}

func TestDelegateTransferUsed(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DelegateTransfer(delegateReq("bob", "a1", "d1")); err != nil {
		t.Fatal(err)
	}
	// 同一授权不能再次使用
	if _, err := r.DelegateTransfer(delegateReq("bob", "a1", "d2")); !errors.Is(err, ErrAuthUsed) {
		t.Fatalf("used auth: %v", err)
	}
}

func TestDelegateTransferInactiveAccounts(t *testing.T) {
	exp := time.Now().Add(time.Hour)

	// 受托人停用
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DelegateTransfer(delegateReq("bob", "a1", "d1")); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive trustee: %v", err)
	}

	// 接收人停用
	r2 := mustCreate(t, tempDir(t))
	authWorld(t, r2)
	if _, err := r2.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	if err := r2.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.DelegateTransfer(delegateReq("bob", "a1", "d1")); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive receiver: %v", err)
	}

	// 授权人停用
	r3 := mustCreate(t, tempDir(t))
	authWorld(t, r3)
	if _, err := r3.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	if err := r3.DeactivateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := r3.DelegateTransfer(delegateReq("bob", "a1", "d1")); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive authorizer: %v", err)
	}
}

func TestDelegateTransferVersionConflict(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	// 持有人继续直接转让，版本变化
	if _, err := r.Transfer(xferReq("alice", "i1", "dave", 1, "t1")); err != nil {
		t.Fatal(err)
	}
	// 代转返回版本冲突
	if _, err := r.DelegateTransfer(delegateReq("bob", "a1", "d1")); !errors.Is(err, ErrConflict) {
		t.Fatalf("version conflict: %v", err)
	}
	// 即使藏品后来回到原授权人手中也不能恢复旧授权
	if _, err := r.Transfer(xferReq("dave", "i1", "alice", 2, "t2")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DelegateTransfer(delegateReq("bob", "a1", "d2")); !errors.Is(err, ErrConflict) {
		t.Fatalf("auth must not be restored after item returns: %v", err)
	}
}

// ---- 撤销 ----

func TestRevokeAuthBasic(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}

	res, err := r.RevokeAuth(revokeReq("alice", "a1", "r1"))
	if err != nil {
		t.Fatalf("RevokeAuth: %v", err)
	}
	if res.AuthID != "a1" || res.TxSeq != 3 {
		t.Fatalf("unexpected revoke result: %+v", res)
	}
	a, _ := r.GetAuth("a1")
	if !a.Revoked || a.Used {
		t.Fatalf("auth should be revoked: %+v", a)
	}
	ah, _ := r.AuthHistory("i1")
	if len(ah) != 2 || ah[1].Kind != "revoke" || ah[1].Operator != "alice" ||
		!ah[1].Before.Exists || ah[1].Before.Revoked ||
		!ah[1].After.Exists || !ah[1].After.Revoked || ah[1].After.Used {
		t.Fatalf("revoke history entry wrong: %+v", ah)
	}
}

func TestRevokeAuthIdempotent(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RevokeAuth(revokeReq("alice", "a1", "r1")); err != nil {
		t.Fatal(err)
	}
	// 已撤销时再次撤销不增加记录
	if _, err := r.RevokeAuth(revokeReq("alice", "a1", "r2")); err != nil {
		t.Fatalf("re-revoke should succeed: %v", err)
	}
	ah, _ := r.AuthHistory("i1")
	if len(ah) != 2 {
		t.Fatalf("re-revoke must not add history: %d", len(ah))
	}
}

func TestRevokeAuthUsedRejected(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DelegateTransfer(delegateReq("bob", "a1", "d1")); err != nil {
		t.Fatal(err)
	}
	// 已使用的授权拒绝撤销
	if _, err := r.RevokeAuth(revokeReq("alice", "a1", "r1")); !errors.Is(err, ErrAuthUsed) {
		t.Fatalf("revoke used: %v", err)
	}
}

func TestRevokeAuthNonAuthorizer(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RevokeAuth(revokeReq("bob", "a1", "r1")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-authorizer revoke: %v", err)
	}
}

// ---- 幂等 ----

func TestAuthIdempotency(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)

	// 创建授权幂等
	req := createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")
	first, err := r.CreateAuth(req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.CreateAuth(req)
	if err != nil || !again.Replayed || again.TxSeq != first.TxSeq {
		t.Fatalf("create replay: %+v %v", again, err)
	}
	// 参数变化报请求号冲突
	req.Trustee = "dave"
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("create params conflict: %v", err)
	}

	// 撤销幂等
	rreq := revokeReq("alice", "a1", "r1")
	rfirst, err := r.RevokeAuth(rreq)
	if err != nil {
		t.Fatal(err)
	}
	ragain, err := r.RevokeAuth(rreq)
	if err != nil || !ragain.Replayed || ragain.TxSeq != rfirst.TxSeq {
		t.Fatalf("revoke replay: %+v %v", ragain, err)
	}

	// 代转幂等
	dreq := delegateReq("bob", "a2", "d1")
	if _, err := r.CreateAuth(createAuthReq("a2", "i1", "bob", "carol", exp, 1, "c2")); err != nil {
		t.Fatal(err)
	}
	// 先撤销 a1 不影响 a2；用 a2 代转
	dfirst, err := r.DelegateTransfer(dreq)
	if err != nil {
		t.Fatal(err)
	}
	dagain, err := r.DelegateTransfer(dreq)
	if err != nil || !dagain.Replayed || dagain.TxSeq != dfirst.TxSeq ||
		dagain.Version != dfirst.Version || dagain.ToID != dfirst.ToID {
		t.Fatalf("delegate replay: %+v %v", dagain, err)
	}
	// 参数变化报请求号冲突
	dreq.Reason = "changed"
	if _, err := r.DelegateTransfer(dreq); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("delegate params conflict: %v", err)
	}
}

func TestAuthRequestIDSharedRange(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	// 发行用了 "req-i1"，创建授权再用同一号 => 冲突
	req := createAuthReq("a2", "i1", "bob", "carol", exp, 1, "req-i1")
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("shared request id across kinds must conflict: %v", err)
	}
}

func TestAuthValidationErrorsDoNotConsumeRequestID(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)

	// 引用不存在的受托人不占用请求号
	req := createAuthReq("a1", "i1", "ghost", "carol", exp, 1, "c1")
	if _, err := r.CreateAuth(req); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// 补建受托人后用原请求号可成功执行
	if err := r.RegisterAccount("ghost", ""); err != nil {
		t.Fatal(err)
	}
	req.Trustee = "ghost"
	if _, err := r.CreateAuth(req); err != nil {
		t.Fatalf("retry after referenced object created should succeed: %v", err)
	}
}

func TestDelegateReplayAfterStateChanges(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	dreq := delegateReq("bob", "a1", "d1")
	first, err := r.DelegateTransfer(dreq)
	if err != nil {
		t.Fatal(err)
	}

	// 成功代转后藏品再次易手，重提仍返回原结果
	if _, err := r.Transfer(xferReq("carol", "i1", "dave", 2, "t1")); err != nil {
		t.Fatal(err)
	}
	again, err := r.DelegateTransfer(dreq)
	if err != nil || !again.Replayed || again.Version != first.Version ||
		again.ToID != first.ToID || again.TxSeq != first.TxSeq {
		t.Fatalf("replay after ownership change: %+v %v", again, err)
	}

	// 成功代转后停用受托人，重提仍返回原结果
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	again, err = r.DelegateTransfer(dreq)
	if err != nil || !again.Replayed {
		t.Fatalf("replay after deactivation: %+v %v", again, err)
	}
}

func TestDelegateReplayAfterExpiry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(200 * time.Millisecond)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	dreq := delegateReq("bob", "a1", "d1")
	first, err := r.DelegateTransfer(dreq)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	// 已到期，重提仍返回原成功结果
	again, err := r.DelegateTransfer(dreq)
	if err != nil || !again.Replayed || again.TxSeq != first.TxSeq {
		t.Fatalf("replay after expiry: %+v %v", again, err)
	}
}

func TestRejectedDelegateReplaysSameRejection(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	// 非受托人操作被拒绝
	dreq := delegateReq("carol", "a1", "d1")
	if _, err := r.DelegateTransfer(dreq); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	// 即使后来状态变化，重提仍返回首次拒绝
	if _, err := r.RevokeAuth(revokeReq("alice", "a1", "r1")); err != nil {
		t.Fatal(err)
	}
	again, err := r.DelegateTransfer(dreq)
	if !errors.Is(err, ErrForbidden) || !again.Replayed {
		t.Fatalf("rejected delegate must replay first rejection: %+v %v", again, err)
	}
}

// ---- 并发 ----

func TestConcurrentDelegateVsDirectTransfer(t *testing.T) {
	for range 20 {
		r := mustCreate(t, filepath.Join(t.TempDir(), "d"))
		authWorld(t, r)
		exp := time.Now().Add(time.Hour)
		if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var delegateErr, directErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, delegateErr = r.DelegateTransfer(delegateReq("bob", "a1", "d1"))
		}()
		go func() {
			defer wg.Done()
			_, directErr = r.Transfer(xferReq("alice", "i1", "dave", 1, "t1"))
		}()
		wg.Wait()
		// 恰有一笔成功
		succ := 0
		if delegateErr == nil {
			succ++
		}
		if directErr == nil {
			succ++
		}
		if succ != 1 {
			t.Fatalf("exactly one of delegate/direct should succeed, got %d (delegate=%v direct=%v)",
				succ, delegateErr, directErr)
		}
		h, _ := r.GetHolding("i1")
		hist, _ := r.History("i1")
		if delegateErr == nil {
			if h.OwnerID != "carol" || h.Version != 2 || len(hist) != 2 {
				t.Fatalf("delegate-first state inconsistent: %+v hist=%d", h, len(hist))
			}
		} else {
			if h.OwnerID != "dave" || h.Version != 2 || len(hist) != 2 {
				t.Fatalf("direct-first state inconsistent: %+v hist=%d", h, len(hist))
			}
		}
	}
}

func TestConcurrentRevokeVsDelegate(t *testing.T) {
	for range 20 {
		r := mustCreate(t, filepath.Join(t.TempDir(), "d"))
		authWorld(t, r)
		exp := time.Now().Add(time.Hour)
		if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var delegateErr, revokeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, delegateErr = r.DelegateTransfer(delegateReq("bob", "a1", "d1"))
		}()
		go func() {
			defer wg.Done()
			_, revokeErr = r.RevokeAuth(revokeReq("alice", "a1", "r1"))
		}()
		wg.Wait()
		// 撤销不一定成功：若代转先完成，授权已使用，撤销按规则被拒。
		if revokeErr != nil && !errors.Is(revokeErr, ErrAuthUsed) {
			t.Fatalf("revoke either succeeds or is rejected for used auth: %v", revokeErr)
		}
		a, _ := r.GetAuth("a1")
		h, _ := r.GetHolding("i1")
		if delegateErr == nil {
			// 代转先成功：授权已使用，撤销被拒
			if !a.Used || a.Revoked {
				t.Fatalf("delegate-first: auth should be used not revoked: %+v", a)
			}
			if h.OwnerID != "carol" || h.Version != 2 {
				t.Fatalf("delegate-first holding wrong: %+v", h)
			}
		} else {
			// 撤销先成功：代转被拒
			if !a.Revoked || a.Used {
				t.Fatalf("revoke-first: auth should be revoked not used: %+v", a)
			}
			if h.OwnerID != "alice" || h.Version != 1 {
				t.Fatalf("revoke-first holding wrong: %+v", h)
			}
		}
	}
}

func TestConcurrentDeactivateVsDelegate(t *testing.T) {
	for range 20 {
		r := mustCreate(t, filepath.Join(t.TempDir(), "d"))
		authWorld(t, r)
		exp := time.Now().Add(time.Hour)
		if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var delegateErr, deactErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, delegateErr = r.DelegateTransfer(delegateReq("bob", "a1", "d1"))
		}()
		go func() {
			defer wg.Done()
			deactErr = r.DeactivateAccount("bob")
		}()
		wg.Wait()
		if deactErr != nil {
			t.Fatalf("deactivate should always succeed: %v", deactErr)
		}
		a, _ := r.GetAuth("a1")
		h, _ := r.GetHolding("i1")
		if delegateErr == nil {
			if !a.Used || h.OwnerID != "carol" || h.Version != 2 {
				t.Fatalf("delegate-first state inconsistent: %+v %+v", a, h)
			}
		} else {
			if a.Used || h.OwnerID != "alice" || h.Version != 1 {
				t.Fatalf("deactivate-first state inconsistent: %+v %+v", a, h)
			}
		}
	}
}

// ---- 查询 ----

func TestAuthHistoryQuery(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RevokeAuth(revokeReq("alice", "a1", "r1")); err != nil {
		t.Fatal(err)
	}
	// 撤销后不能代转；再创建 a2 并代转
	if _, err := r.CreateAuth(createAuthReq("a2", "i1", "bob", "dave", exp, 1, "c2")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DelegateTransfer(delegateReq("bob", "a2", "d1")); err != nil {
		t.Fatal(err)
	}
	ah, err := r.AuthHistory("i1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ah) != 4 {
		t.Fatalf("want 4 auth history entries (create, revoke, create, use), got %d", len(ah))
	}
	kinds := []string{ah[0].Kind, ah[1].Kind, ah[2].Kind, ah[3].Kind}
	want := []string{"create", "revoke", "create", "use"}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("entry %d kind: got %s want %s", i, kinds[i], want[i])
		}
	}
	// 不存在的藏品
	if _, err := r.AuthHistory("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown item auth history: %v", err)
	}
	// 不存在的授权
	if _, err := r.GetAuth("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown auth: %v", err)
	}
}

// ---- 持久化 ----

func TestAuthPersistence(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	authWorld(t, r)
	exp := time.Now().Add(time.Hour)
	if _, err := r.CreateAuth(createAuthReq("a1", "i1", "bob", "carol", exp, 1, "c1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DelegateTransfer(delegateReq("bob", "a1", "d1")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()
	// 授权状态保留
	a, err := r2.GetAuth("a1")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Used || a.UsedTxID != 3 {
		t.Fatalf("auth must persist: %+v", a)
	}
	// 持有保留
	h, _ := r2.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("holding must persist: %+v", h)
	}
	// 历史保留
	hist, _ := r2.History("i1")
	if len(hist) != 2 || hist[1].AuthID != "a1" {
		t.Fatalf("history must persist: %+v", hist)
	}
	ah, _ := r2.AuthHistory("i1")
	if len(ah) != 2 {
		t.Fatalf("auth history must persist: %d", len(ah))
	}
	// 幂等结果保留：原代转请求重放首次结果
	again, err := r2.DelegateTransfer(delegateReq("bob", "a1", "d1"))
	if err != nil || !again.Replayed || again.Version != 2 || again.TxSeq != 3 {
		t.Fatalf("idempotent result must persist: %+v %v", again, err)
	}
}

func TestOldDataOpensUnchanged(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupWorld(t, r)
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "t1")); err != nil {
		t.Fatal(err)
	}
	r.Close()

	// 用旧版本快照（无授权字段）直接打开
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("open old data: %v", err)
	}
	defer r2.Close()
	hist, _ := r2.History("i1")
	if len(hist) != 2 {
		t.Fatalf("old history must be unchanged: %d", len(hist))
	}
	h, _ := r2.GetHolding("i1")
	if h.OwnerID != "bob" || h.Version != 2 {
		t.Fatalf("old holding must be unchanged: %+v", h)
	}
	// 旧数据上仍可正常创建授权（当前持有人为 bob，版本 2）
	if err := r2.RegisterAccount("carol", ""); err != nil {
		t.Fatal(err)
	}
	if err := r2.RegisterAccount("dave", ""); err != nil {
		t.Fatal(err)
	}
	req := createAuthReq("a1", "i1", "carol", "dave", time.Now().Add(time.Hour), 2, "c1")
	req.Operator = "bob"
	req.ExpectedOwner = "bob"
	if _, err := r2.CreateAuth(req); err != nil {
		t.Fatalf("create auth on old data: %v", err)
	}
}
