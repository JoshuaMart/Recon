package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/jomar/recon/api/middleware"
	"golang.org/x/crypto/bcrypt"
)

type AuthHandler struct {
	passwordHash string
	jwtSecret    string
	jwtExpiry    time.Duration

	mu            sync.RWMutex
	refreshTokens map[string]time.Time // token -> expiry
}

func NewAuthHandler(passwordHash, jwtSecret string, jwtExpiry time.Duration) *AuthHandler {
	return &AuthHandler{
		passwordHash:  passwordHash,
		jwtSecret:     jwtSecret,
		jwtExpiry:     jwtExpiry,
		refreshTokens: make(map[string]time.Time),
	}
}

type loginRequest struct {
	Password string `json:"password"`
}

type authResponse struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(h.passwordHash), []byte(req.Password)); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid password")
		return
	}

	resp, err := h.issueTokens()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	h.mu.RLock()
	expiry, exists := h.refreshTokens[req.RefreshToken]
	h.mu.RUnlock()

	if !exists || time.Now().After(expiry) {
		if exists {
			h.mu.Lock()
			delete(h.refreshTokens, req.RefreshToken)
			h.mu.Unlock()
		}
		writeError(w, http.StatusUnauthorized, "invalid or expired refresh token")
		return
	}

	// Invalidate the used refresh token
	h.mu.Lock()
	delete(h.refreshTokens, req.RefreshToken)
	h.mu.Unlock()

	resp, err := h.issueTokens()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *AuthHandler) issueTokens() (*authResponse, error) {
	token, expiresAt, err := middleware.GenerateToken(h.jwtSecret, h.jwtExpiry)
	if err != nil {
		return nil, err
	}

	refreshToken, err := generateRefreshToken()
	if err != nil {
		return nil, err
	}

	// Refresh token valid for 7 days
	h.mu.Lock()
	h.refreshTokens[refreshToken] = time.Now().Add(7 * 24 * time.Hour)
	h.mu.Unlock()

	return &authResponse{
		Token:        token,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt.Format(time.RFC3339),
	}, nil
}

func generateRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
