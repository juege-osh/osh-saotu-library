package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func finalizeImage(tx *sql.Tx, id, owner int64, status, reason string) error {
	res, e := tx.Exec(`update library_images set status=?,reason=?,reviewed_at=? where id=? and status='pending'`, status, reason, appNow(), id)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("图片已经处理过了")
	}
	if status == "approved" {
		if e = reward(tx, owner, "search", 1, fmt.Sprintf("approved:%d", id)); e != nil {
			return e
		}
		if e = reward(tx, owner, "download", 10, fmt.Sprintf("approved:%d", id)); e != nil {
			return e
		}
	}
	rows, e := tx.Query(`select user_id,multiplier from library_reviews where image_id=? and decision=? and effective=0`, id, status)
	if e != nil {
		return e
	}
	type vote struct {
		id int64
		m  int
	}
	votes := []vote{}
	for rows.Next() {
		var v vote
		if e = rows.Scan(&v.id, &v.m); e != nil {
			rows.Close()
			return e
		}
		votes = append(votes, v)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return e
	}
	rows.Close()
	for _, v := range votes {
		src := fmt.Sprintf("review:%d", id)
		if e = reward(tx, v.id, "download", v.m, src); e != nil {
			return e
		}
		if e = reward(tx, v.id, "upload", v.m, src); e != nil {
			return e
		}
		if _, e = tx.Exec(`update library_reviews set effective=1 where image_id=? and user_id=? and effective=0`, id, v.id); e != nil {
			return e
		}
		if _, e = tx.Exec(`update library_accounts set review_count=review_count+1 where user_id=?`, v.id); e != nil {
			return e
		}
	}
	return nil
}
func (s *server) reviewImage(w http.ResponseWriter, r *http.Request, u *currentUser) {
	var in struct {
		ImageID  int64  `json:"imageId"`
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if decodeJSON(r, &in) != nil || (in.Decision != "approved" && in.Decision != "rejected") || len([]rune(in.Reason)) > 300 {
		apiError(w, 400, "审核格式不正确")
		return
	}
	if in.Decision == "rejected" && strings.TrimSpace(in.Reason) == "" {
		apiError(w, 400, "请填写驳回原因")
		return
	}
	role := s.libraryRole(u.ID)
	tx, e := s.appDB.BeginTx(r.Context(), nil)
	if e != nil {
		apiError(w, 500, "审核保存失败")
		return
	}
	defer tx.Rollback()
	var owner int64
	var status string
	if tx.QueryRow(`select user_id,status from library_images where id=?`, in.ImageID).Scan(&owner, &status) != nil {
		apiError(w, 404, "图片不存在")
		return
	}
	if owner == u.ID {
		apiError(w, 403, "不能审核自己的图片")
		return
	}
	if status != "pending" {
		apiError(w, 409, "图片已经有审核结果")
		return
	}
	var daily int
	tx.QueryRow(`select count(*) from library_reviews where user_id=? and created_at>?`, u.ID, appTime(time.Now().Add(-24*time.Hour))).Scan(&daily)
	if daily >= 50 && role == "user" {
		apiError(w, 429, "今天已审核 50 张，明天再来")
		return
	}
	multiplier := 1
	if role != "user" {
		multiplier = 2
	}
	if _, e = tx.Exec(`insert into library_accounts(user_id) values(?) on conflict do nothing`, u.ID); e != nil {
		apiError(w, 500, "账户读取失败")
		return
	}
	if _, e = tx.Exec(`insert into library_reviews(image_id,user_id,decision,reason,multiplier,created_at) values(?,?,?,?,?,?)`, in.ImageID, u.ID, in.Decision, strings.TrimSpace(in.Reason), multiplier, appNow()); e != nil {
		apiError(w, 409, "你已经审核过这张图片")
		return
	}
	result := "pending"
	reason := ""
	if role != "user" {
		result = in.Decision
		reason = in.Reason
	} else {
		var approvals, rejections int
		tx.QueryRow(`select coalesce(sum(decision='approved'),0),coalesce(sum(decision='rejected'),0) from library_reviews where image_id=?`, in.ImageID).Scan(&approvals, &rejections)
		if rejections >= 2 {
			result = "rejected"
			tx.QueryRow(`select reason from library_reviews where image_id=? and decision='rejected' order by created_at desc limit 1`, in.ImageID).Scan(&reason)
		} else if approvals >= 3 {
			result = "approved"
		}
	}
	if result != "pending" {
		if e = finalizeImage(tx, in.ImageID, owner, result, reason); e != nil {
			apiError(w, 500, "审核结算失败")
			return
		}
	}
	if e = tx.Commit(); e != nil {
		apiError(w, 500, "审核保存失败")
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "status": result, "quota": s.quota(u.ID)})
}
func (s *server) applyAdmin(w http.ResponseWriter, r *http.Request, u *currentUser) {
	s.ensureLibraryAccount(u.ID)
	var count int
	s.appDB.QueryRow(`select review_count from library_accounts where user_id=?`, u.ID).Scan(&count)
	if count < 10 || s.libraryRole(u.ID) != "user" {
		apiError(w, 403, "完成 10 次有效审核后可申请管理员")
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if decodeJSON(r, &in) != nil || strings.TrimSpace(in.Reason) == "" || len([]rune(in.Reason)) > 500 {
		apiError(w, 400, "请写一段申请理由，最多 500 字")
		return
	}
	if _, e := s.appDB.Exec(`insert into library_admin_applications(user_id,reason,created_at) values(?,?,?)`, u.ID, in.Reason, appNow()); e != nil {
		apiError(w, 409, "已有申请待创始人处理")
		return
	}
	jsonResponse(w, map[string]any{"ok": true})
}
func (s *server) listApplications(w http.ResponseWriter, r *http.Request, u *currentUser) {
	where := "a.user_id=?"
	args := []any{u.ID}
	if s.libraryRole(u.ID) == "founder" {
		where = "1=1"
		args = nil
	}
	rows, e := s.appDB.Query(`select a.id,a.user_id,u.username,a.reason,a.status,a.decision_reason,a.created_at,l.review_count from library_admin_applications a join users u on u.id=a.user_id join library_accounts l on l.user_id=a.user_id where `+where+` order by a.id desc limit 100`, args...)
	if e != nil {
		apiError(w, 500, "申请读取失败")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, uid int64
		var name, reason, status, dr, at string
		var count int
		rows.Scan(&id, &uid, &name, &reason, &status, &dr, &at, &count)
		items = append(items, map[string]any{"id": id, "userId": uid, "username": name, "reason": reason, "status": status, "decisionReason": dr, "createdAt": at, "reviewCount": count})
	}
	jsonResponse(w, map[string]any{"items": items})
}
func (s *server) decideApplication(w http.ResponseWriter, r *http.Request, u *currentUser) {
	if s.libraryRole(u.ID) != "founder" {
		apiError(w, 403, "只有创始人能处理管理员申请")
		return
	}
	var in struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if decodeJSON(r, &in) != nil || len([]rune(in.Reason)) > 500 {
		apiError(w, 400, "申请处理格式不正确")
		return
	}
	tx, e := s.appDB.BeginTx(r.Context(), nil)
	if e != nil {
		apiError(w, 500, "申请处理失败")
		return
	}
	defer tx.Rollback()
	var target int64
	var status string
	var count int
	e = tx.QueryRow(`select a.user_id,a.status,l.review_count from library_admin_applications a join library_accounts l on l.user_id=a.user_id where a.id=?`, r.PathValue("id")).Scan(&target, &status, &count)
	if e != nil || status != "pending" {
		apiError(w, 409, "申请不存在或已经处理")
		return
	}
	status = "rejected"
	if in.Approve {
		if count < 10 {
			apiError(w, 409, "有效审核次数不足")
			return
		}
		status = "approved"
		if _, e = tx.Exec(`update library_accounts set role='admin' where user_id=?`, target); e != nil {
			apiError(w, 500, "管理员授权失败")
			return
		}
	}
	if _, e = tx.Exec(`update library_admin_applications set status=?,decision_reason=?,decided_at=?,decided_by=? where id=?`, status, in.Reason, appNow(), u.ID, r.PathValue("id")); e != nil {
		apiError(w, 500, "申请保存失败")
		return
	}
	if e = tx.Commit(); e != nil {
		apiError(w, 500, "申请保存失败")
		return
	}
	jsonResponse(w, map[string]any{"ok": true})
}
func (s *server) moderateImage(w http.ResponseWriter, r *http.Request, u *currentUser) {
	if s.libraryRole(u.ID) == "user" {
		apiError(w, 403, "需要管理员权限")
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if decodeJSON(r, &in) != nil || strings.TrimSpace(in.Reason) == "" || len([]rune(in.Reason)) > 300 {
		apiError(w, 400, "请填写下架原因")
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	tx, e := s.appDB.BeginTx(r.Context(), nil)
	if e != nil {
		apiError(w, 500, "下架失败")
		return
	}
	defer tx.Rollback()
	res, e := tx.Exec(`update library_images set status='hidden',reason=?,reviewed_at=? where id=? and status='approved'`, in.Reason, appNow(), id)
	if e != nil {
		apiError(w, 500, "下架失败")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		apiError(w, 404, "图片不存在或已下架")
		return
	}
	if _, e = tx.Exec(`update library_reports set status='hidden',decided_at=?,decided_by=? where image_id=? and status='pending'`, appNow(), u.ID, id); e != nil || tx.Commit() != nil {
		apiError(w, 500, "下架失败")
		return
	}
	jsonResponse(w, map[string]any{"ok": true})
}
