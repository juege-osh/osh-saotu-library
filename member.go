package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type memberPlanVO struct {
	ID             int64  `json:"id"`
	PlanCode       string `json:"planCode"`
	PlanName       string `json:"planName"`
	DurationMonths *int   `json:"durationMonths,omitempty"`
	PriceCents     int    `json:"priceCents"`
	Price          string `json:"price"`
	MonthlyPoints  Points `json:"monthlyPoints"`
}

type memberStatusVO struct {
	Active        bool    `json:"active"`
	MemberType    string  `json:"memberType"`
	PlanCode      *string `json:"planCode,omitempty"`
	ExpireAt      *string `json:"expireAt,omitempty"`
	RemainingDays *int64  `json:"remainingDays,omitempty"`
}

type adminGrantMemberRequest struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	PlanCode string `json:"plan_code"`
}

type planRecord struct {
	id             int64
	code           string
	name           string
	durationMonths sql.NullInt64
	priceCents     int
	monthlyPoints  int
}

func (s *server) memberPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := s.loadMemberPlans()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "会员套餐读取失败")
		return
	}
	jsonResponse(w, map[string]any{"plans": plans})
}

func (s *server) memberStatus(w http.ResponseWriter, r *http.Request, user *currentUser) {
	status := s.loadMemberStatus(user.ID)
	jsonResponse(w, map[string]any{"status": status})
}

func (s *server) memberCenter(w http.ResponseWriter, r *http.Request, user *currentUser) {
	plans, err := s.loadMemberPlans()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "会员中心读取失败")
		return
	}
	status := s.loadMemberStatus(user.ID)
	jsonResponse(w, map[string]any{"status": status, "plans": plans})
}

func (s *server) adminGrantMember(w http.ResponseWriter, r *http.Request, admin *currentUser) {
	var req adminGrantMemberRequest
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "请求格式不正确")
		return
	}
	req.PlanCode = strings.TrimSpace(req.PlanCode)
	if req.PlanCode == "" {
		apiError(w, http.StatusBadRequest, "请选择会员套餐")
		return
	}
	targetID, err := s.resolveUserID(req.UserID, req.Username, req.Email)
	if err != nil {
		apiError(w, http.StatusNotFound, "用户不存在")
		return
	}
	tx, err := s.appDB.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "会员开通失败")
		return
	}
	defer tx.Rollback()
	membershipID, err := s.grantMembershipTx(tx, targetID, req.PlanCode, "admin_grant",
		fmt.Sprintf("admin-grant:%d:%d", admin.ID, time.Now().UnixNano()))
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err = tx.Commit(); err != nil {
		apiError(w, http.StatusInternalServerError, "会员开通失败")
		return
	}
	_ = s.expirePointLots(targetID)
	target, _ := s.loadCurrentUserByID(r.Context(), targetID)
	jsonResponse(w, map[string]any{"ok": true, "membershipId": membershipID, "user": target})
}

func (s *server) loadMemberPlans() ([]memberPlanVO, error) {
	rows, err := s.appDB.Query(`select id, plan_code, plan_name, duration_months, price_cents, monthly_points
		from member_plans
		where status = 1
		order by sort asc, id asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plans := []memberPlanVO{}
	for rows.Next() {
		var plan planRecord
		if err = rows.Scan(&plan.id, &plan.code, &plan.name, &plan.durationMonths, &plan.priceCents, &plan.monthlyPoints); err != nil {
			return nil, err
		}
		vo := plan.toVO()
		plans = append(plans, vo)
	}
	return plans, rows.Err()
}

func (plan planRecord) toVO() memberPlanVO {
	vo := memberPlanVO{
		ID:            plan.id,
		PlanCode:      plan.code,
		PlanName:      plan.name,
		PriceCents:    plan.priceCents,
		Price:         fmt.Sprintf("%.2f", float64(plan.priceCents)/100),
		MonthlyPoints: Points(plan.monthlyPoints),
	}
	if plan.durationMonths.Valid {
		months := int(plan.durationMonths.Int64)
		vo.DurationMonths = &months
	}
	return vo
}

func (s *server) loadPlan(code string) (planRecord, error) {
	var plan planRecord
	err := s.appDB.QueryRow(`select id, plan_code, plan_name, duration_months, price_cents, monthly_points
		from member_plans
		where plan_code = ? and status = 1
		limit 1`, code).Scan(&plan.id, &plan.code, &plan.name, &plan.durationMonths, &plan.priceCents, &plan.monthlyPoints)
	return plan, err
}

func (s *server) loadMemberStatus(userID int64) memberStatusVO {
	status := memberStatusVO{Active: false, MemberType: roleNormal}
	var planCode, expire sql.NullString
	err := s.appDB.QueryRow(`select plan_code, expire_at
		from user_memberships
		where user_id = ?
		  and status = 'active'
		  and (expire_at is null or expire_at > ?)
		order by case when expire_at is null then 1 else 0 end desc, expire_at desc
		limit 1`, userID, appNow()).Scan(&planCode, &expire)
	if err != nil {
		return status
	}
	status.Active = true
	status.MemberType = roleVIP
	if planCode.Valid {
		status.PlanCode = &planCode.String
	}
	if expire.Valid {
		status.ExpireAt = &expire.String
		if t, err := time.Parse(time.RFC3339Nano, expire.String); err == nil {
			days := int64(time.Until(t).Hours() / 24)
			if days < 0 {
				days = 0
			}
			status.RemainingDays = &days
		}
	}
	return status
}

func (s *server) grantMembershipTx(tx *sql.Tx, userID int64, planCode, sourceType, sourceNo string) (int64, error) {
	membershipID, err := s.grantMembershipEntitlementTx(tx, userID, planCode, sourceType, sourceNo)
	if err != nil {
		return 0, err
	}
	plan, err := loadPlanTx(tx, planCode)
	if err != nil {
		return 0, errorsWithDefault(err, "会员套餐不存在")
	}
	if err = createOrderTx(tx, userID, "subscription", plan.name, int64(plan.priceCents), int64(plan.monthlyPoints), plan.code, sourceType, sourceNo); err != nil {
		return 0, err
	}
	return membershipID, nil
}

// grantMembershipEntitlementTx applies a paid or administrative membership
// grant without creating a second business order. Payment orders already
// exist before the entitlement transaction starts.
func (s *server) grantMembershipEntitlementTx(tx *sql.Tx, userID int64, planCode, sourceType, sourceNo string) (int64, error) {
	plan, err := loadPlanTx(tx, planCode)
	if err != nil {
		return 0, errorsWithDefault(err, "会员套餐不存在")
	}
	now := time.Now().UTC()
	start := now
	var currentExpire sql.NullString
	err = tx.QueryRow(`select expire_at
		from user_memberships
		where user_id = ?
		  and status = 'active'
		  and (expire_at is null or expire_at > ?)
		order by case when expire_at is null then 1 else 0 end desc, expire_at desc
		limit 1`, userID, appNow()).Scan(&currentExpire)
	if err == nil && !currentExpire.Valid {
		start = now
	}
	if err == nil && currentExpire.Valid && plan.durationMonths.Valid {
		if parsed, parseErr := time.Parse(time.RFC3339Nano, currentExpire.String); parseErr == nil && parsed.After(now) {
			start = parsed
		}
	}
	// Preserve the unused Daka welcome month when a user pays during their trial.
	if strings.HasPrefix(plan.code, "daka_") && plan.durationMonths.Valid {
		var trial string
		if tx.QueryRow(`select trial_until from daka_plans where user_id=?`, userID).Scan(&trial) == nil {
			if until, e := time.Parse(time.RFC3339Nano, trial); e == nil && until.After(start) {
				start = until
			}
		}
	}
	var expireValue any
	if plan.durationMonths.Valid && plan.durationMonths.Int64 > 0 {
		expire := start.AddDate(0, int(plan.durationMonths.Int64), 0)
		formatted := appTime(expire)
		expireValue = formatted
	}
	result, err := tx.Exec(`insert into user_memberships
		(user_id, plan_code, start_at, expire_at, status, source_type, source_no, created_at, updated_at)
		values (?, ?, ?, ?, 'active', ?, ?, ?, ?)`,
		userID, plan.code, appTime(start), expireValue, sourceType, sourceNo, appNow(), appNow())
	if err != nil {
		return 0, err
	}
	membershipID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	var roleCode string
	var roleLevel int
	if err = tx.QueryRow(`select role_code, role_level from users where id = ?`, userID).Scan(&roleCode, &roleLevel); err != nil {
		return 0, err
	}
	if roleLevel < levelAdmin && roleCode != roleAdmin {
		_, err = tx.Exec(`update users
			set role_code = ?, role_level = ?, vip_expire_time = ?, updated_at = ?
			where id = ?`,
			roleVIP, levelVIP, expireValue, appNow(), userID)
		if err != nil {
			return 0, err
		}
	}
	if err = grantVipCycleTx(tx, userID, membershipID, sourceNo, plan.monthlyPoints, now); err != nil {
		return 0, err
	}
	return membershipID, nil
}

func grantVipCycleTx(tx *sql.Tx, userID, membershipID int64, sourceNo string, points int, now time.Time) error {
	if points <= 0 {
		return nil
	} // Daka membership has no credit quota.
	cycleStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	cycleEnd := cycleStart.AddDate(0, 1, 0)
	cycleKey := cycleStart.Format("2006-01")
	inserted, err := tx.Exec(`insert or ignore into vip_point_cycles
		(user_id, cycle_key, cycle_start_at, cycle_end_at, points, source_membership_id, source_order_no, status, created_at)
		values (?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		userID, cycleKey, appTime(cycleStart), appTime(cycleEnd), points, membershipID, sourceNo, appNow())
	if err != nil {
		return err
	}
	rows, err := inserted.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		var cycleID int64
		var existingPoints int
		var existingLot sql.NullInt64
		err = tx.QueryRow(`select id, points, lot_id from vip_point_cycles where user_id = ? and cycle_key = ?`,
			userID, cycleKey).Scan(&cycleID, &existingPoints, &existingLot)
		if err != nil {
			return err
		}
		if points <= existingPoints {
			return nil
		}
		delta := points - existingPoints
		lotID, recordID, _, err := grantPointsTx(tx, userID, int64(delta), "grant", "vip_cycle_upgrade",
			fmt.Sprintf("vip-cycle-upgrade:%d:%s:%d", userID, cycleKey, points), &cycleEnd, "VIP升级补发当月额度")
		if err != nil {
			return err
		}
		_, err = tx.Exec(`update vip_point_cycles
			set points = ?, lot_id = coalesce(lot_id, ?), record_id = coalesce(record_id, ?), status = 'success'
			where id = ?`, points, lotID, recordID, cycleID)
		return err
	}
	lotID, recordID, _, err := grantPointsTx(tx, userID, int64(points), "grant", "vip_cycle",
		fmt.Sprintf("vip-cycle:%d:%s", userID, cycleKey), &cycleEnd, "VIP月度周期积分")
	if err != nil {
		return err
	}
	_, err = tx.Exec(`update vip_point_cycles
		set lot_id = ?, record_id = ?, status = 'success'
		where user_id = ? and cycle_key = ?`,
		lotID, recordID, userID, cycleKey)
	return err
}

func errorsWithDefault(err error, message string) error {
	if err == nil {
		return nil
	}
	if err == sql.ErrNoRows {
		return errors.New(message)
	}
	return err
}

func loadPlanTx(tx *sql.Tx, code string) (planRecord, error) {
	var plan planRecord
	err := tx.QueryRow(`select id, plan_code, plan_name, duration_months, price_cents, monthly_points
		from member_plans
		where plan_code = ? and status = 1
		limit 1`, code).Scan(&plan.id, &plan.code, &plan.name, &plan.durationMonths, &plan.priceCents, &plan.monthlyPoints)
	return plan, err
}
