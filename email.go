package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

const emailCodeAttempts = 5

var errEmailCode = errors.New("验证码错误、已失效或尝试次数过多，请重新获取")

type smtpConfig struct {
	host, port, user, password, from, security string
}

func loadSMTPConfig() (smtpConfig, error) {
	c := smtpConfig{
		host: strings.TrimSpace(os.Getenv("SMTP_HOST")), port: strings.TrimSpace(os.Getenv("SMTP_PORT")),
		user: strings.TrimSpace(os.Getenv("SMTP_USER")), password: os.Getenv("SMTP_PASSWORD"),
		from: strings.TrimSpace(os.Getenv("SMTP_FROM")), security: strings.ToLower(strings.TrimSpace(os.Getenv("SMTP_SECURITY"))),
	}
	if c.port == "" {
		c.port = "465"
	}
	if c.from == "" {
		c.from = c.user
	}
	if c.security == "" {
		c.security = "starttls"
		if c.port == "465" {
			c.security = "tls"
		}
	}
	port, err := strconv.Atoi(c.port)
	if err != nil || port < 1 || port > 65535 || c.host == "" || c.user == "" || c.password == "" {
		return c, errors.New("SMTP_HOST, SMTP_PORT, SMTP_USER and SMTP_PASSWORD must be configured")
	}
	from, err := mail.ParseAddress(c.from)
	if err != nil || from.Address != c.from || strings.ContainsAny(c.from+c.host+c.user, "\r\n") {
		return c, errors.New("invalid SMTP sender or host")
	}
	if c.security != "tls" && c.security != "starttls" {
		return c, errors.New("SMTP_SECURITY must be tls or starttls")
	}
	return c, nil
}

func sendVerificationEmail(ctx context.Context, email, purpose, code string) error {
	c, err := loadSMTPConfig()
	if err != nil {
		return err
	}
	return c.send(ctx, email, purpose, code)
}

func (c smtpConfig) send(ctx context.Context, email, purpose, code string) error {
	to, err := mail.ParseAddress(email)
	if err != nil || to.Address != email || strings.ContainsAny(email, "\r\n") {
		return errors.New("invalid recipient")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tlsConfig := &tls.Config{ServerName: c.host, MinVersion: tls.VersionTLS12}
	address := net.JoinHostPort(c.host, c.port)
	var conn net.Conn
	if c.security == "tls" {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return fmt.Errorf("SMTP connect: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, c.host)
	if err != nil {
		return fmt.Errorf("SMTP greeting: %w", err)
	}
	defer client.Close()
	if c.security == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP server does not support required STARTTLS")
		}
		if err = client.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("SMTP STARTTLS: %w", err)
		}
	}
	if err = client.Auth(smtp.PlainAuth("", c.user, c.password, c.host)); err != nil {
		return fmt.Errorf("SMTP authentication: %w", err)
	}
	if err = client.Mail(c.from); err != nil {
		return fmt.Errorf("SMTP sender: %w", err)
	}
	if err = client.Rcpt(email); err != nil {
		return fmt.Errorf("SMTP recipient: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	message, err := verificationMessage(c.from, email, purpose, code)
	if err != nil {
		return err
	}
	if _, err = writer.Write(message); err != nil {
		return fmt.Errorf("SMTP message: %w", err)
	}
	if err = writer.Close(); err != nil {
		return fmt.Errorf("SMTP delivery: %w", err)
	}
	// DATA acceptance is the delivery result; a failed QUIT must not trigger a resend.
	_ = client.Quit()
	return nil
}

func verificationMessage(from, email, purpose, code string) ([]byte, error) {
	scene := map[string]string{"register": "注册账号", "reset_password": "重置密码", "change_email": "修改邮箱"}[purpose]
	if scene == "" {
		return nil, errors.New("invalid email purpose")
	}
	id, err := randomToken(18)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n",
		(&mail.Address{Name: "OSH 灵感库", Address: from}).String(), (&mail.Address{Address: email}).String(),
		mime.QEncoding.Encode("UTF-8", "OSH 灵感库 - "+scene+"验证码"), time.Now().Format(time.RFC1123Z), id, strings.SplitN(from, "@", 2)[1])
	writer := quotedprintable.NewWriter(&out)
	if _, err = fmt.Fprintf(writer, "你正在进行%s操作。\r\n\r\n验证码：%s\r\n\r\n验证码 10 分钟内有效，仅可使用一次。请勿将验证码提供给任何人。\r\n如果不是你本人操作，请忽略此邮件。\r\n\r\nOSH 灵感库\r\n", scene, code); err != nil {
		return nil, err
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (s *server) sendEmailCode(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, 400, "请求格式不正确")
		return
	}
	email, purpose := strings.ToLower(strings.TrimSpace(req.Email)), strings.TrimSpace(req.Purpose)
	parsed, err := mail.ParseAddress(email)
	if !emailPattern.MatchString(email) || len(email) > 254 || err != nil || parsed.Address != email {
		apiError(w, 400, "邮箱格式不正确")
		return
	}
	if purpose != "register" && purpose != "reset_password" && purpose != "change_email" {
		apiError(w, 400, "验证码用途不正确")
		return
	}
	if purpose == "change_email" {
		if _, err = s.currentUser(r); err != nil {
			apiError(w, 401, "请先登录")
			return
		}
	}
	var exists, active int
	if err = s.appDB.QueryRow(`select count(*), coalesce(sum(case when status=0 then 1 else 0 end),0)
		from users where lower(email)=? and delete_flag=0`, email).Scan(&exists, &active); err != nil {
		apiError(w, 500, "验证码发送失败")
		return
	}
	if purpose != "reset_password" && exists > 0 {
		apiError(w, 409, "邮箱已被使用")
		return
	}
	if purpose == "reset_password" && active == 0 {
		apiError(w, 400, "该邮箱未注册或账号不可用")
		return
	}
	sender := s.emailSender
	if sender == nil {
		if _, err = loadSMTPConfig(); err != nil {
			apiError(w, 503, "邮件服务未配置，请联系管理员")
			return
		}
		sender = sendVerificationEmail
	}
	number, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		apiError(w, 500, "验证码生成失败")
		return
	}
	code := fmt.Sprintf("%06d", number.Int64())
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	id, retry, err := s.reserveEmailCode(email, purpose, code, ip)
	if err != nil {
		apiError(w, 500, "验证码发送失败")
		return
	}
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		apiError(w, 429, "发送过于频繁，请稍后重试")
		return
	}
	if err = sender(r.Context(), email, purpose, code); err != nil {
		_, _ = s.appDB.Exec(`update email_verification_codes set delivery_status='failed', expires_at=? where id=?`, appNow(), id)
		log.Printf("Verification email delivery failed: %v", err)
		w.Header().Set("Retry-After", "60")
		apiError(w, 502, "验证码邮件发送失败，请稍后重试")
		return
	}
	if err = s.activateEmailCode(id, email, purpose); err != nil {
		apiError(w, 500, "验证码保存失败，请稍后重新获取")
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "message": "验证码已发送，请查看邮箱（包括垃圾邮件）", "retryAfter": 60, "expiresIn": 600})
}

func (s *server) reserveEmailCode(email, purpose, code, ip string) (int64, int, error) {
	tx, err := s.appDB.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var latest sql.NullString
	var emailCount, ipCount int
	if err = tx.QueryRow(`select max(created_at),count(*) from email_verification_codes where email=? and created_at>?`, email, appTime(now.Add(-time.Hour))).Scan(&latest, &emailCount); err != nil {
		return 0, 0, err
	}
	if latest.Valid {
		last, err := time.Parse(time.RFC3339Nano, latest.String)
		if err != nil {
			return 0, 0, err
		}
		if remaining := 60 - now.Sub(last).Seconds(); remaining > 0 {
			return 0, int(remaining) + 1, nil
		}
	}
	if err = tx.QueryRow(`select count(*) from email_verification_codes where request_ip=? and created_at>?`, ip, appTime(now.Add(-time.Hour))).Scan(&ipCount); err != nil {
		return 0, 0, err
	}
	if emailCount >= 5 || ipCount >= 20 {
		return 0, 3600, nil
	}
	result, err := tx.Exec(`insert into email_verification_codes(email,purpose,code_hash,expires_at,created_at,delivery_status,request_ip)
		values(?,?,?,?,?,'pending',?)`, email, purpose, emailCodeHash(email, purpose, code), appTime(now.Add(10*time.Minute)), appTime(now), ip)
	if err != nil {
		return 0, 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	return id, 0, tx.Commit()
}

func (s *server) activateEmailCode(id int64, email, purpose string) error {
	tx, err := s.appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	if _, err = tx.Exec(`update email_verification_codes set used_at=? where email=? and purpose=? and id<>? and used_at is null`, appTime(now), email, purpose, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`update email_verification_codes set delivery_status='sent',expires_at=? where id=?`, appTime(now.Add(10*time.Minute)), id); err != nil {
		return err
	}
	return tx.Commit()
}

func verifyEmailCode(db *sql.DB, email, purpose, code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("请输入邮箱验证码")
	}
	// Commit failed attempts separately so a rejected account operation cannot undo them.
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	var hash string
	err = tx.QueryRow(`select id,code_hash from email_verification_codes where email=? and purpose=?
		and delivery_status='sent' and used_at is null and expires_at>? and attempts<? order by id desc limit 1`,
		strings.ToLower(strings.TrimSpace(email)), purpose, appNow(), emailCodeAttempts).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return errEmailCode
	}
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(emailCodeHash(email, purpose, code))) != 1 {
		if _, err = tx.Exec(`update email_verification_codes set attempts=attempts+1 where id=?`, id); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return errEmailCode
	}
	return nil
}

func markEmailCodeUsedTx(tx *sql.Tx, email, purpose, code string) error {
	// Recheck and consume atomically with the account mutation to prevent code reuse.
	result, err := tx.Exec(`update email_verification_codes set used_at=? where id=(
		select id from email_verification_codes where email=? and purpose=? and delivery_status='sent'
		and used_at is null and expires_at>? and attempts<? order by id desc limit 1) and code_hash=?`,
		appNow(), strings.ToLower(strings.TrimSpace(email)), purpose, appNow(), emailCodeAttempts, emailCodeHash(email, purpose, code))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errEmailCode
	}
	return nil
}
