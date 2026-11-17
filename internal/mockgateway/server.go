// Package mockgateway simulates a third-party payment provider (think ECPay / NewebPay / Stripe Checkout).
// It runs as a separate HTTP server and only talks to the shop through:
//   - POST /v1/payments          (shop -> gateway, create a hosted payment session)
//   - browser redirect to /pay/{session_id}, then back to return_url
//   - POST notify_url            (gateway -> shop, signed server-to-server callback, retried on failure)
package mockgateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"checkout/internal/infra/paymentgateway"
	"checkout/pkg/signature"
)

type session struct {
	ID          string
	Req         paymentgateway.CreatePaymentRequest
	Status      string // pending / SUCCESS / FAILED
	VirtualAcct string
	CreatedAt   time.Time
}

type Server struct {
	publicUrl  string
	apiKey     string
	secret     string
	mu         sync.Mutex
	sessions   map[string]*session
	httpClient *http.Client
}

func NewServer(publicUrl, apiKey, secret string) *Server {
	return &Server{publicUrl: publicUrl, apiKey: apiKey, secret: secret,
		sessions: map[string]*session{}, httpClient: &http.Client{Timeout: 5 * time.Second}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/payments", s.createPayment)
	mux.HandleFunc("GET /pay/{id}", s.payPage)
	mux.HandleFunc("POST /pay/{id}", s.submitPay)
	return mux
}

func (s *Server) createPayment(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || r.Header.Get("X-Api-Key") != s.apiKey || !signature.Verify(s.secret, body, r.Header.Get(signature.Header)) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var req paymentgateway.CreatePaymentRequest
	if err := json.Unmarshal(body, &req); err != nil || req.MerchantTradeNo == "" || req.Amount <= 0 || req.NotifyUrl == "" {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}
	sess := &session{ID: "ps_" + randomHex(8), Req: req, Status: "pending", CreatedAt: time.Now()}
	if req.Method == "atm" {
		sess.VirtualAcct = "9999" + strings.ToUpper(randomHex(5))
	}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(paymentgateway.CreatePaymentResponse{SessionID: sess.ID, PaymentUrl: s.publicUrl + "/pay/" + sess.ID})
}

func (s *Server) payPage(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		http.Error(w, "payment session not found", http.StatusNotFound)
		return
	}
	if sess.Status != "pending" {
		http.Redirect(w, r, sess.Req.ReturnUrl, http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = payTemplate.Execute(w, sess)
}

func (s *Server) submitPay(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		http.Error(w, "payment session not found", http.StatusNotFound)
		return
	}
	_ = r.ParseForm()

	s.mu.Lock()
	if sess.Status != "pending" { // double submit
		s.mu.Unlock()
		http.Redirect(w, r, sess.Req.ReturnUrl, http.StatusSeeOther)
		return
	}
	status, message := paymentgateway.NotificationSuccess, ""
	card := strings.ReplaceAll(r.FormValue("card_number"), " ", "")
	switch {
	case r.FormValue("action") == "cancel":
		status, message = paymentgateway.NotificationFailed, "使用者取消付款"
	case sess.Req.Method == "credit_card" && strings.HasSuffix(card, "0002"):
		status, message = paymentgateway.NotificationFailed, "卡片遭拒絕（模擬）"
	}
	sess.Status = status
	s.mu.Unlock()

	n := paymentgateway.Notification{
		MerchantTradeNo: sess.Req.MerchantTradeNo, SessionID: sess.ID, TransactionID: "TXN" + strings.ToUpper(randomHex(6)),
		Status: status, Amount: sess.Req.Amount, Method: sess.Req.Method, Message: message,
	}
	// First attempt inline so the shopper usually sees the final result immediately; retries in background.
	if err := s.notify(sess.Req.NotifyUrl, n); err != nil {
		slog.Warn("[mock-gateway] notify failed, will retry", "session", sess.ID, "err", err)
		go s.retryNotify(sess.Req.NotifyUrl, n)
	}
	http.Redirect(w, r, sess.Req.ReturnUrl, http.StatusSeeOther)
}

func (s *Server) notify(url string, n paymentgateway.Notification) error {
	body, _ := json.Marshal(n)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(signature.Header, signature.Sign(s.secret, body))
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("merchant responded %d", resp.StatusCode)
	}
	return nil
}

func (s *Server) retryNotify(url string, n paymentgateway.Notification) {
	backoff := 2 * time.Second
	for attempt := 1; attempt <= 5; attempt++ {
		time.Sleep(backoff)
		if err := s.notify(url, n); err == nil {
			return
		} else {
			slog.Warn("[mock-gateway] notify retry failed", "attempt", attempt, "err", err)
		}
		backoff *= 2
	}
}

func (s *Server) get(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var payTemplate = template.Must(template.New("pay").Funcs(template.FuncMap{
	"money": func(v int64) string {
		s := fmt.Sprintf("%d", v)
		var out []byte
		for i := range s {
			if i > 0 && (len(s)-i)%3 == 0 {
				out = append(out, ',')
			}
			out = append(out, s[i])
		}
		return string(out)
	},
}).Parse(`<!doctype html>
<html lang="zh-Hant"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>MockPay 模擬金流</title>
<style>
body{margin:0;font-family:-apple-system,"PingFang TC","Noto Sans TC",sans-serif;background:#eef2f7;color:#1f2937}
.bar{background:#0f172a;color:#fff;padding:14px 20px;font-weight:700;letter-spacing:.5px}
.bar small{font-weight:400;opacity:.7;margin-left:8px}
.card{max-width:440px;margin:32px auto;background:#fff;border-radius:12px;box-shadow:0 8px 30px rgba(15,23,42,.08);padding:24px}
.amount{font-size:30px;font-weight:700;margin:4px 0 16px}
.muted{color:#6b7280;font-size:13px}
label{display:block;font-size:13px;margin:14px 0 6px;color:#374151}
input{width:100%;box-sizing:border-box;padding:10px 12px;border:1px solid #d1d5db;border-radius:8px;font-size:15px}
.row{display:flex;gap:12px}.row>div{flex:1}
.box{background:#f8fafc;border:1px dashed #cbd5e1;border-radius:8px;padding:12px;margin-top:12px;font-size:14px;line-height:1.8}
button{width:100%;padding:12px;border:0;border-radius:8px;font-size:16px;font-weight:600;cursor:pointer;margin-top:18px}
.pay{background:#2563eb;color:#fff}.cancel{background:transparent;color:#6b7280;margin-top:8px}
.hint{font-size:12px;color:#92400e;background:#fffbeb;border-radius:6px;padding:8px 10px;margin-top:12px}
</style></head><body>
<div class="bar">MockPay<small>第三方金流模擬環境</small></div>
<form class="card" method="post">
  <div class="muted">{{.Req.Description}}・交易編號 {{.Req.MerchantTradeNo}}</div>
  <div class="amount">NT$ {{money .Req.Amount}}</div>
  {{if eq .Req.Method "credit_card"}}
    <label>卡號</label><input name="card_number" value="4242 4242 4242 4242" inputmode="numeric">
    <div class="row"><div><label>有效期限</label><input value="12/30"></div><div><label>CVC</label><input value="123"></div></div>
    <div class="hint">測試卡：4242… 付款成功；卡號結尾 0002（例 4000 0000 0000 0002）模擬拒刷</div>
    <button class="pay" name="action" value="pay">確認付款</button>
  {{else if eq .Req.Method "aftee"}}
    <label>手機號碼</label><input name="phone" value="0912345678">
    <div class="box">AFTEE 先享後付：收到繳費通知後 14 天內完成繳款即可。</div>
    <button class="pay" name="action" value="pay">同意並完成</button>
  {{else}}
    <div class="box">銀行代碼：<b>812 台新銀行</b><br>虛擬帳號：<b>{{.VirtualAcct}}</b><br>應繳金額：<b>NT$ {{money .Req.Amount}}</b></div>
    <div class="hint">實際情況下買家會離開頁面去轉帳，金流商入帳後才通知商家。這裡用按鈕模擬「已入帳」。</div>
    <button class="pay" name="action" value="pay">模擬已完成轉帳</button>
  {{end}}
  <button class="cancel" name="action" value="cancel" formnovalidate>取消付款</button>
</form></body></html>`))
