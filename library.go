package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func initLibraryDB(db *sql.DB) error {
	for _, q := range []string{
		`create table if not exists library_accounts(user_id integer primary key references users(id),role text not null default 'user',search_bonus integer not null default 0 check(search_bonus>=0),download_bonus integer not null default 0 check(download_bonus>=0),upload_bonus integer not null default 0 check(upload_bonus>=0),review_count integer not null default 0)`,
		`create table if not exists library_daily(user_id integer not null,day text not null,search_used integer not null default 0,download_used integer not null default 0,upload_used integer not null default 0,primary key(user_id,day))`,
		`create table if not exists library_images(id integer primary key autoincrement,user_id integer not null references users(id),title text not null,tags text not null,keywords text not null,scene text not null,description text not null,source text not null,category text not null,sha256 text not null unique,filename text not null,preview text not null,width integer not null,height integer not null,bytes integer not null,status text not null default 'pending',reason text not null default '',downloads integer not null default 0,created_at text not null,reviewed_at text)`,
		`create index if not exists library_images_status on library_images(status,id)`,
		`create virtual table if not exists library_fts using fts5(title,tags,keywords,scene,description,tokenize='trigram')`,
		`create table if not exists library_reviews(image_id integer not null references library_images(id),user_id integer not null references users(id),decision text not null,reason text not null,multiplier integer not null,effective integer not null default 0,created_at text not null,primary key(image_id,user_id))`,
		`create table if not exists library_ledger(id integer primary key autoincrement,user_id integer not null,kind text not null,amount integer not null,source text not null,created_at text not null,unique(user_id,kind,source))`,
		`create table if not exists library_searches(token text primary key,user_id integer not null,query text not null,category text not null,created_at text not null)`,
		`create table if not exists library_downloads(token text primary key,user_id integer not null,image_id integer not null,created_at text not null)`,
		`create table if not exists library_admin_applications(id integer primary key autoincrement,user_id integer not null,reason text not null,status text not null default 'pending',decision_reason text not null default '',created_at text not null,decided_at text,decided_by integer)`,
		`create table if not exists library_reports(id integer primary key autoincrement,image_id integer not null references library_images(id),user_id integer not null references users(id),reason text not null,status text not null default 'pending',created_at text not null,decided_at text,decided_by integer,unique(image_id,user_id))`,
		`create index if not exists library_reports_pending on library_reports(status,id)`,
		`create table if not exists library_favorites(user_id integer not null references users(id),image_id integer not null references library_images(id),created_at text not null,primary key(user_id,image_id))`,
		`create index if not exists library_favorites_image on library_favorites(image_id)`,
		`create unique index if not exists library_application_pending on library_admin_applications(user_id) where status='pending'`,
		`update member_plans set status=0 where plan_code not like 'saotu_%'`, `update point_packages set status=0`,
	} {
		if _, e := db.Exec(q); e != nil {
			return e
		}
	}
	for i, p := range []struct {
		code, name string
		months     any
		price      int
	}{{"saotu_month", "每日骚图月会员", 1, 100}, {"saotu_year", "每日骚图年会员", 12, 800}, {"saotu_lifetime", "每日骚图永久会员", nil, 1800}} {
		if _, e := db.Exec(`insert into member_plans(plan_code,plan_name,duration_months,price_cents,monthly_points,status,sort,created_at,updated_at) values(?,?,?,?,0,1,?,?,?) on conflict(plan_code) do nothing`, p.code, p.name, p.months, p.price, i+1, appNow(), appNow()); e != nil {
			return e
		}
	}
	return nil
}
func dayKey() string { return time.Now().UTC().Add(8 * time.Hour).Format("2006-01-02") }
func (s *server) ensureLibraryAccount(id int64) error {
	_, e := s.appDB.Exec(`insert into library_accounts(user_id) values(?) on conflict do nothing`, id)
	return e
}
func (s *server) libraryRole(id int64) string {
	if s.founderID > 0 && id == s.founderID {
		return "founder"
	}
	var role string
	s.appDB.QueryRow(`select role from library_accounts where user_id=?`, id).Scan(&role)
	if role == "admin" {
		return role
	}
	return "user"
}
func premiumTx(tx *sql.Tx, id int64) bool {
	var n int
	tx.QueryRow(`select count(*) from user_memberships where user_id=? and plan_code like 'saotu_%' and status='active' and (expire_at is null or expire_at>?)`, id, appNow()).Scan(&n)
	return n > 0
}
func (s *server) libraryPremium(id int64) bool {
	var n int
	s.appDB.QueryRow(`select count(*) from user_memberships where user_id=? and plan_code like 'saotu_%' and status='active' and (expire_at is null or expire_at>?)`, id, appNow()).Scan(&n)
	return n > 0
}

// Daily quota is spent first. Contributions persist across dates, and every change has a unique receipt.
func consumeQuota(tx *sql.Tx, id int64, kind, source string) error {
	if kind != "search" && kind != "download" && kind != "upload" {
		return errors.New("额度类型不正确")
	}
	if kind != "upload" && premiumTx(tx, id) {
		return nil
	}
	if _, e := tx.Exec(`insert into library_accounts(user_id) values(?) on conflict do nothing`, id); e != nil {
		return e
	}
	if _, e := tx.Exec(`insert into library_daily(user_id,day) values(?,?) on conflict do nothing`, id, dayKey()); e != nil {
		return e
	}
	res, e := tx.Exec(`update library_daily set `+kind+`_used=`+kind+`_used+1 where user_id=? and day=? and `+kind+`_used<1`, id, dayKey())
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		res, e = tx.Exec(`update library_accounts set `+kind+`_bonus=`+kind+`_bonus-1 where user_id=? and `+kind+`_bonus>0`, id)
		if e != nil {
			return e
		}
		n, _ = res.RowsAffected()
		if n == 0 {
			return errors.New("今天的额度用完了，可以投稿、参与审核或开通会员")
		}
	}
	_, e = tx.Exec(`insert into library_ledger(user_id,kind,amount,source,created_at) values(?,?,-1,?,?)`, id, kind, source, appNow())
	return e
}
func reward(tx *sql.Tx, id int64, kind string, amount int, source string) error {
	if kind != "search" && kind != "download" && kind != "upload" {
		return errors.New("额度类型不正确")
	}
	if _, e := tx.Exec(`insert into library_accounts(user_id) values(?) on conflict do nothing`, id); e != nil {
		return e
	}
	res, e := tx.Exec(`insert into library_ledger(user_id,kind,amount,source,created_at) values(?,?,?,?,?) on conflict do nothing`, id, kind, amount, source, appNow())
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil
	}
	_, e = tx.Exec(`update library_accounts set `+kind+`_bonus=`+kind+`_bonus+? where user_id=?`, amount, id)
	return e
}
func (s *server) quota(id int64) map[string]any {
	var a, b, c, count int
	s.appDB.QueryRow(`select search_bonus,download_bonus,upload_bonus,review_count from library_accounts where user_id=?`, id).Scan(&a, &b, &c, &count)
	var x, y, z int
	s.appDB.QueryRow(`select search_used,download_used,upload_used from library_daily where user_id=? and day=?`, id, dayKey()).Scan(&x, &y, &z)
	return map[string]any{"search": a + max(0, 1-x), "download": b + max(0, 1-y), "upload": c + max(0, 1-z), "reviewCount": count, "premium": s.libraryPremium(id), "role": s.libraryRole(id), "resetTimezone": "Asia/Shanghai", "day": dayKey()}
}
func (s *server) libraryBootstrap(w http.ResponseWriter, r *http.Request) {
	var count, pending int
	s.appDB.QueryRow(`select count(*) from library_images where status='approved'`).Scan(&count)
	s.appDB.QueryRow(`select count(*) from library_images where status='pending'`).Scan(&pending)
	var u *currentUser
	var quota any
	if user, e := s.currentUser(r); e == nil {
		u = user
		if e = s.ensureLibraryAccount(u.ID); e != nil {
			apiError(w, 500, "账户读取失败")
			return
		}
		quota = s.quota(u.ID)
	}
	plans, e := s.loadMemberPlans()
	if e != nil {
		apiError(w, 500, "套餐读取失败")
		return
	}
	payment := s.payProvider == "huifu" && s.huifu != nil && s.huifu.validateConfiguration() == nil
	jsonResponse(w, map[string]any{"user": u, "quota": quota, "count": count, "pending": pending, "plans": plans, "paymentEnabled": payment, "sharedAccounts": s.authBridge != ""})
}

type libraryImage struct {
	ID          int64  `json:"id"`
	UserID      int64  `json:"-"`
	Title       string `json:"title"`
	Tags        string `json:"tags"`
	Keywords    string `json:"keywords"`
	Scene       string `json:"scene"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Category    string `json:"category"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Bytes       int    `json:"bytes"`
	Status      string `json:"status"`
	Reason      string `json:"reason"`
	Downloads   int    `json:"downloads"`
	Created     string `json:"createdAt"`
	Author      string `json:"author"`
	Votes       int    `json:"votes"`
}

const imageColumns = `i.id,i.user_id,i.title,i.tags,i.keywords,i.scene,i.description,i.source,i.category,i.width,i.height,i.bytes,i.status,i.reason,i.downloads,i.created_at,u.username,(select count(*) from library_reviews where image_id=i.id)`

func (s *server) imageList(where string, args []any, page int) ([]libraryImage, int, error) {
	var total int
	e := s.appDB.QueryRow(`select count(*) from library_images i where `+where, args...).Scan(&total)
	if e != nil {
		return nil, 0, e
	}
	params := append(append([]any{}, args...), 24, (page-1)*24)
	rows, e := s.appDB.Query(`select `+imageColumns+` from library_images i join users u on u.id=i.user_id where `+where+` order by i.id desc limit ? offset ?`, params...)
	if e != nil {
		return nil, 0, e
	}
	defer rows.Close()
	items := []libraryImage{}
	for rows.Next() {
		var i libraryImage
		if e = rows.Scan(&i.ID, &i.UserID, &i.Title, &i.Tags, &i.Keywords, &i.Scene, &i.Description, &i.Source, &i.Category, &i.Width, &i.Height, &i.Bytes, &i.Status, &i.Reason, &i.Downloads, &i.Created, &i.Author, &i.Votes); e != nil {
			return nil, 0, e
		}
		items = append(items, i)
	}
	return items, total, rows.Err()
}
func pageNumber(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	return max(1, min(n, 100000))
}
func (s *server) sendImages(w http.ResponseWriter, r *http.Request, where string, args []any) {
	items, count, e := s.imageList(where, args, pageNumber(r))
	if e != nil {
		apiError(w, 500, "图片读取失败")
		return
	}
	jsonResponse(w, map[string]any{"items": items, "total": count, "page": pageNumber(r), "pages": max(1, (count+23)/24)})
}
func (s *server) browseImages(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("q") != "" || r.URL.Query().Get("category") != "" {
		apiError(w, 400, "请通过搜索入口查询")
		return
	}
	token := r.URL.Query().Get("searchToken")
	if token == "" {
		s.sendImages(w, r, "i.status='approved'", nil)
		return
	}
	u, e := s.currentUser(r)
	if e != nil {
		apiError(w, 401, "请先登录")
		return
	}
	var q, cat, created string
	e = s.appDB.QueryRow(`select query,category,created_at from library_searches where token=? and user_id=?`, token, u.ID).Scan(&q, &cat, &created)
	at, _ := time.Parse(time.RFC3339Nano, created)
	if e != nil || time.Since(at) > 30*time.Minute {
		apiError(w, 410, "搜索结果已过期，请重新查询")
		return
	}
	where, args := searchWhere(q, cat)
	s.sendImages(w, r, where, args)
}
func (s *server) imageDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		apiError(w, 404, "图片不存在")
		return
	}
	items, _, err := s.imageList("i.status='approved' and i.id=?", []any{id}, 1)
	if err != nil {
		apiError(w, 500, "图片读取失败")
		return
	}
	if len(items) == 0 {
		apiError(w, 404, "图片不存在或已下架")
		return
	}
	jsonResponse(w, map[string]any{"item": items[0]})
}
func searchWhere(q, cat string) (string, []any) {
	where := "i.status='approved'"
	args := []any{}
	if cat != "" {
		where += " and i.category=?"
		args = append(args, cat)
	}
	// Unicode words are combined with AND; trigram FTS indexes long terms, two-character Chinese terms use instr.
	words := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	for _, word := range words {
		if len([]rune(word)) >= 3 {
			where += " and i.id in (select rowid from library_fts where library_fts match ?)"
			args = append(args, `"`+strings.ReplaceAll(word, `"`, `""`)+`"`)
		} else {
			where += " and instr(lower(i.title||' '||i.tags||' '||i.keywords||' '||i.scene||' '||i.description),?)>0"
			args = append(args, word)
		}
	}
	return where, args
}
func (s *server) searchImages(w http.ResponseWriter, r *http.Request, u *currentUser) {
	var in struct {
		Query    string `json:"query"`
		Category string `json:"category"`
	}
	if decodeJSON(r, &in) != nil {
		apiError(w, 400, "查询格式不正确")
		return
	}
	in.Query = strings.TrimSpace(in.Query)
	if len([]rune(in.Query)) > 80 || len(strings.Fields(in.Query)) > 12 || !validCategory(in.Category, true) || (in.Query == "" && in.Category == "") {
		apiError(w, 400, "请输入关键词或选择分类，关键词最多 80 字")
		return
	}
	where, args := searchWhere(in.Query, in.Category)
	items, count, e := s.imageList(where, args, 1)
	if e != nil {
		apiError(w, 500, "查询失败，额度未扣除")
		return
	}
	token, e := randomToken(24)
	if e != nil {
		apiError(w, 500, "查询创建失败")
		return
	}
	tx, e := s.appDB.BeginTx(r.Context(), nil)
	if e != nil {
		apiError(w, 500, "查询创建失败")
		return
	}
	defer tx.Rollback()
	if e = consumeQuota(tx, u.ID, "search", "search:"+token); e != nil {
		apiError(w, 429, e.Error())
		return
	}
	if _, e = tx.Exec(`insert into library_searches values(?,?,?,?,?)`, token, u.ID, in.Query, in.Category, appNow()); e != nil {
		apiError(w, 500, "查询保存失败")
		return
	}
	if e = tx.Commit(); e != nil {
		apiError(w, 500, "查询保存失败")
		return
	}
	jsonResponse(w, map[string]any{"items": items, "total": count, "page": 1, "pages": max(1, (count+23)/24), "searchToken": token, "quota": s.quota(u.ID)})
}
func validCategory(c string, empty bool) bool {
	return (empty && c == "") || c == "吊图" || c == "搞图" || c == "骚图" || c == "表情包" || c == "其他"
}
func (s *server) myImages(w http.ResponseWriter, r *http.Request, u *currentUser) {
	s.sendImages(w, r, "i.user_id=?", []any{u.ID})
}
func (s *server) reviewQueue(w http.ResponseWriter, r *http.Request, u *currentUser) {
	s.sendImages(w, r, "i.status='pending' and i.user_id<>? and not exists(select 1 from library_reviews v where v.image_id=i.id and v.user_id=?)", []any{u.ID, u.ID})
}
func (s *server) quotaLedger(w http.ResponseWriter, r *http.Request, u *currentUser) {
	rows, e := s.appDB.Query(`select kind,amount,source,created_at from library_ledger where user_id=? order by id desc limit 100`, u.ID)
	if e != nil {
		apiError(w, 500, "额度记录读取失败")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var kind, source, at string
		var amount int
		rows.Scan(&kind, &amount, &source, &at)
		items = append(items, map[string]any{"kind": kind, "amount": amount, "source": source, "createdAt": at})
	}
	jsonResponse(w, map[string]any{"items": items})
}
func (s *server) libraryPayment(w http.ResponseWriter, r *http.Request, u *currentUser) {
	var in struct {
		PlanCode string `json:"planCode"`
		Channel  string `json:"channel,omitempty"`
	}
	if decodeJSON(r, &in) != nil {
		apiError(w, 400, "请求格式不正确")
		return
	}
	if in.PlanCode != "saotu_month" && in.PlanCode != "saotu_year" && in.PlanCode != "saotu_lifetime" {
		apiError(w, 400, "请选择有效的会员套餐")
		return
	}
	b, _ := json.Marshal(paymentPurchaseRequest{OrderType: "subscription", PlanCode: in.PlanCode, Channel: in.Channel})
	c := r.Clone(r.Context())
	c.Body = &byteReadCloser{bytes.NewReader(b)}
	s.createPaymentOrder(w, c, u)
}

type byteReadCloser struct{ *bytes.Reader }

func (b *byteReadCloser) Close() error { return nil }

var _ = fmt.Sprint
