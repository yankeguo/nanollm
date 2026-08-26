package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	adminCookieName = "nanollm_admin"
	adminCookieTTL  = 12 * time.Hour
)

func adminCookieKey(username, password string) []byte {
	sum := sha256.Sum256([]byte("nanollm-admin\n" + username + "\n" + password))
	return sum[:]
}

func constEqSHA256(a, b string) bool {
	aa := sha256.Sum256([]byte(a))
	bb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(aa[:], bb[:]) == 1
}

func signAdminCookie(key []byte, username string, exp time.Time) string {
	expStr := strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(username + "|" + expStr))
	return expStr + "|" + hex.EncodeToString(mac.Sum(nil))
}

func verifyAdminCookie(key []byte, username, value string, now time.Time) bool {
	expStr, macHex, ok := strings.Cut(value, "|")
	if !ok || expStr == "" || macHex == "" {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || now.Unix() >= exp {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(username + "|" + expStr))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(macHex)
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

func cookieSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) setAdminCookie(w http.ResponseWriter, r *http.Request) {
	exp := time.Now().UTC().Add(adminCookieTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    signAdminCookie(s.adminKey, s.Config.Admin.Username, exp),
		Path:     "/",
		MaxAge:   int(adminCookieTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(r),
	})
}

func (s *Server) clearAdminCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(r),
	})
}

func (s *Server) adminCookieValid(r *http.Request) bool {
	c, err := r.Cookie(adminCookieName)
	if err != nil || c.Value == "" || s.Config == nil {
		return false
	}
	return verifyAdminCookie(s.adminKey, s.Config.Admin.Username, c.Value, time.Now().UTC())
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.adminCookieValid(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) checkAdminLogin(user, pass string) bool {
	if s.Config == nil {
		return false
	}
	userOK := constEqSHA256(user, s.Config.Admin.Username)
	passOK := constEqSHA256(pass, s.Config.Admin.Password)
	return userOK && passOK
}
