package registry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 本文件为创建代转授权补充"保存失败"场景的回归保障，与单件发行、单件转让
// （issue_save_failure_test.go、transfer_save_failure_test.go）行为对齐：
// 成功与状态类业务拒绝都以保存完成为准——结果尚未写入原登记册而落盘失败
// 时，必须返回实际保存错误，不能报告创建成功，也不能只返回原业务拒绝；
// 结果为空（无授权编号、无状态、绑定版本为零、不标回放、业务错误为空），
// 请求号与授权变更序号都不被这次未保存的结果消耗，即使失败后原数据暂时
// 无法读取，也不留下新增授权、创建记录或请求号占用。保存条件恢复后用
// 完全相同的请求重提，按当时的业务状态重新判断：拒绝条件仍在则重新保存
// 此次拒绝并返回对应业务错误，状态已变为满足请求则正常创建。
//
// 失败注入方式与整批共用：在临时文件路径 .registry.json.tmp 上预先建一个
// 目录，save 在 OpenFile 阶段即以 EISDIR 失败，原快照完好可读。

// assertCreateAuthSaveFailureEmpty 核对"创建授权保存失败"的返回：error 是
// 保存错误而非任何业务拒绝；结果为空——不携带授权编号、状态、绑定版本，
// 业务错误为空，也不标记为重复返回。
func assertCreateAuthSaveFailureEmpty(t *testing.T, res CreateAuthorizationResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if res.AuthID != "" || res.Status != "" || res.GrantVer != 0 ||
		res.Replayed || res.Err != nil {
		t.Fatalf("保存失败必须返回空结果: %+v", res)
	}
}

// assertCreateAuthRequestFree 核对请求号未被这次未保存的结果占用、授权变更
// 序号未被消耗（在同一个已打开的登记册上检查）。
func assertCreateAuthRequestFree(t *testing.T, r *Registry, req CreateAuthorizationRequest, wantNextAuthSeq int64) {
	t.Helper()
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatalf("未保存的结果不应占用请求号 %s", req.RequestID)
	}
	if r.state.NextAuthSeq != wantNextAuthSeq {
		t.Fatalf("NextAuthSeq = %d, want %d", r.state.NextAuthSeq, wantNextAuthSeq)
	}
}

// assertAuthAbsent 核对授权未留下任何痕迹：按编号查询不存在，藏品没有任何
// 授权变更记录，持有保持操作前状态。
func assertAuthAbsent(t *testing.T, r *Registry, authID, itemID, owner string, ver int64) {
	t.Helper()
	if _, err := r.GetAuthorization(authID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败后授权 %s 应查询为不存在: %v", authID, err)
	}
	evs, err := r.AuthorizationHistory(itemID)
	if err != nil {
		t.Fatalf("AuthorizationHistory %s: %v", itemID, err)
	}
	if len(evs) != 0 {
		t.Fatalf("失败后 %s 不应有授权变更记录: %+v", itemID, evs)
	}
	h, err := r.GetHolding(itemID)
	if err != nil {
		t.Fatalf("GetHolding %s: %v", itemID, err)
	}
	if h.OwnerID != owner || h.Version != ver {
		t.Fatalf("失败后 %s 持有被改变: %+v", itemID, h)
	}
}

// TestCreateAuthRejectSaveFailureRetrySameState 覆盖：期望持有版本不符的
// 拒绝落盘失败时返回保存错误、结果为空、请求号不被占用；保存恢复且拒绝
// 条件仍在时，用完全相同的请求重提会重新保存此次拒绝，保存成功后才返回
// 业务错误（首次重提不标回放）；此后相同请求回放该拒绝。
func TestCreateAuthRejectSaveFailureRetrySameState(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	// i1 当前为 alice 版本 1，请求期望版本 2：ErrConflict。
	req := createAuthReq(r, "a1", time.Hour)
	req.ExpectedVer = 2

	blockBatchSave(t, r)
	res, err := r.CreateAuthorization(req)
	assertCreateAuthSaveFailureEmpty(t, res, err)
	assertCreateAuthRequestFree(t, r, req, 0)
	assertAuthAbsent(t, r, "a1", "i1", "alice", 1)

	// 保存条件未恢复时再次提交同一请求：仍按当时状态重新判断并再次尝试
	// 保存，不能把上次未保存的拒绝当成已保存的拒绝回放。
	res, err = r.CreateAuthorization(req)
	assertCreateAuthSaveFailureEmpty(t, res, err)
	assertCreateAuthRequestFree(t, r, req, 0)

	// 保存条件恢复、拒绝条件仍在：重提重新保存此次拒绝，保存成功后返回
	// 业务错误本身并附授权编号，这次重新处理不标记为重复。
	restoreBatchSave(t, r)
	res, err = r.CreateAuthorization(req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("重提应在保存成功后返回业务拒绝 ErrConflict: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrConflict) ||
		res.Status != "" || res.GrantVer != 0 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; !ok {
		t.Fatal("拒绝保存成功后应登记请求结果")
	}
	assertAuthAbsent(t, r, "a1", "i1", "alice", 1)

	// 拒绝已保存：相同内容重提回放原拒绝。
	res, err = r.CreateAuthorization(req)
	if !errors.Is(err, ErrConflict) || !res.Replayed || res.AuthID != "a1" ||
		!errors.Is(res.Err, ErrConflict) {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}
}

// TestCreateAuthRejectSaveFailureRetryAfterStateChange 覆盖：未保存的冲突
// 拒绝不阻碍后续重提——保存条件恢复后，藏品经一笔合法转让恰好达到原请求
// 要求的持有人与版本且授权仍未到期，用完全相同的请求重提应正常创建，
// 不能回放那次未保存的冲突。
func TestCreateAuthRejectSaveFailureRetryAfterStateChange(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	// bob 为操作者，期望 bob 持有版本 2：当前为 alice 版本 1，先被冲突拒绝。
	req := CreateAuthorizationRequest{
		Operator: "bob", Reason: "委托代转", RequestID: "ca-rej2",
		AuthID: "a1", ItemID: "i1", TrusteeID: "alice", ToID: "carol",
		ExpectedOwner: "bob", ExpectedVer: 2,
		ExpiresAt: r.now().Add(time.Hour),
	}

	blockBatchSave(t, r)
	res, err := r.CreateAuthorization(req)
	assertCreateAuthSaveFailureEmpty(t, res, err)
	assertCreateAuthRequestFree(t, r, req, 0)

	// 保存条件恢复后，alice 把 i1 合法转让给 bob：i1 变为 bob 版本 2，
	// 恰好满足原请求的期望持有人与版本。
	restoreBatchSave(t, r)
	if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "rt-i1")); err != nil {
		t.Fatal(err)
	}

	// 用完全相同的原请求重提：按当前业务状态判断，正常创建不标回放。
	res, err = r.CreateAuthorization(req)
	if err != nil {
		t.Fatalf("状态满足后重提应正常创建，不能回放未保存的冲突: %v", err)
	}
	if res.Replayed || res.Err != nil || res.AuthID != "a1" ||
		res.Status != AuthActive || res.GrantVer != 2 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	a, err := r.GetAuthorization("a1")
	if err != nil {
		t.Fatal(err)
	}
	if a.GranterID != "bob" || a.TrusteeID != "alice" || a.ToID != "carol" ||
		a.GrantVer != 2 || a.Status != AuthActive {
		t.Fatalf("授权内容异常: %+v", a)
	}
	// 授权不改变持有关系。
	if h, _ := r.GetHolding("i1"); h.OwnerID != "bob" || h.Version != 2 {
		t.Fatalf("创建授权不应改变持有: %+v", h)
	}

	// 成功结果保存后，相同请求回放首次成功。
	replay, err := r.CreateAuthorization(req)
	if err != nil || !replay.Replayed || replay.AuthID != "a1" || replay.GrantVer != 2 {
		t.Fatalf("成功后重提应回放: %+v, err %v", replay, err)
	}
}

// TestCreateAuthRejectSaveFailureAccountInactive 覆盖：受托账户停用的拒绝
// 落盘失败时同样返回保存错误、结果为空；保存恢复后重提重新保存拒绝并返回
// 账户错误。
func TestCreateAuthRejectSaveFailureAccountInactive(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if err := r.DeactivateAccount("bob"); err != nil {
		t.Fatal(err)
	}
	req := createAuthReq(r, "a1", time.Hour)

	blockBatchSave(t, r)
	res, err := r.CreateAuthorization(req)
	assertCreateAuthSaveFailureEmpty(t, res, err)
	assertCreateAuthRequestFree(t, r, req, 0)
	assertAuthAbsent(t, r, "a1", "i1", "alice", 1)

	restoreBatchSave(t, r)
	res, err = r.CreateAuthorization(req)
	if !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("重提应在保存成功后返回 ErrAccountInactive: %v", err)
	}
	if res.AuthID != "a1" || !errors.Is(res.Err, ErrAccountInactive) || res.Replayed {
		t.Fatalf("账户停用拒绝的重提结果异常: %+v", res)
	}
}

// TestCreateAuthRejectSaveFailureIDOccupied 覆盖：授权编号已占用的拒绝落盘
// 失败时返回保存错误；原授权及其变更记录完整保留，请求号与授权变更序号都
// 不被消耗；保存恢复后重提重新保存编号占用拒绝。
func TestCreateAuthRejectSaveFailureIDOccupied(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	// a1 已由一次成功的创建占用。
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := createAuthReq(r, "a1", 2*time.Hour)
	req.RequestID = "ca-dup"

	blockBatchSave(t, r)
	res, err := r.CreateAuthorization(req)
	assertCreateAuthSaveFailureEmpty(t, res, err)
	assertCreateAuthRequestFree(t, r, req, 1)

	// 原授权及其创建记录保持原状。
	a, err := r.GetAuthorization("a1")
	if err != nil {
		t.Fatalf("原授权不应被失败影响: %v", err)
	}
	if a.GranterID != "alice" || a.TrusteeID != "bob" || a.GrantVer != 1 ||
		a.Status != AuthActive {
		t.Fatalf("原授权内容被改变: %+v", a)
	}
	evs, err := r.AuthorizationHistory("i1")
	if err != nil || len(evs) != 1 || evs[0].Kind != "create" || evs[0].AuthID != "a1" {
		t.Fatalf("原授权变更记录被改变: %+v, err %v", evs, err)
	}

	restoreBatchSave(t, r)
	res, err = r.CreateAuthorization(req)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("重提应在保存成功后返回 ErrAlreadyExists: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrAlreadyExists) {
		t.Fatalf("编号占用拒绝的重提结果异常: %+v", res)
	}
	// 原授权仍保持原状。
	a, err = r.GetAuthorization("a1")
	if err != nil || a.GranterID != "alice" || a.GrantVer != 1 {
		t.Fatalf("原授权仍应保持原状: %+v, err %v", a, err)
	}
}

// TestCreateAuthSuccessSaveFailure 覆盖：创建成功但结果落盘失败时返回保存
// 错误、结果为空；同一个仍打开的登记册上授权查询为不存在、授权历史为空、
// 授权变更序号与请求号不被消耗、持有保持原状；保存恢复后其他操作成功落盘
// 不夹带这次未保存的授权，用原请求重提正常创建一次。
func TestCreateAuthSuccessSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	req := createAuthReq(r, "a1", time.Hour)

	// 业务参数完全合规，失败只可能来自写入阶段。
	blockBatchSave(t, r)
	res, err := r.CreateAuthorization(req)
	assertCreateAuthSaveFailureEmpty(t, res, err)
	assertCreateAuthRequestFree(t, r, req, 0)
	assertAuthAbsent(t, r, "a1", "i1", "alice", 1)

	// 保存条件恢复，另一笔完全无关的正常操作成功落盘：不能把这次未保存
	// 的授权一起写入。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", ""); err != nil {
		t.Fatalf("其他正常操作应能成功保存: %v", err)
	}
	assertAuthAbsent(t, r, "a1", "i1", "alice", 1)
	assertCreateAuthRequestFree(t, r, req, 0)

	// 用完全相同的请求重提：正常创建一次，不标回放、不报请求号冲突。
	res, err = r.CreateAuthorization(req)
	if err != nil {
		t.Fatalf("保存恢复后重提应正常创建: %v", err)
	}
	if res.Replayed || res.Err != nil || res.AuthID != "a1" ||
		res.Status != AuthActive || res.GrantVer != 1 {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if r.state.NextAuthSeq != 1 {
		t.Fatalf("NextAuthSeq = %d, want 1", r.state.NextAuthSeq)
	}
	evs, err := r.AuthorizationHistory("i1")
	if err != nil || len(evs) != 1 || evs[0].Seq != 1 || evs[0].Kind != "create" ||
		evs[0].AuthID != "a1" || evs[0].RequestID != req.RequestID {
		t.Fatalf("授权变更记录异常: %+v, err %v", evs, err)
	}

	// 成功结果保存后，相同请求回放首次成功。
	replay, err := r.CreateAuthorization(req)
	if err != nil || !replay.Replayed || replay.AuthID != "a1" ||
		replay.Status != AuthActive || replay.GrantVer != 1 {
		t.Fatalf("成功后重提应回放: %+v, err %v", replay, err)
	}
}

// TestCreateAuthSuccessSaveFailureUnreadable 覆盖：创建结果落盘失败且原数据
// 暂时无法读取（状态无法按磁盘重建）时，这次操作也不能在同一登记册中留下
// 任何新增授权、创建记录或请求号占用；恢复正常读写后其他操作成功保存也不
// 夹带这次未保存的授权，用原请求重提仍正常创建。
func TestCreateAuthSuccessSaveFailureUnreadable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	req := createAuthReq(r, "a1", time.Hour)

	// 同时注入写入失败与读取失败：临时文件路径被目录占据，原数据文件
	// 暂时替换为目录（读取得到错误而非快照）。
	blockBatchSave(t, r)
	bak := filepath.Join(r.dir, "registry.json.bak")
	if err := os.Rename(dataFile(r.dir), bak); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dataFile(r.dir), dirMode); err != nil {
		t.Fatal(err)
	}

	res, err := r.CreateAuthorization(req)
	assertCreateAuthSaveFailureEmpty(t, res, err)
	// 状态未能按磁盘重建时，内存中同样不能留下这次未保存的创建。
	assertCreateAuthRequestFree(t, r, req, 0)
	assertAuthAbsent(t, r, "a1", "i1", "alice", 1)

	// 恢复正常读写。
	if err := os.Remove(dataFile(r.dir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bak, dataFile(r.dir)); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)

	// 其他操作成功保存不能把这次未保存的授权带入登记册。
	if err := r.RegisterAccount("dave", ""); err != nil {
		t.Fatalf("其他正常操作应能成功保存: %v", err)
	}
	assertAuthAbsent(t, r, "a1", "i1", "alice", 1)
	assertCreateAuthRequestFree(t, r, req, 0)

	// 用原请求号与相同参数重提：正常创建一次。
	res, err = r.CreateAuthorization(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常创建: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || res.Status != AuthActive || res.GrantVer != 1 {
		t.Fatalf("重提结果异常: %+v", res)
	}
}

// TestCreateAuthValidationErrorIgnoresSaveFailure 覆盖：必填内容缺失、到期
// 时间不合法或引用对象不存在的请求不占用请求号、不要求保存，即使存储暂时
// 不可写也仍返回原参数/对象错误，不能改报保存错误。
func TestCreateAuthValidationErrorIgnoresSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)

	blockBatchSave(t, r)
	defer restoreBatchSave(t, r)

	// 必填缺失（缺授权编号）：返回 ErrInvalidArgument，不是保存错误。
	missing := createAuthReq(r, "a1", time.Hour)
	missing.AuthID = ""
	if _, err := r.CreateAuthorization(missing); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}

	// 到期时间不晚于当前时间：返回 ErrInvalidArgument，不是保存错误。
	expired := createAuthReq(r, "a2", -time.Hour)
	if _, err := r.CreateAuthorization(expired); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("到期时间不合法应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}

	// 引用不存在的藏品：返回 ErrNotFound，不是保存错误。
	notFound := createAuthReq(r, "a3", time.Hour)
	notFound.ItemID = "nope"
	res, err := r.CreateAuthorization(notFound)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("引用不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.AuthID != "a3" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("引用不存在的结果异常: %+v", res)
	}

	// 引用不存在的受托账户：返回 ErrNotFound，不是保存错误。
	ghost := createAuthReq(r, "a4", time.Hour)
	ghost.TrusteeID = "ghost"
	if _, err := r.CreateAuthorization(ghost); !errors.Is(err, ErrNotFound) {
		t.Fatalf("受托人不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}

	// 无需保存的错误一律不占用请求号，也不消耗授权变更序号。
	for _, req := range []CreateAuthorizationRequest{missing, expired, notFound, ghost} {
		if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
			t.Fatalf("参数/对象错误不应占用请求号 %s", req.RequestID)
		}
	}
	if r.state.NextAuthSeq != 0 {
		t.Fatalf("NextAuthSeq = %d, want 0", r.state.NextAuthSeq)
	}
}
