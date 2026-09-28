package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type passwordChangeRequest struct {
	OldPassword     string `json:"old_password"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
}

type emailChangeRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

type deleteAccountRequest struct {
	Password string `json:"password"`
}

type adminUserRequest struct {
	UserID int64  `json:"user_id"`
	Reason string `json:"reason"`
}

type orderVO struct {
	ID          int64   `json:"id"`
	OrderNo     string  `json:"orderNo"`
	OrderType   string  `json:"orderType"`
	Subject     string  `json:"subject"`
	AmountCents int64   `json:"amountCents"`
	Points      Points  `json:"points"`
	PlanCode    *string `json:"planCode,omitempty"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"createdAt"`
}

func (s *server) userProfile(w http.ResponseWriter, r *http.Request, user *currentUser) {
	balance, _ := s.loadPointBalance(user.ID)
	jsonResponse(w, map[string]any{
		"user":           user,
		"balance":        balance,
		"memberStatus":   s.loadMemberStatus(user.ID),
		"quotaPoints":    balance.GrantPoints,
		"rechargePoints": balance.PaidPoints,
	})
}

func (s *server) changePassword(w http.ResponseWriter, r *http.Request, user *currentUser) {
	var req passwordChangeRequest
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	if len(req.NewPassword) < 8 || len(req.NewPassword) > 128 {
		apiError(w, http.StatusBadRequest, "新密码长度需为 8-128 位")
		return
	}
	if req.NewPassword != req.ConfirmPassword {
		apiError(w, http.StatusBadRequest, "两次输入的密码不一致")
		return
	}
	var stored string
	if err := s.appDB.QueryRow(`select password_hash from users where id = ? and delete_flag = 0`, user.ID).Scan(&stored); err != nil {
		apiError(w, http.StatusInternalServerError, "密码修改失败")
		return
	}
	if !verifyPassword(req.OldPassword, stored) {
		apiError(w, http.StatusBadRequest, "原密码错误")
		return
	}
	hash, err := hashPassword(req.NewPassword)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "密码处理失败")
		return
	}
	if _, err = s.appDB.Exec(`update users set password_hash = ?, updated_at = ? where id = ? and delete_flag = 0`,
		hash, appNow(), user.ID); err != nil {
		apiError(w, http.StatusInternalServerError, "密码修改失败")
		return
	}
	jsonResponse(w, map[string]any{"ok": true})
}

func (s *server) changeEmail(w http.ResponseWriter, r *http.Request, user *currentUser) {
	var req emailChangeRequest
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !emailPattern.MatchString(email) {
		apiError(w, http.StatusBadRequest, "邮箱格式不正确")
		return
	}
	var exists int
	if err := s.appDB.QueryRow(`select count(*) from users where lower(email) = ? and id <> ? and delete_flag = 0`,
		email, user.ID).Scan(&exists); err != nil {
		apiError(w, http.StatusInternalServerError, "邮箱修改失败")
		return
	}
	if exists > 0 {
		apiError(w, http.StatusConflict, "邮箱已被使用")
		return
	}
	if err := verifyEmailCode(s.appDB, email, "change_email", req.Code); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	tx, err := s.appDB.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "邮箱修改失败")
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`update users set email = ?, updated_at = ? where id = ? and delete_flag = 0`, email, appNow(), user.ID); err != nil {
		apiError(w, http.StatusInternalServerError, "邮箱修改失败")
		return
	}
	if err = markEmailCodeUsedTx(tx, email, "change_email", req.Code); err != nil {
		apiError(w, http.StatusBadRequest, "验证码错误或已过期")
		return
	}
	if err = tx.Commit(); err != nil {
		apiError(w, http.StatusInternalServerError, "邮箱修改失败")
		return
	}
	jsonResponse(w, map[string]any{"ok": true})
}

func (s *server) deleteAccount(w http.ResponseWriter, r *http.Request, user *currentUser) {
	var req deleteAccountRequest
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	var stored string
	if err := s.appDB.QueryRow(`select password_hash from users where id = ?`, user.ID).Scan(&stored); err != nil {
		apiError(w, http.StatusInternalServerError, "注销失败")
		return
	}
	if !verifyPassword(req.Password, stored) {
		apiError(w, http.StatusBadRequest, "密码错误")
		return
	}
	if err := s.softDeleteUser(r.Context(), user.ID); err != nil {
		apiError(w, http.StatusInternalServerError, "注销失败")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecureRequest(r)})
	jsonResponse(w, map[string]any{"ok": true})
}

func (s *server) orderRecords(w http.ResponseWriter, r *http.Request, user *currentUser) {
	orders, err := s.loadOrders(user.ID, 80)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "订单记录读取失败")
		return
	}
	jsonResponse(w, map[string]any{"orders": orders})
}

func (s *server) adminUsers(w http.ResponseWriter, r *http.Request, admin *currentUser) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	role := strings.TrimSpace(r.URL.Query().Get("role"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	clauses := []string{"u.delete_flag = 0"}
	args := []any{}
	if query != "" {
		clauses = append(clauses, "(lower(u.username) like ? or lower(u.email) like ? or cast(u.id as text) = ?)")
		args = append(args, "%"+query+"%", "%"+query+"%", query)
	}
	if role == roleNormal || role == roleVIP || role == roleAdmin {
		clauses = append(clauses, "u.role_code = ?")
		args = append(args, role)
	}
	if status == "active" {
		clauses = append(clauses, "u.status = 0")
	} else if status == "blocked" {
		clauses = append(clauses, "u.status = 1")
	}
	rows, err := s.appDB.Query(`select u.id, u.username, u.email, u.status, u.role_code, u.role_level,
			u.vip_expire_time, coalesce(a.total_points, 0), coalesce(a.grant_points, 0), coalesce(a.paid_points, 0), u.created_at
		from users u
		left join point_accounts a on a.user_id = u.id
		where `+strings.Join(clauses, " and ")+`
		order by u.id desc
		limit 100`, args...)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "用户列表读取失败")
		return
	}
	defer rows.Close()
	users := []currentUser{}
	for rows.Next() {
		var user currentUser
		var vipExpire sql.NullString
		if err = rows.Scan(&user.ID, &user.Username, &user.Email, &user.Status, &user.RoleCode, &user.Level,
			&vipExpire, &user.TotalPoints, &user.GrantPoints, &user.PaidPoints, &user.CreatedAt); err != nil {
			apiError(w, http.StatusInternalServerError, "用户列表读取失败")
			return
		}
		if vipExpire.Valid {
			user.VIPExpireTime = &vipExpire.String
		}
		users = append(users, user)
	}
	jsonResponse(w, map[string]any{"users": users, "operator": admin.ID})
}

func (s *server) adminUserDetail(w http.ResponseWriter, r *http.Request, admin *currentUser) {
	idText := strings.TrimPrefix(r.URL.Path, "/api/admin/users/")
	userID, err := strconv.ParseInt(strings.Trim(idText, "/"), 10, 64)
	if err != nil || userID <= 0 {
		apiError(w, http.StatusBadRequest, "用户 ID 不正确")
		return
	}
	user, err := s.adminLoadUser(userID)
	if err != nil {
		apiError(w, http.StatusNotFound, "用户不存在")
		return
	}
	orders, _ := s.loadOrders(userID, 50)
	records, _ := s.loadPointRecords(userID, 50)
	jsonResponse(w, map[string]any{"user": user, "memberStatus": s.loadMemberStatus(userID), "orders": orders, "pointRecords": records, "operator": admin.ID})
}

func (s *server) adminBlockUser(w http.ResponseWriter, r *http.Request, admin *currentUser) {
	s.setUserBlocked(w, r, admin, true)
}

func (s *server) adminUnblockUser(w http.ResponseWriter, r *http.Request, admin *currentUser) {
	s.setUserBlocked(w, r, admin, false)
}

func (s *server) setUserBlocked(w http.ResponseWriter, r *http.Request, admin *currentUser, blocked bool) {
	var req adminUserRequest
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	if req.UserID <= 0 {
		apiError(w, http.StatusBadRequest, "用户 ID 不正确")
		return
	}
	if req.UserID == admin.ID && blocked {
		apiError(w, http.StatusBadRequest, "不能拉黑自己")
		return
	}
	status := 0
	reason := any(nil)
	if blocked {
		status = 1
		reason = strings.TrimSpace(req.Reason)
	}
	result, err := s.appDB.Exec(`update users set status = ?, blocked_reason = ?, updated_at = ? where id = ? and delete_flag = 0`,
		status, reason, appNow(), req.UserID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "用户状态更新失败")
		return
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		apiError(w, http.StatusNotFound, "用户不存在")
		return
	}
	if blocked {
		_, _ = s.appDB.Exec(`update user_sessions set revoked_at = ?, updated_at = ? where user_id = ? and revoked_at is null`,
			appNow(), appNow(), req.UserID)
	}
	jsonResponse(w, map[string]any{"ok": true})
}

func (s *server) softDeleteUser(ctx context.Context, userID int64) error {
	tx, err := s.appDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := appNow()
	result, err := tx.Exec(`update users set delete_flag = 1, deleted_at = ?, updated_at = ? where id = ? and delete_flag = 0`, now, now, userID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	if _, err = tx.Exec(`update user_sessions set revoked_at = ?, updated_at = ? where user_id = ? and revoked_at is null`, now, now, userID); err != nil {
		return err
	}
	if _, err = tx.Exec(`update email_verification_codes set used_at = ? where email = (select email from users where id = ?) and used_at is null`, now, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *server) adminLoadUser(userID int64) (*currentUser, error) {
	var user currentUser
	var vipExpire sql.NullString
	err := s.appDB.QueryRow(`select u.id, u.username, u.email, u.status, u.role_code, u.role_level,
			u.vip_expire_time, coalesce(a.total_points, 0), coalesce(a.grant_points, 0), coalesce(a.paid_points, 0), u.created_at
		from users u
		left join point_accounts a on a.user_id = u.id
		where u.id = ? and u.delete_flag = 0`, userID).Scan(&user.ID, &user.Username, &user.Email, &user.Status,
		&user.RoleCode, &user.Level, &vipExpire, &user.TotalPoints, &user.GrantPoints, &user.PaidPoints, &user.CreatedAt)
	user.QuotaPoints = user.GrantPoints
	user.RechargePoints = user.PaidPoints
	if vipExpire.Valid {
		user.VIPExpireTime = &vipExpire.String
	}
	return &user, err
}

func (s *server) loadOrders(userID int64, limit int) ([]orderVO, error) {
	rows, err := s.appDB.Query(`select id, order_no, order_type, subject, amount_cents, points, plan_code, status, created_at
		from orders where user_id = ? order by id desc limit ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []orderVO{}
	for rows.Next() {
		var order orderVO
		var plan sql.NullString
		if err = rows.Scan(&order.ID, &order.OrderNo, &order.OrderType, &order.Subject, &order.AmountCents,
			&order.Points, &plan, &order.Status, &order.CreatedAt); err != nil {
			return nil, err
		}
		if plan.Valid {
			order.PlanCode = &plan.String
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (s *server) loadPointRecords(userID int64, limit int) ([]pointRecordVO, error) {
	rows, err := s.appDB.Query(`select id, change_type, point_type, biz_type, biz_no,
			change_amount, grant_amount, paid_amount, before_balance, after_balance,
			lot_expire_at, remark, created_at
		from point_records where user_id = ? order by id desc limit ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []pointRecordVO{}
	for rows.Next() {
		var record pointRecordVO
		var expire, remark sql.NullString
		if err = rows.Scan(&record.ID, &record.ChangeType, &record.PointType, &record.BizType, &record.BizNo,
			&record.ChangeAmount, &record.GrantAmount, &record.PaidAmount, &record.BeforeBalance,
			&record.AfterBalance, &expire, &remark, &record.CreatedAt); err != nil {
			return nil, err
		}
		if expire.Valid {
			record.LotExpireAt = &expire.String
		}
		if remark.Valid {
			record.Remark = &remark.String
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func createOrderTx(tx *sql.Tx, userID int64, orderType, subject string, amountCents, points int64, planCode any, sourceType, sourceNo string) error {
	now := appNow()
	orderNo := fmt.Sprintf("%s:%d:%d", orderType, userID, time.Now().UnixNano())
	_, err := tx.Exec(`insert into orders
		(user_id, order_no, order_type, subject, amount_cents, points, plan_code, status, source_type, source_no, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, 'success', ?, ?, ?, ?)`,
		userID, orderNo, orderType, subject, amountCents, points, planCode, sourceType, sourceNo, now, now)
	return err
}
