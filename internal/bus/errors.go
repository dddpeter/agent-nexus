package bus

import "errors"

// 常见错误。
var (
	ErrEmptyType = errors.New("bus: event type must not be empty")
)
