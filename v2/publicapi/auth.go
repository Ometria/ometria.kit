package publicapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Headers the API gateway sets once it has validated the client's API key.
const (
	HeaderAccountID    = "Ometria-Account-Id"
	HeaderAccessScopes = "Ometria-Access-Scopes"
)

// Access scopes RequireScopeByMethod checks.
const (
	ScopeRead  = "read:default"
	ScopeWrite = "write:default"
)

// Messages of the errors Authenticate and RequireScopeByMethod write, as
// PHP's error handler words them ("Forbidden: " followed by the AuthError).
const (
	msgAccountHeaderMissing = "Forbidden: Ometria-Account-Id header missing"
	msgScopesHeaderMissing  = "Forbidden: Ometria-Access-Scopes header missing"
	msgAuthorizationFailed  = "Forbidden: Authorization failed"
	msgIncorrectScopes      = "Forbidden: Incorrect access scopes"
	msgInternalServerError  = "Internal server error"
)

// ErrAccountNotFound is what an AccountLookup returns for an account that
// doesn't exist or isn't active.
var ErrAccountNotFound = errors.New("publicapi: account not found")

// Account is the account a request was authenticated as.
type Account struct {
	ID       int
	IsActive bool
	DbShard  string
	DbName   string
}

// AccountLookup finds accounts for Authenticate.
//
// Get returns ErrAccountNotFound (possibly wrapped) when the account doesn't
// exist or isn't active, which is how api-accounts answers PHP's
// "GET /v1/accounts/{id}?status=active", and any other error when the lookup
// itself failed. Implementations that cache accounts must fall through to a
// live lookup on a cache miss before returning ErrAccountNotFound, so an
// account created since the cache was filled can use the API straight away.
type AccountLookup interface {
	Get(ctx context.Context, accountID int) (Account, error)
}

// AccountLookupFunc adapts a function to AccountLookup.
type AccountLookupFunc func(ctx context.Context, accountID int) (Account, error)

// Get calls f.
func (f AccountLookupFunc) Get(ctx context.Context, accountID int) (Account, error) {
	return f(ctx, accountID)
}

type authKey struct{}

type auth struct {
	account Account
	scopes  []string
}

// ContextWithAuth returns ctx carrying account and scopes, as Authenticate
// stores them. It's for testing handlers without the middleware.
func ContextWithAuth(ctx context.Context, account Account, scopes []string) context.Context {
	return context.WithValue(ctx, authKey{}, auth{account: account, scopes: scopes})
}

// AccountFromContext returns the account Authenticate authenticated the
// request as, and false if it didn't run.
func AccountFromContext(ctx context.Context) (Account, bool) {
	a, ok := ctx.Value(authKey{}).(auth)
	return a.account, ok
}

// ScopesFromContext returns the access scopes of the request, split on
// commas, or nil if Authenticate didn't run.
func ScopesFromContext(ctx context.Context) []string {
	a, _ := ctx.Value(authKey{}).(auth)
	return a.scopes
}

// Authenticate returns middleware that loads the account named by the
// Ometria-Account-Id header and puts it, and the Ometria-Access-Scopes, in
// the request's context. Read the package documentation's trust boundary
// before using it. In PHP's order, it responds:
//
//   - 403 "Forbidden: Ometria-Account-Id header missing" when the account
//     header is missing, empty or "0" (PHP's falsy values). It wins when both
//     headers are missing.
//   - 403 "Forbidden: Ometria-Access-Scopes header missing" likewise.
//   - 500 "Internal server error" when the lookup returns any error,
//     ErrAccountNotFound included, and when the id isn't an integer. PHP
//     sends the id to api-accounts, which answers 404 for inactive, unknown
//     and non-numeric ids alike (and parses ids with strconv.Atoi, so "+7"
//     and "007" are account 7 there too); BaseController's 404 and 500
//     branches throw an APIError it never imported, so each is a fatal
//     error that PHP's shutdown handler turns into this generic 500.
//   - 403 "Forbidden: Authorization failed" when the lookup returns an
//     account that isn't active. PHP has this branch but never reaches it,
//     because api-accounts doesn't return inactive accounts.
//
// PHP retries a failed api-accounts call up to three times; a transport
// error or an unexpected status that persists ends in the same 500 as here.
// Lookups here leave retries to the AccountLookup.
func Authenticate(lookup AccountLookup) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			idHeader := r.Header.Get(HeaderAccountID)
			if !phpTruthy(idHeader) {
				Error(w, r, http.StatusForbidden, msgAccountHeaderMissing)
				return
			}
			scopesHeader := r.Header.Get(HeaderAccessScopes)
			if !phpTruthy(scopesHeader) {
				Error(w, r, http.StatusForbidden, msgScopesHeaderMissing)
				return
			}

			id, err := strconv.Atoi(idHeader)
			if err != nil {
				Error(w, r, http.StatusInternalServerError, msgInternalServerError)
				return
			}
			account, err := lookup.Get(r.Context(), id)
			if err != nil {
				Error(w, r, http.StatusInternalServerError, msgInternalServerError)
				return
			}
			if !account.IsActive {
				Error(w, r, http.StatusForbidden, msgAuthorizationFailed)
				return
			}

			if s := logStateFrom(r.Context()); s != nil {
				s.accountID = strconv.Itoa(account.ID)
			}
			// PHP's explode: no trimming, so "a, b" holds " b".
			scopes := strings.Split(scopesHeader, ",")
			next.ServeHTTP(w, r.WithContext(ContextWithAuth(r.Context(), account, scopes)))
		})
	}
}

// RequireScopeByMethod returns middleware that checks the request's access
// scopes, as BaseController does: GET needs "read:default", and PUT, POST,
// PATCH and DELETE need "write:default". Other methods, HEAD and OPTIONS
// among them, need nothing. Scopes match exactly, so "write:default,
// read:default" (with a space) has no read scope. A request without the
// scope gets 403 "Forbidden: Incorrect access scopes".
//
// The method is compared in upper case, so "get" needs "read:default" as
// GET does, should one get past Routes, which rejects it as nginx does. PHP
// compares the method as sent, which would let a lowercase "get" through
// without the read scope if nginx didn't stop it first.
//
// It reads the scopes Authenticate stored, so it must run inside it; without
// them, every method that needs a scope is refused.
func RequireScopeByMethod() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var need string
			switch phpMethod(r) {
			case http.MethodGet:
				need = ScopeRead
			case http.MethodPut, http.MethodPost, http.MethodPatch, http.MethodDelete:
				need = ScopeWrite
			}
			if need != "" && !slices.Contains(ScopesFromContext(r.Context()), need) {
				Error(w, r, http.StatusForbidden, msgIncorrectScopes)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
