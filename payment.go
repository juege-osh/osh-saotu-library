package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type pointPackageVO struct {
	PackageCode string `json:"packageCode"`
	PackageName string `json:"packageName"`
	Points      Points `json:"points"`
	PriceCents  int64  `json:"priceCents"`
	Price       string `json:"price"`
}

type paymentPurchaseRequest struct {
	OrderType   string `json:"order_type"`
	PlanCode    string `json:"plan_code"`
	PackageCode string `json:"package_code"`
	Channel     string `json:"channel"`
}

type paymentOrderVO struct {
	ID              int64   `json:"id"`
	OrderNo         string  `json:"orderNo"`
	PaymentNo       string  `json:"paymentNo"`
	OrderType       string  `json:"orderType"`
	Subject         string  `json:"subject"`
	AmountCents     int64   `json:"amountCents"`
	Amount          string  `json:"amount"`
	Points          Points  `json:"points"`
	PlanCode        *string `json:"planCode,omitempty"`
	Status          string  `json:"status"`
	Provider        string  `json:"provider"`
	Channel         string  `json:"channel"`
	PlatformTradeNo *string `json:"platformTradeNo,omitempty"`
	PayURL          *string `json:"payUrl,omitempty"`
	QRCode          *string `json:"qrcode,omitempty"`
	ExpireAt        *string `json:"expireAt,omitempty"`
	PaidAt          *string `json:"paidAt,omitempty"`
	CreatedAt       string  `json:"createdAt"`
}

type paymentRow struct {
	PaymentNo   string
	OrderNo     string
	Provider    string
	Channel     string
	AmountCents int64
	Status      string
	OrderType   string
	UserID      int64
}

type pointPackageRecord struct {
	code   string
	name   string
	points int64
	price  int64
}

type adminPaymentPricesVO struct {
	Plans            []memberPlanVO   `json:"plans"`
	RechargePackages []pointPackageVO `json:"rechargePackages"`
}

type adminPaymentPriceRequest struct {
	Kind       string `json:"kind"`
	Code       string `json:"code"`
	PriceCents int64  `json:"price_cents"`
}

func (s *server) paymentProducts(w http.ResponseWriter, r *http.Request) {
	plans, err := s.loadMemberPlans()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "会员套餐读取失败")
		return
	}
	packages, err := s.loadPointPackages()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "充值套餐读取失败")
		return
	}
	jsonResponse(w, map[string]any{
		"provider":         s.paymentProvider(),
		"plans":            plans,
		"rechargePackages": packages,
	})
}

func (s *server) adminPaymentPrices(w http.ResponseWriter, r *http.Request, _ *currentUser) {
	if r.Method == http.MethodGet {
		catalog, err := s.loadAdminPaymentPrices()
		if err != nil {
			apiError(w, http.StatusInternalServerError, "支付价格读取失败")
			return
		}
		jsonResponse(w, catalog)
		return
	}
	if !sameOriginJSON(w, r) {
		return
	}
	var req adminPaymentPriceRequest
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	req.Kind = strings.TrimSpace(req.Kind)
	req.Code = strings.TrimSpace(req.Code)
	if (req.Kind != "plan" && req.Kind != "package") || req.Code == "" || req.PriceCents < 1 || req.PriceCents > 100000000 {
		apiError(w, http.StatusBadRequest, "价格须在 0.01 元到 1000000 元之间")
		return
	}

	tx, err := s.appDB.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "支付价格保存失败")
		return
	}
	defer tx.Rollback()
	var exists int
	var result sql.Result
	switch req.Kind {
	case "plan":
		err = tx.QueryRow(`select 1 from member_plans where plan_code=? and status=1`, req.Code).Scan(&exists)
		if err == sql.ErrNoRows {
			apiError(w, http.StatusNotFound, "支付套餐不存在或已停用")
			return
		}
		if err != nil {
			apiError(w, http.StatusInternalServerError, "支付价格保存失败")
			return
		}
		result, err = tx.Exec(`update member_plans set price_cents=?, updated_at=? where plan_code=? and status=1`, req.PriceCents, appNow(), req.Code)
	case "package":
		err = tx.QueryRow(`select 1 from point_packages where package_code=? and status=1`, req.Code).Scan(&exists)
		if err == sql.ErrNoRows {
			apiError(w, http.StatusNotFound, "支付套餐不存在或已停用")
			return
		}
		if err != nil {
			apiError(w, http.StatusInternalServerError, "支付价格保存失败")
			return
		}
		result, err = tx.Exec(`update point_packages set price_cents=?, updated_at=? where package_code=? and status=1`, req.PriceCents, appNow(), req.Code)
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "支付价格保存失败")
		return
	}
	if _, err = result.RowsAffected(); err != nil {
		apiError(w, http.StatusInternalServerError, "支付价格保存失败")
		return
	}
	if err = tx.Commit(); err != nil {
		apiError(w, http.StatusInternalServerError, "支付价格保存失败")
		return
	}
	catalog, err := s.loadAdminPaymentPrices()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "支付价格读取失败")
		return
	}
	jsonResponse(w, catalog)
}

func (s *server) loadAdminPaymentPrices() (adminPaymentPricesVO, error) {
	plans, err := s.loadMemberPlans()
	if err != nil {
		return adminPaymentPricesVO{}, err
	}
	packages, err := s.loadPointPackages()
	if err != nil {
		return adminPaymentPricesVO{}, err
	}
	return adminPaymentPricesVO{Plans: plans, RechargePackages: packages}, nil
}

func (s *server) loadPointPackages() ([]pointPackageVO, error) {
	rows, err := s.appDB.Query(`select package_code, package_name, points, price_cents
		from point_packages where status=1 order by sort asc, id asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	packages := []pointPackageVO{}
	for rows.Next() {
		var code, name string
		var points, price int64
		if err = rows.Scan(&code, &name, &points, &price); err != nil {
			return nil, err
		}
		packages = append(packages, pointPackageVO{
			PackageCode: code,
			PackageName: name,
			Points:      Points(points),
			PriceCents:  price,
			Price:       fmt.Sprintf("%.2f", float64(price)/100),
		})
	}
	return packages, rows.Err()
}

func (s *server) loadPointPackage(code string) (pointPackageRecord, error) {
	var pkg pointPackageRecord
	err := s.appDB.QueryRow(`select package_code, package_name, points, price_cents
		from point_packages where package_code=? and status=1 limit 1`, code).
		Scan(&pkg.code, &pkg.name, &pkg.points, &pkg.price)
	return pkg, err
}

func (s *server) paymentProvider() string {
	provider := strings.ToLower(strings.TrimSpace(s.payProvider))
	if provider == "" {
		return "disabled"
	}
	return provider
}

func paymentProviderErrorMessage(err error) string {
	if err == nil {
		return "支付平台下单失败，请稍后查询订单状态"
	}
	detail := strings.TrimSpace(err.Error())
	if strings.HasPrefix(detail, "汇付下单失败:") {
		return "汇付拒绝支付请求：" + strings.TrimSpace(strings.TrimPrefix(detail, "汇付下单失败:"))
	}
	if strings.Contains(detail, "汇付配置") {
		return "汇付支付配置不完整，请联系管理员"
	}
	return "支付平台暂时不可达，请稍后查询订单状态"
}

func (s *server) createPaymentOrder(w http.ResponseWriter, r *http.Request, user *currentUser) {
	if !sameOriginJSON(w, r) {
		return
	}
	var req paymentPurchaseRequest
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	req.OrderType = strings.TrimSpace(req.OrderType)
	req.PlanCode = strings.TrimSpace(req.PlanCode)
	req.PackageCode = strings.TrimSpace(req.PackageCode)
	req.Channel = strings.ToLower(strings.TrimSpace(req.Channel))
	if req.Channel == "" {
		req.Channel = "alipay"
	}
	if req.Channel != "alipay" && req.Channel != "wechat" {
		apiError(w, http.StatusBadRequest, "暂不支持该支付方式")
		return
	}

	var subject string
	var amountCents, points int64
	var planCode any
	if req.OrderType == "subscription" && req.PlanCode != "" && req.PackageCode == "" {
		plan, err := s.loadPlan(req.PlanCode)
		if err != nil {
			apiError(w, http.StatusBadRequest, "会员套餐不存在")
			return
		}
		subject, amountCents, points, planCode = plan.name, int64(plan.priceCents), int64(plan.monthlyPoints), plan.code
	} else if req.OrderType == "recharge" && req.PackageCode != "" && req.PlanCode == "" {
		pkg, err := s.loadPointPackage(req.PackageCode)
		if err != nil {
			apiError(w, http.StatusBadRequest, "充值套餐不存在")
			return
		}
		subject, amountCents, points = pkg.name, pkg.price, pkg.points
	} else {
		apiError(w, http.StatusBadRequest, "请选择一个有效的会员或充值套餐")
		return
	}
	if amountCents < 1 {
		apiError(w, http.StatusBadRequest, "支付金额不正确")
		return
	}
	provider := s.paymentProvider()
	if provider != "mock" && provider != "huifu" {
		apiError(w, http.StatusServiceUnavailable, "当前支付提供方未配置")
		return
	}
	if provider == "huifu" && s.huifu == nil {
		apiError(w, http.StatusServiceUnavailable, "汇付支付配置不完整")
		return
	}

	now := time.Now().UTC()
	orderNo := newPaymentNumber("PAY")
	paymentNo := newPaymentNumber("P")
	expireAt := now.Add(30 * time.Minute)
	tx, err := s.appDB.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "创建支付订单失败")
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`insert into orders
		(user_id, order_no, order_type, subject, amount_cents, points, plan_code, status, source_type, source_no, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, 'pending', 'payment', ?, ?, ?)`,
		user.ID, orderNo, req.OrderType, subject, amountCents, points, planCode, paymentNo, appTime(now), appTime(now)); err != nil {
		apiError(w, http.StatusInternalServerError, "创建支付订单失败")
		return
	}
	requestPayload := fmt.Sprintf(`{"order_type":%q,"channel":%q}`, req.OrderType, req.Channel)
	if _, err = tx.Exec(`insert into payments
		(order_no, payment_no, provider, channel, amount_cents, status, request_payload, expire_at, created_at, updated_at)
		values (?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?)`,
		orderNo, paymentNo, provider, req.Channel, amountCents, requestPayload, appTime(expireAt), appTime(now), appTime(now)); err != nil {
		apiError(w, http.StatusInternalServerError, "创建支付流水失败")
		return
	}
	if err = tx.Commit(); err != nil {
		apiError(w, http.StatusInternalServerError, "创建支付订单失败")
		return
	}

	if provider == "mock" {
		mockURL := "mock://payment/" + orderNo
		_, _ = s.appDB.Exec(`update payments set pay_url=?, qrcode=?, updated_at=? where payment_no=?`, mockURL, mockURL, appNow(), paymentNo)
	} else {
		result, providerErr := s.huifu.CreatePayment(paymentNo, subject, amountCents, req.Channel, expireAt)
		if providerErr != nil {
			log.Printf("汇付预下单失败 payment_no=%s error=%v", paymentNo, providerErr)
			// A transport timeout leaves the local order pending so status/query can reconcile it.
			apiError(w, http.StatusBadGateway, paymentProviderErrorMessage(providerErr))
			return
		}
		_, _ = s.appDB.Exec(`update payments set platform_trade_no=?, pay_url=?, qrcode=?, response_payload=?, updated_at=? where payment_no=?`,
			result.PlatformTradeNo, result.JumpURL, result.JumpURL, result.ResponsePayload, appNow(), paymentNo)
	}
	order, err := s.loadPaymentOrder(user.ID, orderNo)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "支付订单读取失败")
		return
	}
	jsonResponse(w, map[string]any{"order": order})
}

func newPaymentNumber(prefix string) string {
	token, err := randomToken(18)
	if err != nil {
		token = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return prefix + time.Now().UTC().Format("20060102") + token
}

func (s *server) paymentStatus(w http.ResponseWriter, r *http.Request, user *currentUser) {
	orderNo := strings.TrimSpace(r.PathValue("orderNo"))
	order, err := s.loadPaymentOrder(user.ID, orderNo)
	if err != nil {
		apiError(w, http.StatusNotFound, "支付订单不存在")
		return
	}
	if order.Status == "pending" && order.Provider == "huifu" && s.huifu != nil {
		if result, queryErr := s.huifu.QueryPayment(order.PaymentNo, order.AmountCents); queryErr == nil {
			switch result.TransactionStatus {
			case "S":
				_, _ = s.settlePayment(order.PaymentNo, "huifu", result.PlatformTradeNo, order.AmountCents, result.TradeType, result.ResponsePayload, "", true)
			case "F":
				_, _ = s.failPayment(order.PaymentNo, "huifu", result.ResponsePayload, "", true)
			}
			if result.TransactionStatus != "S" && result.TransactionStatus != "F" && result.CloseStatus == "S" {
				_ = s.markPaymentClosed(order.PaymentNo)
			}
			order, _ = s.loadPaymentOrder(user.ID, orderNo)
		}
	}
	jsonResponse(w, map[string]any{"order": order})
}

func (s *server) cancelPayment(w http.ResponseWriter, r *http.Request, user *currentUser) {
	if !sameOriginJSON(w, r) {
		return
	}
	orderNo := strings.TrimSpace(r.PathValue("orderNo"))
	order, err := s.loadPaymentOrder(user.ID, orderNo)
	if err != nil {
		apiError(w, http.StatusNotFound, "支付订单不存在")
		return
	}
	if order.Status != "pending" {
		jsonResponse(w, map[string]any{"order": order})
		return
	}
	if order.Provider == "huifu" {
		if s.huifu == nil {
			apiError(w, http.StatusServiceUnavailable, "汇付支付配置不完整")
			return
		}
		result, closeErr := s.huifu.ClosePayment(order.PaymentNo)
		if closeErr != nil || result.CloseStatus != "S" {
			apiError(w, http.StatusBadGateway, "支付平台关单失败，请稍后重试")
			return
		}
	}
	if err = s.markPaymentClosed(order.PaymentNo); err != nil {
		apiError(w, http.StatusInternalServerError, "取消支付失败")
		return
	}
	order, _ = s.loadPaymentOrder(user.ID, orderNo)
	jsonResponse(w, map[string]any{"order": order})
}

func (s *server) mockPaymentSuccess(w http.ResponseWriter, r *http.Request, user *currentUser) {
	if !sameOriginJSON(w, r) {
		return
	}
	if s.paymentProvider() != "mock" {
		apiError(w, http.StatusNotFound, "本地模拟支付未启用")
		return
	}
	orderNo := strings.TrimSpace(r.PathValue("orderNo"))
	order, err := s.loadPaymentOrder(user.ID, orderNo)
	if err != nil {
		apiError(w, http.StatusNotFound, "支付订单不存在")
		return
	}
	if order.Status == "success" {
		jsonResponse(w, map[string]any{"order": order})
		return
	}
	if order.Status != "pending" {
		apiError(w, http.StatusConflict, "支付订单已结束")
		return
	}
	if _, err = s.settlePayment(order.PaymentNo, "mock", "MOCK-"+order.PaymentNo, order.AmountCents, "mock", "", "", true); err != nil {
		apiError(w, http.StatusInternalServerError, "模拟支付结算失败")
		return
	}
	order, _ = s.loadPaymentOrder(user.ID, orderNo)
	jsonResponse(w, map[string]any{"order": order})
}

func (s *server) huifuNotify(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	if err := r.ParseForm(); err != nil {
		writePaymentAck(w, http.StatusBadRequest, "FAIL")
		return
	}
	signature := r.Form.Get("sign")
	rawData := r.Form.Get("resp_data")
	// 汇付在保存 Webhook 前会发送一条无签名探测请求，要求在 2 秒内返回
	// 纯文本 success。正式通知仍必须经过签名校验后才会进入结算流程。
	if signature == "" && rawData == "" {
		writePaymentAck(w, http.StatusOK, "success")
		return
	}
	if s.huifu == nil || signature == "" || rawData == "" {
		writePaymentAck(w, http.StatusBadRequest, "FAIL")
		return
	}
	result, err := s.huifu.VerifyNotification(rawData, signature)
	if err != nil {
		writePaymentAck(w, http.StatusBadRequest, "FAIL")
		return
	}
	key := notifyKey("huifu", result.PaymentNo, result.PlatformTradeNo, result.TransactionStatus, rawData)
	if result.TransactionStatus == "S" {
		if _, err = s.settlePayment(result.PaymentNo, "huifu", result.PlatformTradeNo, result.AmountCents, result.TradeType, result.ResponsePayload, key, true); err != nil {
			writePaymentAck(w, http.StatusInternalServerError, "FAIL")
			return
		}
	} else if result.TransactionStatus == "F" {
		if _, err = s.failPayment(result.PaymentNo, "huifu", result.ResponsePayload, key, true); err != nil {
			writePaymentAck(w, http.StatusInternalServerError, "FAIL")
			return
		}
	} else {
		writePaymentAck(w, http.StatusBadRequest, "FAIL")
		return
	}
	writePaymentAck(w, http.StatusOK, "RECV_ORD_ID_"+result.PaymentNo)
}

func writePaymentAck(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (s *server) loadPaymentOrder(userID int64, orderNo string) (*paymentOrderVO, error) {
	var order paymentOrderVO
	var plan, platform, payURL, qrcode, expireAt, paidAt sql.NullString
	err := s.appDB.QueryRow(`select o.id,o.order_no,o.order_type,o.subject,o.amount_cents,o.points,o.plan_code,o.status,
		o.created_at,p.payment_no,p.provider,p.channel,p.platform_trade_no,p.pay_url,p.qrcode,p.expire_at,p.paid_at
		from orders o join payments p on p.order_no=o.order_no
		where o.user_id=? and o.order_no=? limit 1`, userID, orderNo).Scan(
		&order.ID, &order.OrderNo, &order.OrderType, &order.Subject, &order.AmountCents, &order.Points, &plan,
		&order.Status, &order.CreatedAt, &order.PaymentNo, &order.Provider, &order.Channel, &platform, &payURL, &qrcode, &expireAt, &paidAt)
	if err != nil {
		return nil, err
	}
	order.Amount = fmt.Sprintf("%.2f", float64(order.AmountCents)/100)
	if plan.Valid {
		order.PlanCode = &plan.String
	}
	if platform.Valid {
		order.PlatformTradeNo = &platform.String
	}
	if payURL.Valid {
		order.PayURL = &payURL.String
	}
	if qrcode.Valid {
		order.QRCode = &qrcode.String
	}
	if expireAt.Valid {
		order.ExpireAt = &expireAt.String
	}
	if paidAt.Valid {
		order.PaidAt = &paidAt.String
	}
	return &order, nil
}

func (s *server) loadPaymentRowTx(tx *sql.Tx, paymentNo string) (paymentRow, error) {
	var row paymentRow
	err := tx.QueryRow(`select p.payment_no,p.order_no,p.provider,p.channel,p.amount_cents,p.status,o.order_type,o.user_id
		from payments p join orders o on o.order_no=p.order_no where p.payment_no=? limit 1`, paymentNo).
		Scan(&row.PaymentNo, &row.OrderNo, &row.Provider, &row.Channel, &row.AmountCents, &row.Status, &row.OrderType, &row.UserID)
	return row, err
}

func (s *server) settlePayment(paymentNo, provider, platformTradeNo string, amountCents int64, channel, responsePayload, notifyKeyValue string, signatureValid bool) (bool, error) {
	tx, err := s.appDB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	row, err := s.loadPaymentRowTx(tx, paymentNo)
	if err != nil {
		return false, err
	}
	if row.Provider != provider || row.AmountCents != amountCents {
		return false, fmt.Errorf("payment verification mismatch")
	}
	if row.Status == "success" {
		if notifyKeyValue != "" {
			_ = insertPaymentNotifyTx(tx, provider, row, platformTradeNo, notifyKeyValue, responsePayload, signatureValid, "S", "ignored", "重复支付通知")
		}
		return true, tx.Commit()
	}
	if row.Status != "pending" {
		return false, fmt.Errorf("payment is not pending")
	}
	if row.OrderType == "subscription" {
		var planCode string
		if err = tx.QueryRow(`select plan_code from orders where order_no=?`, row.OrderNo).Scan(&planCode); err != nil {
			return false, err
		}
		if _, err = s.grantMembershipEntitlementTx(tx, row.UserID, planCode, "payment", row.OrderNo); err != nil {
			return false, err
		}
	} else if row.OrderType == "recharge" {
		var points int64
		if err = tx.QueryRow(`select points from orders where order_no=?`, row.OrderNo).Scan(&points); err != nil {
			return false, err
		}
		if _, _, _, err = grantPointsTx(tx, row.UserID, points, "paid", "payment_recharge", row.OrderNo, nil, "充值积分"); err != nil {
			return false, err
		}
	} else {
		return false, fmt.Errorf("unsupported payment order type")
	}
	now := appNow()
	if _, err = tx.Exec(`update payments set status='success', platform_trade_no=?, channel=coalesce(?,channel), response_payload=?, paid_at=?, updated_at=? where payment_no=? and status='pending'`, platformTradeNo, channel, responsePayload, now, now, paymentNo); err != nil {
		return false, err
	}
	if _, err = tx.Exec(`update orders set status='success', updated_at=? where order_no=? and status='pending'`, now, row.OrderNo); err != nil {
		return false, err
	}
	if notifyKeyValue != "" {
		if err = insertPaymentNotifyTx(tx, provider, row, platformTradeNo, notifyKeyValue, responsePayload, signatureValid, "S", "success", ""); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (s *server) failPayment(paymentNo, provider, responsePayload, notifyKeyValue string, signatureValid bool) (bool, error) {
	tx, err := s.appDB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	row, err := s.loadPaymentRowTx(tx, paymentNo)
	if err != nil {
		return false, err
	}
	if row.Provider != provider {
		return false, fmt.Errorf("payment provider mismatch")
	}
	if row.Status == "success" {
		return true, tx.Commit()
	}
	if row.Status != "pending" {
		return true, tx.Commit()
	}
	now := appNow()
	if _, err = tx.Exec(`update payments set status='failed', response_payload=?, updated_at=? where payment_no=? and status='pending'`, responsePayload, now, paymentNo); err != nil {
		return false, err
	}
	if _, err = tx.Exec(`update orders set status='failed', updated_at=? where order_no=? and status='pending'`, now, row.OrderNo); err != nil {
		return false, err
	}
	if notifyKeyValue != "" {
		if err = insertPaymentNotifyTx(tx, provider, row, "", notifyKeyValue, responsePayload, signatureValid, "F", "success", ""); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (s *server) markPaymentClosed(paymentNo string) error {
	tx, err := s.appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	row, err := s.loadPaymentRowTx(tx, paymentNo)
	if err != nil {
		return err
	}
	if row.Status != "pending" {
		return tx.Commit()
	}
	now := appNow()
	if _, err = tx.Exec(`update payments set status='closed', updated_at=? where payment_no=? and status='pending'`, now, paymentNo); err != nil {
		return err
	}
	if _, err = tx.Exec(`update orders set status='closed', updated_at=? where order_no=? and status='pending'`, now, row.OrderNo); err != nil {
		return err
	}
	return tx.Commit()
}

func notifyKey(provider, paymentNo, platformTradeNo, transactionStatus, raw string) string {
	sum := sha256.Sum256([]byte(provider + "|" + paymentNo + "|" + platformTradeNo + "|" + transactionStatus + "|" + raw))
	return hex.EncodeToString(sum[:])
}

func insertPaymentNotifyTx(tx *sql.Tx, provider string, row paymentRow, platformTradeNo, key, raw string, signatureValid bool, transactionStatus, processStatus, errorMessage string) error {
	if len(raw) > 128*1024 {
		raw = raw[:128*1024]
	}
	_, err := tx.Exec(`insert or ignore into payment_notify_logs
		(provider,payment_no,order_no,platform_trade_no,notify_key,raw_payload,signature_valid,transaction_status,process_status,error_msg,created_at)
		values(?,?,?,?,?,?,?,?,?,?,?)`, provider, row.PaymentNo, row.OrderNo, platformTradeNo, key, raw, boolInt(signatureValid), transactionStatus, processStatus, errorMessage, appNow())
	return err
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
