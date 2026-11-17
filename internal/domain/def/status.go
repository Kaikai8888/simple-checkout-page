package def

// Checkout draft lifecycle: pending -> processing -> completed | failed
const (
	DraftStatusPending    = "pending"
	DraftStatusProcessing = "processing"
	DraftStatusCompleted  = "completed"
	DraftStatusFailed     = "failed"
)

// Order lifecycle: pending_payment -> paid | expired
// refund_required: payment succeeded after the order had already expired.
const (
	OrderStatusPendingPayment = "pending_payment"
	OrderStatusPaid           = "paid"
	OrderStatusExpired        = "expired"
	OrderStatusRefundRequired = "refund_required"
)

const (
	PaymentStatusPending   = "pending"
	PaymentStatusSucceeded = "succeeded"
	PaymentStatusFailed    = "failed"
	PaymentStatusExpired   = "expired"
)

const (
	InvoiceTypePersonal = "personal"
	InvoiceTypeCompany  = "company"
)

const (
	DiscountTypeFixed   = "fixed"
	DiscountTypePercent = "percent"
)
