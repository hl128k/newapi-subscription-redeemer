package redeemer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
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
		UpstreamName:           "NewAPI",
		Host:                   "127.0.0.1",
		Port:                   8789,
		PreviewRateLimit:       10,
		PreviewRateWindow:      time.Minute,
		PreviewMismatchLimit:   5,
		PreviewLockDuration:    15 * time.Minute,
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

func TestLoadEnvFileKeepsExistingEnvironment(t *testing.T) {
	t.Setenv("REDEEMER_ADMIN_PREFIX", "from-env")
	path := filepath.Join(t.TempDir(), ".env.local")
	content := strings.Join([]string{
		"# local config",
		"REDEEMER_ADMIN_PREFIX=from-file",
		"NEWAPI_BASE_URL=\"https://newapi.example.com\" # comment",
		"NEWAPI_ADMIN_ACCESS_TOKEN='token#kept'",
		"export NEWAPI_ADMIN_USER_ID=42",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("REDEEMER_ADMIN_PREFIX"); got != "from-env" {
		t.Fatalf("expected existing env to win, got %q", got)
	}
	if got := os.Getenv("NEWAPI_BASE_URL"); got != "https://newapi.example.com" {
		t.Fatalf("unexpected base url: %q", got)
	}
	if got := os.Getenv("NEWAPI_ADMIN_ACCESS_TOKEN"); got != "token#kept" {
		t.Fatalf("unexpected token: %q", got)
	}
	if got := os.Getenv("NEWAPI_ADMIN_USER_ID"); got != "42" {
		t.Fatalf("unexpected user id: %q", got)
	}
}

func TestParseServeConfigOverridesEnvironmentConfig(t *testing.T) {
	config := Config{
		DBPath:                 "from-env.db",
		NewAPIBaseURL:          "https://env.example.com",
		NewAPIAdminAccessToken: "env-token",
		NewAPIAdminUserID:      1,
		BindMode:               "bind",
		TimeoutSeconds:         20,
		AdminSecret:            "env-secret",
		AdminPrefix:            "xx",
		UpstreamName:           "NewAPI",
		Host:                   "127.0.0.1",
		Port:                   8789,
		PreviewRateLimit:       10,
		PreviewRateWindow:      time.Minute,
		PreviewMismatchLimit:   5,
		PreviewLockDuration:    15 * time.Minute,
	}

	got, err := parseServeConfig(config, []string{
		"--db-path", "from-args.db",
		"--host", "0.0.0.0",
		"--port", "8799",
		"--admin-secret", "arg-secret",
		"--admin-prefix", "/ops/",
		"--upstream-name", "My API",
		"--bind-mode", "create",
		"--timeout-seconds", "3.5",
		"--preview-rate-limit", "3",
		"--preview-rate-window-seconds", "15",
		"--preview-mismatch-limit", "2",
		"--preview-lock-seconds", "30",
		"--newapi-base-url", "https://args.example.com",
		"--newapi-admin-access-token", "arg-token",
		"--newapi-admin-user-id", "42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.DBPath != "from-args.db" || got.Host != "0.0.0.0" || got.Port != 8799 {
		t.Fatalf("startup args did not override runtime config: %#v", got)
	}
	if got.AdminSecret != "arg-secret" || got.AdminPrefix != "ops" {
		t.Fatalf("unexpected admin config: %#v", got)
	}
	if got.UpstreamName != "My API" {
		t.Fatalf("unexpected upstream name: %#v", got)
	}
	if got.BindMode != "create" || got.TimeoutSeconds != 3.5 {
		t.Fatalf("unexpected service config: %#v", got)
	}
	if got.PreviewRateLimit != 3 || got.PreviewRateWindow != 15*time.Second {
		t.Fatalf("unexpected rate limit config: %#v", got)
	}
	if got.PreviewMismatchLimit != 2 || got.PreviewLockDuration != 30*time.Second {
		t.Fatalf("unexpected mismatch lock config: %#v", got)
	}
	if got.NewAPIBaseURL != "https://args.example.com" ||
		got.NewAPIAdminAccessToken != "arg-token" ||
		got.NewAPIAdminUserID != 42 {
		t.Fatalf("unexpected newapi config: %#v", got)
	}
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

func TestPreviewRedeemCodeFetchesUserAndRequiresEmail(t *testing.T) {
	ctx := context.Background()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("New-Api-User") != "99" {
			t.Fatalf("missing New-Api-User header")
		}
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/user/123":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"id":       123,
					"username": "alice",
					"email":    "alice@example.com",
					"group":    "pro",
					"subscription": map[string]any{
						"plan_id": 3,
						"status":  "active",
					},
				},
			})
		case "/api/subscription/admin/plans":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": []map[string]any{
					{
						"plan": map[string]any{
							"id":    3,
							"title": "Pro Monthly",
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected NewAPI lookup: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer stub.Close()

	service := testService(t, stub.URL)
	created, err := service.CreateCodes(ctx, CreateCodesInput{PlanID: 3, Count: 1, Prefix: "CHK"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.PreviewRedeemCode(ctx, created[0]["code"].(string), 123, "Alice@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	user := result["user"].(map[string]any)
	if user["username"] != "alice" || user["email"] != "alice@example.com" {
		t.Fatalf("unexpected user info: %#v", user)
	}
	if result["plan_name"] != "Pro Monthly" {
		t.Fatalf("unexpected plan name: %#v", result)
	}
	if _, err := service.PreviewRedeemCode(ctx, created[0]["code"].(string), 123, "other@example.com"); err == nil {
		t.Fatalf("expected email mismatch error")
	}
}

func TestPreviewEmailMismatchLocksUserAndEmail(t *testing.T) {
	ctx := context.Background()
	lookupCount := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookupCount++
		email := "alice@example.com"
		if r.URL.Path == "/api/user/456" {
			email = "bob@example.com"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"id":    strings.TrimPrefix(r.URL.Path, "/api/user/"),
				"email": email,
			},
		})
	}))
	defer stub.Close()

	service := testService(t, stub.URL)
	service.config.PreviewMismatchLimit = 2
	service.config.PreviewLockDuration = time.Minute
	created, err := service.CreateCodes(ctx, CreateCodesInput{PlanID: 3, Count: 1, Prefix: "LCK"})
	if err != nil {
		t.Fatal(err)
	}
	code := created[0]["code"].(string)
	for i := 0; i < 2; i++ {
		if _, err := service.PreviewRedeemCode(ctx, code, 123, "wrong@example.com"); err == nil {
			t.Fatalf("expected mismatch error")
		}
	}
	if _, err := service.PreviewRedeemCode(ctx, code, 123, "alice@example.com"); err == nil {
		t.Fatalf("expected user id lock")
	}
	if _, err := service.PreviewRedeemCode(ctx, code, 456, "wrong@example.com"); err == nil {
		t.Fatalf("expected email lock")
	}
	if lookupCount != 2 {
		t.Fatalf("expected locked requests to skip NewAPI lookup, got %d lookups", lookupCount)
	}
}

func TestPreviewEndpointIsRateLimited(t *testing.T) {
	service := testService(t, "http://127.0.0.1:1")
	service.config.PreviewRateLimit = 1
	service.config.PreviewRateWindow = time.Minute
	server := httptest.NewServer(NewHandler(service.config, service, fstest.MapFS{}))
	defer server.Close()

	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/redeem/preview", stringsReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if i == 1 && resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("expected second preview request to be rate limited, got %d", resp.StatusCode)
		}
	}
}

func TestStaticHTMLUsesConfiguredUpstreamName(t *testing.T) {
	service := testService(t, "http://127.0.0.1:1")
	config := service.config
	config.UpstreamName = "My API"
	server := httptest.NewServer(NewHandler(config, service, fstest.MapFS{
		"web/index.html": {Data: []byte("<title>{{UPSTREAM_NAME}}</title>")},
	}))
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "My API") {
		t.Fatalf("expected configured upstream name in HTML, got %q", string(body))
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

func TestRedeemCodeAuditIncludesVerifiedUser(t *testing.T) {
	ctx := context.Background()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/user/123":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"id":       123,
					"username": "alice",
					"email":    "alice@example.com",
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/subscription/admin/bind":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"message": "ok",
			})
		default:
			t.Fatalf("unexpected NewAPI request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer stub.Close()

	service := testService(t, stub.URL)
	created, err := service.CreateCodes(ctx, CreateCodesInput{PlanID: 3, Count: 1, Prefix: "USR"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RedeemCode(ctx, RedeemInput{
		Code:   created[0]["code"].(string),
		UserID: 123,
		Email:  "Alice@Example.com",
	}); err != nil {
		t.Fatal(err)
	}

	events, err := service.ListAuditEvents(ctx, "code.redeemed", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected one redeemed event, got %d", len(events))
	}
	metadata := events[0]["metadata"].(map[string]any)
	if metadata["redeem_username"] != "alice" || metadata["redeem_email"] != "alice@example.com" {
		t.Fatalf("expected verified user metadata, got %#v", metadata)
	}
	if int64(metadata["redeem_user_id"].(float64)) != 123 {
		t.Fatalf("unexpected redeem user id: %#v", metadata)
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

func TestAdminPlansEndpointReturnsSubscriptionNames(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/subscription/admin/plans" {
			t.Fatalf("unexpected plans lookup: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": []map[string]any{
				{
					"plan": map[string]any{
						"id":      8,
						"title":   "Team Annual",
						"enabled": false,
					},
				},
			},
		})
	}))
	defer stub.Close()

	service := testService(t, stub.URL)
	server := httptest.NewServer(NewHandler(service.config, service, fstest.MapFS{}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/xx/api/v1/admin/plans", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Admin-Secret", "test-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected plans status 200, got %d", resp.StatusCode)
	}
	var payload struct {
		Success bool             `json:"success"`
		Data    []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Success || len(payload.Data) != 1 {
		t.Fatalf("unexpected plans payload: %#v", payload)
	}
	if payload.Data[0]["plan_name"] != "Team Annual" || payload.Data[0]["enabled"] != false {
		t.Fatalf("expected normalized plan name and enabled flag, got %#v", payload.Data[0])
	}
}

func stringsReader(value string) *strings.Reader {
	return strings.NewReader(value)
}
