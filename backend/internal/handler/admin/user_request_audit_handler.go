package admin

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

type UserRequestAuditHandler struct {
	service *service.UserRequestAuditService
}

func NewUserRequestAuditHandler(svc *service.UserRequestAuditService) *UserRequestAuditHandler {
	return &UserRequestAuditHandler{service: svc}
}

type userRequestAuditListItem struct {
	ID                 int64          `json:"id"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	ExpiresAt          time.Time      `json:"expires_at"`
	UserID             int64          `json:"user_id"`
	UserLabel          string         `json:"user_label"`
	RequestCount       int            `json:"request_count"`
	ConversationKey    string         `json:"conversation_key"`
	APIKeyID           int64          `json:"api_key_id"`
	GroupID            *int64         `json:"group_id,omitempty"`
	GroupName          string         `json:"group_name,omitempty"`
	Protocol           string         `json:"protocol"`
	Endpoint           string         `json:"endpoint"`
	RequestedModel     string         `json:"requested_model"`
	UpstreamModel      string         `json:"upstream_model,omitempty"`
	ClientRequestID    string         `json:"client_request_id,omitempty"`
	ResponseID         string         `json:"response_id,omitempty"`
	PreviousResponseID string         `json:"previous_response_id,omitempty"`
	FallbackHash       string         `json:"fallback_hash"`
	Status             string         `json:"status"`
	InputUsage         map[string]any `json:"input_usage,omitempty"`
	OutputUsage        map[string]any `json:"output_usage,omitempty"`
	CacheUsage         map[string]any `json:"cache_usage,omitempty"`
	LastError          string         `json:"last_error,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}

func (h *UserRequestAuditHandler) List(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	filter, err := parseUserRequestAuditFilter(c, page, pageSize)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	items, total, err := h.service.List(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]userRequestAuditListItem, 0, len(items))
	for _, item := range items {
		out = append(out, userRequestAuditListItem{ID: item.ID, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, ExpiresAt: item.ExpiresAt, UserID: item.UserID, APIKeyID: item.APIKeyID, GroupID: item.GroupID, GroupName: item.GroupName, Protocol: item.Protocol, Endpoint: item.Endpoint, RequestedModel: item.RequestedModel, UpstreamModel: item.UpstreamModel, ClientRequestID: item.ClientRequestID, ResponseID: item.ResponseID, PreviousResponseID: item.PreviousResponseID, FallbackHash: item.FallbackHash, Status: item.Status, InputUsage: item.InputUsage, OutputUsage: item.OutputUsage, CacheUsage: item.CacheUsage, LastError: item.LastError, Metadata: item.Metadata})
		out[len(out)-1].UserLabel = item.UserLabel
		out[len(out)-1].RequestCount = item.RequestCount
		out[len(out)-1].ConversationKey = item.ConversationKey
	}
	response.Paginated(c, out, total, page, pageSize)
}

func (h *UserRequestAuditHandler) Get(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid user request audit id")
		return
	}
	item, err := h.service.GetByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		response.ErrorWithDetails(c, http.StatusNotFound, "User request audit not found", "USER_REQUEST_AUDIT_NOT_FOUND", nil)
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *UserRequestAuditHandler) Conversation(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid conversation id")
		return
	}
	// Long conversations are read in request windows: a page costs the selected
	// slice instead of every stored history copy, so opening them stays bounded.
	offset, err := strconv.Atoi(strings.TrimSpace(c.Query("request_offset")))
	if err != nil || offset < 0 {
		offset = 0
	}
	limit, err := strconv.Atoi(strings.TrimSpace(c.Query("request_limit")))
	if err != nil || limit < 0 {
		limit = 0
	}
	conversation, err := h.service.GetConversationWindow(c.Request.Context(), id, offset, limit)
	if errors.Is(err, sql.ErrNoRows) {
		response.ErrorWithDetails(c, http.StatusNotFound, "Conversation not found", "USER_REQUEST_AUDIT_NOT_FOUND", nil)
		return
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// Each captured request stores a full copy of the history it replayed, so
	// serializing them alongside the deduplicated messages would make the reader
	// response grow quadratically. The reader renders messages; raw ChatML stays
	// available on the single-request detail endpoint.
	for _, request := range conversation.Requests {
		request.RequestChatML = ""
		request.ResponseChatML = ""
	}
	response.Success(c, conversation)
}

func parseUserRequestAuditFilter(c *gin.Context, page, pageSize int) (service.UserRequestAuditFilter, error) {
	filter := service.UserRequestAuditFilter{Page: page, PageSize: pageSize, GroupName: strings.TrimSpace(c.Query("group_name")), Protocol: strings.TrimSpace(c.Query("protocol")), RequestedModel: strings.TrimSpace(c.Query("requested_model")), ResponseID: strings.TrimSpace(c.Query("response_id")), ClientRequestID: strings.TrimSpace(c.Query("client_request_id")), Status: strings.TrimSpace(c.Query("status")), Q: strings.TrimSpace(c.Query("q"))}
	parseID := func(name string) (*int64, error) {
		raw := strings.TrimSpace(c.Query(name))
		if raw == "" {
			return nil, nil
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			return nil, errors.New("Invalid " + name)
		}
		return &value, nil
	}
	var err error
	if filter.UserID, err = parseID("user_id"); err != nil {
		return filter, err
	}
	if filter.APIKeyID, err = parseID("api_key_id"); err != nil {
		return filter, err
	}
	if filter.GroupID, err = parseID("group_id"); err != nil {
		return filter, err
	}
	parseTime := func(name string) (*time.Time, error) {
		raw := strings.TrimSpace(c.Query(name))
		if raw == "" {
			return nil, nil
		}
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, errors.New("Invalid " + name + ", expect RFC3339")
		}
		return &value, nil
	}
	if filter.StartTime, err = parseTime("start_time"); err != nil {
		return filter, err
	}
	if filter.EndTime, err = parseTime("end_time"); err != nil {
		return filter, err
	}
	return filter, nil
}

type userRequestAuditConfigRequest struct {
	RetentionDays        int      `json:"retention_days,omitempty"`
	CleanupIntervalHours int      `json:"cleanup_interval_hours,omitempty"`
	GroupIDs             *[]int64 `json:"group_ids"`
}

func (h *UserRequestAuditHandler) GetConfig(c *gin.Context) {
	cfg := h.service.GetArchiveConfig()
	if cfg.GroupIDs == nil {
		cfg.GroupIDs = []int64{}
	}
	response.Success(c, gin.H{"retention_days": cfg.RetentionDays, "cleanup_interval_hours": cfg.CleanupIntervalHours, "group_ids": cfg.GroupIDs, "storage": h.service.StorageStatus()})
}

func (h *UserRequestAuditHandler) UpdateConfig(c *gin.Context) {
	var req userRequestAuditConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid audit config")
		return
	}
	cfg := h.service.GetArchiveConfig()
	if req.RetentionDays > 0 {
		cfg.RetentionDays = req.RetentionDays
	}
	if req.CleanupIntervalHours > 0 {
		cfg.CleanupIntervalHours = req.CleanupIntervalHours
	}
	if req.GroupIDs != nil {
		cfg.GroupIDs = append([]int64{}, (*req.GroupIDs)...)
	}
	var err error
	if req.GroupIDs == nil {
		err = h.service.SetArchiveConfigPreservingGroups(cfg)
	} else {
		err = h.service.SetArchiveConfig(cfg)
	}
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, cfg)
}
func (h *UserRequestAuditHandler) Groups(c *gin.Context) {
	filter, err := parseUserRequestAuditFilter(c, 1, 200)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	stats, err := h.service.GroupStats(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, stats)
}

// Models lists the models present under the current filters so the console can
// offer real choices instead of free-typed model names.
func (h *UserRequestAuditHandler) Models(c *gin.Context) {
	filter, err := parseUserRequestAuditFilter(c, 1, 200)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	// The facet list feeds the model picker itself, so the model predicate is
	// dropped: options stay browsable while protocol/group/time filters narrow them.
	filter.RequestedModel = ""
	facets, err := h.service.ModelFacets(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, facets)
}

func (h *UserRequestAuditHandler) Export(c *gin.Context) {
	var req struct {
		Format          string  `json:"format"`
		UserID          *int64  `json:"user_id"`
		APIKeyID        *int64  `json:"api_key_id"`
		GroupID         *int64  `json:"group_id"`
		GroupName       string  `json:"group_name"`
		Protocol        string  `json:"protocol"`
		RequestedModel  string  `json:"requested_model"`
		ResponseID      string  `json:"response_id"`
		ClientRequestID string  `json:"client_request_id"`
		Status          string  `json:"status"`
		Q               string  `json:"q"`
		StartTime       string  `json:"start_time"`
		EndTime         string  `json:"end_time"`
		ConversationIDs []int64 `json:"conversation_ids"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			response.BadRequest(c, "invalid export request")
			return
		}
	}
	filter, err := parseUserRequestAuditFilter(c, 1, 200)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if req.UserID != nil {
		filter.UserID = req.UserID
	}
	if req.APIKeyID != nil {
		filter.APIKeyID = req.APIKeyID
	}
	if req.GroupID != nil {
		filter.GroupID = req.GroupID
	}
	if req.GroupName != "" {
		filter.GroupName = req.GroupName
	}
	if req.Protocol != "" {
		filter.Protocol = req.Protocol
	}
	if req.RequestedModel != "" {
		filter.RequestedModel = req.RequestedModel
	}
	if req.ResponseID != "" {
		filter.ResponseID = req.ResponseID
	}
	if req.ClientRequestID != "" {
		filter.ClientRequestID = req.ClientRequestID
	}
	if req.Status != "" {
		filter.Status = req.Status
	}
	if req.Q != "" {
		filter.Q = req.Q
	}
	parseBodyTime := func(raw string) (*time.Time, error) {
		if strings.TrimSpace(raw) == "" {
			return nil, nil
		}
		v, e := time.Parse(time.RFC3339, raw)
		return &v, e
	}
	if req.StartTime != "" {
		filter.StartTime, err = parseBodyTime(req.StartTime)
		if err != nil {
			response.BadRequest(c, "invalid start_time")
			return
		}
	}
	if req.EndTime != "" {
		filter.EndTime, err = parseBodyTime(req.EndTime)
		if err != nil {
			response.BadRequest(c, "invalid end_time")
			return
		}
	}
	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "jsonl"
	}
	if format != "json" && format != "jsonl" {
		response.BadRequest(c, "format must be json or jsonl")
		return
	}
	for _, id := range []*int64{filter.UserID, filter.APIKeyID, filter.GroupID} {
		if id != nil && *id <= 0 {
			response.BadRequest(c, "filter IDs must be positive integers")
			return
		}
	}
	if filter.StartTime != nil && filter.EndTime != nil && filter.StartTime.After(*filter.EndTime) {
		response.BadRequest(c, "start_time must not be after end_time")
		return
	}
	job, err := h.service.CreateExport(c.Request.Context(), filter, format, req.ConversationIDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, job)
}
func (h *UserRequestAuditHandler) Status(c *gin.Context) {
	response.Success(c, h.service.StorageStatus())
}
func (h *UserRequestAuditHandler) Cleanup(c *gin.Context) {
	deleted, err := h.service.CleanupNow(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": deleted, "storage": h.service.StorageStatus()})
}

func (h *UserRequestAuditHandler) Exports(c *gin.Context) {
	response.Success(c, h.service.ExportJobs())
}

func (h *UserRequestAuditHandler) DownloadExport(c *gin.Context) {
	file, filename, err := h.service.OpenExport(c.Param("job_id"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Content-Type", "application/gzip")
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Header("Cache-Control", "no-store")
	http.ServeContent(c.Writer, c.Request, filename, info.ModTime(), file)
}

func (h *UserRequestAuditHandler) Clear(c *gin.Context) {
	deleted, err := h.service.ClearAudits(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": deleted, "storage": h.service.StorageStatus()})
}
