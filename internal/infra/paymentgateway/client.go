// Package paymentgateway is the client for the (mocked) third-party payment provider.
package paymentgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"checkout/pkg/signature"
)

type CreatePaymentRequest struct {
	MerchantTradeNo string `json:"merchant_trade_no"`
	Amount          int64  `json:"amount"`
	Method          string `json:"method"`
	Description     string `json:"description"`
	ReturnUrl       string `json:"return_url"` // browser is redirected here after paying
	NotifyUrl       string `json:"notify_url"` // server-to-server callback
}

type CreatePaymentResponse struct {
	SessionID  string `json:"session_id"`
	PaymentUrl string `json:"payment_url"`
}

// Notification is the callback body sent by the gateway.
type Notification struct {
	MerchantTradeNo string `json:"merchant_trade_no"`
	SessionID       string `json:"session_id"`
	TransactionID   string `json:"transaction_id"`
	Status          string `json:"status"` // SUCCESS | FAILED
	Amount          int64  `json:"amount"`
	Method          string `json:"method"`
	Message         string `json:"message,omitempty"`
}

const (
	NotificationSuccess = "SUCCESS"
	NotificationFailed  = "FAILED"
)

type Client struct {
	baseUrl    string
	apiKey     string
	secret     string
	httpClient *http.Client
}

func NewClient(baseUrl, apiKey, secret string) *Client {
	return &Client{baseUrl: baseUrl, apiKey: apiKey, secret: secret, httpClient: &http.Client{Timeout: 5 * time.Second}}
}

func (c *Client) CreatePayment(ctx context.Context, in CreatePaymentRequest) (CreatePaymentResponse, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return CreatePaymentResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseUrl+"/v1/payments", bytes.NewReader(body))
	if err != nil {
		return CreatePaymentResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set(signature.Header, signature.Sign(c.secret, body))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return CreatePaymentResponse{}, fmt.Errorf("call gateway: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return CreatePaymentResponse{}, fmt.Errorf("gateway returned %d: %s", resp.StatusCode, respBody)
	}
	var out CreatePaymentResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return CreatePaymentResponse{}, fmt.Errorf("decode gateway response: %w", err)
	}
	if out.SessionID == "" || out.PaymentUrl == "" {
		return CreatePaymentResponse{}, fmt.Errorf("gateway response missing session")
	}
	return out, nil
}

// ParseNotification verifies the signature and decodes the callback body.
func (c *Client) ParseNotification(body []byte, sig string) (Notification, bool) {
	if !signature.Verify(c.secret, body, sig) {
		return Notification{}, false
	}
	var n Notification
	if err := json.Unmarshal(body, &n); err != nil {
		return Notification{}, false
	}
	return n, true
}
