package main

import (
	"net/http"
	"strconv"
)

func favoriteID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *server) listFavorites(w http.ResponseWriter, r *http.Request, u *currentUser) {
	rows, err := s.appDB.QueryContext(r.Context(), `select f.image_id from library_favorites f join library_images i on i.id=f.image_id where f.user_id=? and i.status='approved' order by f.created_at desc`, u.ID)
	if err != nil {
		apiError(w, 500, "收藏读取失败")
		return
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		apiError(w, 500, "收藏读取失败")
		return
	}
	if r.URL.Query().Get("ids") == "1" {
		jsonResponse(w, map[string]any{"ids": ids})
		return
	}
	page := pageNumber(r)
	items, count, err := s.imageList("i.status='approved' and exists(select 1 from library_favorites f where f.image_id=i.id and f.user_id=?)", []any{u.ID}, page)
	if err != nil {
		apiError(w, 500, "收藏读取失败")
		return
	}
	jsonResponse(w, map[string]any{"ids": ids, "items": items, "total": count, "page": page, "pages": max(1, (count+23)/24)})
}

func (s *server) addFavorite(w http.ResponseWriter, r *http.Request, u *currentUser) {
	id, ok := favoriteID(r)
	if !ok {
		apiError(w, 404, "图片不存在")
		return
	}
	var approved int
	if err := s.appDB.QueryRowContext(r.Context(), `select count(*) from library_images where id=? and status='approved'`, id).Scan(&approved); err != nil {
		apiError(w, 500, "收藏失败")
		return
	}
	if approved == 0 {
		apiError(w, 404, "图片不存在或已下架")
		return
	}
	if _, err := s.appDB.ExecContext(r.Context(), `insert into library_favorites(user_id,image_id,created_at) values(?,?,?) on conflict do nothing`, u.ID, id, appNow()); err != nil {
		apiError(w, 500, "收藏失败")
		return
	}
	jsonResponse(w, map[string]any{"saved": true})
}

func (s *server) removeFavorite(w http.ResponseWriter, r *http.Request, u *currentUser) {
	id, ok := favoriteID(r)
	if !ok {
		apiError(w, 404, "图片不存在")
		return
	}
	if _, err := s.appDB.ExecContext(r.Context(), `delete from library_favorites where user_id=? and image_id=?`, u.ID, id); err != nil {
		apiError(w, 500, "取消收藏失败")
		return
	}
	jsonResponse(w, map[string]any{"saved": false})
}
