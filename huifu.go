package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	huifuPreorderPath = "/v2/trade/payment/jspay"
	huifuQueryPath    = "/v2/trade/payment/scanpay/query"
	huifuClosePath    = "/v2/trade/payment/scanpay/close"
	huifuTimeLayout   = "20060102150405"
	huifuSuccessCode  = "00000000"
	huifuPendingCode  = "00000100"
)

var huifuLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type huifuPayClient struct {
	baseURL        string
	sysID          string
	productID      string
	huifuID        string
	privateKey     *rsa.PrivateKey
	publicKey      *rsa.PublicKey
	notifyURL      string
	proxyToken     string
	connectTimeout time.Duration
	readTimeout    time.Duration
	httpClient     *http.Client
}

type huifuPaymentResult struct {
	PaymentNo         string
	PlatformTradeNo   string
	TransactionStatus string
	CloseStatus       string
	OrderStatus       string
	TradeType         string
	AmountCents       int64
	JumpURL           string
	ResponsePayload   string
}

func newHuifuPayClientFromEnv() *huifuPayClient {
	connect := envInt("PAY_HUIFU_CONNECT_TIMEOUT_MS", 5000)
	read := envInt("PAY_HUIFU_READ_TIMEOUT_MS", 10000)
	return &huifuPayClient{
		baseURL:        strings.TrimRight(strings.TrimSpace(os.Getenv("PAY_HUIFU_BASE_URL")), "/"),
		sysID:          strings.TrimSpace(os.Getenv("PAY_HUIFU_SYS_ID")),
		productID:      strings.TrimSpace(os.Getenv("PAY_HUIFU_PRODUCT_ID")),
		huifuID:        strings.TrimSpace(os.Getenv("PAY_HUIFU_ID")),
		privateKey:     loadHuifuPrivateKey(os.Getenv("PAY_HUIFU_PRIVATE_KEY_FILE")),
		publicKey:      loadHuifuPublicKey(os.Getenv("PAY_HUIFU_PLATFORM_PUBLIC_KEY_FILE")),
		notifyURL:      strings.TrimSpace(os.Getenv("PAY_HUIFU_NOTIFY_URL")),
		proxyToken:     strings.TrimSpace(os.Getenv("PAY_HUIFU_PROXY_TOKEN")),
		connectTimeout: time.Duration(connect) * time.Millisecond,
		readTimeout:    time.Duration(read) * time.Millisecond,
		httpClient:     &http.Client{},
	}
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func loadHuifuPrivateKey(path string) *rsa.PrivateKey {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || len(data) > 64*1024 {
		return nil
	}
	if block, _ := pem.Decode(data); block != nil {
		data = block.Bytes
	}
	key, err := x509.ParsePKCS8PrivateKey(data)
	if err != nil {
		return nil
	}
	private, _ := key.(*rsa.PrivateKey)
	return private
}

func loadHuifuPublicKey(path string) *rsa.PublicKey {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || len(data) > 64*1024 {
		return nil
	}
	if block, _ := pem.Decode(data); block != nil {
		data = block.Bytes
	}
	key, err := x509.ParsePKIXPublicKey(data)
	if err != nil {
		return nil
	}
	public, _ := key.(*rsa.PublicKey)
	return public
}

func (c *huifuPayClient) validateConfiguration() error {
	if c == nil || c.baseURL == "" || c.sysID == "" || c.productID == "" || c.huifuID == "" || c.privateKey == nil || c.publicKey == nil {
		return errors.New("汇付配置不完整")
	}
	if !strings.HasPrefix(strings.ToLower(c.baseURL), "https://") {
		return errors.New("汇付接口地址必须使用 HTTPS")
	}
	if c.notifyURL == "" {
		return errors.New("汇付通知地址未配置")
	}
	return nil
}

func (c *huifuPayClient) CreatePayment(paymentNo, subject string, amountCents int64, channel string, expireAt time.Time) (huifuPaymentResult, error) {
	if err := c.validateConfiguration(); err != nil {
		return huifuPaymentResult{}, err
	}
	tradeType := huifuPCTradeType(channel)
	date := paymentDate(paymentNo)
	data := map[string]any{
		"req_date":        date,
		"req_seq_id":      paymentNo,
		"huifu_id":        c.huifuID,
		"trade_type":      tradeType,
		"goods_desc":      truncatePaymentSubject(subject),
		"trans_amt":       fmt.Sprintf("%.2f", float64(amountCents)/100),
		"delay_acct_flag": "N",
		"notify_url":      c.notifyURL,
		"mer_priv":        paymentNo,
		"time_expire":     expireAt.In(huifuLocation).Format(huifuTimeLayout),
	}
	response, raw, err := c.post(huifuPreorderPath, data)
	if err != nil {
		return huifuPaymentResult{}, err
	}
	if code := stringValue(response["resp_code"]); !isHuifuPreorderAccepted(code) {
		return huifuPaymentResult{}, fmt.Errorf("汇付下单失败: %s", stringValue(response["resp_desc"]))
	}
	if stringValue(response["huifu_id"]) != c.huifuID || stringValue(response["req_seq_id"]) != paymentNo {
		return huifuPaymentResult{}, errors.New("汇付下单返回与本地订单不一致")
	}
	jump := firstValue(response, "qr_code", "jump_url", "pay_info")
	if jump == "" {
		return huifuPaymentResult{}, errors.New("汇付下单未返回二维码")
	}
	return huifuPaymentResult{PaymentNo: paymentNo, PlatformTradeNo: firstValue(response, "hf_seq_id", "pre_order_id"), JumpURL: jump, ResponsePayload: raw}, nil
}

func isHuifuPreorderAccepted(code string) bool {
	return code == huifuSuccessCode || code == huifuPendingCode
}

// 汇付 PC 聚合正扫 uses the native trade types. The older osh-backend
// integration and the PC payment flow both use these values.
func huifuPCTradeType(channel string) string {
	if strings.EqualFold(strings.TrimSpace(channel), "wechat") {
		return "T_NATIVE"
	}
	return "A_NATIVE"
}

func (c *huifuPayClient) QueryPayment(paymentNo string, expectedAmountCents int64) (huifuPaymentResult, error) {
	if err := c.validateConfiguration(); err != nil {
		return huifuPaymentResult{}, err
	}
	data := map[string]any{
		"req_date":       time.Now().UTC().Format("20060102"),
		"req_seq_id":     "Q" + newPaymentNumber("Q"),
		"huifu_id":       c.huifuID,
		"org_req_date":   paymentDate(paymentNo),
		"org_req_seq_id": paymentNo,
	}
	response, raw, err := c.post(huifuQueryPath, data)
	if err != nil {
		return huifuPaymentResult{}, err
	}
	if code := stringValue(response["resp_code"]); code != "00000000" {
		return huifuPaymentResult{}, fmt.Errorf("汇付查单失败: %s", stringValue(response["resp_desc"]))
	}
	result, err := huifuResultFromResponse(response, raw, paymentNo)
	if err != nil {
		return huifuPaymentResult{}, err
	}
	if result.TransactionStatus == "S" && result.OrderStatus != "" && result.OrderStatus != "1" {
		return huifuPaymentResult{}, errors.New("汇付查单订单状态不是成功")
	}
	if result.AmountCents != 0 && result.AmountCents != expectedAmountCents {
		return huifuPaymentResult{}, errors.New("汇付查单金额不一致")
	}
	return result, nil
}

func (c *huifuPayClient) ClosePayment(paymentNo string) (huifuPaymentResult, error) {
	if err := c.validateConfiguration(); err != nil {
		return huifuPaymentResult{}, err
	}
	data := map[string]any{
		"req_date":       time.Now().UTC().Format("20060102"),
		"req_seq_id":     "C" + newPaymentNumber("C"),
		"huifu_id":       c.huifuID,
		"org_req_date":   paymentDate(paymentNo),
		"org_req_seq_id": paymentNo,
	}
	response, raw, err := c.post(huifuClosePath, data)
	if err != nil {
		return huifuPaymentResult{}, err
	}
	if code := stringValue(response["resp_code"]); code != "00000000" {
		return huifuPaymentResult{}, fmt.Errorf("汇付关单失败: %s", stringValue(response["resp_desc"]))
	}
	return huifuPaymentResult{PaymentNo: paymentNo, CloseStatus: firstValue(response, "close_stat", "trans_stat"), ResponsePayload: raw}, nil
}

func (c *huifuPayClient) VerifyNotification(rawData, signature string) (huifuPaymentResult, error) {
	if err := c.validateConfiguration(); err != nil {
		return huifuPaymentResult{}, err
	}
	if rawData == "" || signature == "" || !verifyHuifuRaw(rawData, signature, c.publicKey) {
		return huifuPaymentResult{}, errors.New("汇付通知验签失败")
	}
	var response map[string]any
	if err := json.Unmarshal([]byte(rawData), &response); err != nil {
		return huifuPaymentResult{}, errors.New("汇付通知数据格式错误")
	}
	if stringValue(response["huifu_id"]) != c.huifuID {
		return huifuPaymentResult{}, errors.New("汇付通知商户号不一致")
	}
	result, err := huifuResultFromResponse(response, rawData, firstValue(response, "org_req_seq_id", "req_seq_id"))
	if err != nil {
		return huifuPaymentResult{}, err
	}
	if result.TransactionStatus != "S" && result.TransactionStatus != "F" {
		return huifuPaymentResult{}, errors.New("汇付通知不是支付终态")
	}
	if result.TransactionStatus == "S" && result.OrderStatus != "" && result.OrderStatus != "1" {
		return huifuPaymentResult{}, errors.New("汇付通知订单状态不是成功")
	}
	if result.TransactionStatus == "S" && result.PlatformTradeNo == "" {
		return huifuPaymentResult{}, errors.New("汇付通知缺少平台交易号")
	}
	if result.PaymentNo == "" {
		return huifuPaymentResult{}, errors.New("汇付通知缺少支付流水号")
	}
	return result, nil
}

func (c *huifuPayClient) post(path string, data map[string]any) (map[string]any, string, error) {
	signature, err := signHuifuData(data, c.privateKey)
	if err != nil {
		return nil, "", err
	}
	body := map[string]any{"sys_id": c.sysID, "product_id": c.productID, "data": data, "sign": signature}
	requestBytes, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := contextWithTimeout(c.connectTimeout + c.readTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(requestBytes))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Accept", "application/json")
	if c.proxyToken != "" {
		req.Header.Set("X-OSH-Huifu-Proxy-Token", c.proxyToken)
	}
	client := c.httpClient
	if client == nil {
		client = &http.Client{}
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", fmt.Errorf("汇付网关 HTTP %d", response.StatusCode)
	}
	rawBytes, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(rawBytes) > 1024*1024 {
		return nil, "", errors.New("汇付响应过大")
	}
	var root struct {
		Data map[string]any `json:"data"`
		Sign string         `json:"sign"`
	}
	if err = json.Unmarshal(rawBytes, &root); err != nil || root.Data == nil || root.Sign == "" {
		return nil, "", errors.New("汇付响应格式错误")
	}
	if !verifyHuifuData(root.Data, root.Sign, c.publicKey) {
		return nil, "", errors.New("汇付响应验签失败")
	}
	return root.Data, string(rawBytes), nil
}

func huifuResultFromResponse(data map[string]any, raw, fallbackPaymentNo string) (huifuPaymentResult, error) {
	amount, err := paymentAmountCents(firstValue(data, "trans_amt", "pay_amt"))
	if err != nil {
		return huifuPaymentResult{}, err
	}
	return huifuPaymentResult{
		PaymentNo:         firstValue(data, "org_req_seq_id", "req_seq_id", "mer_priv"),
		PlatformTradeNo:   firstValue(data, "org_hf_seq_id", "hf_seq_id", "pre_order_id"),
		TransactionStatus: firstValue(data, "trans_stat"),
		CloseStatus:       firstValue(data, "close_stat"),
		OrderStatus:       firstValue(data, "order_stat"),
		TradeType:         firstValue(data, "pay_type", "trans_type", "trade_type"),
		AmountCents:       amount,
		JumpURL:           firstValue(data, "qr_code", "jump_url", "pay_info"),
		ResponsePayload:   raw,
		// Notifications normally contain org_req_seq_id; query responses do too.
		// Keep the caller's payment number as a final fallback for older gateways.
	}.withPaymentFallback(fallbackPaymentNo), nil
}

func (r huifuPaymentResult) withPaymentFallback(fallback string) huifuPaymentResult {
	if r.PaymentNo == "" {
		r.PaymentNo = fallback
	}
	return r
}

func paymentDate(paymentNo string) string {
	if len(paymentNo) >= 9 && strings.HasPrefix(paymentNo, "P") {
		return paymentNo[1:9]
	}
	return time.Now().UTC().Format("20060102")
}

func paymentAmountCents(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	rat, ok := new(big.Rat).SetString(value)
	if !ok {
		return 0, errors.New("汇付金额格式错误")
	}
	rat.Mul(rat, big.NewRat(100, 1))
	if !rat.IsInt() || !rat.Num().IsInt64() {
		return 0, errors.New("汇付金额精度错误")
	}
	return rat.Num().Int64(), nil
}

func signHuifuData(data map[string]any, private *rsa.PrivateKey) (string, error) {
	if private == nil {
		return "", errors.New("汇付私钥未配置")
	}
	raw, err := canonicalJSON(data)
	if err != nil {
		return "", err
	}
	return signHuifuRaw(string(raw), private)
}

func signHuifuRaw(raw string, private *rsa.PrivateKey) (string, error) {
	if private == nil || raw == "" {
		return "", errors.New("汇付签名参数不完整")
	}
	hash := sha256.Sum256([]byte(raw))
	signature, err := rsa.SignPKCS1v15(rand.Reader, private, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(signature), nil
}

func verifyHuifuData(data map[string]any, signature string, public *rsa.PublicKey) bool {
	raw, err := canonicalJSON(data)
	if err != nil {
		return false
	}
	return verifyHuifuRaw(string(raw), signature, public)
}

func verifyHuifuRaw(raw, signature string, public *rsa.PublicKey) bool {
	if public == nil || raw == "" || signature == "" {
		return false
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	hash := sha256.Sum256([]byte(raw))
	return rsa.VerifyPKCS1v15(public, crypto.SHA256, hash[:], signatureBytes) == nil
}

func canonicalJSON(data map[string]any) ([]byte, error) {
	clean := make(map[string]any, len(data))
	for key, value := range data {
		if value != nil {
			clean[key] = value
		}
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(clean); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func firstValue(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(data[key]); value != "" {
			return value
		}
	}
	return ""
}

func truncatePaymentSubject(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "OSH灵感库商品"
	}
	runes := []rune(value)
	if len(runes) > 40 {
		return string(runes[:40])
	}
	return value
}

// contextWithTimeout is kept local so payment code remains independent from
// the request context used by handlers and can be reused by polling.
func contextWithTimeout(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}
