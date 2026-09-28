package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

type pointBalanceVO struct {
	UserID         int64  `json:"userId"`
	TotalPoints    Points `json:"totalPoints"`
	GrantPoints    Points `json:"grantPoints"`
	PaidPoints     Points `json:"paidPoints"`
	QuotaPoints    Points `json:"quotaPoints"`
	RechargePoints Points `json:"rechargePoints"`
}

type pointRecordVO struct {
	ID            int64   `json:"id"`
	ChangeType    string  `json:"changeType"`
	PointType     string  `json:"pointType"`
	BizType       string  `json:"bizType"`
	BizNo         string  `json:"bizNo"`
	ChangeAmount  Points  `json:"changeAmount"`
	GrantAmount   Points  `json:"grantAmount"`
	PaidAmount    Points  `json:"paidAmount"`
	BeforeBalance Points  `json:"beforeBalance"`
	AfterBalance  Points  `json:"afterBalance"`
	LotExpireAt   *string `json:"lotExpireAt,omitempty"`
	Remark        *string `json:"remark,omitempty"`
	CreatedAt     string  `json:"createdAt"`
}

type adminPointAdjustRequest struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Amount   Points `json:"amount"`
	Remark   string `json:"remark"`
}

func (s *server) pointBalance(w http.ResponseWriter, r *http.Request, user *currentUser) {
	if err := s.expirePointLots(user.ID); err != nil {
		apiError(w, http.StatusInternalServerError, "积分同步失败")
		return
	}
	balance, err := s.loadPointBalance(user.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "积分余额读取失败")
		return
	}
	jsonResponse(w, map[string]any{"balance": balance})
}

func (s *server) pointRecords(w http.ResponseWriter, r *http.Request, user *currentUser) {
	if err := s.expirePointLots(user.ID); err != nil {
		apiError(w, http.StatusInternalServerError, "积分同步失败")
		return
	}
	rows, err := s.appDB.Query(`select id, change_type, point_type, biz_type, biz_no,
			change_amount, grant_amount, paid_amount, before_balance, after_balance,
			lot_expire_at, remark, created_at
		from point_records
		where user_id = ?
		order by id desc
		limit 80`, user.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "积分流水读取失败")
		return
	}
	defer rows.Close()
	records := []pointRecordVO{}
	for rows.Next() {
		var record pointRecordVO
		var expire, remark sql.NullString
		if err = rows.Scan(&record.ID, &record.ChangeType, &record.PointType, &record.BizType, &record.BizNo,
			&record.ChangeAmount, &record.GrantAmount, &record.PaidAmount, &record.BeforeBalance,
			&record.AfterBalance, &expire, &remark, &record.CreatedAt); err != nil {
			apiError(w, http.StatusInternalServerError, "积分流水读取失败")
			return
		}
		if expire.Valid {
			record.LotExpireAt = &expire.String
		}
		if remark.Valid {
			record.Remark = &remark.String
		}
		records = append(records, record)
	}
	jsonResponse(w, map[string]any{"records": records})
}

func (s *server) adminAdjustPoints(w http.ResponseWriter, r *http.Request, admin *currentUser) {
	var req adminPointAdjustRequest
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	if req.Amount == 0 || req.Amount > 100000000*pointScale || req.Amount < -100000000*pointScale {
		apiError(w, http.StatusBadRequest, "单次调整积分须非零且不超过一亿")
		return
	}
	targetID, err := s.resolveUserID(req.UserID, req.Username, req.Email)
	if err != nil {
		apiError(w, http.StatusNotFound, "用户不存在")
		return
	}
	tx, err := s.appDB.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "积分调整失败")
		return
	}
	defer tx.Rollback()
	sourceNo := fmt.Sprintf("admin-adjust:%d:%d", targetID, time.Now().UnixNano())
	if req.Amount > 0 {
		_, _, _, err = grantPointsTx(tx, targetID, int64(req.Amount), "paid", "admin_adjust", sourceNo, nil, req.Remark)
		if err == nil {
			err = createOrderTx(tx, targetID, "recharge", "管理员充值额度", 0, int64(req.Amount), nil, "admin_adjust", sourceNo)
		}
	} else {
		err = consumePointsTx(tx, targetID, -int64(req.Amount), "admin_adjust", sourceNo, req.Remark)
	}
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err = tx.Commit(); err != nil {
		apiError(w, http.StatusInternalServerError, "积分调整失败")
		return
	}
	balance, _ := s.loadPointBalance(targetID)
	jsonResponse(w, map[string]any{"ok": true, "balance": balance, "operator": admin.ID})
}

func ensurePointAccountTx(tx *sql.Tx, userID int64) error {
	_, err := tx.Exec(`insert or ignore into point_accounts
		(user_id, total_points, grant_points, paid_points, updated_at)
		values (?, 0, 0, 0, ?)`, userID, appNow())
	return err
}

func grantPointsTx(tx *sql.Tx, userID int64, amount int64, pointType, bizType, bizNo string, expireAt *time.Time, remark string) (int64, int64, bool, error) {
	if amount <= 0 {
		return 0, 0, false, errors.New("积分数量必须大于 0")
	}
	if pointType != "grant" && pointType != "paid" {
		return 0, 0, false, errors.New("积分类型不正确")
	}
	now := appNow()
	inserted, err := insertOperationTx(tx, userID, bizType, bizNo, now)
	if err != nil {
		return 0, 0, false, err
	}
	if !inserted {
		var status string
		if err = tx.QueryRow(`select status from point_operations
			where user_id = ? and biz_type = ? and biz_no = ?`, userID, bizType, bizNo).Scan(&status); err != nil {
			return 0, 0, false, err
		}
		if status == "success" {
			return 0, 0, true, nil
		}
	}
	if err = ensurePointAccountTx(tx, userID); err != nil {
		return 0, 0, false, err
	}
	var before, grantBefore, paidBefore int64
	if err = tx.QueryRow(`select total_points, grant_points, paid_points
		from point_accounts where user_id = ?`, userID).Scan(&before, &grantBefore, &paidBefore); err != nil {
		return 0, 0, false, err
	}
	var expireValue any
	if expireAt != nil {
		expireValue = appTime(*expireAt)
	}
	lotResult, err := tx.Exec(`insert into point_lots
		(user_id, point_type, source_type, source_no, total_amount, remaining_amount, expire_at, status, remark, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, 'available', ?, ?, ?)`,
		userID, pointType, bizType, bizNo, amount, amount, expireValue, remark, now, now)
	if err != nil {
		return 0, 0, false, err
	}
	lotID, err := lotResult.LastInsertId()
	if err != nil {
		return 0, 0, false, err
	}
	grantAmount, paidAmount := int64(0), int64(0)
	if pointType == "grant" {
		grantAmount = amount
	} else {
		paidAmount = amount
	}
	if before > math.MaxInt64-amount {
		return 0, 0, false, errors.New("积分余额超出范围")
	}
	after := before + amount
	recordResult, err := tx.Exec(`insert into point_records
		(user_id, change_type, point_type, biz_type, biz_no, lot_id, change_amount,
		 grant_amount, paid_amount, before_balance, after_balance, lot_expire_at, remark, created_at)
		values (?, 'income', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, pointType, bizType, bizNo, lotID, amount, grantAmount, paidAmount,
		before, after, expireValue, remark, now)
	if err != nil {
		return 0, 0, false, err
	}
	recordID, err := recordResult.LastInsertId()
	if err != nil {
		return 0, 0, false, err
	}
	if _, err = tx.Exec(`update point_accounts
		set total_points = ?, grant_points = ?, paid_points = ?, updated_at = ?
		where user_id = ?`,
		after, grantBefore+grantAmount, paidBefore+paidAmount, now, userID); err != nil {
		return 0, 0, false, err
	}
	if err = finishOperationTx(tx, userID, bizType, bizNo, now); err != nil {
		return 0, 0, false, err
	}
	return lotID, recordID, false, nil
}

func consumePointsTx(tx *sql.Tx, userID int64, amount int64, bizType, bizNo, remark string) error {
	if amount <= 0 {
		return errors.New("积分数量必须大于 0")
	}
	now := appNow()
	inserted, err := insertOperationTx(tx, userID, bizType, bizNo, now)
	if err != nil {
		return err
	}
	if !inserted {
		var status string
		if err = tx.QueryRow(`select status from point_operations
			where user_id = ? and biz_type = ? and biz_no = ?`, userID, bizType, bizNo).Scan(&status); err != nil {
			return err
		}
		if status == "success" {
			return nil
		}
	}
	if err = ensurePointAccountTx(tx, userID); err != nil {
		return err
	}
	var before, grantBefore, paidBefore int64
	if err = tx.QueryRow(`select total_points, grant_points, paid_points
		from point_accounts where user_id = ?`, userID).Scan(&before, &grantBefore, &paidBefore); err != nil {
		return err
	}
	if before < amount {
		return errors.New("积分余额不足")
	}
	rows, err := tx.Query(`select id, point_type, remaining_amount, expire_at
		from point_lots
		where user_id = ?
		  and status = 'available'
		  and remaining_amount > 0
		  and (expire_at is null or expire_at > ?)
		order by case when expire_at is null then 1 else 0 end,
		         expire_at asc,
		         case when point_type = 'grant' then 0 else 1 end,
		         id asc`, userID, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	type lotSlice struct {
		id        int64
		pointType string
		amount    int64
		expire    sql.NullString
	}
	slices := []lotSlice{}
	remaining := amount
	for rows.Next() && remaining > 0 {
		var id, available int64
		var pointType string
		var expire sql.NullString
		if err = rows.Scan(&id, &pointType, &available, &expire); err != nil {
			return err
		}
		use := available
		if use > remaining {
			use = remaining
		}
		slices = append(slices, lotSlice{id: id, pointType: pointType, amount: use, expire: expire})
		remaining -= use
	}
	if remaining > 0 {
		return errors.New("积分批次余额不足")
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	after := before - amount
	grantUsed, paidUsed := int64(0), int64(0)
	recordBalance := before
	for _, slice := range slices {
		status := "available"
		if err = tx.QueryRow(`select case when remaining_amount - ? <= 0 then 'exhausted' else 'available' end
			from point_lots where id = ?`, slice.amount, slice.id).Scan(&status); err != nil {
			return err
		}
		if _, err = tx.Exec(`update point_lots
			set remaining_amount = remaining_amount - ?, status = ?, updated_at = ?
			where id = ?`, slice.amount, status, now, slice.id); err != nil {
			return err
		}
		grantAmount, paidAmount := int64(0), int64(0)
		if slice.pointType == "grant" {
			grantAmount = slice.amount
			grantUsed += slice.amount
		} else {
			paidAmount = slice.amount
			paidUsed += slice.amount
		}
		var expireValue any
		if slice.expire.Valid {
			expireValue = slice.expire.String
		}
		if _, err = tx.Exec(`insert into point_records
			(user_id, change_type, point_type, biz_type, biz_no, lot_id, change_amount,
			 grant_amount, paid_amount, before_balance, after_balance, lot_expire_at, remark, created_at)
			values (?, 'expense', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			userID, slice.pointType, bizType, bizNo, slice.id, slice.amount, grantAmount, paidAmount,
			recordBalance, recordBalance-slice.amount, expireValue, remark, now); err != nil {
			return err
		}
		recordBalance -= slice.amount
	}
	if _, err = tx.Exec(`update point_accounts
		set total_points = ?, grant_points = ?, paid_points = ?, updated_at = ?
		where user_id = ?`,
		after, grantBefore-grantUsed, paidBefore-paidUsed, now, userID); err != nil {
		return err
	}
	return finishOperationTx(tx, userID, bizType, bizNo, now)
}

func insertOperationTx(tx *sql.Tx, userID int64, bizType, bizNo, now string) (bool, error) {
	result, err := tx.Exec(`insert or ignore into point_operations
		(user_id, biz_type, biz_no, status, created_at, updated_at)
		values (?, ?, ?, 'pending', ?, ?)`, userID, bizType, bizNo, now, now)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func finishOperationTx(tx *sql.Tx, userID int64, bizType, bizNo, now string) error {
	_, err := tx.Exec(`update point_operations
		set status = 'success', updated_at = ?
		where user_id = ? and biz_type = ? and biz_no = ?`,
		now, userID, bizType, bizNo)
	return err
}

func (s *server) expirePointLots(userID int64) error {
	tx, err := s.appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = ensurePointAccountTx(tx, userID); err != nil {
		return err
	}
	now := appNow()
	rows, err := tx.Query(`select id, point_type, remaining_amount, expire_at
		from point_lots
		where user_id = ?
		  and status = 'available'
		  and remaining_amount > 0
		  and expire_at is not null
		  and expire_at <= ?
		order by expire_at asc, id asc`, userID, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	type expiredLot struct {
		id        int64
		pointType string
		amount    int64
		expire    string
	}
	lots := []expiredLot{}
	for rows.Next() {
		var lot expiredLot
		if err = rows.Scan(&lot.id, &lot.pointType, &lot.amount, &lot.expire); err != nil {
			return err
		}
		lots = append(lots, lot)
	}
	for _, lot := range lots {
		var before, grantBefore, paidBefore int64
		if err = tx.QueryRow(`select total_points, grant_points, paid_points from point_accounts where user_id = ?`,
			userID).Scan(&before, &grantBefore, &paidBefore); err != nil {
			return err
		}
		grantAmount, paidAmount := int64(0), int64(0)
		if lot.pointType == "grant" {
			grantAmount = lot.amount
		} else {
			paidAmount = lot.amount
		}
		after := before - lot.amount
		if _, err = tx.Exec(`update point_lots set status = 'expired', remaining_amount = 0, updated_at = ? where id = ?`,
			now, lot.id); err != nil {
			return err
		}
		if _, err = tx.Exec(`insert into point_records
			(user_id, change_type, point_type, biz_type, biz_no, lot_id, change_amount,
			 grant_amount, paid_amount, before_balance, after_balance, lot_expire_at, remark, created_at)
			values (?, 'expire', ?, 'point_expire', ?, ?, ?, ?, ?, ?, ?, ?, '积分过期', ?)`,
			userID, lot.pointType, fmt.Sprintf("expire:%d", lot.id), lot.id, lot.amount, grantAmount,
			paidAmount, before, after, lot.expire, now); err != nil {
			return err
		}
		if _, err = tx.Exec(`update point_accounts
			set total_points = ?, grant_points = ?, paid_points = ?, updated_at = ?
			where user_id = ?`,
			after, grantBefore-grantAmount, paidBefore-paidAmount, now, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *server) syncUserPoints(userID int64) error {
	if err := s.expirePointLots(userID); err != nil {
		return err
	}
	tx, err := s.appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var membershipID int64
	var points int
	var source sql.NullString
	err = tx.QueryRow(`select m.id, p.monthly_points, m.source_no
		from user_memberships m join member_plans p on p.plan_code=m.plan_code
		where m.user_id=? and m.status='active' and (m.expire_at is null or m.expire_at>?)
		order by p.monthly_points desc limit 1`, userID, appNow()).Scan(&membershipID, &points, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = grantVipCycleTx(tx, userID, membershipID, source.String, points, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *server) loadPointBalance(userID int64) (pointBalanceVO, error) {
	var balance pointBalanceVO
	balance.UserID = userID
	err := s.appDB.QueryRow(`select coalesce(total_points, 0), coalesce(grant_points, 0), coalesce(paid_points, 0)
		from point_accounts where user_id = ?`, userID).Scan(&balance.TotalPoints, &balance.GrantPoints, &balance.PaidPoints)
	balance.QuotaPoints = balance.GrantPoints
	balance.RechargePoints = balance.PaidPoints
	if err == sql.ErrNoRows {
		return balance, nil
	}
	return balance, err
}

func (s *server) resolveUserID(userID int64, username, email string) (int64, error) {
	if userID > 0 {
		var id int64
		err := s.appDB.QueryRow(`select id from users where id = ? and delete_flag = 0`, userID).Scan(&id)
		return id, err
	}
	identity := strings.ToLower(strings.TrimSpace(email))
	if identity == "" {
		identity = strings.ToLower(strings.TrimSpace(username))
	}
	if identity == "" {
		return 0, sql.ErrNoRows
	}
	var id int64
	err := s.appDB.QueryRow(`select id from users
		where delete_flag = 0 and (lower(username) = ? or lower(email) = ?)
		limit 1`, identity, identity).Scan(&id)
	return id, err
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}
