package redeemer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func testService(t *testing.T, baseURL string) *Service {
	t.Helper()
	config := Config{
		DBPath:                 filepath.Join(t.TempDir(), "redeemer.db"),
		NewAPIBaseURL:          baseURL,
		NewAPIAdminAccessToken: "admin-token",
		NewAPIAdminUserID:      99,
		BindMode:               "bind",
		TimeoutSeconds:         20,
		AdminSecret:            "test-secret",
		AdminPrefix:            "xx",
		Host:                   "127.0.0.1",
		Port:                   8789,
	}
	service, err := NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = service.Close()
	})
	if err := service.InitDB(); err != nil {
		t.Fatal(err)
	}
	return service
}

func TestCreateCodesWritesAuditEvent(t *testing.T) {
	ctx := context.Background()
	service := testService(t, "http://127.0.0.1:1")
	created, err := service.CreateCodes(ctx, CreateCodesInput{
		PlanID:         4,
		Count:          2,
		Prefix:         "LOG",
		Note:           "audit test",
		AuditActorType: "test",
		AuditActorID:   "case",
		AuditMetadata:  map[string]any{"source": "unit"},
	})
	if err != nil {
		t.Fatal(err)
	}

	events, err := service.ListAuditEvents(ctx, "codes.created", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected one audit event, got %d", len(events))
	}
	if events[0]["actor_type"] != "test" || events[0]["actor_id"] != "case" {
		t.Fatalf("unexpected actor: %#v", events[0])
	}
	metadata := events[0]["metadata"].(map[string]any)
	if int(metadata["count"].(float64)) != 2 {
		t.Fatalf("unexpected count metadata: %#v", metadata)
	}
	codes := metadata["codes"].([]any)
	if codes[0] != created[0]["code"] {
		t.Fatalf("expected code metadata to include created code")
	}
}

func TestRedeemCodeSuccess(t *testing.T) {
	ctx := context.Background()
	var capturedPath string
	var capturedHeaders http.Header
	var capturedBody map[string]any
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedHeaders = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"message": "用户分组将升级到 pro",
			},
		})
	}))
	defer stub.Close()

	service := testService(t, stub.URL)
	created, err := service.CreateCodes(ctx, CreateCodesInput{PlanID: 3, Count: 1, Prefix: "PRO"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.RedeemCode(ctx, RedeemInput{Code: created[0]["code"].(string), UserID: 123})
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "used" {
		t.Fatalf("expected used status, got %#v", result["status"])
	}
	if capturedPath != "/api/subscription/admin/bind" {
		t.Fatalf("unexpected path: %s", capturedPath)
	}
	if capturedHeaders.Get("New-Api-User") != "99" {
		t.Fatalf("missing New-Api-User header")
	}
	if capturedHeaders.Get("Authorization") != "Bearer admin-token" {
		t.Fatalf("missing Authorization header")
	}
	if int64(capturedBody["user_id"].(float64)) != 123 || int64(capturedBody["plan_id"].(float64)) != 3 {
		t.Fatalf("unexpected body: %#v", capturedBody)
	}
}

func TestAdminAPIRequiresPrefixAndReturnsAudit(t *testing.T) {
	service := testService(t, "http://127.0.0.1:1")
	server := httptest.NewServer(NewHandler(service.config, service, fstest.MapFS{}))
	defer server.Close()

	body := `{"plan_id":8,"count":1,"prefix":"ADM"}`
	oldReq, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/admin/codes", stringsReader(body))
	if err != nil {
		t.Fatal(err)
	}
	oldReq.Header.Set("Content-Type", "application/json")
	oldReq.Header.Set("X-Admin-Secret", "test-secret")
	oldResp, err := http.DefaultClient.Do(oldReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = oldResp.Body.Close()
	if oldResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected old admin API 404, got %d", oldResp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodPost, server.URL+"/xx/api/v1/admin/codes", stringsReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Secret", "test-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected create status 201, got %d", resp.StatusCode)
	}

	auditReq, err := http.NewRequest(http.MethodGet, server.URL+"/xx/api/v1/admin/audit-events?event_type=codes.created&limit=5", nil)
	if err != nil {
		t.Fatal(err)
	}
	auditReq.Header.Set("X-Admin-Secret", "test-secret")
	auditResp, err := http.DefaultClient.Do(auditReq)
	if err != nil {
		t.Fatal(err)
	}
	defer auditResp.Body.Close()
	if auditResp.StatusCode != http.StatusOK {
		t.Fatalf("expected audit status 200, got %d", auditResp.StatusCode)
	}
	var payload struct {
		Success bool             `json:"success"`
		Data    []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(auditResp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Success || len(payload.Data) != 1 || payload.Data[0]["event_type"] != "codes.created" {
		t.Fatalf("unexpected audit payload: %#v", payload)
	}
}

func stringsReader(value string) *strings.Reader {
	return strings.NewReader(value)
}
