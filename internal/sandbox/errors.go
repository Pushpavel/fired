package sandbox

import (
	"errors"
	"fmt"
	"net/http"
)

// Error carries an HTTP status and machine-readable code.
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func errf(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Msg: fmt.Sprintf(format, a...)}
}

var (
	ErrNotFound = &Error{Status: http.StatusNotFound, Code: "not_found", Msg: "sandbox not found"}
	ErrCapacity = &Error{Status: http.StatusServiceUnavailable, Code: "capacity", Msg: "not enough memory budget to run another sandbox"}
)

func badRequest(format string, a ...any) *Error {
	return errf(http.StatusBadRequest, "invalid_request", format, a...)
}

func conflict(format string, a ...any) *Error {
	return errf(http.StatusConflict, "conflict", format, a...)
}

// AsError converts any error to *Error (500 if unknown).
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Status: http.StatusInternalServerError, Code: "internal", Msg: err.Error()}
}
