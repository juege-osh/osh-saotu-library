package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"
)

var usageLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type quotaUsage struct {
	Total     Points `json:"total"`
	Remaining Points `json:"remaining"`
}

type resourceUsage struct {
	Kind      string `json:"kind"`
	Purchases int64  `json:"purchases"`
	Points    Points `json:"points"`
}

type dailyUsage struct {
	Date  string `json:"date"`
	Video Points `json:"video"`
	Image Points `json:"image"`
}

type userUsageVO struct {
	Quota        quotaUsage      `json:"quota"`
	Distribution []resourceUsage `json:"distribution"`
	Daily        []dailyUsage    `json:"daily"`
	TimeZone     string          `json:"timeZone"`
}

func (s *server) userUsage(w http.ResponseWriter, r *http.Request, user *currentUser) {
	days := 10
	if raw := r.URL.Query().Get("days"); raw != "" {
		var err error
		days, err = strconv.Atoi(raw)
		if err != nil || days < 1 || days > 10 {
			apiError(w, http.StatusBadRequest, "统计天数须为 1 到 10 天")
			return
		}
	}
	usage, err := s.loadUserUsage(r.Context(), user.ID, days, time.Now())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "使用记录读取失败")
		return
	}
	jsonResponse(w, usage)
}

func (s *server) loadUserUsage(ctx context.Context, userID int64, days int, now time.Time) (userUsageVO, error) {
	result := userUsageVO{
		Distribution: []resourceUsage{{Kind: "video"}, {Kind: "image"}},
		Daily:        []dailyUsage{}, TimeZone: "Asia/Shanghai",
	}
	if days < 1 || days > 10 {
		return result, errors.New("invalid usage window")
	}
	localNow := now.In(usageLocation)
	start := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, usageLocation).AddDate(0, 0, 1-days)
	indexes := make(map[string]int, days)
	for i := 0; i < days; i++ {
		date := start.AddDate(0, 0, i).Format("2006-01-02")
		indexes[date] = i
		result.Daily = append(result.Daily, dailyUsage{Date: date})
	}
	tx, err := s.appDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	// Exhausted but unexpired lots remain in the denominator of the quota bar.
	err = tx.QueryRowContext(ctx, `select coalesce(sum(total_amount),0),coalesce(sum(remaining_amount),0)
		from point_lots where user_id=? and point_type='grant' and status in ('available','exhausted')
		and (expire_at is null or julianday(expire_at)>julianday(?))`, userID, appTime(now)).Scan(&result.Quota.Total, &result.Quota.Remaining)
	if err != nil {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, `select resource_kind,count(*),coalesce(sum(points),0)
		from resource_unlocks where user_id=? and julianday(created_at)<=julianday(?) group by resource_kind`, userID, appTime(now))
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var item resourceUsage
		if err = rows.Scan(&item.Kind, &item.Purchases, &item.Points); err != nil {
			rows.Close()
			return result, err
		}
		if item.Kind == "video" {
			result.Distribution[0] = item
		} else if item.Kind == "image" {
			result.Distribution[1] = item
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	// Use the historical unlock charge, not current prices or per-lot ledger rows.
	rows, err = tx.QueryContext(ctx, `select date(created_at,'+8 hours'),resource_kind,sum(points)
		from resource_unlocks where user_id=? and julianday(created_at)>=julianday(?)
		and julianday(created_at)<=julianday(?) group by date(created_at,'+8 hours'),resource_kind`, userID, appTime(start), appTime(now))
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var date, kind string
		var points Points
		if err = rows.Scan(&date, &kind, &points); err != nil {
			rows.Close()
			return result, err
		}
		if i, exists := indexes[date]; exists {
			if kind == "video" {
				result.Daily[i].Video = points
			} else if kind == "image" {
				result.Daily[i].Image = points
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}
