package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/litebox/litebox/internal/adjustment"
	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/user"
)

const (
	actionUserAdjust = "user.adjust"
	actionUserBatch  = "user.batch"
)

// adjustRequest 是一次人工续期或额度调整。
//
// 每种 action 只看它自己需要的字段,其余忽略 —— 前端按选中的操作
// 填一个值即可,不必为每种操作准备一套不同的请求体。
type adjustRequest struct {
	Action adjustment.Action `json:"action"`
	// ADD_QUOTA 用:增量字节,可为负(表示扣减)。
	QuotaDeltaBytes int64 `json:"quota_delta_bytes"`
	// SET_QUOTA 用:绝对值,0 表示不限量。
	QuotaBytes int64 `json:"quota_bytes"`
	// EXTEND_EXPIRY 用:延长天数,可为负。
	ExpiryDeltaDays int `json:"expiry_delta_days"`
	// SET_EXPIRY 用:绝对时间,空串表示改为不过期。
	ExpiresAt string `json:"expires_at"`
	// CHANGE_TIER 用。
	AccessTierID int64 `json:"access_tier_id"`
	// Remark 会展示给用户,不要写内部说明。
	Remark string `json:"remark"`
}

func (s *Server) handleAdjustUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	var req adjustRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	after, params, err := s.applyAdjustment(r.Context(), id, req, &admin.ID)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			writeError(w, http.StatusNotFound, "用户不存在")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.adjustments.Record(r.Context(), params); err != nil {
		// 调整已经生效了,记录失败不能反过来说操作失败 ——
		// 那会让管理员再点一次,把流量加两遍。
		s.logger.Error("记录调整失败", "error", err, "user_id", id)
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionUserAdjust,
		TargetType: "user", TargetID: after.UserCode,
		Detail:   adjustment.ActionText(req.Action) + ";" + req.Remark,
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, s.toDetailResponse(r.Context(), after))
}

// applyAdjustment 执行一次调整并返回记录参数。
//
// 一律经 user.Service —— 额度与到期变化会改变用户是否可服务,
// 直接调 Store 的话数据库改了而节点上的凭据没变。
func (s *Server) applyAdjustment(ctx context.Context, id int64, req adjustRequest,
	adminID *int64) (*user.User, adjustment.Params, error) {
	before, err := s.users.Store().Get(ctx, id)
	if err != nil {
		return nil, adjustment.Params{}, err
	}

	params := adjustment.Params{
		ProxyUserID: id,
		Action:      req.Action,
		Remark:      req.Remark,
		AdminUserID: adminID,
		Before:      snapshot(before),
	}

	var after *user.User
	switch req.Action {
	case adjustment.ActionAddQuota:
		// 不限量的用户加流量没有意义:0 + N 会把他从"不限"变成"只有 N",
		// 这与管理员点这个按钮的意图正好相反。
		if before.QuotaBytes == 0 {
			return nil, params, errors.New("该用户当前是不限量,请改用「设置流量额度」")
		}
		next := before.QuotaBytes + req.QuotaDeltaBytes
		if next < 0 {
			next = 0
		}
		params.QuotaDeltaBytes = next - before.QuotaBytes
		after, err = s.users.Update(ctx, id, user.UpdateParams{QuotaBytes: &next})

	case adjustment.ActionSetQuota:
		if req.QuotaBytes < 0 {
			return nil, params, errors.New("流量额度不能为负数")
		}
		quota := req.QuotaBytes
		params.QuotaDeltaBytes = quota - before.QuotaBytes
		after, err = s.users.Update(ctx, id, user.UpdateParams{QuotaBytes: &quota})

	case adjustment.ActionResetTraffic:
		after, err = s.users.ResetTraffic(ctx, id)

	case adjustment.ActionExtendExpiry:
		if req.ExpiryDeltaDays == 0 {
			return nil, params, errors.New("延长天数不能为 0")
		}
		// 基准取"到期时间与现在之中较晚的那个":已经过期的用户从今天起算,
		// 否则给一个过期三个月的人续 30 天,他仍然是过期状态。
		base := time.Now().UTC()
		if before.ExpiresAt != nil && *before.ExpiresAt != "" {
			if exp, perr := time.Parse(time.RFC3339, *before.ExpiresAt); perr == nil && exp.After(base) {
				base = exp
			}
		}
		next := base.AddDate(0, 0, req.ExpiryDeltaDays).Format(time.RFC3339)
		params.ExpiryDeltaDays = req.ExpiryDeltaDays
		ptr := &next
		after, err = s.users.Update(ctx, id, user.UpdateParams{ExpiresAt: &ptr})

	case adjustment.ActionSetExpiry:
		if req.ExpiresAt == "" {
			var none *string
			after, err = s.users.Update(ctx, id, user.UpdateParams{ExpiresAt: &none})
		} else {
			if _, perr := time.Parse(time.RFC3339, req.ExpiresAt); perr != nil {
				return nil, params, fmt.Errorf("到期时间格式非法,应为 RFC3339: %w", perr)
			}
			value := req.ExpiresAt
			ptr := &value
			after, err = s.users.Update(ctx, id, user.UpdateParams{ExpiresAt: &ptr})
		}

	case adjustment.ActionChangeTier:
		if req.AccessTierID <= 0 {
			return nil, params, errors.New("请选择访问等级")
		}
		tier := req.AccessTierID
		after, err = s.users.Update(ctx, id, user.UpdateParams{AccessTierID: &tier})

	case adjustment.ActionEnableUser:
		after, err = s.users.SetEnabled(ctx, id, true)

	case adjustment.ActionDisableUser:
		after, err = s.users.SetEnabled(ctx, id, false)

	default:
		return nil, params, fmt.Errorf("未知的调整类型 %q", req.Action)
	}

	if err != nil {
		return nil, params, err
	}
	params.After = snapshot(after)
	return after, params, nil
}

func snapshot(u *user.User) adjustment.Snapshot {
	s := adjustment.Snapshot{
		QuotaBytes:   u.QuotaBytes,
		UsedTotal:    u.UsedTotal(),
		AccessTierID: u.AccessTierID,
		Status:       string(u.Status),
	}
	if u.ExpiresAt != nil {
		s.ExpiresAt = *u.ExpiresAt
	}
	return s
}

func (s *Server) handleUserAdjustments(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	records, err := s.adjustments.ListByUser(r.Context(), id, limit)
	if err != nil {
		s.logger.Error("查询调整记录失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": records})
}

// batchRequest 是批量操作。用户数以 10 人为量级,不做分批与异步:
// 一次请求内顺序执行,把每个用户的结果都带回去。
type batchRequest struct {
	UserIDs []int64 `json:"user_ids"`
	adjustRequest
}

type batchItemResult struct {
	UserID int64  `json:"user_id"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) handleBatchAdjust(w http.ResponseWriter, r *http.Request) {
	var req batchRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if len(req.UserIDs) == 0 {
		writeError(w, http.StatusBadRequest, "请至少选择一个用户")
		return
	}
	if len(req.UserIDs) > 100 {
		writeError(w, http.StatusBadRequest, "一次最多处理 100 个用户")
		return
	}
	admin := adminFromContext(r.Context())

	// 部分失败不回滚已成功的那些:批量操作里最常见的失败是
	// "这个用户是不限量,加不了流量",为它把其余人的续期一起撤销毫无道理。
	// 逐条返回结果,由管理员决定要不要单独处理。
	results := make([]batchItemResult, 0, len(req.UserIDs))
	var succeeded int
	for _, id := range req.UserIDs {
		after, params, err := s.applyAdjustment(r.Context(), id, req.adjustRequest, &admin.ID)
		if err != nil {
			results = append(results, batchItemResult{UserID: id, OK: false, Error: err.Error()})
			continue
		}
		if err := s.adjustments.Record(r.Context(), params); err != nil {
			s.logger.Error("记录批量调整失败", "error", err, "user_id", id)
		}
		succeeded++
		results = append(results, batchItemResult{UserID: id, OK: true})
		_ = after
	}

	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionUserBatch,
		TargetType: "user", TargetID: "",
		Detail: fmt.Sprintf("%s:%d 个用户中 %d 个成功;%s",
			adjustment.ActionText(req.Action), len(req.UserIDs), succeeded, req.Remark),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: succeeded == len(req.UserIDs),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"total": len(req.UserIDs), "succeeded": succeeded, "items": results,
	})
}

// handlePortalAdjustments 返回用户自己能看到的调整记录。
func (s *Server) handlePortalAdjustments(w http.ResponseWriter, r *http.Request) {
	identity := portalFromContext(r.Context())
	records, err := s.adjustments.PublicByUser(r.Context(), identity.ProxyUserID, 20)
	if err != nil {
		s.writePortalError(w, err, "查询调整记录失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": records})
}
