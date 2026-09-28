package main

import (
	"database/sql"
	"time"
)

const (
	roleNormal = "normal"
	roleVIP    = "vip"
	roleAdmin  = "admin"

	levelPublic = 0
	levelNormal = 1
	levelVIP    = 2
	levelAdmin  = 3

	registerGiftPoints int64 = 888 * pointScale
)

func appTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func appNow() string {
	return appTime(time.Now())
}

func initAppDB(db *sql.DB) error {
	pragmas := []string{
		"pragma foreign_keys = on",
		"pragma journal_mode = wal",
		"pragma busy_timeout = 5000",
	}
	for _, stmt := range pragmas {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	statements := []string{
		`create table if not exists users (
			id integer primary key autoincrement,
			username text not null,
			email text not null,
			password_hash text not null,
			avatar text,
			status integer not null default 0,
			blocked_reason text,
			role_code text not null default 'normal',
			role_level integer not null default 1,
			vip_expire_time text,
			created_at text not null,
			updated_at text not null,
			delete_flag integer not null default 0 check(delete_flag in (0, 1)),
			deleted_at text
		)`,
		`create table if not exists user_sessions (
			id integer primary key autoincrement,
			user_id integer not null,
			session_hash text not null unique,
			expires_at text not null,
			created_at text not null,
			last_seen_at text,
			updated_at text,
			revoked_at text,
			foreign key(user_id) references users(id)
		)`,
		`create index if not exists idx_user_sessions_user on user_sessions(user_id, expires_at)`,
		`create table if not exists api_permission_rules (
			id integer primary key autoincrement,
			method text not null,
			path_pattern text not null,
			required_level integer not null,
			enabled integer not null default 1,
			remark text,
			created_at text not null,
			updated_at text not null,
			unique(method, path_pattern)
		)`,
		`create table if not exists member_plans (
			id integer primary key autoincrement,
			plan_code text not null unique,
			plan_name text not null,
			duration_months integer,
			price_cents integer not null,
			monthly_points integer not null,
			status integer not null default 1,
			sort integer not null default 0,
			created_at text not null,
			updated_at text not null
		)`,
		`create table if not exists user_memberships (
			id integer primary key autoincrement,
			user_id integer not null,
			plan_code text not null,
			start_at text not null,
			expire_at text,
			status text not null,
			source_type text not null,
			source_no text,
			created_at text not null,
			updated_at text not null,
			foreign key(user_id) references users(id)
		)`,
		`create index if not exists idx_user_memberships_active on user_memberships(user_id, status, expire_at)`,
		`create table if not exists point_accounts (
			user_id integer primary key,
			total_points integer not null default 0,
			grant_points integer not null default 0,
			paid_points integer not null default 0,
			updated_at text not null,
			foreign key(user_id) references users(id)
		)`,
		`create table if not exists point_lots (
			id integer primary key autoincrement,
			user_id integer not null,
			point_type text not null,
			source_type text not null,
			source_no text not null,
			total_amount integer not null,
			remaining_amount integer not null,
			expire_at text,
			status text not null,
			remark text,
			created_at text not null,
			updated_at text not null,
			foreign key(user_id) references users(id)
		)`,
		`create index if not exists idx_point_lots_available on point_lots(user_id, status, expire_at)`,
		`create index if not exists idx_point_lots_source on point_lots(user_id, source_type, source_no)`,
		`create table if not exists point_records (
			id integer primary key autoincrement,
			user_id integer not null,
			change_type text not null,
			point_type text not null,
			biz_type text not null,
			biz_no text not null,
			related_biz_type text,
			related_biz_no text,
			lot_id integer,
			change_amount integer not null,
			grant_amount integer not null default 0,
			paid_amount integer not null default 0,
			before_balance integer not null,
			after_balance integer not null,
			lot_expire_at text,
			remark text,
			created_at text not null,
			foreign key(user_id) references users(id)
		)`,
		`create index if not exists idx_point_records_user on point_records(user_id, created_at desc)`,
		`create index if not exists idx_point_records_biz on point_records(user_id, biz_type, biz_no)`,
		`create table if not exists point_operations (
			id integer primary key autoincrement,
			user_id integer not null,
			biz_type text not null,
			biz_no text not null,
			status text not null,
			created_at text not null,
			updated_at text not null,
			unique(user_id, biz_type, biz_no)
		)`,
		`create table if not exists vip_point_cycles (
			id integer primary key autoincrement,
			user_id integer not null,
			cycle_key text not null,
			cycle_start_at text not null,
			cycle_end_at text not null,
			points integer not null,
			lot_id integer,
			record_id integer,
			source_membership_id integer,
			source_order_no text,
			status text not null,
			created_at text not null,
			unique(user_id, cycle_key)
		)`,
		`create table if not exists email_verification_codes (
			id integer primary key autoincrement,
			email text not null,
			purpose text not null,
			code_hash text not null,
			expires_at text not null,
			used_at text,
			created_at text not null
		)`,
		`create index if not exists idx_email_codes_lookup on email_verification_codes(email, purpose, expires_at)`,
		`create table if not exists orders (
			id integer primary key autoincrement,
			user_id integer not null,
			order_no text not null unique,
			order_type text not null,
			subject text not null,
			amount_cents integer not null default 0,
			points integer not null default 0,
			plan_code text,
			status text not null,
			source_type text,
			source_no text,
			created_at text not null,
			updated_at text not null,
			foreign key(user_id) references users(id)
		)`,
		`create index if not exists idx_orders_user on orders(user_id, created_at desc)`,
		`create table if not exists point_packages (
			id integer primary key autoincrement,
			package_code text not null unique,
			package_name text not null,
			points integer not null,
			price_cents integer not null,
			status integer not null default 1,
			sort integer not null default 0,
			created_at text not null,
			updated_at text not null
		)`,
		`create table if not exists payments (
			id integer primary key autoincrement,
			order_no text not null unique references orders(order_no),
			payment_no text not null unique,
			provider text not null,
			channel text not null,
			amount_cents integer not null,
			status text not null,
			platform_trade_no text,
			pay_url text,
			qrcode text,
			request_payload text,
			response_payload text,
			expire_at text,
			paid_at text,
			created_at text not null,
			updated_at text not null
		)`,
		`create index if not exists idx_payments_status_expire on payments(status, expire_at)`,
		`create table if not exists payment_notify_logs (
			id integer primary key autoincrement,
			provider text not null,
			payment_no text,
			order_no text,
			platform_trade_no text,
			notify_key text not null unique,
			raw_payload text,
			signature_valid integer not null default 0,
			transaction_status text,
			process_status text not null,
			error_msg text,
			created_at text not null
		)`,
		`create index if not exists idx_payment_notify_payment on payment_notify_logs(payment_no, created_at desc)`,
		`create table if not exists resource_unlocks (
			user_id integer not null references users(id) on delete cascade,
			resource_kind text not null check(resource_kind in ('video', 'image')),
			resource_id text not null,
			points integer not null,
			created_at text not null,
			primary key(user_id, resource_kind, resource_id)
		)`,
		`create table if not exists resource_pricing (
			id integer primary key check(id=1),
			multiplier integer not null default 100 check(multiplier between 10 and 100),
			version integer not null default 1,
			updated_at text not null,
			updated_by integer
		)`,
		`create index if not exists idx_resource_unlocks_user_time on resource_unlocks(user_id,created_at desc)`,
		`create table if not exists pricing_changes (
			id integer primary key autoincrement,
			old_multiplier integer not null,
			new_multiplier integer not null,
			version integer not null,
			operator_id integer not null,
			created_at text not null
		)`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	if err := ensureColumn(db, "users", "blocked_reason", "text"); err != nil {
		return err
	}
	if err := migrateUserDeletion(db); err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"attempts", "integer not null default 0"},
		{"delivery_status", "text not null default 'sent'"},
		{"request_ip", "text not null default ''"},
	} {
		if err := ensureColumn(db, "email_verification_codes", column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`create index if not exists idx_email_codes_rate on email_verification_codes(request_ip, created_at)`); err != nil {
		return err
	}
	if err := migratePointPrecision(db); err != nil {
		return err
	}
	if _, err := db.Exec(`insert or ignore into resource_pricing(id,updated_at) values(1,?)`, appNow()); err != nil {
		return err
	}
	return seedAppDB(db)
}

func migrateUserDeletion(db *sql.DB) error {
	if err := ensureColumn(db, "users", "delete_flag", "integer not null default 0 check(delete_flag in (0, 1))"); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Preserve old deletion timestamps while making the flag the active-account filter.
	for _, stmt := range []string{
		`update users set delete_flag = 1 where deleted_at is not null and delete_flag = 0`,
		`drop index if exists uk_users_username`,
		`drop index if exists uk_users_email`,
		`create unique index uk_users_username on users(username) where delete_flag = 0`,
		`create unique index uk_users_email on users(email) where delete_flag = 0`,
	} {
		if _, err = tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query("pragma table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err = rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	_, err = db.Exec("alter table " + table + " add column " + column + " " + definition)
	return err
}

func seedAppDB(db *sql.DB) error {
	now := appNow()
	plans := []struct {
		code   string
		name   string
		months any
		price  int
		points int
		sort   int
	}{
		{"vip_month", "月度 VIP", 1, 880, 1288, 10},
		{"vip_year", "年度 VIP", 12, 8800, 1888, 20},
		{"vip_permanent", "永久 VIP", nil, 18800, 3888, 30},
	}
	for _, plan := range plans {
		_, err := db.Exec(`insert into member_plans
			(plan_code, plan_name, duration_months, price_cents, monthly_points, status, sort, created_at, updated_at)
			values (?, ?, ?, ?, ?, 1, ?, ?, ?)
			on conflict(plan_code) do nothing`,
			plan.code, plan.name, plan.months, plan.price, plan.points*pointScale, plan.sort, now, now)
		if err != nil {
			return err
		}
	}
	packages := []struct {
		code   string
		name   string
		points int
		price  int
		sort   int
	}{
		{"points_1000", "1000 充值积分", 1000, 1000, 10},
		{"points_5000", "5000 充值积分", 5000, 5000, 20},
		{"points_10000", "10000 充值积分", 10000, 10000, 30},
	}
	for _, pkg := range packages {
		if _, err := db.Exec(`insert into point_packages
			(package_code, package_name, points, price_cents, status, sort, created_at, updated_at)
			values (?, ?, ?, ?, 1, ?, ?, ?)
			on conflict(package_code) do update set
				package_name = excluded.package_name,
				points = excluded.points,
				price_cents = excluded.price_cents,
				status = 1,
				sort = excluded.sort,
				updated_at = excluded.updated_at`,
			pkg.code, pkg.name, pkg.points*pointScale, pkg.price, pkg.sort, now, now); err != nil {
			return err
		}
	}
	rules := []struct {
		method string
		path   string
		level  int
		remark string
	}{
		{"GET", "/api/me", levelPublic, "当前登录用户"},
		{"GET", "/api/member/*", levelNormal, "会员中心"},
		{"GET", "/api/points/*", levelNormal, "积分账户"},
		{"POST", "/api/admin/*", levelAdmin, "管理员接口"},
		{"POST", "/api/resources/*/unlock", levelNormal, "按基础价格乘倍率解锁资源"},
		{"GET", "/api/assets", levelNormal, "本人已购资产"},
		{"GET", "/api/user/usage", levelNormal, "本人额度及购买使用统计"},
		{"GET", "/api/admin/pricing", levelAdmin, "读取资产购买倍率"},
		{"POST", "/api/admin/pricing", levelAdmin, "设置资产购买倍率"},
		{"GET", "/api/cases/*", levelNormal, "已解锁的视频提示词"},
		{"GET", "/api/images/*", levelNormal, "已解锁的图片提示词"},
		{"GET", "/media/*/video", levelNormal, "已解锁视频"},
		{"GET", "/media/*/input/*", levelNormal, "已解锁参考原图；压缩预览公开"},
		{"GET", "/image-media/*/original", levelNormal, "已解锁图片原图"},
	}
	for _, rule := range rules {
		_, err := db.Exec(`insert into api_permission_rules
			(method, path_pattern, required_level, enabled, remark, created_at, updated_at)
			values (?, ?, ?, 1, ?, ?, ?)
			on conflict(method, path_pattern) do update set
				required_level = excluded.required_level,
				enabled = 1,
				remark = excluded.remark,
				updated_at = excluded.updated_at`,
			rule.method, rule.path, rule.level, rule.remark, now, now)
		if err != nil {
			return err
		}
	}
	return nil
}
