package main

import (
	"fmt"
	"net/http"
)

func (s *server) robots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	fmt.Fprint(w, "User-agent: *\nAllow: /\nDisallow: /api/\nAllow: /api/library/previews/\nSitemap: https://saotu.osh.lol/sitemap.xml\n")
}

func (s *server) sitemap(w http.ResponseWriter, r *http.Request) {
	rows, err := s.appDB.QueryContext(r.Context(), `select id from library_images where status='approved' order by id desc limit 10000`)
	if err != nil {
		apiError(w, 500, "站点地图暂时不可用")
		return
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			apiError(w, 500, "站点地图暂时不可用")
			return
		}
		ids = append(ids, id)
	}
	if rows.Err() != nil {
		apiError(w, 500, "站点地图暂时不可用")
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>https://saotu.osh.lol/</loc></url>`)
	for _, id := range ids {
		fmt.Fprintf(w, `<url><loc>https://saotu.osh.lol/?image=%d</loc></url>`, id)
	}
	fmt.Fprint(w, `</urlset>`)
}
