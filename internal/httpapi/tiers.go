package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/audit"
)

const actionTierUpdate = "tier.update"

func (s *Server) handleListTiers(w http.ResponseWriter, r *http.Request) {
	tiers, err := s.tiers.List(r.Context())
	if err != nil {
		s.logger.Error("查询访问等级失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": tiers})
}

// updateTierRequest 只含可改字段。code 与 level 不开放:
// 前者是程序内的引用标识,后者决定继承关系 —— 改 level 会让所有用户
// 可用的节点集合同时变化,而页面上看不出发生了什么。
type updateTierRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	SortOrder   int    `json:"sort_order"`
}

func (s *Server) handleUpdateTier(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "等级 ID 非法")
		return
	}
	var req updateTierRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	tier, err := s.tiers.Update(r.Context(), id, access.UpdateParams{
		Name:        req.Name,
		Description: req.Description,
		SortOrder:   req.SortOrder,
	})
	if err != nil {
		if errors.Is(err, access.ErrTierNotFound) {
			writeError(w, http.StatusNotFound, "访问等级不存在")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionTierUpdate,
		TargetType: "access_tier", TargetID: tier.Code,
		Detail: "等级名称改为 " + tier.Name, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, tier)
}
