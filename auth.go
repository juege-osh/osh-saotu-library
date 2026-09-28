package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookieName = "prompt_session"
	sessionTTL        = 7 * 24 * time.Hour
	passwordRounds    = 150000
)

var (
	usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_\p{Han}][A-Za-z0-9_\-\p{Han}]{1,31}$`)
	emailPattern    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

type currentUser struct {
	ID             int64   `json:"id"`
	Username       string  `json:"username"`
	Email          string  `json:"email"`
	Status         int     `json:"status"`
	RoleCode       string  `json:"roleCode"`
	Level          int     `json:"level"`
	VIPExpireTime  *string `json:"vipExpireTime,omitempty"`
	TotalPoints    Points  `json:"totalPoints"`
	GrantPoints    Points  `json:"grantPoints"`
	PaidPoints     Points  `json:"paidPoints"`
	QuotaPoints    Points  `json:"quotaPoints"`
	RechargePoints Points  `json:"rechargePoints"`
	CreatedAt      string  `json:"createdAt,omitempty"`
}

type authRequest struct {
	Username        string `json:"username"`
	Email           string `json:"email"`
	Login           string `json:"login"`
	Password        string `json:"password"`
	ConfirmPassword string `json:"confirm_password"`
	Code            string `json:"code"`
	Purpose         string `json:"purpose"`
}

func apiError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": message})
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}

func (s *server) register(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	username := strings.TrimSpace(req.Username)
	email := strings.ToLower(strings.TrimSpace(req.Email))
	password := req.Password
	if !usernamePattern.MatchString(username) {
		apiError(w, http.StatusBadRequest, "用户名需为 2-32 位中英文、数字、下划线或短横线")
		return
	}
	if !emailPattern.MatchString(email) {
		apiError(w, http.StatusBadRequest, "邮箱格式不正确")
		return
	}
	if len(password) < 8 || len(password) > 128 {
		apiError(w, http.StatusBadRequest, "密码长度需为 8-128 位")
		return
	}
	if password != req.ConfirmPassword {
		apiError(w, http.StatusBadRequest, "两次输入的密码不一致")
		return
	}
	if err := verifyEmailCode(s.appDB, email, "register", req.Code); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := hashPassword(password)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "密码处理失败")
		return
	}

	tx, err := s.appDB.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "注册失败")
		return
	}
	defer tx.Rollback()

	roleCode, roleLevel := roleNormal, levelNormal
	now := appNow()
	result, err := tx.Exec(`insert into users
		(username, email, password_hash, status, role_code, role_level, created_at, updated_at)
		values (?, ?, ?, 0, ?, ?, ?, ?)`,
		username, email, hash, roleCode, roleLevel, now, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			apiError(w, http.StatusConflict, "用户名或邮箱已存在")
			return
		}
		apiError(w, http.StatusInternalServerError, "注册失败")
		return
	}
	userID, err := result.LastInsertId()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "注册失败")
		return
	}
	if err = ensurePointAccountTx(tx, userID); err != nil {
		apiError(w, http.StatusInternalServerError, "积分账户初始化失败")
		return
	}
	if err = markEmailCodeUsedTx(tx, email, "register", req.Code); err != nil {
		apiError(w, http.StatusBadRequest, "验证码错误或已过期")
		return
	}
	if _, _, _, err = grantPointsTx(tx, userID, registerGiftPoints, "paid", "register_gift",
		fmt.Sprintf("register:%d", userID), nil, "新用户注册赠送充值积分"); err != nil {
		apiError(w, http.StatusInternalServerError, "注册积分发放失败")
		return
	}
	if err = tx.Commit(); err != nil {
		apiError(w, http.StatusInternalServerError, "注册失败")
		return
	}
	if err = s.createSession(w, r, userID); err != nil {
		apiError(w, http.StatusInternalServerError, "登录态创建失败")
		return
	}
	user, err := s.loadCurrentUserByID(r.Context(), userID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "用户信息读取失败")
		return
	}
	jsonResponse(w, map[string]any{"authenticated": true, "user": user})
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	login := strings.ToLower(strings.TrimSpace(req.Login))
	if login == "" {
		login = strings.ToLower(strings.TrimSpace(req.Username))
	}
	if login == "" || req.Password == "" {
		apiError(w, http.StatusBadRequest, "账号和密码不能为空")
		return
	}
	var userID int64
	var stored string
	var status int
	err := s.appDB.QueryRow(`select id, password_hash, status
		from users
		where delete_flag = 0 and (lower(username) = ? or lower(email) = ?)
		limit 1`, login, login).Scan(&userID, &stored, &status)
	if err == sql.ErrNoRows || !verifyPassword(req.Password, stored) {
		apiError(w, http.StatusUnauthorized, "账号或密码错误")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "登录失败")
		return
	}
	if status != 0 {
		apiError(w, http.StatusForbidden, "账号已被禁用")
		return
	}
	if err = s.createSession(w, r, userID); err != nil {
		apiError(w, http.StatusInternalServerError, "登录态创建失败")
		return
	}
	user, err := s.loadCurrentUserByID(r.Context(), userID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "用户信息读取失败")
		return
	}
	jsonResponse(w, map[string]any{"authenticated": true, "user": user})
}

func (s *server) resetPassword(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !emailPattern.MatchString(email) {
		apiError(w, http.StatusBadRequest, "邮箱格式不正确")
		return
	}
	if len(req.Password) < 8 || len(req.Password) > 128 {
		apiError(w, http.StatusBadRequest, "密码长度需为 8-128 位")
		return
	}
	if req.Password != req.ConfirmPassword {
		apiError(w, http.StatusBadRequest, "两次输入的密码不一致")
		return
	}
	if err := verifyEmailCode(s.appDB, email, "reset_password", req.Code); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := hashPassword(req.Password)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "密码处理失败")
		return
	}
	tx, err := s.appDB.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "重置密码失败")
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec(`update users set password_hash = ?, updated_at = ? where lower(email) = ? and delete_flag = 0 and status = 0`,
		hash, appNow(), email)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "重置密码失败")
		return
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		apiError(w, http.StatusNotFound, "邮箱未注册")
		return
	}
	if err = markEmailCodeUsedTx(tx, email, "reset_password", req.Code); err != nil {
		apiError(w, http.StatusBadRequest, "验证码错误或已过期")
		return
	}
	if _, err = tx.Exec(`update user_sessions set revoked_at = ?, updated_at = ?
		where user_id in (select id from users where lower(email) = ?) and revoked_at is null`,
		appNow(), appNow(), email); err != nil {
		apiError(w, http.StatusInternalServerError, "重置密码失败")
		return
	}
	if err = tx.Commit(); err != nil {
		apiError(w, http.StatusInternalServerError, "重置密码失败")
		return
	}
	jsonResponse(w, map[string]any{"ok": true})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil && cookie.Value != "" {
		_, _ = s.appDB.Exec(`update user_sessions set revoked_at = ?, updated_at = ?
			where session_hash = ? and revoked_at is null`,
			appNow(), appNow(), tokenHash(cookie.Value))
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecureRequest(r),
	})
	jsonResponse(w, map[string]any{"ok": true})
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	s.writeMe(w, r, false)
}

func (s *server) writeMe(w http.ResponseWriter, r *http.Request, require bool) {
	user, err := s.currentUser(r)
	if err != nil {
		if require {
			apiError(w, http.StatusUnauthorized, "请先登录")
			return
		}
		jsonResponse(w, map[string]any{"authenticated": false})
		return
	}
	_ = s.expirePointLots(user.ID)
	user, _ = s.loadCurrentUserByID(r.Context(), user.ID)
	jsonResponse(w, map[string]any{"authenticated": true, "user": user})
}

func (s *server) createSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	token, err := randomToken(32)
	if err != nil {
		return err
	}
	now := time.Now()
	expires := now.Add(sessionTTL)
	_, err = s.appDB.Exec(`insert into user_sessions
		(user_id, session_hash, expires_at, created_at, last_seen_at, updated_at)
		values (?, ?, ?, ?, ?, ?)`,
		userID, tokenHash(token), appTime(expires), appTime(now), appTime(now), appTime(now))
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecureRequest(r),
	})
	return nil
}

func (s *server) currentUser(r *http.Request) (*currentUser, error) {
	if s.authBridge != "" {
		return s.bridgeCurrentUser(r)
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, errors.New("no session")
	}
	userID, err := s.userIDBySession(r.Context(), cookie.Value)
	if err != nil {
		return nil, err
	}
	if err = s.syncUserPoints(userID); err != nil {
		return nil, err
	}
	return s.loadCurrentUserByID(r.Context(), userID)
}

func (s *server) userIDBySession(ctx context.Context, token string) (int64, error) {
	var userID int64
	now := appNow()
	err := s.appDB.QueryRowContext(ctx, `select s.user_id
		from user_sessions s
		join users u on u.id = s.user_id
		where s.session_hash = ?
		  and s.revoked_at is null
		  and s.expires_at > ?
		  and u.delete_flag = 0
		  and u.status = 0
		limit 1`, tokenHash(token), now).Scan(&userID)
	if err != nil {
		return 0, err
	}
	_, _ = s.appDB.ExecContext(ctx, `update user_sessions set last_seen_at = ?, updated_at = ? where session_hash = ?`,
		now, now, tokenHash(token))
	return userID, nil
}

func (s *server) loadCurrentUserByID(ctx context.Context, userID int64) (*currentUser, error) {
	var user currentUser
	var vipExpire sql.NullString
	err := s.appDB.QueryRowContext(ctx, `select u.id, u.username, u.email, u.status, u.role_code, u.role_level,
			u.vip_expire_time, coalesce(a.total_points, 0), coalesce(a.grant_points, 0), coalesce(a.paid_points, 0)
		from users u
		left join point_accounts a on a.user_id = u.id
		where u.id = ? and u.delete_flag = 0 and u.status = 0
		limit 1`, userID).Scan(&user.ID, &user.Username, &user.Email, &user.Status, &user.RoleCode, &user.Level,
		&vipExpire, &user.TotalPoints, &user.GrantPoints, &user.PaidPoints)
	if err != nil {
		return nil, err
	}
	if vipExpire.Valid {
		user.VIPExpireTime = &vipExpire.String
	}
	user.QuotaPoints = user.GrantPoints
	user.RechargePoints = user.PaidPoints
	if user.Level >= levelAdmin {
		user.Level = levelAdmin
		user.RoleCode = roleAdmin
		return &user, nil
	}
	activeExpire, active := s.activeMembershipExpire(ctx, userID)
	if active {
		user.Level = levelVIP
		user.RoleCode = roleVIP
		user.VIPExpireTime = activeExpire
		return &user, nil
	}
	user.Level = levelNormal
	user.RoleCode = roleNormal
	user.VIPExpireTime = nil
	return &user, nil
}

func (s *server) activeMembershipExpire(ctx context.Context, userID int64) (*string, bool) {
	var expire sql.NullString
	err := s.appDB.QueryRowContext(ctx, `select expire_at
		from user_memberships
		where user_id = ?
		  and status = 'active'
		  and (expire_at is null or expire_at > ?)
		order by case when expire_at is null then 1 else 0 end desc, expire_at desc
		limit 1`, userID, appNow()).Scan(&expire)
	if err != nil {
		return nil, false
	}
	if expire.Valid {
		return &expire.String, true
	}
	return nil, true
}

func randomToken(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func emailCodeHash(email, purpose, code string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email)) + "|" + purpose + "|" + strings.TrimSpace(code)))
	return hex.EncodeToString(sum[:])
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := pbkdf2SHA256([]byte(password), salt, passwordRounds, 32)
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%s",
		passwordRounds,
		base64.RawURLEncoding.EncodeToString(salt),
		base64.RawURLEncoding.EncodeToString(key)), nil
}

func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	rounds, err := strconv.Atoi(parts[1])
	if err != nil || rounds <= 0 {
		return false
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, rounds, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	hashLen := sha256.Size
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	var counter [4]byte
	for block := 1; block <= blocks; block++ {
		binary.BigEndian.PutUint32(counter[:], uint32(block))
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		mac.Write(counter[:])
		u := mac.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			mac = hmac.New(sha256.New, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func isSecureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
