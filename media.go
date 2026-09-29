package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (s *server) uploadImage(w http.ResponseWriter, r *http.Request, u *currentUser) {
	// Bound the entire multipart body before the parser can spill it to disk.
	r.Body = http.MaxBytesReader(w, r.Body, 13<<20)
	if e := r.ParseMultipartForm(16 << 20); e != nil {
		apiError(w, 400, "图片上传失败，文件最大 12 MB")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	val := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	title, tags, keywords, scene, desc, source, cat := val("title"), val("tags"), val("keywords"), val("scene"), val("description"), val("source"), val("category")
	if title == "" || len([]rune(title)) > 80 || len([]rune(tags)) > 200 || len([]rune(keywords)) > 200 || len([]rune(scene)) > 120 || len([]rune(desc)) > 1000 || source == "" || len([]rune(source)) > 500 || !validCategory(cat, false) || val("rights") != "yes" {
		apiError(w, 400, "请填写标题、分类和来源，并确认拥有分享权限")
		return
	}
	f, h, e := r.FormFile("image")
	if e != nil {
		apiError(w, 400, "请选择一张图片")
		return
	}
	defer f.Close()
	if h.Size > 12<<20 {
		apiError(w, 400, "图片最大 12 MB")
		return
	}
	data, e := io.ReadAll(io.LimitReader(f, (12<<20)+1))
	if e != nil || len(data) > 12<<20 {
		apiError(w, 400, "图片读取失败或超过 12 MB")
		return
	}
	cfg, format, e := image.DecodeConfig(bytes.NewReader(data))
	if e != nil || cfg.Width < 32 || cfg.Height < 32 || cfg.Width > 16000 || cfg.Height > 16000 || int64(cfg.Width)*int64(cfg.Height) > 40000000 {
		apiError(w, 400, "支持 JPG、PNG、GIF、WebP，图片最大 4000 万像素")
		return
	}
	ext := map[string]string{"jpeg": ".jpg", "png": ".png", "gif": ".gif", "webp": ".webp"}[format]
	if ext == "" {
		apiError(w, 400, "不支持这种图片格式")
		return
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	var exists int
	s.appDB.QueryRow(`select count(*) from library_images where sha256=?`, hash).Scan(&exists)
	if exists > 0 {
		apiError(w, 409, "这张图片已经有人上传过了")
		return
	}
	img, _, e := image.Decode(bytes.NewReader(data))
	if e != nil {
		apiError(w, 400, "图片无法解码")
		return
	}
	pw, ph := cfg.Width, cfg.Height
	if pw > 480 || ph > 480 {
		ratio := min(480.0/float64(pw), 480.0/float64(ph))
		pw = max(1, int(float64(pw)*ratio))
		ph = max(1, int(float64(ph)*ratio))
	}
	thumb := image.NewRGBA(image.Rect(0, 0, pw, ph))
	draw.ApproxBiLinear.Scale(thumb, thumb.Bounds(), img, img.Bounds(), draw.Over, nil)
	var buf bytes.Buffer
	if e = jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 70}); e != nil {
		apiError(w, 500, "预览创建失败")
		return
	}
	filename := hash + ext
	preview := hash + "-preview.jpg"
	tx, e := s.appDB.BeginTx(r.Context(), nil)
	if e != nil {
		apiError(w, 500, "投稿保存失败")
		return
	}
	defer tx.Rollback()
	if e = consumeQuota(tx, u.ID, "upload", "upload:"+hash); e != nil {
		apiError(w, 429, e.Error())
		return
	}
	var committed bool
	newPaths := []string{}
	defer func() {
		if !committed {
			for _, path := range newPaths {
				_ = os.Remove(path)
			}
		}
	}()
	for _, asset := range []struct {
		name string
		data []byte
	}{{filename, data}, {preview, buf.Bytes()}} {
		path := filepath.Join(s.mediaDir, asset.name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			apiError(w, 409, "图片文件已存在或无法保存，额度未扣除")
			return
		}
		newPaths = append(newPaths, path)
		_, err = file.Write(asset.data)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			apiError(w, 500, "图片保存失败，额度未扣除")
			return
		}
	}
	result, e := tx.Exec(`insert into library_images(user_id,title,tags,keywords,scene,description,source,category,sha256,filename,preview,width,height,bytes,created_at) values(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, u.ID, title, tags, keywords, scene, desc, source, cat, hash, filename, preview, cfg.Width, cfg.Height, len(data), appNow())
	if e != nil {
		apiError(w, 409, "投稿未保存，可能是重复图片，额度未扣除")
		return
	}
	id, _ := result.LastInsertId()
	if _, e = tx.Exec(`insert into library_fts(rowid,title,tags,keywords,scene,description) values(?,?,?,?,?,?)`, id, title, tags, keywords, scene, desc); e != nil {
		apiError(w, 500, "索引保存失败")
		return
	}
	if e = tx.Commit(); e != nil {
		apiError(w, 500, "投稿保存失败")
		return
	}
	committed = true
	jsonResponse(w, map[string]any{"ok": true, "id": id, "status": "pending", "quota": s.quota(u.ID)})
}
func (s *server) previewImage(w http.ResponseWriter, r *http.Request) {
	var path, status string
	var owner int64
	e := s.appDB.QueryRow(`select preview,status,user_id from library_images where id=?`, r.PathValue("id")).Scan(&path, &status, &owner)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	if status != "approved" {
		u, e := s.currentUser(r)
		if e != nil || (u.ID != owner && status != "pending" && s.libraryRole(u.ID) == "user") {
			http.NotFound(w, r)
			return
		}
	}
	if status == "approved" {
		w.Header().Set("Cache-Control", "private, max-age=120")
	} else {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, filepath.Join(s.mediaDir, filepath.Base(path)))
}
func (s *server) downloadImage(w http.ResponseWriter, r *http.Request, u *currentUser) {
	id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if e != nil || id <= 0 {
		apiError(w, 400, "图片编号不正确")
		return
	}
	var filename string
	e = s.appDB.QueryRow(`select filename from library_images where id=? and status='approved'`, id).Scan(&filename)
	if e != nil {
		apiError(w, 404, "图片暂不可下载")
		return
	}
	if _, e = os.Stat(filepath.Join(s.mediaDir, filepath.Base(filename))); e != nil {
		apiError(w, 503, "原图暂不可用，额度未扣除")
		return
	}
	// Reuse an unexpired receipt for retries and double clicks on the same image.
	var token string
	e = s.appDB.QueryRow(`select token from library_downloads where image_id=? and user_id=? and created_at>? order by created_at desc limit 1`, id, u.ID, appTime(time.Now().Add(-10*time.Minute))).Scan(&token)
	if e == nil {
		jsonResponse(w, map[string]any{"url": fmt.Sprintf("/api/library/downloads/%d?token=%s", id, token), "quota": s.quota(u.ID)})
		return
	}
	token, e = randomToken(24)
	if e != nil {
		apiError(w, 500, "下载创建失败")
		return
	}
	tx, e := s.appDB.BeginTx(r.Context(), nil)
	if e != nil {
		apiError(w, 500, "下载创建失败")
		return
	}
	defer tx.Rollback()
	// Single-writer transaction checks again to avoid concurrent duplicate charges.
	var existing string
	if tx.QueryRow(`select token from library_downloads where image_id=? and user_id=? and created_at>? order by created_at desc limit 1`, id, u.ID, appTime(time.Now().Add(-10*time.Minute))).Scan(&existing) == nil {
		tx.Rollback()
		jsonResponse(w, map[string]any{"url": fmt.Sprintf("/api/library/downloads/%d?token=%s", id, existing), "quota": s.quota(u.ID)})
		return
	}
	if e = consumeQuota(tx, u.ID, "download", "download:"+token); e != nil {
		apiError(w, 429, e.Error())
		return
	}
	if _, e = tx.Exec(`insert into library_downloads values(?,?,?,?)`, token, u.ID, id, appNow()); e != nil {
		apiError(w, 500, "下载创建失败")
		return
	}
	if _, e = tx.Exec(`update library_images set downloads=downloads+1 where id=?`, id); e != nil {
		apiError(w, 500, "下载创建失败")
		return
	}
	if e = tx.Commit(); e != nil {
		apiError(w, 500, "下载创建失败")
		return
	}
	jsonResponse(w, map[string]any{"url": fmt.Sprintf("/api/library/downloads/%d?token=%s", id, token), "quota": s.quota(u.ID)})
}
func (s *server) downloadTicket(w http.ResponseWriter, r *http.Request, u *currentUser) {
	var name, created string
	var id int64
	e := s.appDB.QueryRow(`select i.filename,d.created_at,i.id from library_downloads d join library_images i on i.id=d.image_id where d.token=? and d.user_id=? and d.image_id=? and i.status='approved'`, r.URL.Query().Get("token"), u.ID, r.PathValue("id")).Scan(&name, &created, &id)
	at, _ := time.Parse(time.RFC3339Nano, created)
	if e != nil || time.Since(at) > 10*time.Minute {
		apiError(w, 403, "下载链接已过期或无权访问")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="saotu-%d%s"`, id, filepath.Ext(name)))
	http.ServeFile(w, r, filepath.Join(s.mediaDir, filepath.Base(name)))
}
