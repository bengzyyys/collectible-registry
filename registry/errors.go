package registry

import "errors"

// 错误哨兵。业务拒绝与参数错误使用包裹这些哨兵的错误，调用方可用
// errors.Is 判定；返回结果中的 Err 字段同样包裹这些哨兵。
var (
	// ErrNotFound 表示查询或引用的账户、系列或藏品不存在。
	ErrNotFound = errors.New("registry: 对象不存在")

	// ErrAlreadyExists 表示账户、系列或藏品编号已被占用。
	ErrAlreadyExists = errors.New("registry: 对象已存在")

	// ErrAccountInactive 表示账户已停用，不能参与新的发行或转让。
	ErrAccountInactive = errors.New("registry: 账户已停用")

	// ErrSeriesSealed 表示系列已封存，不能继续发行；封存不可撤销。
	ErrSeriesSealed = errors.New("registry: 系列已封存")

	// ErrConflict 表示乐观并发冲突：藏品持有人或期望版本与当前状态不符。
	ErrConflict = errors.New("registry: 持有人或版本不符，转让被拒绝")

	// ErrSameAccount 表示转让的发起人与接收人相同。
	ErrSameAccount = errors.New("registry: 转让发起人与接收人不能相同")

	// ErrForbidden 表示操作者无权执行该操作（例如非系列创建账户发行或封存）。
	ErrForbidden = errors.New("registry: 操作者无权执行该操作")

	// ErrRequestConflict 表示请求号已被同一操作者使用，但本次业务参数不同。
	ErrRequestConflict = errors.New("registry: 请求号冲突")

	// ErrInvalidArgument 表示必填内容缺失或参数不合法。
	ErrInvalidArgument = errors.New("registry: 参数不合法")

	// ErrAuthRevoked 表示授权已被撤销，不能用于代转。
	ErrAuthRevoked = errors.New("registry: 授权已撤销")

	// ErrAuthExpired 表示授权已到期，不能用于代转。
	ErrAuthExpired = errors.New("registry: 授权已到期")

	// ErrAuthUsed 表示授权已被使用，不能再次代转。
	ErrAuthUsed = errors.New("registry: 授权已使用")

	// ErrCorrupt 表示登记册数据已存在但无法读取或解析。
	ErrCorrupt = errors.New("registry: 登记册数据无法读取")

	// ErrLocked 表示登记册正被另一个进程以独占方式使用。
	ErrLocked = errors.New("registry: 登记册已被占用")
)
