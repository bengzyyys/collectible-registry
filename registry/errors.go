package registry

import "fmt"

// ErrorCode 是登记册错误的分类代码。
type ErrorCode string

const (
	// ErrInvalidRequest 表示请求缺少必填内容或参数不合法。
	ErrInvalidRequest ErrorCode = "invalid_request"
	// ErrNotFound 表示查询或引用的对象不存在。
	ErrNotFound ErrorCode = "not_found"
	// ErrAlreadyExists 表示编号重复（账户、系列、藏品编号已存在）。
	ErrAlreadyExists ErrorCode = "already_exists"
	// ErrInactive 表示账户已停用，不能发行、发起或接收转让。
	ErrInactive ErrorCode = "inactive"
	// ErrSealed 表示系列已封存，不能继续发行。
	ErrSealed ErrorCode = "sealed"
	// ErrHolderMismatch 表示转让请求的当前持有人与登记不符。
	ErrHolderMismatch ErrorCode = "holder_mismatch"
	// ErrVersionMismatch 表示转让请求的期望版本与当前版本不符。
	ErrVersionMismatch ErrorCode = "version_mismatch"
	// ErrRequestConflict 表示同一请求号被用于不同业务参数。
	ErrRequestConflict ErrorCode = "request_conflict"
	// ErrForbidden 表示操作者无权执行该操作（如非创建者发行或封存）。
	ErrForbidden ErrorCode = "forbidden"
	// ErrConflict 表示与当前状态冲突（如重复停用、自我转让）。
	ErrConflict ErrorCode = "conflict"
	// ErrCorruptData 表示持久化数据损坏，无法读取。
	ErrCorruptData ErrorCode = "corrupt_data"
	// ErrIO 表示写入或同步持久化数据失败。
	ErrIO ErrorCode = "io_error"
	// ErrClosed 表示登记册已关闭，不能继续操作。
	ErrClosed ErrorCode = "closed"
)

// Error 是登记册操作返回的明确错误，包含分类代码与可读信息。
type Error struct {
	Code    ErrorCode
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}
