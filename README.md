# 本地数字藏品登记与转让

在本机运行的数字藏品登记册：账户登记与停用、系列创建与封存、单件藏品
发行、基于期望版本的乐观并发转让，以及持有与历史查询。所有数据保存在
调用者指定的本机目录中，成功操作返回前已原子落盘。

## 使用

```bash
go test ./...
```

## 能力概览

入口在 `registry` 包：

- `Create(dir)` / `Open(dir)` / `Close()`：在指定数据位置新建、打开、
  关闭登记册。目录中没有数据时 `Open` 明确报错（不会当成空登记册），
  数据无法读取时返回 `ErrCorrupt`。同一目录同时只能被一个进程独占打开。
- 账户：`RegisterAccount` 唯一编号登记、`DeactivateAccount` 停用；
  停用后原有藏品与历史仍可查询，但不能发行、发起或接收转让。
- 系列：`CreateSeries`（记录创建账户与文字元数据）、`SealSeries`
  （仅创建账户可操作；封存后不能发行，已发行藏品仍可转让，不可撤销）。
- 发行 `Issue`：唯一藏品编号 + 系列 + 批次号 + 元数据 + 初始持有人；
  初始持有人必须已登记且可用，持有版本从 1 开始。
- 转让 `Transfer`：当前持有人发起，接收人已登记、可用且不同于自己；
  必须给出期望持有人与期望版本，不符、藏品不存在或账户停用均明确拒绝，
  失败不改变持有、不产生历史。每次成功转让版本加 1。
- 限时一次性代转授权：
  - `CreateAuthorization`：当前持有人为一件已发行藏品创建授权，指定
    唯一授权编号、受托账户、固定接收账户、绝对到期时间，并提交期望
    持有人与版本。受托人或接收人不能是授权人（返回 `ErrSameAccount`），
    受托人可与接收人相同；到期时间必须晚于当前时间（否则
    `ErrInvalidArgument`）。同一藏品可有多份授权，各自绑定创建时的
    持有版本；授权不改变持有关系，也不限制持有人继续直接转让。
  - `ProxyTransfer`：受托人凭授权发起代转，藏品与接收人均以授权记载
    为准。成功后持有人变为接收人、版本加一，授权记为已使用并关联该
    笔转让；代转同样出现在藏品 `History` 中，操作者为实际受托账户，
    历史条目的 `AuthID` 可追溯授权。非受托人（`ErrForbidden`）、已
    撤销（`ErrAuthorizationRevoked`）、已到期
    （`ErrAuthorizationExpired`，到期时间点起即不可用）、已使用
    （`ErrAuthorizationUsed`）分别明确拒绝；授权人、受托人或接收人
    任一停用返回 `ErrAccountInactive`；持有版本已变化返回
    `ErrConflict`——即使藏品后来回到原授权人手中，旧授权也不恢复。
  - `RevokeAuthorization`：仅授权人可撤销尚未使用的授权；已撤销后
    再次撤销不增加记录（幂等），已使用时拒绝撤销。
  - 查询：`GetAuthorization` 按编号返回授权内容与当前状态
    （active/revoked/expired/used，到期按当前时间实时判断）；
    `AuthorizationHistory` 按藏品查看创建、撤销、使用记录中的操作者、
    原因、请求号与前后状态。
- 查询：`GetAccount` / `GetSeries` / `GetItem` / `GetHolding` /
  `HoldingsOf` / `History`。不存在的对象返回包裹 `ErrNotFound` 的错误；
  历史按顺序包含发行与历次成功转让（含代转）的操作者、原因、前后持有
  人与版本，发行前的持有人与版本为空。
- 幂等：发行、转让、创建/撤销授权与代转都需要操作者、原因、请求号；
  同一操作者的请求号在全部操作间共用。相同业务参数重提返回首次的成功
  结果或状态类业务拒绝（成功代转即使在到期、停用或再次易手后重提，仍
  返回原转让结果），任一参数改变返回 `ErrRequestConflict`，并发重复
  提交只生效一次。参数错误与引用不存在不占用请求号。
- 持久化：每次操作在同一临界区内修改状态并以临时文件 + fsync + 原子
  rename 落盘，进程被直接终止后重开，登记、停用、封存、持有、授权、
  授权变更记录、请求结果与历史都完整保留，不会出现"已换人却没有对应
  历史"的中间状态。未收到结果的请求可用原请求号重试，返回已保存结果
  或完整执行一次。旧版本数据可直接打开，原历史不变。

## 错误判定

业务错误均为哨兵（`errors.Is` 判定）：`ErrNotFound`、`ErrAlreadyExists`、
`ErrAccountInactive`、`ErrSeriesSealed`、`ErrConflict`、`ErrSameAccount`、
`ErrForbidden`、`ErrAuthorizationRevoked`、`ErrAuthorizationExpired`、
`ErrAuthorizationUsed`、`ErrRequestConflict`、`ErrInvalidArgument`、
`ErrCorrupt`、`ErrLocked`。
