package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	_ "modernc.org/sqlite"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed index.html style.css app.js jokes.json hero.png web-vendor/qrcode.min.js
var assets embed.FS

type server struct {
	db, imageDB, appDB                                *sql.DB
	root, r2Access, r2Secret, payProvider, authBridge string
	mediaClient                                       *http.Client
	emailSender                                       func(context.Context, string, string, string) error
	huifu                                             *huifuPayClient
	mediaDir                                          string
	founderID                                         int64
}

func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(v)
}
func textParam(r *http.Request, key string, n int) string {
	s := strings.TrimSpace(r.URL.Query().Get(key))
	if len(s) > n {
		s = s[:n]
	}
	return s
}
func (s *server) bridgeCurrentUser(r *http.Request) (*currentUser, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, errors.New("no session")
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", s.authBridge+"/api/me", nil)
	if err != nil {
		return nil, err
	}
	req.AddCookie(cookie)
	req.Header.Set("X-Forwarded-Proto", "https")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var data struct {
		Authenticated bool         `json:"authenticated"`
		User          *currentUser `json:"user"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&data) != nil || !data.Authenticated || data.User == nil {
		return nil, errors.New("invalid session")
	}
	u := data.User
	if u.Status != 0 {
		return nil, errors.New("unavailable account")
	}
	level := levelNormal
	role := roleNormal
	if u.Level >= levelAdmin {
		level = levelAdmin
		role = roleAdmin
	}
	_, err = s.appDB.ExecContext(r.Context(), `insert into users(id,username,email,password_hash,status,role_code,role_level,created_at,updated_at) values(?,?,?,'shared-account',0,?,?,?,?) on conflict(id) do update set username=excluded.username,email=excluded.email,status=0,delete_flag=0,updated_at=excluded.updated_at`, u.ID, u.Username, u.Email, role, level, appNow(), appNow())
	if err != nil {
		return nil, err
	}
	u.Level = level
	u.RoleCode = role
	return u, nil
}
func (s *server) authProxy() http.Handler {
	target, _ := url.Parse(s.authBridge)
	proxy := httputil.NewSingleHostReverseProxy(target)
	original := proxy.Director
	proxy.Director = func(r *http.Request) {
		original(r)
		r.Host = "prompt.osh.lol"
		r.Header.Set("X-Forwarded-Proto", "https")
		if r.Header.Get("Origin") != "" {
			r.Header.Set("Origin", "https://prompt.osh.lol")
		}
		r.Header.Del("Referer")
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		apiError(w, 503, "账号服务暂时没有响应，请稍后再试。")
	}
	return proxy
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	if s.authBridge != "" {
		for _, route := range []string{"POST /api/auth/login", "POST /api/auth/register", "POST /api/auth/logout", "POST /api/auth/email-code", "POST /api/auth/reset-password", "GET /api/me", "POST /api/user/password", "POST /api/user/email"} {
			mux.Handle(route, s.authProxy())
		}
	} else {
		mux.HandleFunc("POST /api/auth/login", s.login)
		mux.HandleFunc("POST /api/auth/register", s.register)
		mux.HandleFunc("POST /api/auth/logout", s.logout)
		mux.HandleFunc("POST /api/auth/email-code", s.sendEmailCode)
		mux.HandleFunc("POST /api/auth/reset-password", s.resetPassword)
		mux.HandleFunc("GET /api/me", s.me)
	}
	mux.HandleFunc("GET /api/library/bootstrap", s.libraryBootstrap)
	mux.HandleFunc("GET /api/library/images", s.browseImages)
	mux.HandleFunc("GET /api/library/images/{id}", s.imageDetail)
	mux.HandleFunc("GET /api/library/jokes", s.browseJokes)
	mux.HandleFunc("GET /api/library/favorites", s.requireLevel(1, s.listFavorites))
	mux.HandleFunc("PUT /api/library/favorites/{id}", s.requireLevel(1, s.addFavorite))
	mux.HandleFunc("DELETE /api/library/favorites/{id}", s.requireLevel(1, s.removeFavorite))
	mux.HandleFunc("POST /api/library/search", s.requireLevel(1, s.searchImages))
	mux.HandleFunc("POST /api/library/uploads", s.requireLevel(1, s.uploadImage))
	mux.HandleFunc("GET /api/library/mine", s.requireLevel(1, s.myImages))
	mux.HandleFunc("GET /api/library/review-queue", s.requireLevel(1, s.reviewQueue))
	mux.HandleFunc("POST /api/library/reviews", s.requireLevel(1, s.reviewImage))
	mux.HandleFunc("POST /api/library/reports", s.requireLevel(1, s.reportImage))
	mux.HandleFunc("GET /api/library/reports", s.requireLevel(1, s.listReports))
	mux.HandleFunc("POST /api/library/reports/{id}", s.requireLevel(1, s.decideReport))
	mux.HandleFunc("POST /api/library/downloads/{id}", s.requireLevel(1, s.downloadImage))
	mux.HandleFunc("GET /api/library/downloads/{id}", s.requireLevel(1, s.downloadTicket))
	mux.HandleFunc("GET /api/library/previews/{id}", s.previewImage)
	mux.HandleFunc("POST /api/library/admin-applications", s.requireLevel(1, s.applyAdmin))
	mux.HandleFunc("GET /api/library/admin-applications", s.requireLevel(1, s.listApplications))
	mux.HandleFunc("POST /api/library/admin-applications/{id}", s.requireLevel(1, s.decideApplication))
	mux.HandleFunc("POST /api/library/moderate/{id}", s.requireLevel(1, s.moderateImage))
	mux.HandleFunc("GET /api/library/ledger", s.requireLevel(1, s.quotaLedger))
	mux.HandleFunc("POST /api/library/payments", s.requireLevel(1, s.libraryPayment))
	mux.HandleFunc("GET /api/library/payments/{orderNo}", s.requireLevel(1, s.paymentStatus))
	mux.HandleFunc("GET /api/library/orders", s.requireLevel(1, s.orderRecords))
	mux.HandleFunc("POST /api/payments/notify/huifu", s.huifuNotify)
	mux.HandleFunc("GET /api/payments/notify/huifu", s.huifuNotify)
	mux.HandleFunc("GET /api/payments/products", s.paymentProducts)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]any{"ok": true, "service": "saotu-osh", "version": "1.0.0"})
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		switch name {
		case "index.html", "style.css", "app.js", "hero.png", "web-vendor/qrcode.min.js":
		default:
			http.NotFound(w, r)
			return
		}
		data, err := assets.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(name)))
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(data)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if r.Method != "GET" && r.Method != "HEAD" && r.URL.Path != "/api/payments/notify/huifu" {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, e := url.Parse(origin)
				if e != nil || u.Host != r.Host {
					apiError(w, 403, "请求来源不正确")
					return
				}
			}
			limit := int64(1 << 20)
			if r.URL.Path == "/api/library/uploads" {
				limit = 16 << 20
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		mux.ServeHTTP(w, r)
	})
}
func main() {
	port := flag.Int("port", 18887, "HTTP port")
	dbPath := flag.String("app-db", "saotu.sqlite3", "SQLite path")
	media := flag.String("media-dir", "media", "private image storage")
	flag.Parse()
	db, err := sql.Open("sqlite", "file:"+*dbPath+"?mode=rwc")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err = initAppDB(db); err != nil {
		log.Fatal(err)
	}
	if err = initLibraryDB(db); err != nil {
		log.Fatal(err)
	}
	if err = os.MkdirAll(*media, 0700); err != nil {
		log.Fatal(err)
	}
	if err = seedInitialLibrary(db, *media); err != nil {
		log.Fatal(err)
	}
	founder, _ := strconv.ParseInt(os.Getenv("FOUNDER_USER_ID"), 10, 64)
	provider := strings.ToLower(os.Getenv("PAY_PROVIDER"))
	if provider == "" {
		provider = "disabled"
	}
	s := &server{appDB: db, mediaDir: *media, payProvider: provider, authBridge: strings.TrimRight(os.Getenv("AUTH_BRIDGE"), "/"), founderID: founder}
	if provider == "huifu" {
		s.huifu = newHuifuPayClientFromEnv()
	}
	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", *port), Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second}
	log.Printf("每日骚图 http://%s", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}
func resourcePrice(kind string) int64 {
	if kind == "video" {
		return 5 * pointScale
	}
	if kind == "image" {
		return pointScale
	}
	return 0
}
