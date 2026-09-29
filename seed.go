package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/image/draw"
)

//go:embed seed-manifest.json seed-media/*
var seedAssets embed.FS

type seedEntry struct {
	File, Title, Category, Tags, Keywords, Scene, Description, Source string
	License, LicenseURL, Author, OriginalTitle, SHA256                string
	Bytes                                                             int
}

// Starter content is published by the site, not submitted by a user. It never earns quota.
func seedInitialLibrary(db *sql.DB, mediaDir string) error {
	manifestData, err := seedAssets.ReadFile("seed-manifest.json")
	if err != nil {
		return err
	}
	var manifest []seedEntry
	if err = json.Unmarshal(manifestData, &manifest); err != nil {
		return err
	}
	if _, err = db.Exec(`insert into users(id,username,email,password_hash,status,role_code,role_level,created_at,updated_at) values(-1,'图库编辑部','seed@local.invalid','system',0,'normal',1,?,?) on conflict(id) do nothing`, appNow(), appNow()); err != nil {
		return err
	}
	for _, item := range manifest {
		if err = seedOneImage(db, mediaDir, item); err != nil {
			return fmt.Errorf("导入 %s: %w", item.File, err)
		}
	}
	return nil
}

func seedOneImage(db *sql.DB, mediaDir string, item seedEntry) error {
	var exists int
	if err := db.QueryRow(`select count(*) from library_images where sha256=?`, item.SHA256).Scan(&exists); err != nil || exists > 0 {
		return err
	}
	if item.File == "" || filepath.Base(item.File) != item.File || !validCategory(item.Category, false) || item.License != "CC0" && item.License != "Public domain" {
		return fmt.Errorf("素材清单无效")
	}
	data, err := seedAssets.ReadFile("seed-media/" + item.File)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if hash != item.SHA256 || len(data) != item.Bytes || len(data) > 12<<20 {
		return fmt.Errorf("素材大小或校验值不符")
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil || format != "jpeg" && format != "png" {
		return fmt.Errorf("素材格式无法读取")
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w < 32 || h < 32 || int64(w)*int64(h) > 40000000 {
		return fmt.Errorf("素材尺寸无效")
	}
	pw, ph := w, h
	if pw > 480 || ph > 480 {
		ratio := min(480.0/float64(pw), 480.0/float64(ph))
		pw = max(1, int(float64(pw)*ratio))
		ph = max(1, int(float64(ph)*ratio))
	}
	thumb := image.NewRGBA(image.Rect(0, 0, pw, ph))
	draw.ApproxBiLinear.Scale(thumb, thumb.Bounds(), img, bounds, draw.Over, nil)
	var preview bytes.Buffer
	if err = jpeg.Encode(&preview, thumb, &jpeg.Options{Quality: 70}); err != nil {
		return err
	}
	ext := ".jpg"
	if format == "png" {
		ext = ".png"
	}
	filename, previewName := hash+ext, hash+"-preview.jpg"
	for _, file := range []struct {
		name string
		data []byte
	}{{filename, data}, {previewName, preview.Bytes()}} {
		path := filepath.Join(mediaDir, file.name)
		if existing, readErr := os.ReadFile(path); readErr == nil {
			if !bytes.Equal(existing, file.data) {
				return fmt.Errorf("素材文件冲突")
			}
			continue
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
		if err = os.WriteFile(path, file.data, 0600); err != nil {
			return err
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	desc := strings.TrimSpace(item.Description + " 授权：" + item.License + "；原作者：" + item.Author + "。")
	result, err := tx.Exec(`insert into library_images(user_id,title,tags,keywords,scene,description,source,category,sha256,filename,preview,width,height,bytes,status,created_at,reviewed_at) values(-1,?,?,?,?,?,?,?,?,?,?,?,?,?,'approved',?,?)`, item.Title, item.Tags, item.Keywords, item.Scene, desc, item.Source, item.Category, hash, filename, previewName, w, h, len(data), appNow(), appNow())
	if err != nil {
		return err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`insert into library_fts(rowid,title,tags,keywords,scene,description) values(?,?,?,?,?,?)`, id, item.Title, item.Tags, item.Keywords, item.Scene, desc); err != nil {
		return err
	}
	return tx.Commit()
}
