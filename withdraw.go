package main

import (
	"fmt"
	"net/http"
	"strconv"
)

// withdrawPending lets an author take back a submission before it is published.
// The upload credit is returned as a persistent contribution credit.
func (s *server) withdrawPending(w http.ResponseWriter, r *http.Request, u *currentUser) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		apiError(w, 400, "图片编号不正确")
		return
	}
	tx, err := s.appDB.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, 500, "撤回失败")
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec(`update library_images set status='hidden',reason='投稿者撤回',reviewed_at=? where id=? and user_id=? and status='pending'`, appNow(), id, u.ID)
	if err != nil {
		apiError(w, 500, "撤回失败")
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		apiError(w, 409, "投稿已经有结果，或这张图不属于你")
		return
	}
	if err = reward(tx, u.ID, "upload", 1, fmt.Sprintf("withdraw:%d", id)); err != nil || tx.Commit() != nil {
		apiError(w, 500, "撤回失败")
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "quota": s.quota(u.ID)})
}
