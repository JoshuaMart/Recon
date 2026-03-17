package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestJWTAuth_ValidToken(t *testing.T) {
	secret := "test-secret-32-characters-long!!"
	token, _, err := GenerateToken(secret, time.Hour)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	handler := JWTAuth(secret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ClaimsFromContext(r.Context())
		if !ok || claims == nil {
			t.Error("expected claims in context")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestJWTAuth_MissingHeader(t *testing.T) {
	handler := JWTAuth("secret")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestJWTAuth_InvalidFormat(t *testing.T) {
	handler := JWTAuth("secret")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic abc123")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestJWTAuth_InvalidToken(t *testing.T) {
	handler := JWTAuth("secret")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer invalid.token.here")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestJWTAuth_ExpiredToken(t *testing.T) {
	secret := "test-secret-32-characters-long!!"
	token, _, err := GenerateToken(secret, -time.Hour) // expired 1h ago
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	handler := JWTAuth(secret)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestJWTAuth_WrongSecret(t *testing.T) {
	token, _, _ := GenerateToken("secret-1-32-characters-long!!!!!", time.Hour)

	handler := JWTAuth("secret-2-32-characters-long!!!!!")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestGenerateToken(t *testing.T) {
	secret := "test-secret-32-characters-long!!"
	token, expiresAt, err := GenerateToken(secret, 24*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token == "" {
		t.Error("expected non-empty token")
	}
	if expiresAt.Before(time.Now()) {
		t.Error("expected future expiry")
	}
}
