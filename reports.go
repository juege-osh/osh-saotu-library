package main

import (
	"net/http"
	"strconv"
	"strings"
)

func (s *server) reportImage(w http.ResponseWriter, r *http.Request, u *currentUser) {
	var in struct {
		ImageID int64  `json:"imageId"`
		Reason  string `json:"reason"`
	}
	if decodeJSON(r, &in) != nil || in.ImageID <= 0 {
		apiError(w, 400, "举报内容不正确")
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len([]rune(in.Reason)) < 5 || len([]rune(in.Reason)) > 300 {
		apiError(w, 400, "请用 5 到 300 字说明问题")
		return
	}
	result, err := s.appDB.ExecContext(r.Context(), `insert into library_reports(image_id,user_id,reason,created_at) select id,?,?,? from library_images where id=? and status='approved'`, u.ID, in.Reason, appNow(), in.ImageID)
	if err != nil {
		apiError(w, 409, "你已经举报过这张图片")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		apiError(w, 404, "图片不存在或已下架")
		return
	}
	jsonResponse(w, map[string]any{"ok": true})
}

func (s *server) listReports(w http.ResponseWriter, r *http.Request, u *currentUser) {
	if s.libraryRole(u.ID) == "user" {
		apiError(w, 403, "需要管理员权限")
		return
	}
	rows, err := s.appDB.QueryContext(r.Context(), `select r.id,r.image_id,i.title,u.username,r.reason,r.created_at from library_reports r join library_images i on i.id=r.image_id join users u on u.id=r.user_id where r.status='pending' order by r.id desc limit 100`)
	if err != nil {
		apiError(w, 500, "举报记录读取失败")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, imageID int64
		var title, reporter, reason, created string
		if err = rows.Scan(&id, &imageID, &title, &reporter, &reason, &created); err != nil {
			apiError(w, 500, "举报记录读取失败")
			return
		}
		items = append(items, map[string]any{"id": id, "imageId": imageID, "title": title, "reporter": reporter, "reason": reason, "createdAt": created})
	}
	if rows.Err() != nil {
		apiError(w, 500, "举报记录读取失败")
		return
	}
	jsonResponse(w, map[string]any{"items": items})
}

func (s *server) decideReport(w http.ResponseWriter, r *http.Request, u *currentUser) {
	if s.libraryRole(u.ID) == "user" {
		apiError(w, 403, "需要管理员权限")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err != nil || id <= 0 || decodeJSON(r, &in) != nil || in.Decision != "dismiss" && in.Decision != "hide" || len([]rune(in.Reason)) > 300 {
		apiError(w, 400, "处理内容不正确")
		return
	}
	tx, err := s.appDB.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, 500, "举报处理失败")
		return
	}
	defer tx.Rollback()
	var imageID int64
	var reportReason string
	if tx.QueryRow(`select image_id,reason from library_reports where id=? and status='pending'`, id).Scan(&imageID, &reportReason) != nil {
		apiError(w, 409, "举报已经处理")
		return
	}
	if in.Decision == "hide" {
		reason := strings.TrimSpace(in.Reason)
		if reason == "" {
			reason = reportReason
		}
		result, e := tx.Exec(`update library_images set status='hidden',reason=?,reviewed_at=? where id=? and status='approved'`, reason, appNow(), imageID)
		if e != nil {
			apiError(w, 500, "图片下架失败")
			return
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			apiError(w, 409, "图片已经下架")
			return
		}
		_, err = tx.Exec(`update library_reports set status='hidden',decided_at=?,decided_by=? where image_id=? and status='pending'`, appNow(), u.ID, imageID)
	} else {
		_, err = tx.Exec(`update library_reports set status='dismissed',decided_at=?,decided_by=? where id=? and status='pending'`, appNow(), u.ID, id)
	}
	if err != nil || tx.Commit() != nil {
		apiError(w, 500, "举报处理失败")
		return
	}
	jsonResponse(w, map[string]any{"ok": true})
}
