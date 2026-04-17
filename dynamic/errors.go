package dynamic

import "errors"

var (
	ErrModelNotFound    = errors.New("model not found in registry")
	ErrRecordNotFound   = errors.New("record not found")
	ErrForbidden        = errors.New("permission denied")
	ErrInvalidInput     = errors.New("invalid input")
	ErrInvalidID        = errors.New("invalid id")
)
