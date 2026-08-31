package dynamic

// approvals_handler.go — the Fiber surface of the approvals primitive.
//
// Mounted separately from the CRUD routes because hosts typically hang it off
// their own /api prefix with their own auth middleware:
//
//	GET  /approvals            list (org-scoped; ?status=&kind=&model=&mine=1&for_me=1&page=&per_page=)
//	GET  /approvals/count      badge counts {for_me, mine}
//	GET  /approvals/:id        one request (org-scoped, lazily expired)
//	POST /approvals/:id/approve {reason?}  — role-gated; replays the mutation
//	POST /approvals/:id/reject  {reason?}  — role-gated
//
// Every response uses the kernel envelope {success, data, meta?}.

import (
	"errors"
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// MountApprovals registers the approvals routes under r (typically an
// /approvals group). Middleware (auth) runs before every route.
func (h *Handler) MountApprovals(r fiber.Router, middleware ...fiber.Handler) {
	registerMut(r.Get, "/approvals", middleware, h.approvalsList)
	registerMut(r.Get, "/approvals/count", middleware, h.approvalsCount)
	registerMut(r.Get, "/approvals/:id", middleware, h.approvalsGet)
	registerMut(r.Post, "/approvals/:id/approve", middleware, h.approvalsApprove)
	registerMut(r.Post, "/approvals/:id/reject", middleware, h.approvalsReject)
}

func (h *Handler) approvalsList(c fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	q := ApprovalListQuery{
		Status: c.Query("status"),
		Kind:   c.Query("kind"),
		Model:  c.Query("model"),
		Mine:   queryBool(c, "mine"),
		ForMe:  queryBool(c, "for_me"),
	}
	if v := c.Query("page"); v != "" {
		q.Page, _ = strconv.Atoi(v)
	}
	if v := c.Query("per_page"); v != "" {
		q.PerPage, _ = strconv.Atoi(v)
	}
	items, total, err := h.service.ListApprovals(c, u, q)
	if err != nil {
		return h.approvalError(c, err, nil)
	}
	perPage := q.PerPage
	if perPage <= 0 {
		perPage = 50
	}
	page := q.Page
	if page <= 0 {
		page = 1
	}
	return c.JSON(fiber.Map{
		"success": true,
		"data":    items,
		"meta": fiber.Map{
			"total":        total,
			"per_page":     perPage,
			"current_page": page,
		},
	})
}

func (h *Handler) approvalsCount(c fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	counts, err := h.service.CountApprovals(c, u)
	if err != nil {
		return h.approvalError(c, err, nil)
	}
	return c.JSON(fiber.Map{"success": true, "data": counts})
}

func (h *Handler) approvalsGet(c fiber.Ctx) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, ErrInvalidID.Error())
	}
	req, err := h.service.GetApproval(c, u, id)
	if err != nil {
		return h.approvalError(c, err, req)
	}
	return c.JSON(fiber.Map{"success": true, "data": req.View(h.service.CanDecideApproval(c, u, req))})
}

// approvalDecisionBody is the approve/reject request body.
type approvalDecisionBody struct {
	Reason string `json:"reason"`
}

func (h *Handler) approvalsApprove(c fiber.Ctx) error {
	return h.approvalsDecide(c, true)
}

func (h *Handler) approvalsReject(c fiber.Ctx) error {
	return h.approvalsDecide(c, false)
}

func (h *Handler) approvalsDecide(c fiber.Ctx, approve bool) error {
	u := h.user(c)
	if u == nil {
		return respondErr(c, fiber.StatusUnauthorized, "not authenticated")
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return respondErr(c, fiber.StatusBadRequest, ErrInvalidID.Error())
	}
	var body approvalDecisionBody
	if len(c.Body()) > 0 {
		if err := c.Bind().Body(&body); err != nil {
			return respondErr(c, fiber.StatusBadRequest, "invalid body")
		}
	}
	var req *ApprovalRequest
	if approve {
		req, err = h.service.ApproveRequest(c, u, id, body.Reason)
	} else {
		req, err = h.service.RejectRequest(c, u, id, body.Reason)
	}
	if err != nil {
		return h.approvalError(c, err, req)
	}
	return c.JSON(fiber.Map{"success": true, "data": req.View(false)})
}

// approvalError maps approvals-specific failures onto the envelope. `req` (when
// non-nil) rides in `data` so the client can render the request's real state
// after a lost race (already decided / expired) or a failed replay.
func (h *Handler) approvalError(c fiber.Ctx, err error, req *ApprovalRequest) error {
	var applyErr *ApprovalApplyError
	status := 0
	code := ""
	switch {
	case errors.As(err, &applyErr):
		// Decision recorded, replay failed: the request is marked failed with
		// the stored cause. 422 — the mutation could not be applied.
		status, code = fiber.StatusUnprocessableEntity, "approval_apply_failed"
		if applyErr.Request != nil {
			req = applyErr.Request
		}
	case errors.Is(err, ErrApprovalReasonRequired):
		status, code = fiber.StatusUnprocessableEntity, "approval_reason_required"
	case errors.Is(err, ErrApprovalExpired):
		status, code = fiber.StatusConflict, "approval_expired"
	case errors.Is(err, ErrApprovalNotPending):
		status, code = fiber.StatusConflict, "approval_not_pending"
	case errors.Is(err, ErrForbidden):
		status, code = fiber.StatusForbidden, "forbidden"
	case errors.Is(err, ErrRecordNotFound):
		status, code = fiber.StatusNotFound, "not_found"
	case errors.Is(err, ErrTenantScopeUnavailable):
		status, code = fiber.StatusForbidden, "forbidden"
	default:
		status, code = fiber.StatusInternalServerError, "internal_error"
	}
	body := fiber.Map{
		"success": false,
		"message": err.Error(),
		"error":   fiber.Map{"code": code, "message": err.Error()},
	}
	if req != nil {
		body["data"] = req.View(false)
	}
	return c.Status(status).JSON(body)
}

func queryBool(c fiber.Ctx, key string) bool {
	switch c.Query(key) {
	case "1", "true", "yes":
		return true
	}
	return false
}
