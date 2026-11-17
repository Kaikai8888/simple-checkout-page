package def

import "errors"

type ErrorCode string

const (
	ErrInvalidRequest   ErrorCode = "INVALID_REQUEST"
	ErrProductNotFound  ErrorCode = "PRODUCT_NOT_FOUND"
	ErrItemsChanged     ErrorCode = "ITEMS_CHANGED" // price changed / stock insufficient / sold out
	ErrCouponInvalid    ErrorCode = "COUPON_INVALID"
	ErrAmountMismatch   ErrorCode = "AMOUNT_MISMATCH"
	ErrDraftNotFound    ErrorCode = "CHECKOUT_NOT_FOUND"
	ErrOrderNotFound    ErrorCode = "ORDER_NOT_FOUND"
	ErrOrderNotPayable  ErrorCode = "ORDER_NOT_PAYABLE"
	ErrPaymentNotFound  ErrorCode = "PAYMENT_NOT_FOUND"
	ErrInvalidSignature ErrorCode = "INVALID_SIGNATURE"
	ErrPaymentGateway   ErrorCode = "PAYMENT_GATEWAY_ERROR"
	ErrCacheWrite       ErrorCode = "CACHE_WRITE_FAILED"
	ErrInternal         ErrorCode = "INTERNAL_ERROR"
)

// AppError is a business error that can be returned to the client as-is.
type AppError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Details any       `json:"details,omitempty"`
}

func (e *AppError) Error() string { return string(e.Code) + ": " + e.Message }

func NewError(code ErrorCode, message string) *AppError {
	return &AppError{Code: code, Message: message}
}

func (e *AppError) WithDetails(details any) *AppError {
	cp := *e
	cp.Details = details
	return &cp
}

func AsAppError(err error) (*AppError, bool) {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr, true
	}
	return nil, false
}
