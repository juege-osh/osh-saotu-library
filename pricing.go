package main

import (
	"database/sql"
	"net/http"
	"net/url"
	"strings"
)

type resourcePricing struct {
	Multiplier Points `json:"multiplier"`
	Version    int64  `json:"version"`
	Video      Points `json:"video"`
	Image      Points `json:"image"`
}

type pricingReader interface{ QueryRow(string, ...any) *sql.Row }

func readPricing(db pricingReader) (resourcePricing, error) {
	var p resourcePricing
	err := db.QueryRow(`select multiplier,version from resource_pricing where id=1`).Scan(&p.Multiplier, &p.Version)
	p.Video = Points(resourcePrice("video") * int64(p.Multiplier) / pointScale)
	p.Image = Points(resourcePrice("image") * int64(p.Multiplier) / pointScale)
	return p, err
}

func (s *server) pricing(w http.ResponseWriter, r *http.Request) {
	p, err := readPricing(s.appDB)
	if err != nil {
		apiError(w, 500, "价格读取失败")
		return
	}
	jsonResponse(w, p)
}

func sameOriginJSON(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		apiError(w, 415, "请使用 JSON 请求")
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) || (u.Scheme != "http" && u.Scheme != "https") {
			apiError(w, 403, "请求来源不允许")
			return false
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		apiError(w, 403, "请求来源不允许")
		return false
	}
	return true
}

func (s *server) updatePricing(w http.ResponseWriter, r *http.Request, admin *currentUser) {
	if !sameOriginJSON(w, r) {
		return
	}
	var req struct {
		Multiplier Points `json:"multiplier"`
		Version    int64  `json:"version"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Multiplier < 10 || req.Multiplier > 100 || req.Version < 1 {
		apiError(w, 400, "倍率须在 0.1 到 1 之间，最多两位小数，并提供当前价格版本")
		return
	}
	tx, err := s.appDB.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, 500, "价格保存失败")
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`update resource_pricing set id=id where id=1`); err != nil {
		apiError(w, 500, "价格保存失败")
		return
	}
	old, err := readPricing(tx)
	if err != nil {
		apiError(w, 500, "价格读取失败")
		return
	}
	if old.Version != req.Version {
		apiError(w, 409, "倍率已被修改，请刷新后重试")
		return
	}
	if old.Multiplier == req.Multiplier {
		jsonResponse(w, old)
		return
	}
	_, err = tx.Exec(`update resource_pricing set multiplier=?,version=version+1,updated_at=?,updated_by=? where id=1`, int64(req.Multiplier), appNow(), admin.ID)
	if err == nil {
		_, err = tx.Exec(`insert into pricing_changes(old_multiplier,new_multiplier,version,operator_id,created_at) values(?,?,?,?,?)`, int64(old.Multiplier), int64(req.Multiplier), old.Version+1, admin.ID, appNow())
	}
	if err != nil {
		apiError(w, 500, "价格保存失败")
		return
	}
	p, err := readPricing(tx)
	if err != nil {
		apiError(w, 500, "价格读取失败")
		return
	}
	if err = tx.Commit(); err != nil {
		apiError(w, 500, "价格保存失败")
		return
	}
	jsonResponse(w, p)
}
