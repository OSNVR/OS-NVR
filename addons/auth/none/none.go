// SPDX-License-Identifier: GPL-2.0-or-later

package none

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"nvr"
	"nvr/pkg/log"
	"nvr/pkg/storage"
	"nvr/pkg/web/auth"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

func init() {
	nvr.SetAuthenticator(NewAuthenticator)

	// Remove logout button.
	nvr.RegisterTplSubHook(func(pageFiles map[string]string) error {
		re := regexp.MustCompile(`<div id="logout">(\n.*){6}`)

		pageFiles["sidebar.tpl"] = re.ReplaceAllString(pageFiles["sidebar.tpl"], "")

		return nil
	})
}

// Authenticator implements auth.Authenticator.
type Authenticator struct {
	path     string // Path to save user information.
	accounts map[string]auth.Account
	hashCost int

	token string
	mu    sync.Mutex
}

// NewAuthenticator creates a authenticator similar to
// basic.Authenticator but it allows all requests.
func NewAuthenticator(env storage.ConfigEnv, _ *log.Logger) (auth.Authenticator, error) {
	path := filepath.Join(env.ConfigDir, "users.json")
	a := Authenticator{
		path:     path,
		accounts: make(map[string]auth.Account),
		hashCost: auth.DefaultBcryptHashCost,

		token: auth.GenToken(),
	}

	file, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			a.accounts = make(map[string]auth.Account)
			return &a, nil
		}
		return nil, err
	}

	err = json.Unmarshal(file, &a.accounts)
	if err != nil {
		return nil, err
	}

	return &a, nil
}

// ValidateRequest allows all requests.
func (a *Authenticator) ValidateRequest(_ *http.Request) auth.ValidateResponse {
	return auth.ValidateResponse{
		IsValid: true,
		User: auth.Account{
			ID:       "none",
			Username: "noAuth",
			IsAdmin:  true,
			Token:    a.token,
		},
	}
}

// AuthDisabled True.
func (a *Authenticator) AuthDisabled() bool {
	return true
}

// UsersList returns a obfuscated user list.
func (a *Authenticator) UsersList() map[string]auth.AccountObfuscated {
	defer a.mu.Unlock()
	a.mu.Lock()

	list := make(map[string]auth.AccountObfuscated)
	for id, user := range a.accounts {
		list[id] = auth.AccountObfuscated{
			ID:       user.ID,
			Username: user.Username,
			IsAdmin:  user.IsAdmin,
		}
	}
	return list
}

// Errors.
var (
	ErrIDMissing       = errors.New("missing ID")
	ErrUsernameMissing = errors.New("missing username")
	ErrPasswordMissing = errors.New("password is required for new users")
	ErrUserNotExist    = errors.New("user does not exist")
)

// UserSet set user details.
func (a *Authenticator) UserSet(req auth.SetUserRequest) error {
	defer a.mu.Unlock()
	a.mu.Lock()

	if req.ID == "" {
		return ErrIDMissing
	}

	if req.Username == "" {
		return ErrUsernameMissing
	}

	_, exists := a.accounts[req.ID]
	if !exists && req.PlainPassword == "" {
		return ErrPasswordMissing
	}

	user := a.accounts[req.ID]
	a.mu.Unlock()

	user.ID = req.ID
	user.Username = req.Username
	user.IsAdmin = req.IsAdmin
	if req.PlainPassword != "" {
		hashedNewPassword, _ := bcrypt.GenerateFromPassword([]byte(req.PlainPassword), a.hashCost)
		user.Password = hashedNewPassword
	}

	a.mu.Lock()
	a.accounts[user.ID] = user

	if err := a.SaveUsersToFile(); err != nil {
		return fmt.Errorf("could not save users to file: %w", err)
	}

	return nil
}

// UserDelete allows basic auth users to be deleted.
func (a *Authenticator) UserDelete(id string) error {
	defer a.mu.Unlock()
	a.mu.Lock()
	if _, exists := a.accounts[id]; !exists {
		return ErrUserNotExist
	}
	delete(a.accounts, id)

	if err := a.SaveUsersToFile(); err != nil {
		return err
	}

	return nil
}

// User allows all requests.
func (a *Authenticator) User(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

// Admin allows all requests.
func (a *Authenticator) Admin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

// CSRF blocks invalid Cross-site request forgery tokens.
// The request needs to have the token in the "X-CSRF-TOKEN" header.
func (a *Authenticator) CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-CSRF-TOKEN")
		if token != a.token {
			http.Error(w, "Invalid CSRF-token.", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// MyToken returns the CSRF-token.
func (a *Authenticator) MyToken() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(a.token)); err != nil {
			http.Error(w, "could not write", http.StatusInternalServerError)
			return
		}
	})
}

// Logout returns 404.
func (a *Authenticator) Logout() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
}

// SaveUsersToFile saves json file.
func (a *Authenticator) SaveUsersToFile() error {
	users, _ := json.MarshalIndent(a.accounts, "", "  ")

	err := os.WriteFile(a.path, users, 0o600)
	if err != nil {
		return err
	}

	return nil
}
