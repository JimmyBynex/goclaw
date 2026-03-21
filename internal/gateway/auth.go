package gateway

import (
	"net/http"
	"strings"
)

type Auth struct {
	token string
}

func NewAuth(token string) *Auth {
	return &Auth{token: token}
}

func (a *Auth) Validate(r *http.Request) bool {
	if a.token == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		auth = strings.TrimPrefix(auth, "Bearer ")
		return auth == a.token
	}

	query := r.URL.Query().Get("token")
	return query == a.token
}
