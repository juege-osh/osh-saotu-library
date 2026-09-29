package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLibrary(t *testing.T) *server {
	t.Helper()
	db, e := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.sqlite3"))
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if e = initAppDB(db); e != nil {
		t.Fatal(e)
	}
	if e = initLibraryDB(db); e != nil {
		t.Fatal(e)
	}
	s := &server{appDB: db, mediaDir: t.TempDir(), founderID: 1, payProvider: "mock"}
	for id := int64(1); id <= 6; id++ {
		_, e = db.Exec(`insert into users(id,username,email,password_hash,created_at,updated_at) values(?,?,?,'shared-account',?,?)`, id, fmt.Sprintf("user%d", id), fmt.Sprintf("u%d@example.invalid", id), appNow(), appNow())
		if e != nil {
			t.Fatal(e)
		}
		if e = s.ensureLibraryAccount(id); e != nil {
			t.Fatal(e)
		}
	}
	return s
}
func user(id int64) *currentUser {
	return &currentUser{ID: id, Level: 1, Username: fmt.Sprintf("user%d", id), Email: fmt.Sprintf("u%d@example.invalid", id)}
}
func call(s *server, id int64, h func(http.ResponseWriter, *http.Request, *currentUser), path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h(w, r, user(id))
	return w
}
func seedImage(t *testing.T, s *server, owner int64, status string) int64 {
	t.Helper()
	res, e := s.appDB.Exec(`insert into library_images(user_id,title,tags,keywords,scene,description,source,category,sha256,filename,preview,width,height,bytes,status,created_at) values(?,'周一摸鱼','猫猫 打工人','上班 office','群聊 接梗','疲惫的星期一','原创','搞图',?,?,?,100,100,3,?,?)`, owner, fmt.Sprintf("hash-%d-%d", owner, time.Now().UnixNano()), "test.png", "preview.jpg", status, appNow())
	if e != nil {
		t.Fatal(e)
	}
	id, _ := res.LastInsertId()
	_, e = s.appDB.Exec(`insert into library_fts(rowid,title,tags,keywords,scene,description) values(?,'周一摸鱼','猫猫 打工人','上班 office','群聊 接梗','疲惫的星期一')`, id)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(s.mediaDir, "test.png"), []byte("img"), 0600)
	return id
}
func TestFavoritesArePrivateAndHideWithImage(t *testing.T) {
	s := testLibrary(t)
	approved := seedImage(t, s, 2, "approved")
	pending := seedImage(t, s, 2, "pending")
	request := func(method, path string, id int64, h func(http.ResponseWriter, *http.Request, *currentUser)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.SetPathValue("id", path[strings.LastIndex(path, "/")+1:])
		w := httptest.NewRecorder()
		h(w, r, user(id))
		return w
	}
	if w := request("PUT", fmt.Sprintf("/api/library/favorites/%d", pending), 3, s.addFavorite); w.Code != 404 {
		t.Fatal("pending image saved", w.Body.String())
	}
	path := fmt.Sprintf("/api/library/favorites/%d", approved)
	for n := 0; n < 2; n++ {
		if w := request("PUT", path, 3, s.addFavorite); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	list := request("GET", "/api/library/favorites", 3, s.listFavorites)
	var result struct {
		IDs   []int64 `json:"ids"`
		Total int     `json:"total"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &result); err != nil || result.Total != 1 || len(result.IDs) != 1 || result.IDs[0] != approved {
		t.Fatal(list.Body.String(), err)
	}
	other := request("GET", "/api/library/favorites", 4, s.listFavorites)
	if !strings.Contains(other.Body.String(), `"total":0`) {
		t.Fatal("favorite leaked to another user", other.Body.String())
	}
	s.appDB.Exec(`update library_images set status='hidden' where id=?`, approved)
	hidden := request("GET", "/api/library/favorites", 3, s.listFavorites)
	if !strings.Contains(hidden.Body.String(), `"total":0`) {
		t.Fatal("hidden image remained in favorites", hidden.Body.String())
	}
	if w := request("DELETE", path, 3, s.removeFavorite); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
func TestPunctuationOnlySearchDoesNotSpendQuota(t *testing.T) {
	s := testLibrary(t)
	seedImage(t, s, 2, "approved")
	w := call(s, 3, s.searchImages, "/api/library/search", `{"query":"？！...","category":""}`)
	if w.Code != http.StatusBadRequest {
		t.Fatal("punctuation-only query accepted", w.Code, w.Body.String())
	}
	if got := s.quota(3)["search"]; got != 1 {
		t.Fatal("invalid query spent search quota", got)
	}
}
func TestQuotaAtomicAndPersistent(t *testing.T) {
	s := testLibrary(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tx, e := s.appDB.Begin()
			if e != nil {
				return
			}
			defer tx.Rollback()
			if e = consumeQuota(tx, 2, "download", fmt.Sprint(n)); e == nil {
				if tx.Commit() == nil {
					mu.Lock()
					success++
					mu.Unlock()
				}
			}
		}(n)
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("overspent daily quota: %d", success)
	}
	tx, _ := s.appDB.Begin()
	if e := reward(tx, 2, "download", 10, "approved:1"); e != nil {
		t.Fatal(e)
	}
	reward(tx, 2, "download", 10, "approved:1")
	tx.Commit()
	if s.quota(2)["download"].(int) != 10 {
		t.Fatal("duplicate reward")
	}
	s.appDB.Exec(`update library_daily set day='2000-01-01' where user_id=2`)
	if s.quota(2)["download"].(int) != 11 {
		t.Fatal("daily reset lost bonus")
	}
}
func TestConsensusRewardsAndSelfReview(t *testing.T) {
	s := testLibrary(t)
	id := seedImage(t, s, 2, "pending")
	body := fmt.Sprintf(`{"imageId":%d,"decision":"approved","reason":""}`, id)
	if call(s, 2, s.reviewImage, "/reviews", body).Code != 403 {
		t.Fatal("self review")
	}
	for _, v := range []int64{3, 4} {
		if w := call(s, v, s.reviewImage, "/reviews", body); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	if s.quota(2)["download"].(int) != 1 {
		t.Fatal("premature reward")
	}
	if call(s, 3, s.reviewImage, "/reviews", body).Code != 409 {
		t.Fatal("duplicate review")
	}
	if w := call(s, 5, s.reviewImage, "/reviews", body); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if s.quota(2)["download"].(int) != 11 || s.quota(2)["search"].(int) != 2 {
		t.Fatal("upload rewards")
	}
	for _, v := range []int64{3, 4, 5} {
		q := s.quota(v)
		if q["reviewCount"].(int) != 1 || q["download"].(int) != 2 || q["upload"].(int) != 2 {
			t.Fatal(q)
		}
	}
	if call(s, 6, s.reviewImage, "/reviews", body).Code != 409 {
		t.Fatal("finalized image reviewed again")
	}
}
func TestAdminDoubleAndApplications(t *testing.T) {
	s := testLibrary(t)
	s.appDB.Exec(`update library_accounts set role='admin' where user_id=3`)
	id := seedImage(t, s, 2, "pending")
	w := call(s, 3, s.reviewImage, "/reviews", fmt.Sprintf(`{"imageId":%d,"decision":"approved"}`, id))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	q := s.quota(3)
	if q["download"].(int) != 3 || q["upload"].(int) != 3 {
		t.Fatal(q)
	}
	if call(s, 4, s.applyAdmin, "/apply", `{"reason":"愿意帮忙"}`).Code != 403 {
		t.Fatal("ineligible application")
	}
	s.appDB.Exec(`update library_accounts set review_count=10 where user_id=4`)
	if w = call(s, 4, s.applyAdmin, "/apply", `{"reason":"愿意帮忙"}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if call(s, 4, s.applyAdmin, "/apply", `{"reason":"重复"}`).Code != 409 {
		t.Fatal("duplicate application")
	}
	r := httptest.NewRequest("POST", "/applications/1", strings.NewReader(`{"approve":true}`))
	r.SetPathValue("id", "1")
	wr := httptest.NewRecorder()
	s.decideApplication(wr, r, user(3))
	if wr.Code != 403 {
		t.Fatal("admin authorized another admin")
	}
	wr = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/applications/1", strings.NewReader(`{"approve":true}`))
	r.SetPathValue("id", "1")
	s.decideApplication(wr, r, user(1))
	if wr.Code != 200 || s.libraryRole(4) != "admin" {
		t.Fatal(wr.Body.String())
	}
}
func TestRejectionRewardsOnlyMatchingVotes(t *testing.T) {
	s := testLibrary(t)
	id := seedImage(t, s, 2, "pending")
	call(s, 3, s.reviewImage, "/", fmt.Sprintf(`{"imageId":%d,"decision":"approved"}`, id))
	for _, v := range []int64{4, 5} {
		w := call(s, v, s.reviewImage, "/", fmt.Sprintf(`{"imageId":%d,"decision":"rejected","reason":"来源不明"}`, id))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	if s.quota(2)["download"].(int) != 1 || s.quota(3)["reviewCount"].(int) != 0 || s.quota(4)["reviewCount"].(int) != 1 {
		t.Fatal("wrong reject reward")
	}
}
func TestSearchChineseAndPagination(t *testing.T) {
	s := testLibrary(t)
	seedImage(t, s, 2, "approved")
	for _, q := range []string{"摸鱼", "打工人", "office", "上班 摸鱼"} {
		where, args := searchWhere(q, "")
		items, _, e := s.imageList(where, args, 1)
		if e != nil || len(items) != 1 {
			t.Fatalf("search %s: %v %d", q, e, len(items))
		}
	}
	w := call(s, 3, s.searchImages, "/search", `{"query":"摸鱼","category":"搞图"}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if call(s, 3, s.searchImages, "/search", `{"query":"猫猫"}`).Code != 429 {
		t.Fatal("search limit")
	}
	r := httptest.NewRequest("GET", "/api/library/images?q=摸鱼", nil)
	wr := httptest.NewRecorder()
	s.browseImages(wr, r)
	if wr.Code != 400 {
		t.Fatal("search bypass")
	}
	if s.quota(3)["search"].(int) != 0 {
		t.Fatal("wrong search quota")
	}
}
func TestDownloadOwnershipRetriesAndModeration(t *testing.T) {
	s := testLibrary(t)
	id := seedImage(t, s, 2, "approved")
	download := func(uid int64) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/downloads", nil)
		r.SetPathValue("id", fmt.Sprint(id))
		w := httptest.NewRecorder()
		s.downloadImage(w, r, user(uid))
		return w
	}
	w := download(3)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var d map[string]any
	json.Unmarshal(w.Body.Bytes(), &d)
	url := d["url"].(string)
	w = download(3)
	if w.Code != 200 || s.quota(3)["download"].(int) != 0 {
		t.Fatal("retry charged twice")
	}
	r := httptest.NewRequest("GET", url, nil)
	r.SetPathValue("id", fmt.Sprint(id))
	wr := httptest.NewRecorder()
	s.downloadTicket(wr, r, user(4))
	if wr.Code != 403 {
		t.Fatal("stolen download receipt")
	}
	wr = httptest.NewRecorder()
	s.downloadTicket(wr, r, user(3))
	if wr.Code != 200 {
		t.Fatal(wr.Body.String())
	}
	s.appDB.Exec(`update library_images set status='hidden' where id=?`, id)
	wr = httptest.NewRecorder()
	s.downloadTicket(wr, r, user(3))
	if wr.Code != 403 {
		t.Fatal("hidden image downloadable")
	}
}
func TestUploadValidationDuplicateAndQuota(t *testing.T) {
	s := testLibrary(t)
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	var data bytes.Buffer
	png.Encode(&data, img)
	send := func(uid int64, payload []byte) *httptest.ResponseRecorder {
		var body bytes.Buffer
		m := multipart.NewWriter(&body)
		for k, v := range map[string]string{"title": "猫猫", "category": "搞图", "source": "原创", "rights": "yes"} {
			m.WriteField(k, v)
		}
		f, _ := m.CreateFormFile("image", "cat.png")
		f.Write(payload)
		m.Close()
		r := httptest.NewRequest("POST", "/uploads", &body)
		r.Header.Set("Content-Type", m.FormDataContentType())
		w := httptest.NewRecorder()
		s.uploadImage(w, r, user(uid))
		return w
	}
	if send(2, []byte("not an image")).Code != 400 {
		t.Fatal("invalid image accepted")
	}
	if w := send(3, bytes.Repeat([]byte{'x'}, 13<<20)); w.Code != 400 || !strings.Contains(w.Body.String(), "最大 12 MB") {
		t.Fatal("oversized multipart body accepted", w.Code)
	}
	if w := send(2, data.Bytes()); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if send(3, data.Bytes()).Code != 409 {
		t.Fatal("duplicate accepted")
	}
	if s.quota(3)["upload"].(int) != 1 {
		t.Fatal("duplicate charged")
	}
	var n int
	s.appDB.QueryRow(`select count(*) from library_images where status='pending'`).Scan(&n)
	if n != 1 {
		t.Fatal(n)
	}
	if s.quota(2)["download"].(int) != 1 {
		t.Fatal("unapproved upload rewarded")
	}
}
func TestWithdrawPendingRestoresUploadCreditOnce(t *testing.T) {
	s := testLibrary(t)
	id := seedImage(t, s, 2, "pending")
	tx, err := s.appDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = consumeQuota(tx, 2, "upload", "upload:test-withdraw"); err != nil || tx.Commit() != nil {
		t.Fatal("setup upload quota", err)
	}
	request := func(userID int64) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/library/mine/1/withdraw", nil)
		r.SetPathValue("id", fmt.Sprint(id))
		w := httptest.NewRecorder()
		s.withdrawPending(w, r, user(userID))
		return w
	}
	if w := request(3); w.Code != 409 {
		t.Fatal("other user withdrew image", w.Code)
	}
	if w := request(2); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if got := s.quota(2)["upload"].(int); got != 1 {
		t.Fatal("credit not restored", got)
	}
	if w := request(2); w.Code != 409 || s.quota(2)["upload"].(int) != 1 {
		t.Fatal("duplicate withdrawal credited", w.Code)
	}
	var status string
	s.appDB.QueryRow(`select status from library_images where id=?`, id).Scan(&status)
	if status != "hidden" {
		t.Fatal("withdrawn image still public", status)
	}
}
func TestPremiumExpiryAndPrices(t *testing.T) {
	s := testLibrary(t)
	plans, e := s.loadMemberPlans()
	if e != nil || len(plans) != 3 {
		t.Fatal(e, plans)
	}
	for _, p := range plans {
		want := map[string]int{"saotu_month": 100, "saotu_year": 800, "saotu_lifetime": 1800}[p.PlanCode]
		if p.PriceCents != want {
			t.Fatal(p)
		}
	}
	tx, _ := s.appDB.Begin()
	_, e = s.grantMembershipEntitlementTx(tx, 2, "saotu_month", "test", "membership:test")
	if e != nil {
		t.Fatal(e)
	}
	tx.Commit()
	for n := 0; n < 5; n++ {
		tx, _ = s.appDB.Begin()
		if e = consumeQuota(tx, 2, "search", fmt.Sprint(n)); e != nil {
			t.Fatal(e)
		}
		tx.Commit()
	}
	if s.quota(2)["search"].(int) != 1 {
		t.Fatal("premium spent quota")
	}
	s.appDB.Exec(`update user_memberships set expire_at=? where user_id=2`, appTime(time.Now().Add(-time.Hour)))
	if s.libraryPremium(2) {
		t.Fatal("expired premium")
	}
	if s.libraryPremium(3) {
		t.Fatal("free user premium")
	}
}

func TestPaymentsSettlementAndOwnership(t *testing.T) {
	for _, c := range []struct {
		code   string
		amount int64
	}{{"saotu_month", 100}, {"saotu_year", 800}, {"saotu_lifetime", 1800}} {
		t.Run(c.code, func(t *testing.T) {
			s, u := testLibrary(t), user(1)
			w := call(s, u.ID, s.libraryPayment, "/api/library/payments", fmt.Sprintf(`{"planCode":%q}`, c.code))
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			var out struct {
				Order paymentOrderVO `json:"order"`
			}
			if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
				t.Fatal(e)
			}
			if out.Order.AmountCents != c.amount {
				t.Fatal("wrong price")
			}
			if _, e := s.loadPaymentOrder(999, out.Order.OrderNo); e == nil {
				t.Fatal("other user order accessible")
			}
			if _, e := s.settlePayment(out.Order.PaymentNo, "mock", "trade", c.amount-1, "alipay", "{}", "wrong", true); e == nil {
				t.Fatal("wrong amount accepted")
			}
			for i := 0; i < 2; i++ {
				if _, e := s.settlePayment(out.Order.PaymentNo, "mock", "trade", c.amount, "alipay", "{}", "good", true); e != nil {
					t.Fatal(e)
				}
			}
			var count int
			s.appDB.QueryRow(`select count(*) from user_memberships where user_id=1`).Scan(&count)
			if count != 1 {
				t.Fatal("duplicate entitlement", count)
			}
			if !s.loadMemberStatus(u.ID).Active {
				t.Fatal("membership missing")
			}
		})
	}
}
func TestAuthBridgeCrossOriginAndPrivateMedia(t *testing.T) {
	s, u := testLibrary(t), user(1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, e := r.Cookie(sessionCookieName)
		if e != nil || c.Value != "fixture" {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"authenticated": true, "user": u})
	}))
	defer upstream.Close()
	s.authBridge = upstream.URL
	r := httptest.NewRequest("GET", "/api/library/bootstrap", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "fixture"})
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "user1") {
		t.Fatal(w.Body.String())
	}
	r = httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://attacker.invalid")
	w = httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross origin allowed")
	}
}

func TestInitialSeedProvenanceAndIdempotence(t *testing.T) {
	s := testLibrary(t)
	for n := 0; n < 2; n++ {
		if err := seedInitialLibrary(s.appDB, s.mediaDir); err != nil {
			t.Fatal(err)
		}
	}
	var count, bytes, invalid int
	err := s.appDB.QueryRow(`select count(*),coalesce(sum(bytes),0),sum(case when status!='approved' or user_id!=-1 or (source not like 'https://commons.wikimedia.org/%' and source not like '本站原创%') then 1 else 0 end) from library_images`).Scan(&count, &bytes, &invalid)
	if err != nil || count != 30 || invalid != 0 || bytes > 100_000_000 {
		t.Fatalf("seed state: count=%d bytes=%d invalid=%d err=%v", count, bytes, invalid, err)
	}
	hero, err := assets.ReadFile("hero.png")
	if err != nil || bytes+len(hero) > 100_000_000 {
		t.Fatalf("starter image budget: bytes=%d hero=%d err=%v", bytes, len(hero), err)
	}
	w := httptest.NewRecorder()
	s.browseJokes(w, httptest.NewRequest("GET", "/api/library/jokes", nil))
	var jokes []map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &jokes) != nil || len(jokes) < 20 {
		t.Fatalf("jokes endpoint: code=%d count=%d", w.Code, len(jokes))
	}
	var originalID int64
	var originalHash string
	if err := s.appDB.QueryRow(`select id,sha256 from library_images where title='谁也别说谁'`).Scan(&originalID, &originalHash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.appDB.Exec(`update library_images set sha256='outdated-seed' where id=?`, originalID); err != nil {
		t.Fatal(err)
	}
	if err := seedInitialLibrary(s.appDB, s.mediaDir); err != nil {
		t.Fatal("updated starter image", err)
	}
	var id int64
	var hash string
	if err := s.appDB.QueryRow(`select id,sha256 from library_images where title='谁也别说谁'`).Scan(&id, &hash); err != nil || id != originalID || hash != originalHash {
		t.Fatalf("starter image was duplicated or not repaired: id=%d hash=%s err=%v", id, hash, err)
	}
	if err := s.appDB.QueryRow(`select count(*) from library_images`).Scan(&count); err != nil || count != 30 {
		t.Fatalf("starter image count after repair: %d, %v", count, err)
	}
}
func TestSharePreviewOnlyForApprovedImage(t *testing.T) {
	s := testLibrary(t)
	approved := seedImage(t, s, 2, "approved")
	pending := seedImage(t, s, 2, "pending")
	s.appDB.Exec(`update library_images set title='<script>bad</script>' where id=?`, approved)
	get := func(id int64) string {
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/?image=%d", id), nil))
		if w.Code != 200 {
			t.Fatalf("share page status: %d", w.Code)
		}
		return w.Body.String()
	}
	html := get(approved)
	if !strings.Contains(html, `property="og:image"`) || !strings.Contains(html, fmt.Sprintf("/api/library/previews/%d", approved)) || !strings.Contains(html, `&lt;script&gt;bad&lt;/script&gt;`) {
		t.Fatal("approved image preview missing or unescaped")
	}
	if !strings.Contains(get(pending), `content="https://saotu.osh.lol/hero.png"`) || strings.Contains(get(pending), fmt.Sprintf("/api/library/previews/%d", pending)) {
		t.Fatal("pending image leaked to share preview")
	}
}

func TestSitemapExcludesUnpublishedImages(t *testing.T) {
	s := testLibrary(t)
	approved := seedImage(t, s, 2, "approved")
	pending := seedImage(t, s, 2, "pending")
	hidden := seedImage(t, s, 2, "hidden")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest("GET", "/sitemap.xml", nil))
	content := w.Body.String()
	if w.Code != 200 || !strings.Contains(content, fmt.Sprintf("?image=%d", approved)) || !strings.Contains(content, "?joke=1") || strings.Contains(content, fmt.Sprintf("?image=%d", pending)) || strings.Contains(content, fmt.Sprintf("?image=%d", hidden)) {
		t.Fatal("sitemap publication boundary", w.Code, content)
	}
}

func TestJokeSharePreview(t *testing.T) {
	s := testLibrary(t)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest("GET", "/?joke=1", nil))
	content := w.Body.String()
	if w.Code != 200 || !strings.Contains(content, "老板问我为什么老看表") || !strings.Contains(content, "?joke=1#jokes") {
		t.Fatal("joke share preview missing", w.Code)
	}
	for _, invalid := range []string{"0", "9999", "bad"} {
		w = httptest.NewRecorder()
		s.routes().ServeHTTP(w, httptest.NewRequest("GET", "/?joke="+invalid, nil))
		if strings.Contains(w.Body.String(), "老板问我为什么老看表") {
			t.Fatal("invalid joke leaked a preview", invalid)
		}
	}
}

func TestHealthChecksStorage(t *testing.T) {
	s := testLibrary(t)
	request := func() int {
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, httptest.NewRequest("GET", "/api/health", nil))
		return w.Code
	}
	if code := request(); code != 200 {
		t.Fatal("healthy service rejected", code)
	}
	s.mediaDir = filepath.Join(s.mediaDir, "missing")
	if code := request(); code != 503 {
		t.Fatal("missing media storage not detected", code)
	}
	s.appDB.Close()
	if code := request(); code != 503 {
		t.Fatal("closed database not detected", code)
	}
}

func TestImageDetailPublicOnly(t *testing.T) {
	s := testLibrary(t)
	approved := seedImage(t, s, 1, "approved")
	pending := seedImage(t, s, 2, "pending")
	for _, tc := range []struct {
		id   int64
		code int
	}{{approved, 200}, {pending, 404}} {
		r := httptest.NewRequest("GET", fmt.Sprintf("/api/library/images/%d", tc.id), nil)
		r.SetPathValue("id", fmt.Sprint(tc.id))
		w := httptest.NewRecorder()
		s.imageDetail(w, r)
		if w.Code != tc.code {
			t.Fatalf("image %d: code %d, want %d", tc.id, w.Code, tc.code)
		}
	}
}

func TestRandomImageOnlyReturnsApproved(t *testing.T) {
	s := testLibrary(t)
	pending := seedImage(t, s, 2, "pending")
	w := httptest.NewRecorder()
	s.randomImage(w, httptest.NewRequest("GET", "/api/library/random", nil))
	if w.Code != 404 {
		t.Fatal("pending image appeared in an empty public library", w.Body.String())
	}
	approved := seedImage(t, s, 1, "approved")
	w = httptest.NewRecorder()
	s.randomImage(w, httptest.NewRequest("GET", "/api/library/random", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"id":%d`, approved)) || strings.Contains(w.Body.String(), fmt.Sprintf(`"id":%d`, pending)) {
		t.Fatal("random image did not return the approved item", w.Body.String())
	}
}

func TestReportsAreDeduplicatedAndCanHideImage(t *testing.T) {
	s := testLibrary(t)
	id := seedImage(t, s, 3, "approved")
	body := fmt.Sprintf(`{"imageId":%d,"reason":"这张图片涉及隐私，请核查"}`, id)
	if w := call(s, 2, s.reportImage, "/api/library/reports", body); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := call(s, 2, s.reportImage, "/api/library/reports", body); w.Code != 409 {
		t.Fatal("duplicate report accepted", w.Body.String())
	}
	w := httptest.NewRecorder()
	s.listReports(w, httptest.NewRequest("GET", "/api/library/reports", nil), user(2))
	if w.Code != 403 {
		t.Fatal("ordinary user read report queue")
	}
	w = httptest.NewRecorder()
	s.listReports(w, httptest.NewRequest("GET", "/api/library/reports", nil), user(1))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "涉及隐私") {
		t.Fatal(w.Body.String())
	}
	r := httptest.NewRequest("POST", "/api/library/reports/1", strings.NewReader(`{"decision":"hide"}`))
	r.SetPathValue("id", "1")
	w = httptest.NewRecorder()
	s.decideReport(w, r, user(1))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", fmt.Sprintf("/api/library/images/%d", id), nil)
	r.SetPathValue("id", fmt.Sprint(id))
	s.imageDetail(w, r)
	if w.Code != 404 {
		t.Fatal("hidden image remains public")
	}
}
