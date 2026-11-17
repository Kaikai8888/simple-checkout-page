package web

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"checkout/internal/app"
	"checkout/internal/app/adapter"
	"checkout/internal/domain/def"
	"checkout/pkg/signature"
)

const maxBodyBytes = 1 << 20

type Server struct {
	products *app.ProductService
	checkout *app.CheckoutService
	orders   *app.OrderService
	payments *app.PaymentService
	static   fs.FS
}

func NewServer(products *app.ProductService, checkout *app.CheckoutService, orders *app.OrderService,
	payments *app.PaymentService, static fs.FS) *Server {
	return &Server{products: products, checkout: checkout, orders: orders, payments: payments, static: static}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /api/products", s.listProducts)
	mux.HandleFunc("GET /api/products/{id}", s.getProduct)
	mux.HandleFunc("PUT /api/admin/products/{id}", s.updateProduct)

	mux.HandleFunc("GET /api/checkout/options", s.checkoutOptions)
	mux.HandleFunc("POST /api/coupons/preview", s.previewCoupon)
	mux.HandleFunc("POST /api/checkouts", s.submitCheckout)
	mux.HandleFunc("GET /api/checkouts/{id}", s.getCheckout)

	mux.HandleFunc("GET /api/orders/{id}", s.getOrder)
	mux.HandleFunc("POST /api/orders/{id}/payments", s.startPayment)
	mux.HandleFunc("POST /api/payments/callback", s.paymentCallback)

	mux.Handle("GET /", http.FileServerFS(s.static))
	return logRequests(mux)
}

func (s *Server) listProducts(w http.ResponseWriter, r *http.Request) {
	products, err := s.products.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": products})
}

func (s *Server) getProduct(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, r, def.NewError(def.ErrInvalidRequest, "invalid product id"))
		return
	}
	products, err := s.products.GetProducts(r.Context(), []int64{id})
	if err != nil {
		writeError(w, r, err)
		return
	}
	p, ok := products[id]
	if !ok {
		writeError(w, r, def.NewError(def.ErrProductNotFound, "product not found"))
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// updateProduct is a demo admin endpoint (no auth) used to change price / stock during a live demo.
func (s *Server) updateProduct(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, r, def.NewError(def.ErrInvalidRequest, "invalid product id"))
		return
	}
	var in adapter.UpdateProductInput
	if !decodeJSON(w, r, &in) {
		return
	}
	p, err := s.products.UpdateProduct(r.Context(), id, in)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) checkoutOptions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.checkout.Options())
}

func (s *Server) previewCoupon(w http.ResponseWriter, r *http.Request) {
	var in adapter.CouponPreviewInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := s.checkout.PreviewCoupon(r.Context(), in)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) submitCheckout(w http.ResponseWriter, r *http.Request) {
	var in adapter.SubmitCheckoutInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := s.checkout.Submit(r.Context(), r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

func (s *Server) getCheckout(w http.ResponseWriter, r *http.Request) {
	out, err := s.checkout.GetStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	out, err := s.orders.GetOrder(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) startPayment(w http.ResponseWriter, r *http.Request) {
	out, err := s.payments.StartPayment(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) paymentCallback(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, r, def.NewError(def.ErrInvalidRequest, "cannot read body"))
		return
	}
	if err := s.payments.HandleCallback(r.Context(), body, r.Header.Get(signature.Header)); err != nil {
		writeError(w, r, err) // non-2xx makes the gateway retry
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": "OK"})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(v); err != nil {
		writeError(w, r, def.NewError(def.ErrInvalidRequest, "invalid JSON body"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	appErr, ok := def.AsAppError(err)
	if !ok {
		slog.ErrorContext(r.Context(), "unhandled error", "path", r.URL.Path, "err", err)
		appErr = def.NewError(def.ErrInternal, "系統忙碌中，請稍後再試")
	}
	writeJSON(w, statusOf(appErr.Code), map[string]any{"error": appErr})
}

func statusOf(code def.ErrorCode) int {
	switch code {
	case def.ErrInvalidRequest:
		return http.StatusBadRequest
	case def.ErrInvalidSignature:
		return http.StatusUnauthorized
	case def.ErrProductNotFound, def.ErrDraftNotFound, def.ErrOrderNotFound, def.ErrPaymentNotFound:
		return http.StatusNotFound
	case def.ErrItemsChanged, def.ErrAmountMismatch, def.ErrOrderNotPayable:
		return http.StatusConflict
	case def.ErrCouponInvalid:
		return http.StatusUnprocessableEntity
	case def.ErrPaymentGateway:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic", "path", r.URL.Path, "panic", v)
				writeError(rec, r, errors.New("panic"))
			}
			slog.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rec, r)
	})
}
