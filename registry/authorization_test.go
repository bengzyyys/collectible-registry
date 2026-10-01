package registry

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// authzWorld 建立 alice（持有人/创作者）、bob（受托人）、carol（接收人）
// 三个可用账户，并向 alice 发行藏品 i1（持有版本 1）。
func authzWorld(t *testing.T, r *Registry) {
	t.Helper()
	setupWorld(t, r)
	if err := r.RegisterAccount("carol", "接收人"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
}

func createAuthReq(r *Registry, authID string, expiry time.Duration) CreateAuthorizationRequest {
	return CreateAuthorizationRequest{
		Operator: "alice", Reason: "委托代转", RequestID: "create-" + authID,
		AuthID: authID, ItemID: "i1", TrusteeID: "bob", ToID: "carol",
		ExpectedOwner: "alice", ExpectedVer: 1,
		ExpiresAt: r.now().Add(expiry),
	}
}

func proxyReq(op, authID, rid string) ProxyTransferRequest {
	return ProxyTransferRequest{Operator: op, Reason: "受托执行", RequestID: rid, AuthID: authID}
}

// ---- 创建授权 ----

func TestCreateAuthorization(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)

	res, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour))
	if err != nil {
		t.Fatalf("CreateAuthorization: %v", err)
	}
	if res.Status != AuthActive || res.GrantVer != 1 || res.Replayed {
		t.Fatalf("unexpected create result: %+v", res)
	}
	a, err := r.GetAuthorization("a1")
	if err != nil {
		t.Fatal(err)
	}
	if a.GranterID != "alice" || a.TrusteeID != "bob" || a.ToID != "carol" ||
		a.GrantVer != 1 || a.ItemID != "i1" || a.Status != AuthActive {
		t.Fatalf("authorization content wrong: %+v", a)
	}
	// 授权不改变持有关系
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("creating auth must not change holding: %+v", h)
	}
	// 创建事件
	evs, err := r.AuthorizationHistory("i1")
	if err != nil || len(evs) != 1 {
		t.Fatalf("auth history: %v %v", evs, err)
	}
	if e := evs[0]; e.Kind != "create" || e.Operator != "alice" ||
		e.Reason != "委托代转" || e.RequestID != "create-a1" ||
		e.FromStatus != "" || e.ToStatus != AuthActive {
		t.Fatalf("create event wrong: %+v", e)
	}
	// 藏品历史中不出现授权创建
	hist, _ := r.History("i1")
	if len(hist) != 1 {
		t.Fatalf("create auth must not add item history: %d", len(hist))
	}
}

func TestMultipleAuthorizationsBoundToVersions(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)

	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 持有人继续直接转让不被授权限制
	if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "direct-1")); err != nil {
		t.Fatalf("direct transfer must still work: %v", err)
	}
	// 第二份授权绑定新版本
	r2req := CreateAuthorizationRequest{
		Operator: "bob", Reason: "再次委托", RequestID: "create-a2",
		AuthID: "a2", ItemID: "i1", TrusteeID: "alice", ToID: "carol",
		ExpectedOwner: "bob", ExpectedVer: 2, ExpiresAt: r.now().Add(time.Hour),
	}
	if _, err := r.CreateAuthorization(r2req); err != nil {
		t.Fatalf("second auth: %v", err)
	}
	a1, _ := r.GetAuthorization("a1")
	a2, _ := r.GetAuthorization("a2")
	if a1.GrantVer != 1 || a2.GrantVer != 2 || a1.GranterID != "alice" || a2.GranterID != "bob" {
		t.Fatalf("auths bound to different versions: %+v %+v", a1, a2)
	}
	evs, _ := r.AuthorizationHistory("i1")
	if len(evs) != 2 {
		t.Fatalf("want 2 auth events, got %d", len(evs))
	}
}

func TestCreateAuthorizationValidation(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)

	// 受托人或接收人是授权人 -> 同账户错误（每次用不同请求号，状态类
	// 拒绝会占用请求号）
	req := createAuthReq(r, "a1", time.Hour)
	req.TrusteeID = "alice"
	if _, err := r.CreateAuthorization(req); !errors.Is(err, ErrSameAccount) {
		t.Fatalf("trustee==granter must be same-account, got %v", err)
	}
	req = createAuthReq(r, "a2", time.Hour)
	req.ToID = "alice"
	if _, err := r.CreateAuthorization(req); !errors.Is(err, ErrSameAccount) {
		t.Fatalf("to==granter must be same-account, got %v", err)
	}
	// 受托人与接收人可以相同
	req = createAuthReq(r, "a3", time.Hour)
	req.TrusteeID = "carol"
	if _, err := r.CreateAuthorization(req); err != nil {
		t.Fatalf("trustee may equal receiver: %v", err)
	}
	// 到期时间不晚于当前时间 -> 参数错误（不占用请求号，可复用同号）
	req = createAuthReq(r, "a4", 0)
	if _, err := r.CreateAuthorization(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("expires==now must be invalid, got %v", err)
	}
	req = createAuthReq(r, "a4", -time.Minute)
	if _, err := r.CreateAuthorization(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("past expiry must be invalid, got %v", err)
	}
	// 缺字段
	req = createAuthReq(r, "a5", time.Hour)
	req.Reason = " "
	if _, err := r.CreateAuthorization(req); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank reason: %v", err)
	}
}

func TestCreateAuthorizationBusinessErrors(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)

	// 引用不存在 -> ErrNotFound 且不占用请求号
	req := createAuthReq(r, "a1", time.Hour)
	req.ItemID = "ghost"
	if _, err := r.CreateAuthorization(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing item: %v", err)
	}
	req = createAuthReq(r, "a1", time.Hour)
	req.TrusteeID = "ghost"
	if _, err := r.CreateAuthorization(req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing trustee: %v", err)
	}
	// 授权编号已占用：第二次须换用不同请求号
	if _, err := r.CreateAuthorization(createAuthReq(r, "dup", time.Hour)); err != nil {
		t.Fatal(err)
	}
	dup2 := createAuthReq(r, "dup", time.Hour)
	dup2.RequestID = "create-dup-2"
	if _, err := r.CreateAuthorization(dup2); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate auth id: %v", err)
	}
	// 账户停用
	r.RegisterAccount("dave", "")
	deactReq := createAuthReq(r, "a2", time.Hour)
	deactReq.TrusteeID = "dave"
	if err := r.DeactivateAccount("dave"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateAuthorization(deactReq); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive trustee: %v", err)
	}
	// 持有人或版本不符
	req = createAuthReq(r, "a3", time.Hour)
	req.RequestID = "create-a3-v"
	req.ExpectedVer = 9
	if _, err := r.CreateAuthorization(req); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong version: %v", err)
	}
	req = createAuthReq(r, "a4", time.Hour)
	req.Operator = "carol"
	req.ExpectedOwner = "carol"
	req.ToID = "alice" // 接收人不能与授权人相同，避开同账户错误
	if _, err := r.CreateAuthorization(req); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-holder granter: %v", err)
	}
}

// ---- 代转 ----

func TestProxyTransferSuccess(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}

	res, err := r.ProxyTransfer(proxyReq("bob", "a1", "p1"))
	if err != nil {
		t.Fatalf("ProxyTransfer: %v", err)
	}
	if res.FromID != "alice" || res.ToID != "carol" || res.Version != 2 ||
		res.TxSeq != 2 || res.AuthID != "a1" || res.Replayed {
		t.Fatalf("proxy result wrong: %+v", res)
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("holding after proxy wrong: %+v", h)
	}
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthUsed || a.UsedTxSeq != 2 {
		t.Fatalf("auth should be used: %+v", a)
	}
	// 代转出现在藏品历史中：实际操作者是受托人，可追溯授权
	hist, _ := r.History("i1")
	if len(hist) != 2 {
		t.Fatalf("proxy must appear in item history: %d", len(hist))
	}
	if e := hist[1]; e.Kind != "transfer" || e.Operator != "bob" ||
		e.FromID != "alice" || e.ToID != "carol" || e.FromVersion != 1 ||
		e.ToVersion != 2 || e.AuthID != "a1" {
		t.Fatalf("proxy history entry wrong: %+v", e)
	}
	// 授权变更记录中出现使用事件
	evs, _ := r.AuthorizationHistory("i1")
	if len(evs) != 2 || evs[1].Kind != "use" || evs[1].Operator != "bob" ||
		evs[1].FromStatus != AuthActive || evs[1].ToStatus != AuthUsed ||
		evs[1].TxSeq != 2 || evs[1].RequestID != "p1" {
		t.Fatalf("use event wrong: %+v", evs)
	}
}

func TestProxyTransferRejections(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	r.RegisterAccount("dave", "")
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 非受托人
	if _, err := r.ProxyTransfer(proxyReq("dave", "a1", "x1")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-trustee must be forbidden, got %v", err)
	}
	// 授权不存在 -> ErrNotFound（不占用请求号）
	if _, err := r.ProxyTransfer(proxyReq("bob", "ghost", "x2")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing auth: %v", err)
	}
	// 已撤销
	if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "alice", Reason: "改主意", RequestID: "rv1", AuthID: "a1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a1", "x3")); !errors.Is(err, ErrAuthorizationRevoked) {
		t.Fatalf("revoked auth must be rejected distinctly, got %v", err)
	}

	// 已到期：从到期时间点起不可使用
	if _, err := r.CreateAuthorization(createAuthReq(r, "a2", time.Hour)); err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return time.Unix(2_000_000_000, 0) }
	a2, _ := r.GetAuthorization("a2")
	if a2.Status != AuthExpired {
		t.Fatalf("status at/after expiry must be expired, got %s", a2.Status)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a2", "x4")); !errors.Is(err, ErrAuthorizationExpired) {
		t.Fatalf("expired auth must be rejected distinctly, got %v", err)
	}
	r.now = time.Now

	// 已使用
	if _, err := r.CreateAuthorization(createAuthReq(r, "a3", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a3", "use1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a3", "x5")); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("used auth must be rejected distinctly, got %v", err)
	}

	// 任一相关账户停用都拒绝新的代转。此时藏品经 a3 已在 carol（版本 2）
	// 手中，由 carol 作为授权人再建一份授权，随后停用授权人。
	r.RegisterAccount("dave", "")
	a4req := CreateAuthorizationRequest{
		Operator: "carol", Reason: "委托", RequestID: "create-a4", AuthID: "a4",
		ItemID: "i1", TrusteeID: "bob", ToID: "dave",
		ExpectedOwner: "carol", ExpectedVer: 2, ExpiresAt: r.now().Add(time.Hour),
	}
	if _, err := r.CreateAuthorization(a4req); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a4", "x6")); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive granter must block proxy, got %v", err)
	}
}

func TestProxyVersionConflictAfterMoveAndReturn(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	r.RegisterAccount("dave", "")
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 直接转让使版本变化
	if _, err := r.Transfer(xferReq("alice", "i1", "dave", 1, "d1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a1", "p1")); !errors.Is(err, ErrConflict) {
		t.Fatalf("proxy after version change must conflict, got %v", err)
	}
	// 藏品回到原授权人手中，旧授权仍不能恢复
	if _, err := r.Transfer(xferReq("dave", "i1", "alice", 2, "d2")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a1", "p2")); !errors.Is(err, ErrConflict) {
		t.Fatalf("old auth must not revive when item returns, got %v", err)
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "alice" || h.Version != 3 {
		t.Fatalf("rejected proxy must not change holding: %+v", h)
	}
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthActive {
		t.Fatalf("rejected proxy must leave auth active, got %s", a.Status)
	}
}

// ---- 撤销 ----

func TestRevokeAuthorization(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 非授权人不能撤销
	if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "bob", Reason: "x", RequestID: "rv-bad", AuthID: "a1",
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-granter revoke must be forbidden, got %v", err)
	}
	// 授权人撤销成功
	res, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "alice", Reason: "改主意", RequestID: "rv1", AuthID: "a1",
	})
	if err != nil || res.Status != AuthRevoked {
		t.Fatalf("revoke: %+v %v", res, err)
	}
	evs, _ := r.AuthorizationHistory("i1")
	if len(evs) != 2 || evs[1].Kind != "revoke" || evs[1].FromStatus != AuthActive ||
		evs[1].ToStatus != AuthRevoked {
		t.Fatalf("revoke event wrong: %+v", evs)
	}
	// 再次撤销不增加记录
	if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "alice", Reason: "再撤销", RequestID: "rv2", AuthID: "a1",
	}); err != nil {
		t.Fatalf("re-revoke must succeed: %v", err)
	}
	evs, _ = r.AuthorizationHistory("i1")
	if len(evs) != 2 {
		t.Fatalf("re-revoke must not add event: %d", len(evs))
	}
	// 已使用的授权拒绝撤销
	if _, err := r.CreateAuthorization(createAuthReq(r, "a2", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a2", "p1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "alice", Reason: "x", RequestID: "rv3", AuthID: "a2",
	}); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("revoking used auth must fail, got %v", err)
	}
}

// ---- 幂等 ----

func TestAuthorizationRequestIDSharedAcrossKinds(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 创建授权占用 alice 命名空间的 "create-a1"：alice 发起的其他操作
	// 同号冲突。代转操作者也填 alice——回放优先于受托人资格等状态检查。
	if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "create-a1")); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request id shared with transfer must conflict, got %v", err)
	}
	if _, err := r.ProxyTransfer(proxyReq("alice", "a1", "create-a1")); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request id shared with proxy must conflict, got %v", err)
	}
}

func TestCreateAuthzIdempotency(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	req := createAuthReq(r, "a1", time.Hour)
	r1, err := r.CreateAuthorization(req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := r.CreateAuthorization(req)
	if err != nil {
		t.Fatalf("replay should succeed: %v", err)
	}
	if !r2.Replayed || r2.GrantVer != r1.GrantVer {
		t.Fatalf("replay mismatch: %+v vs %+v", r1, r2)
	}
	evs, _ := r.AuthorizationHistory("i1")
	if len(evs) != 1 {
		t.Fatalf("duplicate create adds event: %d", len(evs))
	}
	// 业务参数变化 -> 冲突
	req.TrusteeID = "carol"
	if _, err := r.CreateAuthorization(req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed params must conflict, got %v", err)
	}
}

func TestProxyTransferIdempotentAfterStateChanges(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := proxyReq("bob", "a1", "p1")
	first, err := r.ProxyTransfer(req)
	if err != nil {
		t.Fatal(err)
	}
	// 成功后代转：藏品再次易手、授权已使用、账户停用、时间越过到期点
	if _, err := r.Transfer(xferReq("carol", "i1", "bob", 2, "t2")); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("alice"); err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return time.Unix(3_000_000_000, 0) }
	again, err := r.ProxyTransfer(req)
	if err != nil {
		t.Fatalf("successful proxy must replay despite later state changes: %v", err)
	}
	if !again.Replayed || again.TxSeq != first.TxSeq || again.Version != 2 ||
		again.ToID != "carol" || again.FromID != "alice" {
		t.Fatalf("replayed proxy != first: %+v vs %+v", again, first)
	}
	hist, _ := r.History("i1")
	if len(hist) != 3 { // issue + proxy + later direct transfer
		t.Fatalf("replay adds no history: %d", len(hist))
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "bob" || h.Version != 3 {
		t.Fatalf("replay must not change holding: %+v", h)
	}
}

func TestProxyRejectedRequestReplaysRejection(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	// 授权很快到期；首次代转在到期后被拒（状态类拒绝，占用请求号）
	req := createAuthReq(r, "a1", time.Minute)
	if _, err := r.CreateAuthorization(req); err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return req.ExpiresAt.Add(time.Second) }
	preq := proxyReq("bob", "a1", "late")
	if _, err := r.ProxyTransfer(preq); !errors.Is(err, ErrAuthorizationExpired) {
		t.Fatalf("first proxy should be expired, got %v", err)
	}
	// 即使授权被撤销（另一个拒绝原因），原请求重放仍返回首次拒绝
	if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "alice", Reason: "x", RequestID: "rv", AuthID: "a1",
	}); err != nil {
		t.Fatal(err)
	}
	res, err := r.ProxyTransfer(preq)
	if !errors.Is(err, ErrAuthorizationExpired) || !res.Replayed {
		t.Fatalf("must replay first rejection: res=%+v err=%v", res, err)
	}
}

func TestNotFoundDoesNotConsumeRequestID(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	// 引用不存在的授权发起代转不占用请求号
	preq := proxyReq("bob", "ghost", "p1")
	if _, err := r.ProxyTransfer(preq); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 之后授权存在时该请求号应仍可用
	if _, err := r.ProxyTransfer(proxyReq("bob", "a1", "p1")); err != nil {
		t.Fatalf("request id must remain usable after not-found rejection: %v", err)
	}
}

// ---- 并发：直接转让与代转争用同一版本 ----

func TestConcurrentProxyVsDirectSameVersion(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	r.RegisterAccount("dave", "")

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				_, errs[i] = r.ProxyTransfer(proxyReq("bob", "a1", fmt.Sprintf("p%d", i)))
			} else {
				_, errs[i] = r.Transfer(xferReq("alice", "i1", "dave", 1, fmt.Sprintf("d%d", i)))
			}
		}()
	}
	wg.Wait()
	succ := 0
	for _, e := range errs {
		if e == nil {
			succ++
		}
	}
	if succ != 1 {
		t.Fatalf("exactly one of direct/proxy must succeed, got %d", succ)
	}
	h, _ := r.GetHolding("i1")
	if h.Version != 2 {
		t.Fatalf("version must be 2, got %d", h.Version)
	}
	hist, _ := r.History("i1")
	if len(hist) != 2 {
		t.Fatalf("exactly one transfer history, got %d", len(hist))
	}
	// 成功的若是代转，持有人必须是授权指定的 carol；若是直接转让，则是 dave
	if hist[1].AuthID == "a1" && h.OwnerID != "carol" {
		t.Fatalf("proxy winner must send to fixed receiver, got %s", h.OwnerID)
	}
}

// 撤销与代转同时发生：存在完整先后次序，撤销先则代转被拒，代转先则
// 授权已使用、撤销被拒。
func TestConcurrentRevokeVsProxy(t *testing.T) {
	for range 20 {
		r := mustCreate(t, filepath.Join(t.TempDir(), "d"))
		authzWorld(t, r)
		if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var proxyErr, revokeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, proxyErr = r.ProxyTransfer(proxyReq("bob", "a1", "p1"))
		}()
		go func() {
			defer wg.Done()
			_, revokeErr = r.RevokeAuthorization(RevokeAuthorizationRequest{
				Operator: "alice", Reason: "争用", RequestID: "rv1", AuthID: "a1",
			})
		}()
		wg.Wait()
		h, _ := r.GetHolding("i1")
		a, _ := r.GetAuthorization("a1")
		switch {
		case proxyErr == nil:
			if revokeErr == nil || !errors.Is(revokeErr, ErrAuthorizationUsed) {
				t.Fatalf("proxy-first must make revoke fail with used, got %v", revokeErr)
			}
			if h.OwnerID != "carol" || h.Version != 2 || a.Status != AuthUsed {
				t.Fatalf("proxy-first state wrong: %+v auth=%s", h, a.Status)
			}
		case errors.Is(proxyErr, ErrAuthorizationRevoked):
			if revokeErr != nil {
				t.Fatalf("revoke-first must succeed, got %v", revokeErr)
			}
			if h.OwnerID != "alice" || h.Version != 1 || a.Status != AuthRevoked {
				t.Fatalf("revoke-first state wrong: %+v auth=%s", h, a.Status)
			}
		default:
			t.Fatalf("unexpected ordering: proxy=%v revoke=%v", proxyErr, revokeErr)
		}
	}
}

// 停用与代转同时发生。
func TestConcurrentDeactivateVsProxy(t *testing.T) {
	for range 20 {
		r := mustCreate(t, filepath.Join(t.TempDir(), "d"))
		authzWorld(t, r)
		if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var proxyErr, deactErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, proxyErr = r.ProxyTransfer(proxyReq("bob", "a1", "p1"))
		}()
		go func() {
			defer wg.Done()
			deactErr = r.DeactivateAccount("carol")
		}()
		wg.Wait()
		if deactErr != nil {
			t.Fatalf("deactivate: %v", deactErr)
		}
		h, _ := r.GetHolding("i1")
		hist, _ := r.History("i1")
		if proxyErr == nil {
			if h.OwnerID != "carol" || h.Version != 2 || len(hist) != 2 {
				t.Fatalf("proxy-first state wrong: %+v hist=%d", h, len(hist))
			}
		} else {
			if !errors.Is(proxyErr, ErrAccountInactive) {
				t.Fatalf("deactivate-first proxy must be inactive-rejected, got %v", proxyErr)
			}
			if h.OwnerID != "alice" || h.Version != 1 || len(hist) != 1 {
				t.Fatalf("deactivate-first state wrong: %+v hist=%d", h, len(hist))
			}
		}
	}
}

// ---- 到期边界 ----

func TestRevokeExpiredAuthorization(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	req := createAuthReq(r, "a1", time.Minute)
	if _, err := r.CreateAuthorization(req); err != nil {
		t.Fatal(err)
	}
	// 到期后仍未使用，授权人可以撤销：记录从 expired 变为 revoked
	r.now = func() time.Time { return req.ExpiresAt.Add(time.Second) }
	res, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "alice", Reason: "到期后撤销", RequestID: "rv1", AuthID: "a1",
	})
	if err != nil || res.Status != AuthRevoked {
		t.Fatalf("revoke expired-unused auth: %+v %v", res, err)
	}
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthRevoked {
		t.Fatalf("want revoked, got %s", a.Status)
	}
	evs, _ := r.AuthorizationHistory("i1")
	if len(evs) != 2 || evs[1].FromStatus != AuthExpired || evs[1].ToStatus != AuthRevoked {
		t.Fatalf("revoke event transition wrong: %+v", evs)
	}
}

func TestProxyRejectedWhenReceiverInactive(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.DeactivateAccount("carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a1", "p1")); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("inactive receiver must block proxy, got %v", err)
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("rejected proxy must not change holding: %+v", h)
	}
}

func TestExpiryBoundary(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	base := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return base }

	req := createAuthReq(r, "a1", time.Hour)
	req.ExpiresAt = base.Add(time.Hour)
	if _, err := r.CreateAuthorization(req); err != nil {
		t.Fatal(err)
	}
	// 到期前一刻可用
	r.now = func() time.Time { return base.Add(time.Hour - time.Nanosecond) }
	if a, _ := r.GetAuthorization("a1"); a.Status != AuthActive {
		t.Fatalf("just before expiry must be active, got %s", a.Status)
	}
	// 正好到期时间点起不可使用
	r.now = func() time.Time { return base.Add(time.Hour) }
	if a, _ := r.GetAuthorization("a1"); a.Status != AuthExpired {
		t.Fatalf("at expiry must be expired, got %s", a.Status)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a1", "p1")); !errors.Is(err, ErrAuthorizationExpired) {
		t.Fatalf("proxy at expiry must fail expired, got %v", err)
	}
}

// ---- 持久化 ----

func TestAuthorizationPersistenceAcrossReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", 24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a1", "p1")); err != nil {
		t.Fatal(err)
	}
	// 代转后藏品在 carol（版本 2）手中，由 carol 再创建并撤销第二份授权
	a2req := CreateAuthorizationRequest{
		Operator: "carol", Reason: "委托2", RequestID: "create-a2", AuthID: "a2",
		ItemID: "i1", TrusteeID: "bob", ToID: "alice",
		ExpectedOwner: "carol", ExpectedVer: 2, ExpiresAt: r.now().Add(time.Hour),
	}
	if _, err := r.CreateAuthorization(a2req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "carol", Reason: "撤销", RequestID: "rv1", AuthID: "a2",
	}); err != nil {
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

	a1, err := r2.GetAuthorization("a1")
	if err != nil || a1.Status != AuthUsed || a1.UsedTxSeq != 2 {
		t.Fatalf("used auth must persist: %+v %v", a1, err)
	}
	a2, err := r2.GetAuthorization("a2")
	if err != nil || a2.Status != AuthRevoked || a2.GranterID != "carol" {
		t.Fatalf("revoked auth must persist: %+v %v", a2, err)
	}
	evs, err := r2.AuthorizationHistory("i1")
	if err != nil || len(evs) != 4 {
		t.Fatalf("auth events must persist: %d %v", len(evs), err)
	}
	hist, err := r2.History("i1")
	if err != nil || len(hist) != 2 || hist[1].AuthID != "a1" || hist[1].Operator != "bob" {
		t.Fatalf("proxy history must persist with trustee and auth trace: %+v %v", hist, err)
	}
	// 成功代转请求号重放
	again, err := r2.ProxyTransfer(proxyReq("bob", "a1", "p1"))
	if err != nil || !again.Replayed || again.TxSeq != 2 {
		t.Fatalf("proxy replay across reopen: %+v %v", again, err)
	}
	// 已撤销授权重开后代转仍被拒
	if _, err := r2.ProxyTransfer(proxyReq("bob", "a2", "p2")); !errors.Is(err, ErrAuthorizationRevoked) {
		t.Fatalf("revoked auth still rejects after reopen: %v", err)
	}
}

// 旧版本写出的数据（没有授权字段）必须能直接打开，原历史不变。
func TestOpenSnapshotWithoutAuthorizationFields(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	setupWorld(t, r)
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "t1")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// 读入旧格式快照（删除新字段后即为旧格式），确认仍可打开。
	b, err := os.ReadFile(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"authzs"`, `"auth_events"`, `"next_auth_seq"`, `"auth_id"`} {
		if bytes.Contains(b, []byte(key)) {
			t.Fatalf("baseline snapshot should not contain %s", key)
		}
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("old snapshot must open: %v", err)
	}
	defer r2.Close()
	hist, _ := r2.History("i1")
	if len(hist) != 2 || hist[1].AuthID != "" {
		t.Fatalf("original history unchanged: %+v", hist)
	}
	if evs, err := r2.AuthorizationHistory("i1"); err != nil || len(evs) != 0 {
		t.Fatalf("old item has empty auth history: %v %v", evs, err)
	}
	// 打开后新功能可正常使用（当前持有人为 bob，版本 2）
	if err := r2.RegisterAccount("carol", "接收人"); err != nil {
		t.Fatal(err)
	}
	newReq := CreateAuthorizationRequest{
		Operator: "bob", Reason: "委托", RequestID: "ca1", AuthID: "a1",
		ItemID: "i1", TrusteeID: "alice", ToID: "carol",
		ExpectedOwner: "bob", ExpectedVer: 2, ExpiresAt: r2.now().Add(time.Hour),
	}
	if _, err := r2.CreateAuthorization(newReq); err != nil {
		t.Fatalf("new feature works on opened old registry: %v", err)
	}
}
