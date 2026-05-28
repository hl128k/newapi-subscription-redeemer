package redeemer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"io/fs"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

const (
	defaultDBPath = "redeemer.db"
	alphabet      = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

var validStatuses = map[string]bool{
	"active":   true,
	"pending":  true,
	"used":     true,
	"disabled": true,
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS subscription_codes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    code TEXT NOT NULL UNIQUE,
    plan_id INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    note TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    expires_at INTEGER,
    used_at INTEGER,
    used_by_user_id INTEGER,
    pending_at INTEGER,
    pending_user_id INTEGER,
    pending_token TEXT,
    newapi_message TEXT,
    newapi_response_json TEXT,
    last_error TEXT
);

CREATE INDEX IF NOT EXISTS idx_subscription_codes_status
    ON subscription_codes(status);
CREATE INDEX IF NOT EXISTS idx_subscription_codes_plan_id
    ON subscription_codes(plan_id);

CREATE TABLE IF NOT EXISTS audit_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type TEXT NOT NULL,
    actor_type TEXT NOT NULL DEFAULT '',
    actor_id TEXT NOT NULL DEFAULT '',
    code TEXT,
    plan_id INTEGER,
    status TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_events_created_at
    ON audit_events(created_at);
CREATE INDEX IF NOT EXISTS idx_audit_events_event_type
    ON audit_events(event_type);
CREATE INDEX IF NOT EXISTS idx_audit_events_code
    ON audit_events(code);

CREATE TABLE IF NOT EXISTS verification_locks (
    key_type TEXT NOT NULL,
    key_value TEXT NOT NULL,
    failure_count INTEGER NOT NULL DEFAULT 0,
    locked_until INTEGER,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (key_type, key_value)
);

CREATE INDEX IF NOT EXISTS idx_verification_locks_locked_until
    ON verification_locks(locked_until);
`

const codeColumns = `
    id, code, plan_id, status, note, metadata_json, created_at, expires_at,
    used_at, used_by_user_id, pending_at, pending_user_id, pending_token,
    newapi_message, newapi_response_json, last_error
`

type Config struct {
	DBPath                 string
	NewAPIBaseURL          string
	NewAPIAdminAccessToken string
	NewAPIAdminUserID      int64
	BindMode               string
	TimeoutSeconds         float64
	AdminSecret            string
	AdminPrefix            string
	UpstreamName           string
	Host                   string
	Port                   int
	PreviewRateLimit       int
	PreviewRateWindow      time.Duration
	PreviewMismatchLimit   int
	PreviewLockDuration    time.Duration
}

func ConfigFromEnv() Config {
	return Config{
		DBPath:                 envString("REDEEMER_DB_PATH", defaultDBPath),
		NewAPIBaseURL:          strings.TrimSpace(os.Getenv("NEWAPI_BASE_URL")),
		NewAPIAdminAccessToken: strings.TrimSpace(os.Getenv("NEWAPI_ADMIN_ACCESS_TOKEN")),
		NewAPIAdminUserID:      envInt64("NEWAPI_ADMIN_USER_ID", 0),
		BindMode:               envString("REDEEMER_BIND_MODE", "bind"),
		TimeoutSeconds:         envFloat("REDEEMER_TIMEOUT_SECONDS", 20),
		AdminSecret:            strings.TrimSpace(os.Getenv("REDEEMER_ADMIN_SECRET")),
		AdminPrefix: normalizeAdminPrefix(
			envString("REDEEMER_ADMIN_PREFIX", envString("REDEEMER_ADMIN_WEB_PREFIX", "xx")),
		),
		UpstreamName:      envString("REDEEMER_UPSTREAM_NAME", "NewAPI"),
		Host:              envString("REDEEMER_HOST", "127.0.0.1"),
		Port:              int(envInt64("REDEEMER_PORT", 8789)),
		PreviewRateLimit:  int(envInt64("REDEEMER_PREVIEW_RATE_LIMIT", 10)),
		PreviewRateWindow: time.Duration(envInt64("REDEEMER_PREVIEW_RATE_WINDOW_SECONDS", 60)) * time.Second,
		PreviewMismatchLimit: int(envInt64(
			"REDEEMER_PREVIEW_MISMATCH_LIMIT",
			5,
		)),
		PreviewLockDuration: time.Duration(envInt64("REDEEMER_PREVIEW_LOCK_SECONDS", 900)) * time.Second,
	}
}

func (c Config) ValidateRuntime(requireAdminSecret bool) error {
	if strings.TrimSpace(c.DBPath) == "" {
		return serviceError("REDEEMER_DB_PATH 未配置", http.StatusInternalServerError)
	}
	if requireAdminSecret && strings.TrimSpace(c.AdminSecret) == "" {
		return serviceError("REDEEMER_ADMIN_SECRET 未配置", http.StatusInternalServerError)
	}
	if c.BindMode != "bind" && c.BindMode != "create" {
		return serviceError("REDEEMER_BIND_MODE 只能是 bind 或 create", http.StatusInternalServerError)
	}
	if strings.TrimSpace(c.NewAPIBaseURL) == "" {
		return serviceError("NEWAPI_BASE_URL 未配置", http.StatusInternalServerError)
	}
	if strings.TrimSpace(c.NewAPIAdminAccessToken) == "" {
		return serviceError("NEWAPI_ADMIN_ACCESS_TOKEN 未配置", http.StatusInternalServerError)
	}
	if c.NewAPIAdminUserID <= 0 {
		return serviceError("NEWAPI_ADMIN_USER_ID 必须大于 0", http.StatusInternalServerError)
	}
	return nil
}

func (c Config) AdminWebPath() string {
	return "/" + c.AdminPrefix + "/admin"
}

func (c Config) AdminAPIPath(path string) string {
	return "/" + c.AdminPrefix + path
}

type ServiceError struct {
	Message string
	Status  int
}

func (e ServiceError) Error() string {
	return e.Message
}

func serviceError(message string, status int) error {
	return ServiceError{Message: message, Status: status}
}

func errorStatus(err error) (int, string) {
	var svcErr ServiceError
	if errors.As(err, &svcErr) {
		return svcErr.Status, svcErr.Message
	}
	return http.StatusInternalServerError, "internal error: " + err.Error()
}

type NewAPIResult struct {
	Message string
	Raw     map[string]any
}

type Service struct {
	config Config
	db     *sql.DB
	client *http.Client
}

func NewService(config Config) (*Service, error) {
	if strings.TrimSpace(config.DBPath) == "" {
		return nil, serviceError("REDEEMER_DB_PATH 未配置", http.StatusInternalServerError)
	}
	if config.DBPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(config.DBPath), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", config.DBPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &Service{
		config: config,
		db:     db,
		client: &http.Client{Timeout: time.Duration(config.TimeoutSeconds * float64(time.Second))},
	}, nil
}

func (s *Service) Close() error {
	return s.db.Close()
}

func (s *Service) InitDB() error {
	_, err := s.db.Exec(schemaSQL)
	return err
}

func (s *Service) CreateCodes(ctx context.Context, input CreateCodesInput) ([]map[string]any, error) {
	if input.PlanID <= 0 {
		return nil, serviceError("plan_id 必须大于 0", http.StatusBadRequest)
	}
	if input.Count <= 0 || input.Count > 1000 {
		return nil, serviceError("count 必须在 1-1000 之间", http.StatusBadRequest)
	}
	now := time.Now().Unix()
	if input.ExpiresAt != nil && *input.ExpiresAt <= now {
		return nil, serviceError("expires_at 必须晚于当前时间", http.StatusBadRequest)
	}

	prefix := normalizePrefix(input.Prefix)
	metadata := input.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollbackUnlessDone(tx)

	created := make([]map[string]any, 0, input.Count)
	for i := 0; i < input.Count; i++ {
		code, err := s.generateUniqueCode(ctx, tx, prefix)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(
			ctx,
			`INSERT INTO subscription_codes (
				code, plan_id, status, note, metadata_json, created_at, expires_at
			) VALUES (?, ?, 'active', ?, ?, ?, ?)`,
			code,
			input.PlanID,
			input.Note,
			string(metadataJSON),
			now,
			input.ExpiresAt,
		)
		if err != nil {
			return nil, err
		}
		created = append(created, map[string]any{
			"code":       code,
			"plan_id":    input.PlanID,
			"status":     "active",
			"note":       input.Note,
			"created_at": now,
			"expires_at": nullableInt(input.ExpiresAt),
			"metadata":   metadata,
		})
	}

	auditMetadata := mergeMetadata(map[string]any{
		"count":      len(created),
		"prefix":     prefix,
		"note":       input.Note,
		"expires_at": nullableInt(input.ExpiresAt),
		"codes":      codesFromMaps(created),
	}, input.AuditMetadata)
	if err := s.insertAuditEvent(ctx, tx, AuditEventInput{
		EventType: input.auditEvent("codes.created"),
		ActorType: input.auditActorType("cli"),
		ActorID:   input.AuditActorID,
		PlanID:    &input.PlanID,
		Status:    "active",
		Message:   fmt.Sprintf("created %d codes", len(created)),
		Metadata:  auditMetadata,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return created, nil
}

func (s *Service) ListCodes(ctx context.Context, status string, planID *int64, limit int) ([]map[string]any, error) {
	if status != "" && !validStatuses[status] {
		return nil, serviceError("无效状态: "+status, http.StatusBadRequest)
	}
	if limit <= 0 || limit > 5000 {
		return nil, serviceError("limit 必须在 1-5000 之间", http.StatusBadRequest)
	}

	clauses := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, status)
	}
	if planID != nil {
		clauses = append(clauses, "plan_id = ?")
		args = append(args, *planID)
	}

	query := "SELECT " + codeColumns + " FROM subscription_codes"
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []map[string]any
	for rows.Next() {
		record, err := scanCode(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, record.toMap())
	}
	return items, rows.Err()
}

func (s *Service) ListAuditEvents(ctx context.Context, eventType, code string, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 5000 {
		return nil, serviceError("limit 必须在 1-5000 之间", http.StatusBadRequest)
	}
	clauses := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if eventType != "" {
		clauses = append(clauses, "event_type = ?")
		args = append(args, eventType)
	}
	if strings.TrimSpace(code) != "" {
		clauses = append(clauses, "code = ?")
		args = append(args, strings.TrimSpace(code))
	}

	query := `SELECT id, event_type, actor_type, actor_id, code, plan_id, status,
		message, metadata_json, created_at FROM audit_events`
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []map[string]any
	for rows.Next() {
		event, err := scanAuditEvent(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, event.toMap())
	}
	return items, rows.Err()
}

func (s *Service) SetCodeStatus(ctx context.Context, input SetStatusInput) (map[string]any, error) {
	code := strings.TrimSpace(input.Code)
	if code == "" {
		return nil, serviceError("code 不能为空", http.StatusBadRequest)
	}
	if input.Status != "active" && input.Status != "disabled" {
		return nil, serviceError("只允许切换到 active 或 disabled", http.StatusBadRequest)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollbackUnlessDone(tx)

	row := tx.QueryRowContext(ctx, "SELECT id, plan_id, status, used_at FROM subscription_codes WHERE code = ?", code)
	var id int64
	var planID int64
	var oldStatus string
	var usedAt sql.NullInt64
	if err := row.Scan(&id, &planID, &oldStatus, &usedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, serviceError("兑换码不存在", http.StatusNotFound)
		}
		return nil, err
	}
	if usedAt.Valid {
		return nil, serviceError("已使用的兑换码不能改状态", http.StatusConflict)
	}
	if oldStatus == "pending" {
		return nil, serviceError("处理中的兑换码不能改状态", http.StatusConflict)
	}

	_, err = tx.ExecContext(ctx, `UPDATE subscription_codes
		SET status = ?, pending_at = NULL, pending_user_id = NULL,
			pending_token = NULL, last_error = NULL
		WHERE id = ?`, input.Status, id)
	if err != nil {
		return nil, err
	}

	if err := s.insertAuditEvent(ctx, tx, AuditEventInput{
		EventType: input.auditEvent("code.status_changed"),
		ActorType: input.auditActorType("cli"),
		ActorID:   input.AuditActorID,
		Code:      &code,
		PlanID:    &planID,
		Status:    input.Status,
		Message:   fmt.Sprintf("status changed from %s to %s", oldStatus, input.Status),
		Metadata: mergeMetadata(map[string]any{
			"old_status": oldStatus,
			"new_status": input.Status,
		}, input.AuditMetadata),
	}); err != nil {
		return nil, err
	}

	record, err := s.fetchCodeByID(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return record.toMap(), nil
}

func (s *Service) BatchCodes(ctx context.Context, input BatchCodesInput) (map[string]any, error) {
	codes, err := normalizeCodeList(input.Codes)
	if err != nil {
		return nil, err
	}

	action := strings.TrimSpace(input.Action)
	targetStatus := ""
	eventType := ""
	switch action {
	case "delete":
		eventType = "codes.deleted"
	case "disable":
		targetStatus = "disabled"
		eventType = "codes.status_changed"
	case "restore":
		targetStatus = "active"
		eventType = "codes.status_changed"
	default:
		return nil, serviceError("批量操作必须是 delete、disable 或 restore", http.StatusBadRequest)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollbackUnlessDone(tx)

	records := make([]*CodeRecord, 0, len(codes))
	for _, code := range codes {
		record, err := s.fetchCodeByCode(ctx, tx, code)
		if err != nil {
			return nil, err
		}
		if action == "delete" {
			if record.Status == "used" {
				return nil, serviceError("已使用的兑换码不能删除", http.StatusConflict)
			}
			if record.Status == "pending" {
				return nil, serviceError("处理中的兑换码不能删除", http.StatusConflict)
			}
		}
		if targetStatus != "" && record.UsedAt.Valid {
			return nil, serviceError("已使用的兑换码不能改状态", http.StatusConflict)
		}
		if targetStatus != "" && record.Status == "pending" {
			return nil, serviceError("处理中的兑换码不能改状态", http.StatusConflict)
		}
		records = append(records, record)
	}

	for _, record := range records {
		if action == "delete" {
			if _, err := tx.ExecContext(ctx, "DELETE FROM subscription_codes WHERE id = ?", record.ID); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE subscription_codes
			SET status = ?, pending_at = NULL, pending_user_id = NULL,
				pending_token = NULL, last_error = NULL
			WHERE id = ?`, targetStatus, record.ID); err != nil {
			return nil, err
		}
	}

	metadata := mergeMetadata(map[string]any{
		"action": action,
		"count":  len(records),
		"codes":  codes,
	}, input.AuditMetadata)
	if targetStatus != "" {
		metadata["new_status"] = targetStatus
	}
	if err := s.insertAuditEvent(ctx, tx, AuditEventInput{
		EventType: eventType,
		ActorType: input.auditActorType("cli"),
		ActorID:   input.AuditActorID,
		Status:    targetStatus,
		Message:   fmt.Sprintf("%s %d codes", action, len(records)),
		Metadata:  metadata,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	result := map[string]any{
		"action": action,
		"count":  len(records),
		"codes":  codes,
	}
	if targetStatus != "" {
		result["status"] = targetStatus
	}
	return result, nil
}

func (s *Service) PreviewRedeemCode(ctx context.Context, code string, userID int64, email string) (map[string]any, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, serviceError("code 不能为空", http.StatusBadRequest)
	}
	if userID <= 0 {
		return nil, serviceError("user_id 必须大于 0", http.StatusBadRequest)
	}
	record, err := s.fetchCodeByCode(ctx, s.db, code)
	if err != nil {
		return nil, err
	}
	if err := validateRedeemable(record); err != nil {
		return nil, err
	}
	user, err := s.verifyNewAPIUserEmail(ctx, userID, email)
	if err != nil {
		return nil, err
	}
	plan, err := s.fetchSubscriptionPlan(ctx, record.PlanID)
	if err != nil {
		return nil, err
	}
	planLimit := subscriptionPlanPurchaseLimit(plan)
	if err := s.enforcePlanPurchaseLimit(ctx, s.db, userID, record.PlanID, planLimit); err != nil {
		return nil, err
	}
	return map[string]any{
		"code":       record.Code,
		"user_id":    userID,
		"plan_id":    record.PlanID,
		"plan_name":  subscriptionPlanName(plan, record.PlanID),
		"plan":       plan,
		"status":     record.Status,
		"expires_at": nullIntToAny(record.ExpiresAt),
		"user":       user,
	}, nil
}

func (s *Service) RedeemCode(ctx context.Context, input RedeemInput) (map[string]any, error) {
	code := strings.TrimSpace(input.Code)
	if code == "" {
		return nil, serviceError("code 不能为空", http.StatusBadRequest)
	}
	if input.UserID <= 0 {
		return nil, serviceError("user_id 必须大于 0", http.StatusBadRequest)
	}
	record, err := s.fetchCodeByCode(ctx, s.db, code)
	if err != nil {
		return nil, err
	}
	if err := validateRedeemable(record); err != nil {
		return nil, err
	}
	var verifiedUser map[string]any
	if strings.TrimSpace(input.Email) != "" {
		user, err := s.verifyNewAPIUserEmail(ctx, input.UserID, input.Email)
		if err != nil {
			return nil, err
		}
		verifiedUser = user
	}
	plan, err := s.fetchSubscriptionPlan(ctx, record.PlanID)
	if err != nil {
		return nil, err
	}
	planLimit := subscriptionPlanPurchaseLimit(plan)
	if err := s.enforcePlanPurchaseLimit(ctx, s.db, input.UserID, record.PlanID, planLimit); err != nil {
		return nil, err
	}
	if input.AuditActorType == "" {
		input.AuditActorType = "user"
	}
	if input.AuditActorID == "" {
		input.AuditActorID = strconv.FormatInt(input.UserID, 10)
	}

	claimed, err := s.claimCode(ctx, code, input.UserID, planLimit)
	if err != nil {
		return nil, err
	}

	newAPIResult, err := s.activateSubscription(ctx, input.UserID, claimed.PlanID)
	if err != nil {
		_ = s.releaseClaim(ctx, claimed.ID, claimed.PendingToken.String, err.Error())
		_ = s.recordAuditEvent(ctx, AuditEventInput{
			EventType: "code.redeem_failed",
			ActorType: input.AuditActorType,
			ActorID:   input.AuditActorID,
			Code:      &code,
			PlanID:    &claimed.PlanID,
			Status:    "active",
			Message:   err.Error(),
			Metadata:  redeemAuditMetadata(input.UserID, verifiedUser, input.AuditMetadata),
		})
		return nil, err
	}

	return s.finalizeClaim(ctx, claimed.ID, claimed.PendingToken.String, input.UserID, verifiedUser, newAPIResult, input)
}

func (s *Service) claimCode(ctx context.Context, code string, userID int64, planLimit int64) (*CodeRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollbackUnlessDone(tx)

	record, err := s.fetchCodeByCode(ctx, tx, code)
	if err != nil {
		return nil, err
	}
	if err := validateRedeemable(record); err != nil {
		return nil, err
	}
	if err := s.enforcePlanPurchaseLimit(ctx, tx, userID, record.PlanID, planLimit); err != nil {
		return nil, err
	}

	token, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	_, err = tx.ExecContext(ctx, `UPDATE subscription_codes
		SET status = 'pending', pending_at = ?, pending_user_id = ?,
			pending_token = ?, last_error = NULL
		WHERE id = ?`, now, userID, token, record.ID)
	if err != nil {
		return nil, err
	}
	record.PendingToken = sql.NullString{String: token, Valid: true}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return record, nil
}

func (s *Service) releaseClaim(ctx context.Context, codeID int64, pendingToken, errorMessage string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE subscription_codes
		SET status = 'active', pending_at = NULL, pending_user_id = NULL,
			pending_token = NULL, last_error = ?
		WHERE id = ? AND status = 'pending' AND pending_token = ?`,
		truncate(errorMessage, 1000), codeID, pendingToken)
	return err
}

func (s *Service) finalizeClaim(ctx context.Context, codeID int64, pendingToken string, userID int64, verifiedUser map[string]any, result NewAPIResult, input RedeemInput) (map[string]any, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollbackUnlessDone(tx)

	raw, err := json.Marshal(result.Raw)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `UPDATE subscription_codes
		SET status = 'used', used_at = ?, used_by_user_id = ?,
			pending_at = NULL, pending_user_id = NULL, pending_token = NULL,
			newapi_message = ?, newapi_response_json = ?, last_error = NULL
		WHERE id = ? AND status = 'pending' AND pending_token = ?`,
		now, userID, result.Message, string(raw), codeID, pendingToken)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, serviceError("兑换状态冲突，未能完成写回", http.StatusConflict)
	}

	record, err := s.fetchCodeByID(ctx, tx, codeID)
	if err != nil {
		return nil, err
	}
	if err := s.insertAuditEvent(ctx, tx, AuditEventInput{
		EventType: "code.redeemed",
		ActorType: input.auditActorType("user"),
		ActorID:   input.AuditActorID,
		Code:      &record.Code,
		PlanID:    &record.PlanID,
		Status:    "used",
		Message:   result.Message,
		Metadata:  redeemAuditMetadata(userID, verifiedUser, input.AuditMetadata),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	item := record.toMap()
	item["newapi"] = result.Raw
	return item, nil
}

func (s *Service) activateSubscription(ctx context.Context, userID, planID int64) (NewAPIResult, error) {
	var path string
	var payload map[string]any
	if s.config.BindMode == "create" {
		path = fmt.Sprintf("/api/subscription/admin/users/%d/subscriptions", userID)
		payload = map[string]any{"plan_id": planID}
	} else {
		path = "/api/subscription/admin/bind"
		payload = map[string]any{"user_id": userID, "plan_id": planID}
	}
	data, err := s.doNewAPIAdminJSON(ctx, http.MethodPost, path, payload)
	if err != nil {
		return NewAPIResult{}, err
	}
	message, _ := data["message"].(string)
	if dataField, ok := data["data"].(map[string]any); ok {
		if nestedMessage, ok := dataField["message"].(string); ok && nestedMessage != "" {
			message = nestedMessage
		}
	}
	if message == "" {
		message = "订阅已激活"
	}
	return NewAPIResult{Message: message, Raw: data}, nil
}

func (s *Service) verifyNewAPIUserEmail(ctx context.Context, userID int64, email string) (map[string]any, error) {
	email = normalizeEmail(email)
	if email == "" {
		return nil, serviceError("email 不能为空", http.StatusBadRequest)
	}
	if err := s.checkVerificationLocks(ctx, userID, email); err != nil {
		return nil, err
	}
	user, err := s.fetchNewAPIUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	actualEmail, _ := user["email"].(string)
	if actualEmail == "" {
		return nil, serviceError("NewAPI 用户信息缺少 email，无法核对", http.StatusBadGateway)
	}
	if normalizeEmail(actualEmail) != email {
		_ = s.recordVerificationMismatch(ctx, userID, email)
		return nil, serviceError("用户邮箱与 NewAPI 记录不匹配", http.StatusBadRequest)
	}
	_ = s.clearVerificationFailures(ctx, userID, email)
	return user, nil
}

func (s *Service) enforcePlanPurchaseLimit(ctx context.Context, q queryer, userID, planID, limit int64) error {
	if limit <= 0 {
		return nil
	}
	var count int64
	err := q.QueryRowContext(ctx, `SELECT COUNT(1) FROM subscription_codes
		WHERE plan_id = ? AND (
			(status = 'used' AND used_by_user_id = ?) OR
			(status = 'pending' AND pending_user_id = ?)
		)`, planID, userID, userID).Scan(&count)
	if err != nil {
		return err
	}
	if count >= limit {
		return serviceError(fmt.Sprintf("该 NewAPI 用户已达到此套餐限兑数量 %d，无法继续兑换", limit), http.StatusConflict)
	}
	return nil
}

func (s *Service) checkVerificationLocks(ctx context.Context, userID int64, email string) error {
	if s.config.PreviewMismatchLimit <= 0 || s.config.PreviewLockDuration <= 0 {
		return nil
	}
	now := time.Now().Unix()
	keys := verificationKeys(userID, email)
	for _, key := range keys {
		var lockedUntil sql.NullInt64
		err := s.db.QueryRowContext(
			ctx,
			`SELECT locked_until FROM verification_locks WHERE key_type = ? AND key_value = ?`,
			key.Type,
			key.Value,
		).Scan(&lockedUntil)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if lockedUntil.Valid && lockedUntil.Int64 > now {
			return serviceError("该用户 ID 或邮箱核对失败次数过多，请稍后再试", http.StatusTooManyRequests)
		}
		if lockedUntil.Valid {
			if err := s.clearVerificationKey(ctx, key); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) recordVerificationMismatch(ctx context.Context, userID int64, email string) error {
	if s.config.PreviewMismatchLimit <= 0 || s.config.PreviewLockDuration <= 0 {
		return nil
	}
	now := time.Now().Unix()
	lockedUntil := now + int64(s.config.PreviewLockDuration/time.Second)
	for _, key := range verificationKeys(userID, email) {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO verification_locks (
				key_type, key_value, failure_count, locked_until, updated_at
			) VALUES (?, ?, 1, CASE WHEN ? <= 1 THEN ? ELSE NULL END, ?)
			ON CONFLICT(key_type, key_value) DO UPDATE SET
				failure_count = verification_locks.failure_count + 1,
				locked_until = CASE
					WHEN verification_locks.failure_count + 1 >= ? THEN ?
					ELSE verification_locks.locked_until
				END,
				updated_at = ?`,
			key.Type,
			key.Value,
			s.config.PreviewMismatchLimit,
			lockedUntil,
			now,
			s.config.PreviewMismatchLimit,
			lockedUntil,
			now,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) clearVerificationFailures(ctx context.Context, userID int64, email string) error {
	for _, key := range verificationKeys(userID, email) {
		if err := s.clearVerificationKey(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) clearVerificationKey(ctx context.Context, key verificationKey) error {
	_, err := s.db.ExecContext(
		ctx,
		`DELETE FROM verification_locks WHERE key_type = ? AND key_value = ?`,
		key.Type,
		key.Value,
	)
	return err
}

func (s *Service) fetchNewAPIUser(ctx context.Context, userID int64) (map[string]any, error) {
	data, err := s.doNewAPIAdminJSON(ctx, http.MethodGet, fmt.Sprintf("/api/user/%d", userID), nil)
	if err != nil {
		return nil, err
	}
	rawUser := unwrapNewAPIData(data)
	user := map[string]any{
		"id":           firstNonNil(rawUser["id"], rawUser["user_id"], userID),
		"username":     firstString(rawUser, "username", "name", "display_name"),
		"email":        firstString(rawUser, "email"),
		"group":        firstString(rawUser, "group", "group_name"),
		"quota":        firstNonNil(rawUser["quota"], rawUser["total_quota"]),
		"used_quota":   firstNonNil(rawUser["used_quota"], rawUser["used_amount"]),
		"subscription": firstNonNil(rawUser["subscription"], rawUser["subscriptions"], rawUser["subscription_info"], rawUser["current_subscription"]),
		"raw":          rawUser,
	}
	return user, nil
}

func (s *Service) fetchSubscriptionPlan(ctx context.Context, planID int64) (map[string]any, error) {
	if planID <= 0 {
		return nil, serviceError("plan_id 必须大于 0", http.StatusBadRequest)
	}
	plans, err := s.ListSubscriptionPlans(ctx)
	if err != nil {
		return nil, err
	}
	for _, plan := range plans {
		if id, ok := intField(plan, "id"); ok && id == planID {
			return plan, nil
		}
	}
	return nil, serviceError(fmt.Sprintf("NewAPI 中不存在套餐 ID %d", planID), http.StatusBadGateway)
}

func (s *Service) ListSubscriptionPlans(ctx context.Context) ([]map[string]any, error) {
	data, err := s.doNewAPIAdminJSON(ctx, http.MethodGet, "/api/subscription/admin/plans", nil)
	if err != nil {
		return nil, err
	}
	if success, ok := data["success"].(bool); ok && !success {
		message := firstString(data, "message")
		if message == "" {
			message = "unknown error"
		}
		return nil, serviceError("NewAPI 查询套餐失败: "+message, http.StatusBadGateway)
	}
	plans := make([]map[string]any, 0)
	for _, item := range subscriptionPlanItems(data) {
		plan := unwrapSubscriptionPlanItem(item)
		id, ok := intField(plan, "id")
		if !ok || id <= 0 {
			continue
		}
		plan["id"] = id
		plan["plan_name"] = subscriptionPlanName(plan, id)
		plans = append(plans, plan)
	}
	return plans, nil
}

func (s *Service) doNewAPIAdminJSON(ctx context.Context, method, path string, payload any) (map[string]any, error) {
	if err := s.config.ValidateRuntime(false); err != nil {
		return nil, err
	}
	base := strings.TrimRight(s.config.NewAPIBaseURL, "/")
	var body io.Reader
	if payload != nil {
		rawBody, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(rawBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	accessToken := s.config.NewAPIAdminAccessToken
	if !strings.HasPrefix(strings.ToLower(accessToken), "bearer ") {
		accessToken = "Bearer " + accessToken
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", accessToken)
	req.Header.Set("New-Api-User", strconv.FormatInt(s.config.NewAPIAdminUserID, 10))

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, serviceError("无法连接 NewAPI: "+err.Error(), http.StatusBadGateway)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, serviceError(fmt.Sprintf("NewAPI 返回 HTTP %d: %s", resp.StatusCode, truncate(string(rawBody), 500)), http.StatusBadGateway)
	}
	var data map[string]any
	if err := json.Unmarshal(rawBody, &data); err != nil {
		return nil, serviceError("NewAPI 返回了无法解析的 JSON", http.StatusBadGateway)
	}
	if ok, exists := data["success"].(bool); exists && !ok {
		message, _ := data["message"].(string)
		if message == "" {
			message = "NewAPI 请求失败"
		}
		return nil, serviceError(message, http.StatusBadGateway)
	}
	return data, nil
}

func (s *Service) fetchCodeByCode(ctx context.Context, q queryer, code string) (*CodeRecord, error) {
	record, err := scanCode(q.QueryRowContext(ctx, "SELECT "+codeColumns+" FROM subscription_codes WHERE code = ?", code))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, serviceError("兑换码不存在", http.StatusNotFound)
		}
		return nil, err
	}
	return record, nil
}

func (s *Service) fetchCodeByID(ctx context.Context, q queryer, id int64) (*CodeRecord, error) {
	return scanCode(q.QueryRowContext(ctx, "SELECT "+codeColumns+" FROM subscription_codes WHERE id = ?", id))
}

func (s *Service) generateUniqueCode(ctx context.Context, tx *sql.Tx, prefix string) (string, error) {
	for i := 0; i < 20; i++ {
		code, err := generateCode(prefix)
		if err != nil {
			return "", err
		}
		var exists int
		err = tx.QueryRowContext(ctx, "SELECT 1 FROM subscription_codes WHERE code = ?", code).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return code, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", serviceError("生成兑换码失败，请重试", http.StatusInternalServerError)
}

func (s *Service) recordAuditEvent(ctx context.Context, input AuditEventInput) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackUnlessDone(tx)
	if err := s.insertAuditEvent(ctx, tx, input); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) insertAuditEvent(ctx context.Context, tx *sql.Tx, input AuditEventInput) error {
	metadata := input.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events (
		event_type, actor_type, actor_id, code, plan_id, status,
		message, metadata_json, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.EventType,
		input.ActorType,
		input.ActorID,
		input.Code,
		input.PlanID,
		input.Status,
		truncate(input.Message, 1000),
		string(metadataJSON),
		time.Now().Unix(),
	)
	return err
}

type Handler struct {
	config             Config
	service            *Service
	webFS              fs.FS
	previewRateLimiter *rateLimiter
}

type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

type verificationKey struct {
	Type  string
	Value string
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:  limit,
		window: window,
		hits:   map[string][]time.Time{},
	}
}

func (r *rateLimiter) Allow(key string, now time.Time) bool {
	if r.limit <= 0 || r.window <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := now.Add(-r.window)
	recent := r.hits[key]
	keep := recent[:0]
	for _, hit := range recent {
		if hit.After(cutoff) {
			keep = append(keep, hit)
		}
	}
	if len(keep) >= r.limit {
		r.hits[key] = keep
		return false
	}
	r.hits[key] = append(keep, now)
	return true
}

func NewHandler(config Config, service *Service, webFS fs.FS) http.Handler {
	return &Handler{
		config:             config,
		service:            service,
		webFS:              webFS,
		previewRateLimiter: newRateLimiter(config.PreviewRateLimit, config.PreviewRateWindow),
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recovered := recover(); recovered != nil {
			h.json(w, http.StatusInternalServerError, response{Success: false, Message: fmt.Sprintf("internal error: %v", recovered)})
		}
	}()
	if r.Method == http.MethodGet {
		h.handleGET(w, r)
		return
	}
	if r.Method == http.MethodPost {
		h.handlePOST(w, r)
		return
	}
	h.json(w, http.StatusNotFound, response{Success: false, Message: "not found"})
}

func (h *Handler) handleGET(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		h.json(w, http.StatusOK, response{Success: true, Message: "ok"})
	case r.URL.Path == h.config.AdminAPIPath("/api/v1/admin/codes"):
		if !h.requireAdmin(w, r) {
			return
		}
		limit := queryInt(r, "limit", 100)
		status := r.URL.Query().Get("status")
		planID := queryOptionalInt(r, "plan_id")
		items, err := h.service.ListCodes(r.Context(), status, planID, limit)
		h.writeResult(w, items, err, http.StatusOK)
	case r.URL.Path == h.config.AdminAPIPath("/api/v1/admin/plans"):
		if !h.requireAdmin(w, r) {
			return
		}
		items, err := h.service.ListSubscriptionPlans(r.Context())
		h.writeResult(w, items, err, http.StatusOK)
	case r.URL.Path == h.config.AdminAPIPath("/api/v1/admin/audit-events"):
		if !h.requireAdmin(w, r) {
			return
		}
		limit := queryInt(r, "limit", 100)
		items, err := h.service.ListAuditEvents(r.Context(), r.URL.Query().Get("event_type"), r.URL.Query().Get("code"), limit)
		h.writeResult(w, items, err, http.StatusOK)
	case h.serveStatic(w, r.URL.Path):
		return
	default:
		h.json(w, http.StatusNotFound, response{Success: false, Message: "not found"})
	}
}

func (h *Handler) handlePOST(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1/redeem/preview":
		if !h.allowPreview(w, r) {
			return
		}
		payload, ok := h.readJSON(w, r)
		if !ok {
			return
		}
		code := stringField(payload, "code")
		userID, _ := intField(payload, "user_id")
		email := stringField(payload, "email")
		result, err := h.service.PreviewRedeemCode(r.Context(), code, userID, email)
		h.writeResultWithMessage(w, result, err, http.StatusOK, "兑换信息可用")
	case "/api/v1/redeem":
		payload, ok := h.readJSON(w, r)
		if !ok {
			return
		}
		code := stringField(payload, "code")
		userID, _ := intField(payload, "user_id")
		email := stringField(payload, "email")
		if email == "" {
			h.writeError(w, serviceError("email 不能为空", http.StatusBadRequest))
			return
		}
		result, err := h.service.RedeemCode(r.Context(), RedeemInput{
			Code:           code,
			UserID:         userID,
			Email:          email,
			AuditActorType: "user",
			AuditActorID:   strconv.FormatInt(userID, 10),
			AuditMetadata:  h.requestMetadata(r),
		})
		message := "订阅已激活"
		if result != nil {
			if m, ok := result["newapi_message"].(string); ok && m != "" {
				message = m
			}
		}
		h.writeResultWithMessage(w, result, err, http.StatusOK, message)
	case h.config.AdminAPIPath("/api/v1/admin/codes"):
		if !h.requireAdmin(w, r) {
			return
		}
		payload, ok := h.readJSON(w, r)
		if !ok {
			return
		}
		planID, _ := intField(payload, "plan_id")
		count, _ := intField(payload, "count")
		expiresAt, err := parseDatetimeToEpoch(payload["expires_at"])
		if err != nil {
			h.writeError(w, err)
			return
		}
		metadata, _ := payload["metadata"].(map[string]any)
		result, err := h.service.CreateCodes(r.Context(), CreateCodesInput{
			PlanID:         planID,
			Count:          int(count),
			Prefix:         stringDefault(payload, "prefix", "SUB"),
			Note:           stringField(payload, "note"),
			ExpiresAt:      expiresAt,
			Metadata:       metadata,
			AuditActorType: "admin",
			AuditActorID:   h.requestActorID(r),
			AuditMetadata:  h.requestMetadata(r),
			AuditEventType: "codes.created",
		})
		h.writeResult(w, result, err, http.StatusCreated)
	case h.config.AdminAPIPath("/api/v1/admin/codes/status"):
		if !h.requireAdmin(w, r) {
			return
		}
		payload, ok := h.readJSON(w, r)
		if !ok {
			return
		}
		result, err := h.service.SetCodeStatus(r.Context(), SetStatusInput{
			Code:           stringField(payload, "code"),
			Status:         stringField(payload, "status"),
			AuditActorType: "admin",
			AuditActorID:   h.requestActorID(r),
			AuditMetadata:  h.requestMetadata(r),
		})
		h.writeResult(w, result, err, http.StatusOK)
	case h.config.AdminAPIPath("/api/v1/admin/codes/batch"):
		if !h.requireAdmin(w, r) {
			return
		}
		payload, ok := h.readJSON(w, r)
		if !ok {
			return
		}
		result, err := h.service.BatchCodes(r.Context(), BatchCodesInput{
			Codes:          stringListField(payload, "codes"),
			Action:         stringField(payload, "action"),
			AuditActorType: "admin",
			AuditActorID:   h.requestActorID(r),
			AuditMetadata:  h.requestMetadata(r),
		})
		h.writeResult(w, result, err, http.StatusOK)
	default:
		h.json(w, http.StatusNotFound, response{Success: false, Message: "not found"})
	}
}

func (h *Handler) allowPreview(w http.ResponseWriter, r *http.Request) bool {
	if h.previewRateLimiter == nil {
		return true
	}
	if h.previewRateLimiter.Allow(h.requestActorID(r), time.Now()) {
		return true
	}
	h.writeError(w, serviceError("核对请求过于频繁，请稍后再试", http.StatusTooManyRequests))
	return false
}

func (h *Handler) serveStatic(w http.ResponseWriter, path string) bool {
	filename := ""
	contentType := ""
	switch {
	case path == "/" || path == "/index.html":
		filename, contentType = "web/index.html", "text/html; charset=utf-8"
	case path == h.config.AdminWebPath() || path == h.config.AdminWebPath()+"/" || path == h.config.AdminWebPath()+".html":
		filename, contentType = "web/admin.html", "text/html; charset=utf-8"
	case path == "/static/styles.css":
		filename, contentType = "web/styles.css", "text/css; charset=utf-8"
	case path == "/static/app.js":
		filename, contentType = "web/app.js", "application/javascript; charset=utf-8"
	default:
		return false
	}
	body, err := fs.ReadFile(h.webFS, filename)
	if err != nil {
		h.json(w, http.StatusNotFound, response{Success: false, Message: "static file not found"})
		return true
	}
	if strings.HasPrefix(contentType, "text/html") {
		body = []byte(strings.ReplaceAll(string(body), "{{UPSTREAM_NAME}}", html.EscapeString(h.config.UpstreamName)))
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return true
}

func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if err := h.config.ValidateRuntime(true); err != nil {
		h.writeError(w, err)
		return false
	}
	provided := strings.TrimSpace(r.Header.Get("X-Admin-Secret"))
	if provided == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			provided = strings.TrimSpace(auth[7:])
		}
	}
	if provided == "" || provided != h.config.AdminSecret {
		h.writeError(w, serviceError("admin auth failed", http.StatusUnauthorized))
		return false
	}
	return true
}

func (h *Handler) readJSON(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	defer r.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
		if errors.Is(err, io.EOF) {
			return map[string]any{}, true
		}
		h.writeError(w, serviceError("请求体不是合法 JSON", http.StatusBadRequest))
		return nil, false
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return payload, true
}

func (h *Handler) requestActorID(r *http.Request) string {
	host := r.RemoteAddr
	if idx := strings.LastIndex(host, ":"); idx > -1 {
		host = host[:idx]
	}
	if host == "" {
		return "unknown"
	}
	return host
}

func (h *Handler) requestMetadata(r *http.Request) map[string]any {
	return map[string]any{
		"source":      "http",
		"remote_addr": h.requestActorID(r),
		"path":        r.URL.Path,
	}
}

func (h *Handler) writeResult(w http.ResponseWriter, data any, err error, status int) {
	h.writeResultWithMessage(w, data, err, status, "")
}

func (h *Handler) writeResultWithMessage(w http.ResponseWriter, data any, err error, status int, message string) {
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.json(w, status, response{Success: true, Message: message, Data: data})
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	status, message := errorStatus(err)
	h.json(w, status, response{Success: false, Message: message})
}

func (h *Handler) json(w http.ResponseWriter, status int, payload response) {
	body, err := json.Marshal(payload)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"success":false,"message":"json encode failed"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

type response struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
}

func Main(args []string, webFS fs.FS) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		printUsage()
		return 0
	}

	ctx := context.Background()
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || args[0] == "serve" {
		if err := loadLocalEnvFile(); err != nil {
			writeStderr(err)
			return 1
		}
		config := ConfigFromEnv()
		serveArgs := args
		if len(args) > 0 && args[0] == "serve" {
			serveArgs = args[1:]
		}
		config, err := parseServeConfig(config, serveArgs)
		if err != nil {
			return 2
		}
		service, err := NewService(config)
		if err != nil {
			writeStderr(err)
			return 1
		}
		defer service.Close()
		return runServe(ctx, service, config, webFS)
	}

	config := ConfigFromEnv()
	service, err := NewService(config)
	if err != nil {
		writeStderr(err)
		return 1
	}
	defer service.Close()

	switch args[0] {
	case "init-db":
		if err := service.InitDB(); err != nil {
			writeStderr(err)
			return 1
		}
		writeStdout(map[string]any{"success": true, "db_path": service.config.DBPath})
	case "create-codes":
		return commandCreateCodes(ctx, service, args[1:])
	case "list-codes":
		return commandListCodes(ctx, service, args[1:])
	case "list-audit":
		return commandListAudit(ctx, service, args[1:])
	case "redeem":
		return commandRedeem(ctx, service, args[1:])
	case "set-status":
		return commandSetStatus(ctx, service, args[1:])
	default:
		writeStderr(fmt.Errorf("unknown command: %s", args[0]))
		return 2
	}
	return 0
}

func parseServeConfig(config Config, args []string) (Config, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	host := fs.String("host", config.Host, "")
	port := fs.Int("port", config.Port, "")
	dbPath := fs.String("db-path", config.DBPath, "")
	adminSecret := fs.String("admin-secret", config.AdminSecret, "")
	adminPrefix := fs.String("admin-prefix", config.AdminPrefix, "")
	upstreamName := fs.String("upstream-name", config.UpstreamName, "")
	bindMode := fs.String("bind-mode", config.BindMode, "")
	timeoutSeconds := fs.Float64("timeout-seconds", config.TimeoutSeconds, "")
	previewRateLimit := fs.Int("preview-rate-limit", config.PreviewRateLimit, "")
	previewRateWindowSeconds := fs.Int64("preview-rate-window-seconds", int64(config.PreviewRateWindow/time.Second), "")
	previewMismatchLimit := fs.Int("preview-mismatch-limit", config.PreviewMismatchLimit, "")
	previewLockSeconds := fs.Int64("preview-lock-seconds", int64(config.PreviewLockDuration/time.Second), "")
	newAPIBaseURL := fs.String("newapi-base-url", config.NewAPIBaseURL, "")
	newAPIAdminAccessToken := fs.String("newapi-admin-access-token", config.NewAPIAdminAccessToken, "")
	newAPIAdminUserID := fs.Int64("newapi-admin-user-id", config.NewAPIAdminUserID, "")
	if err := fs.Parse(args); err != nil {
		return config, err
	}
	config.Host = strings.TrimSpace(*host)
	config.Port = *port
	config.DBPath = strings.TrimSpace(*dbPath)
	config.AdminSecret = strings.TrimSpace(*adminSecret)
	config.AdminPrefix = normalizeAdminPrefix(*adminPrefix)
	config.UpstreamName = strings.TrimSpace(*upstreamName)
	config.BindMode = strings.TrimSpace(*bindMode)
	config.TimeoutSeconds = *timeoutSeconds
	config.PreviewRateLimit = *previewRateLimit
	config.PreviewRateWindow = time.Duration(*previewRateWindowSeconds) * time.Second
	config.PreviewMismatchLimit = *previewMismatchLimit
	config.PreviewLockDuration = time.Duration(*previewLockSeconds) * time.Second
	config.NewAPIBaseURL = strings.TrimSpace(*newAPIBaseURL)
	config.NewAPIAdminAccessToken = strings.TrimSpace(*newAPIAdminAccessToken)
	config.NewAPIAdminUserID = *newAPIAdminUserID
	return config, nil
}

func runServe(ctx context.Context, service *Service, config Config, webFS fs.FS) int {
	if err := service.InitDB(); err != nil {
		writeStderr(err)
		return 1
	}
	addr := fmt.Sprintf("%s:%d", config.Host, config.Port)
	fmt.Println(mustJSON(map[string]any{"success": true, "listening": "http://" + addr, "db_path": service.config.DBPath}))
	if err := http.ListenAndServe(addr, NewHandler(config, service, webFS)); err != nil {
		writeStderr(err)
		return 1
	}
	<-ctx.Done()
	return 0
}

func commandCreateCodes(ctx context.Context, service *Service, args []string) int {
	fs := flag.NewFlagSet("create-codes", flag.ContinueOnError)
	planID := fs.Int64("plan-id", 0, "")
	count := fs.Int("count", 1, "")
	prefix := fs.String("prefix", "SUB", "")
	note := fs.String("note", "", "")
	expiresAtText := fs.String("expires-at", "", "")
	metadataText := fs.String("metadata", "{}", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := service.InitDB(); err != nil {
		writeStderr(err)
		return 1
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(*metadataText), &metadata); err != nil || metadata == nil {
		writeStderr(serviceError("--metadata 必须是 JSON object", http.StatusBadRequest))
		return 1
	}
	expiresAt, err := parseDatetimeToEpoch(*expiresAtText)
	if err != nil {
		writeStderr(err)
		return 1
	}
	result, err := service.CreateCodes(ctx, CreateCodesInput{
		PlanID:         *planID,
		Count:          *count,
		Prefix:         *prefix,
		Note:           *note,
		ExpiresAt:      expiresAt,
		Metadata:       metadata,
		AuditActorType: "cli",
		AuditMetadata:  map[string]any{"source": "cli"},
	})
	return writeCommandResult(result, err)
}

func commandListCodes(ctx context.Context, service *Service, args []string) int {
	fs := flag.NewFlagSet("list-codes", flag.ContinueOnError)
	status := fs.String("status", "", "")
	planIDFlag := fs.Int64("plan-id", 0, "")
	limit := fs.Int("limit", 100, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := service.InitDB(); err != nil {
		writeStderr(err)
		return 1
	}
	var planID *int64
	if *planIDFlag > 0 {
		planID = planIDFlag
	}
	result, err := service.ListCodes(ctx, *status, planID, *limit)
	addISOFields(result, "created_at", "expires_at", "used_at", "pending_at")
	return writeCommandResult(result, err)
}

func commandListAudit(ctx context.Context, service *Service, args []string) int {
	fs := flag.NewFlagSet("list-audit", flag.ContinueOnError)
	eventType := fs.String("event-type", "", "")
	code := fs.String("code", "", "")
	limit := fs.Int("limit", 100, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := service.InitDB(); err != nil {
		writeStderr(err)
		return 1
	}
	result, err := service.ListAuditEvents(ctx, *eventType, *code, *limit)
	addISOFields(result, "created_at")
	return writeCommandResult(result, err)
}

func commandRedeem(ctx context.Context, service *Service, args []string) int {
	fs := flag.NewFlagSet("redeem", flag.ContinueOnError)
	code := fs.String("code", "", "")
	userID := fs.Int64("user-id", 0, "")
	email := fs.String("email", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := service.InitDB(); err != nil {
		writeStderr(err)
		return 1
	}
	result, err := service.RedeemCode(ctx, RedeemInput{
		Code:           *code,
		UserID:         *userID,
		Email:          *email,
		AuditActorType: "cli",
		AuditActorID:   strconv.FormatInt(*userID, 10),
		AuditMetadata:  map[string]any{"source": "cli"},
	})
	if result != nil {
		addISOFields([]map[string]any{result}, "created_at", "expires_at", "used_at", "pending_at")
	}
	return writeCommandResult(result, err)
}

func commandSetStatus(ctx context.Context, service *Service, args []string) int {
	fs := flag.NewFlagSet("set-status", flag.ContinueOnError)
	code := fs.String("code", "", "")
	status := fs.String("status", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := service.InitDB(); err != nil {
		writeStderr(err)
		return 1
	}
	result, err := service.SetCodeStatus(ctx, SetStatusInput{
		Code:           *code,
		Status:         *status,
		AuditActorType: "cli",
		AuditMetadata:  map[string]any{"source": "cli"},
	})
	return writeCommandResult(result, err)
}

func loadLocalEnvFile() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	candidates := []string{filepath.Join(filepath.Dir(exePath), ".env.local")}
	if cwd, err := os.Getwd(); err == nil && cwd != filepath.Dir(exePath) {
		candidates = append(candidates, filepath.Join(cwd, ".env.local"))
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := loadEnvFile(path); err != nil {
			return err
		}
	}
	return nil
}

func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d 环境变量格式应为 KEY=VALUE", path, lineNumber)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("%s:%d 环境变量名不能为空", path, lineNumber)
		}
		value = strings.TrimSpace(stripInlineComment(strings.TrimSpace(value)))
		unquoted, err := unquoteEnvValue(value)
		if err != nil {
			return fmt.Errorf("%s:%d %w", path, lineNumber, err)
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, unquoted); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func stripInlineComment(value string) string {
	inSingleQuote := false
	inDoubleQuote := false
	escaped := false
	for i, r := range value {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && inDoubleQuote {
			escaped = true
			continue
		}
		switch r {
		case '\'':
			if !inDoubleQuote {
				inSingleQuote = !inSingleQuote
			}
		case '"':
			if !inSingleQuote {
				inDoubleQuote = !inDoubleQuote
			}
		case '#':
			if !inSingleQuote && !inDoubleQuote && (i == 0 || unicode.IsSpace(rune(value[i-1]))) {
				return strings.TrimSpace(value[:i])
			}
		}
	}
	return value
}

func unquoteEnvValue(value string) (string, error) {
	if len(value) < 2 {
		return value, nil
	}
	quote := value[0]
	if quote != '\'' && quote != '"' {
		return value, nil
	}
	if value[len(value)-1] != quote {
		return "", fmt.Errorf("环境变量值引号未闭合")
	}
	value = value[1 : len(value)-1]
	if quote == '\'' {
		return value, nil
	}
	replacer := strings.NewReplacer(
		`\\`, `\`,
		`\n`, "\n",
		`\r`, "\r",
		`\t`, "\t",
		`\"`, `"`,
	)
	return replacer.Replace(value), nil
}

func printUsage() {
	fmt.Println(`usage: redeemer {init-db,serve,create-codes,list-codes,list-audit,redeem,set-status} ...

Redeem codes into NewAPI subscriptions`)
}

func writeCommandResult(data any, err error) int {
	if err != nil {
		writeStderr(err)
		return 1
	}
	writeStdout(map[string]any{"success": true, "data": data})
	return 0
}

func writeStdout(data any) {
	fmt.Println(mustJSON(data))
}

func writeStderr(err error) {
	status, message := errorStatus(err)
	_ = status
	fmt.Fprintln(os.Stderr, mustJSON(map[string]any{"success": false, "message": message}))
}

func mustJSON(data any) string {
	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return `{"success":false,"message":"json encode failed"}`
	}
	return string(body)
}

type CreateCodesInput struct {
	PlanID         int64
	Count          int
	Prefix         string
	Note           string
	ExpiresAt      *int64
	Metadata       map[string]any
	AuditActorType string
	AuditActorID   string
	AuditMetadata  map[string]any
	AuditEventType string
}

func (i CreateCodesInput) auditActorType(defaultValue string) string {
	if i.AuditActorType != "" {
		return i.AuditActorType
	}
	return defaultValue
}

func (i CreateCodesInput) auditEvent(defaultValue string) string {
	if i.AuditEventType != "" {
		return i.AuditEventType
	}
	return defaultValue
}

type SetStatusInput struct {
	Code           string
	Status         string
	AuditActorType string
	AuditActorID   string
	AuditMetadata  map[string]any
	AuditEventType string
}

func (i SetStatusInput) auditActorType(defaultValue string) string {
	if i.AuditActorType != "" {
		return i.AuditActorType
	}
	return defaultValue
}

func (i SetStatusInput) auditEvent(defaultValue string) string {
	if i.AuditEventType != "" {
		return i.AuditEventType
	}
	return defaultValue
}

type BatchCodesInput struct {
	Codes          []string
	Action         string
	AuditActorType string
	AuditActorID   string
	AuditMetadata  map[string]any
}

func (i BatchCodesInput) auditActorType(defaultValue string) string {
	if i.AuditActorType != "" {
		return i.AuditActorType
	}
	return defaultValue
}

type RedeemInput struct {
	Code           string
	UserID         int64
	Email          string
	AuditActorType string
	AuditActorID   string
	AuditMetadata  map[string]any
}

func (i RedeemInput) auditActorType(defaultValue string) string {
	if i.AuditActorType != "" {
		return i.AuditActorType
	}
	return defaultValue
}

type AuditEventInput struct {
	EventType string
	ActorType string
	ActorID   string
	Code      *string
	PlanID    *int64
	Status    string
	Message   string
	Metadata  map[string]any
}

type CodeRecord struct {
	ID                 int64
	Code               string
	PlanID             int64
	Status             string
	Note               string
	MetadataJSON       string
	CreatedAt          int64
	ExpiresAt          sql.NullInt64
	UsedAt             sql.NullInt64
	UsedByUserID       sql.NullInt64
	PendingAt          sql.NullInt64
	PendingUserID      sql.NullInt64
	PendingToken       sql.NullString
	NewAPIMessage      sql.NullString
	NewAPIResponseJSON sql.NullString
	LastError          sql.NullString
}

func scanCode(scanner interface{ Scan(dest ...any) error }) (*CodeRecord, error) {
	var record CodeRecord
	err := scanner.Scan(
		&record.ID,
		&record.Code,
		&record.PlanID,
		&record.Status,
		&record.Note,
		&record.MetadataJSON,
		&record.CreatedAt,
		&record.ExpiresAt,
		&record.UsedAt,
		&record.UsedByUserID,
		&record.PendingAt,
		&record.PendingUserID,
		&record.PendingToken,
		&record.NewAPIMessage,
		&record.NewAPIResponseJSON,
		&record.LastError,
	)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func (r *CodeRecord) toMap() map[string]any {
	metadata := map[string]any{}
	_ = json.Unmarshal([]byte(r.MetadataJSON), &metadata)
	item := map[string]any{
		"id":                   r.ID,
		"code":                 r.Code,
		"plan_id":              r.PlanID,
		"status":               r.Status,
		"note":                 r.Note,
		"metadata":             metadata,
		"created_at":           r.CreatedAt,
		"expires_at":           nullIntToAny(r.ExpiresAt),
		"used_at":              nullIntToAny(r.UsedAt),
		"used_by_user_id":      nullIntToAny(r.UsedByUserID),
		"pending_at":           nullIntToAny(r.PendingAt),
		"pending_user_id":      nullIntToAny(r.PendingUserID),
		"newapi_message":       nullStringToAny(r.NewAPIMessage),
		"newapi_response_json": nullStringToAny(r.NewAPIResponseJSON),
		"last_error":           nullStringToAny(r.LastError),
	}
	return item
}

type AuditEvent struct {
	ID           int64
	EventType    string
	ActorType    string
	ActorID      string
	Code         sql.NullString
	PlanID       sql.NullInt64
	Status       string
	Message      string
	MetadataJSON string
	CreatedAt    int64
}

func scanAuditEvent(scanner interface{ Scan(dest ...any) error }) (*AuditEvent, error) {
	var event AuditEvent
	err := scanner.Scan(
		&event.ID,
		&event.EventType,
		&event.ActorType,
		&event.ActorID,
		&event.Code,
		&event.PlanID,
		&event.Status,
		&event.Message,
		&event.MetadataJSON,
		&event.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &event, nil
}

func (e *AuditEvent) toMap() map[string]any {
	metadata := map[string]any{}
	_ = json.Unmarshal([]byte(e.MetadataJSON), &metadata)
	return map[string]any{
		"id":         e.ID,
		"event_type": e.EventType,
		"actor_type": e.ActorType,
		"actor_id":   e.ActorID,
		"code":       nullStringToAny(e.Code),
		"plan_id":    nullIntToAny(e.PlanID),
		"status":     e.Status,
		"message":    e.Message,
		"metadata":   metadata,
		"created_at": e.CreatedAt,
	}
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func validateRedeemable(record *CodeRecord) error {
	switch record.Status {
	case "used":
		return serviceError("兑换码已被使用", http.StatusConflict)
	case "disabled":
		return serviceError("兑换码已停用", http.StatusConflict)
	case "pending":
		return serviceError("兑换码正在处理中，请稍后联系管理员", http.StatusConflict)
	}
	if record.ExpiresAt.Valid && record.ExpiresAt.Int64 <= time.Now().Unix() {
		return serviceError("兑换码已过期", http.StatusGone)
	}
	return nil
}

func normalizeCodeList(codes []string) ([]string, error) {
	if len(codes) == 0 {
		return nil, serviceError("codes 不能为空", http.StatusBadRequest)
	}
	if len(codes) > 5000 {
		return nil, serviceError("codes 最多一次处理 5000 个", http.StatusBadRequest)
	}
	seen := make(map[string]bool, len(codes))
	normalized := make([]string, 0, len(codes))
	for _, code := range codes {
		code = strings.TrimSpace(code)
		if code == "" {
			continue
		}
		if seen[code] {
			continue
		}
		seen[code] = true
		normalized = append(normalized, code)
	}
	if len(normalized) == 0 {
		return nil, serviceError("codes 不能为空", http.StatusBadRequest)
	}
	return normalized, nil
}

func generateCode(prefix string) (string, error) {
	prefix = normalizePrefix(prefix)
	chunks := make([]string, 3)
	for i := range chunks {
		var builder strings.Builder
		for j := 0; j < 4; j++ {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			if err != nil {
				return "", err
			}
			builder.WriteByte(alphabet[n.Int64()])
		}
		chunks[i] = builder.String()
	}
	return prefix + "-" + strings.Join(chunks, "-"), nil
}

func normalizePrefix(prefix string) string {
	var builder strings.Builder
	for _, r := range strings.ToUpper(prefix) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		}
		if builder.Len() >= 12 {
			break
		}
	}
	if builder.Len() == 0 {
		return "SUB"
	}
	return builder.String()
}

func normalizeAdminPrefix(prefix string) string {
	parts := make([]string, 0)
	for _, part := range strings.Split(strings.Trim(strings.TrimSpace(prefix), "/"), "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "xx"
	}
	return strings.Join(parts, "/")
}

func parseDatetimeToEpoch(value any) (*int64, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return nil, nil
		}
		if i, err := strconv.ParseInt(text, 10, 64); err == nil {
			return &i, nil
		}
		if strings.HasSuffix(text, "Z") {
			text = strings.TrimSuffix(text, "Z") + "+00:00"
		}
		t, err := time.Parse(time.RFC3339, text)
		if err != nil {
			return nil, serviceError("expires_at 不是合法的 ISO 时间", http.StatusBadRequest)
		}
		i := t.Unix()
		return &i, nil
	case float64:
		i := int64(v)
		if i == 0 {
			return nil, nil
		}
		return &i, nil
	case int64:
		if v == 0 {
			return nil, nil
		}
		return &v, nil
	default:
		return nil, serviceError("expires_at 必须是 ISO 时间字符串或 Unix 时间戳", http.StatusBadRequest)
	}
}

func formatTimestamp(value any) any {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case int64:
		return time.Unix(v, 0).UTC().Format(time.RFC3339)
	case int:
		return time.Unix(int64(v), 0).UTC().Format(time.RFC3339)
	case float64:
		return time.Unix(int64(v), 0).UTC().Format(time.RFC3339)
	default:
		return nil
	}
}

func addISOFields(items []map[string]any, keys ...string) {
	for _, item := range items {
		for _, key := range keys {
			item[key+"_iso"] = formatTimestamp(item[key])
		}
	}
}

func nullIntToAny(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}

func nullStringToAny(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func nullableInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func codesFromMaps(items []map[string]any) []string {
	codes := make([]string, 0, len(items))
	for _, item := range items {
		if code, ok := item["code"].(string); ok {
			codes = append(codes, code)
		}
	}
	return codes
}

func mergeMetadata(base map[string]any, extra map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for key, value := range extra {
		base[key] = value
	}
	return base
}

func redeemAuditMetadata(userID int64, user map[string]any, extra map[string]any) map[string]any {
	metadata := map[string]any{"user_id": userID, "redeem_user_id": userID}
	if user != nil {
		if username := firstString(user, "username", "name", "display_name"); username != "" {
			metadata["redeem_username"] = username
		}
		if email := firstString(user, "email"); email != "" {
			metadata["redeem_email"] = email
		}
	}
	return mergeMetadata(metadata, extra)
}

func randomHex(bytesCount int) (string, error) {
	data := make([]byte, bytesCount)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", data), nil
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func rollbackUnlessDone(tx *sql.Tx) {
	_ = tx.Rollback()
}

func envString(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func envInt64(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envFloat(name string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func queryInt(r *http.Request, name string, fallback int) int {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func queryOptionalInt(r *http.Request, name string) *int64 {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil
	}
	return &parsed
}

func stringField(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func stringListField(payload map[string]any, key string) []string {
	values, ok := payload[key].([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func stringDefault(payload map[string]any, key, fallback string) string {
	value := stringField(payload, key)
	if value == "" {
		return fallback
	}
	return value
}

func normalizeEmail(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func verificationKeys(userID int64, email string) []verificationKey {
	return []verificationKey{
		{Type: "user_id", Value: strconv.FormatInt(userID, 10)},
		{Type: "email_sha256", Value: emailHash(email)},
	}
}

func emailHash(email string) string {
	sum := sha256.Sum256([]byte(normalizeEmail(email)))
	return fmt.Sprintf("%x", sum[:])
}

func unwrapNewAPIData(data map[string]any) map[string]any {
	for _, key := range []string{"data", "user"} {
		if nested, ok := data[key].(map[string]any); ok {
			return nested
		}
	}
	return data
}

func firstString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func subscriptionPlanItems(data map[string]any) []any {
	for _, candidate := range []any{data["data"], data["plans"], data["items"]} {
		if items, ok := candidate.([]any); ok {
			return items
		}
	}
	if nested, ok := data["data"].(map[string]any); ok {
		for _, candidate := range []any{nested["plans"], nested["items"], nested["list"]} {
			if items, ok := candidate.([]any); ok {
				return items
			}
		}
	}
	return nil
}

func unwrapSubscriptionPlanItem(item any) map[string]any {
	raw, ok := item.(map[string]any)
	if !ok {
		return nil
	}
	if plan, ok := raw["plan"].(map[string]any); ok {
		return plan
	}
	return raw
}

func subscriptionPlanName(plan map[string]any, planID int64) string {
	if name := firstString(plan, "title", "name", "display_name"); name != "" {
		return name
	}
	return fmt.Sprintf("套餐 %d", planID)
}

func subscriptionPlanPurchaseLimit(plan map[string]any) int64 {
	for _, key := range []string{"max_purchase_per_user", "maxPurchasePerUser"} {
		if value, ok := intField(plan, key); ok && value > 0 {
			return value
		}
	}
	return 0
}

func intField(payload map[string]any, key string) (int64, bool) {
	switch value := payload[key].(type) {
	case float64:
		return int64(value), true
	case int64:
		return value, true
	case int:
		return int64(value), true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
