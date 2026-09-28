package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strings"
)

const pointScale = 100

// Points are stored as integer hundredths; JSON exposes the actual point value.
type Points int64

var decimalPoints = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,16})(\.[0-9]{1,2})?$`)

func (p Points) MarshalJSON() ([]byte, error) {
	return []byte(new(big.Rat).SetFrac64(int64(p), pointScale).FloatString(2)), nil
}

func (p *Points) UnmarshalJSON(data []byte) error {
	if len(data) > 32 || !json.Valid(data) || !decimalPoints.Match(data) {
		return errors.New("invalid points")
	}
	value, ok := new(big.Rat).SetString(string(data))
	if !ok {
		return errors.New("invalid points")
	}
	value.Mul(value, big.NewRat(pointScale, 1))
	if !value.IsInt() || !value.Num().IsInt64() {
		return errors.New("points require at most two decimal places")
	}
	*p = Points(value.Num().Int64())
	return nil
}

func migratePointPrecision(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`create table if not exists app_migrations(name text primary key, applied_at text not null)`); err != nil {
		return err
	}
	var done int
	err = tx.QueryRow(`select 1 from app_migrations where name='point_hundredths_v1'`).Scan(&done)
	if err == nil {
		return tx.Commit()
	}
	if err != sql.ErrNoRows {
		return err
	}
	for _, table := range []struct {
		name    string
		columns []string
	}{
		{"point_accounts", []string{"total_points", "grant_points", "paid_points"}},
		{"point_lots", []string{"total_amount", "remaining_amount"}},
		{"point_records", []string{"change_amount", "grant_amount", "paid_amount", "before_balance", "after_balance"}},
		{"member_plans", []string{"monthly_points"}},
		{"vip_point_cycles", []string{"points"}},
		{"orders", []string{"points"}},
		{"resource_unlocks", []string{"points"}},
	} {
		checks, assignments := []string{}, []string{}
		for _, col := range table.columns {
			checks = append(checks, fmt.Sprintf("typeof(%s)!='integer' or %s>%d or %s<%d", col, col, math.MaxInt64/pointScale, col, -math.MaxInt64/pointScale))
			assignments = append(assignments, col+"="+col+"*100")
		}
		var invalid int
		if err = tx.QueryRow("select count(*) from " + table.name + " where " + strings.Join(checks, " or ")).Scan(&invalid); err != nil {
			return err
		}
		if invalid != 0 {
			return fmt.Errorf("invalid or overflowing legacy points in %s", table.name)
		}
		if _, err = tx.Exec("update " + table.name + " set " + strings.Join(assignments, ",")); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`insert into app_migrations values('point_hundredths_v1',?)`, appNow()); err != nil {
		return err
	}
	return tx.Commit()
}
