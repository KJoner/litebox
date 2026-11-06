package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/billing"
	"github.com/litebox/litebox/internal/user"
)

// 用户计费规则(VNext TRAFFIC02 / 08)。默认下一周期生效;立即生效封存旧段。

const actionUserBilling = "user.billing"

type userBillingRequest struct {
	Mode          string `json:"mode"`
	MultiplierPct int    `json:"multiplier_pct"`
	Immediate     bool   `json:"immediate"`
	// Preview 为真时只算不写。
	Preview bool `json:"preview"`
}

func (s *Server) handleSetUserBilling(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	var req userBillingRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	rule := billing.Rule{Mode: billing.Mode(strings.ToUpper(strings.TrimSpace(req.Mode))), MultiplierPct: req.MultiplierPct}
	var (
		change *user.BillingChange
		err    error
	)
	if req.Preview {
		change, err = s.users.Store().PreviewBilling(r.Context(), id, rule, req.Immediate)
	} else {
		change, err = s.users.SetBilling(r.Context(), id, rule, req.Immediate)
	}
	if err != nil {
		switch {
		case errors.Is(err, billing.ErrBadRule), errors.Is(err, user.ErrBillingNoNextPeriod):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			s.writeUserError(w, err, "修改计费规则失败")
		}
		return
	}
	if !req.Preview {
		admin := adminFromContext(r.Context())
		when := "下一周期生效"
		if req.Immediate {
			when = fmt.Sprintf("立即生效(旧段封存;计费流量 %d → %d 字节)", change.BilledBefore, change.BilledAfter)
		}
		code := fmt.Sprint(id)
		if u, err := s.users.Store().Get(r.Context(), id); err == nil {
			code = u.UserCode
		}
		s.audit.Record(r.Context(), audit.Entry{
			AdminUserID: &admin.ID, Action: actionUserBilling, TargetType: "user", TargetID: code,
			Detail:   fmt.Sprintf("%s → %s,%s", change.CurrentLabel, change.NextLabel, when),
			ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
		})
	}
	writeJSON(w, http.StatusOK, change)
}
