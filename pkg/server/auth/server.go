package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-logr/logr"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/weaveworks/weave-gitops/core/logger"
	"github.com/weaveworks/weave-gitops/pkg/featureflags"
)

const (
	LoginOIDC                  string = "oidc"
	LoginUsername              string = "username"
	ClusterUserAuthSecretName  string = "cluster-user-auth"
	DefaultOIDCAuthSecretName  string = "oidc-auth"
	FeatureFlagClusterUser     string = "CLUSTER_USER_AUTH"
	FeatureFlagAnonymousAuth   string = "ANONYMOUS_AUTH"
	FeatureFlagOIDCAuth        string = "OIDC_AUTH"
	FeatureFlagOIDCPassthrough string = "WEAVE_GITOPS_FEATURE_OIDC_AUTH_PASSTHROUGH"

	// ClaimUsername is the default claim for getting the user from OIDC for
	// auth
	ClaimUsername string = "email"

	// ClaimGroups is the default claim for getting the groups from OIDC for
	// auth
	ClaimGroups string = "groups"
)

// DefaultScopes is the set of scopes that we require.
var DefaultScopes = []string{
	oidc.ScopeOpenID,
	oidc.ScopeOfflineAccess,
	ScopeEmail,
	ScopeGroups,
}

// OIDCConfig is used to configure an AuthServer to interact with
// an OIDC issuer.
type OIDCConfig struct {
	IssuerURL      string
	ClientID       string
	ClientSecret   string
	RedirectURL    string
	TokenDuration  time.Duration
	Scopes         []string
	ClaimsConfig   *ClaimsConfig
	UsernamePrefix string
	GroupsPrefix   string
}

// This is only used if the OIDCConfig doesn't have a TokenDuration set. If
// that is set then it is used for both OIDC cookies and other cookies.
const defaultCookieDuration time.Duration = time.Hour

// AuthServerConfig is used to configure an AuthServer.
type AuthServerConfig struct {
	Log                 logr.Logger
	client              *http.Client
	kubernetesClient    ctrlclient.Client
	tokenSignerVerifier TokenSignerVerifier
	OIDCConfig          OIDCConfig
	authMethods         map[AuthMethod]bool
	namespace           string

	noAuthUser     string
	SessionManager SessionManager
}

// AuthServer interacts with an OIDC issuer to handle the OAuth2 process flow.
type AuthServer struct {
	AuthServerConfig
	provider *oidc.Provider
	sm       SessionManager
}

// LoginRequest represents the data submitted by client when the auth flow (non-OIDC) is used.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// UserInfo represents the response returned from the user info handler.
type UserInfo struct {
	Email  string   `json:"email"`
	ID     string   `json:"id"`
	Groups []string `json:"groups"`
}

// NewOIDCConfigFromSecret takes a corev1.Secret and extracts the fields.
//
// The following keys are required in the secret:
//   - issuerURL
//   - clientID
//   - clientSecret
//   - redirectURL
//
// The following keys are optional
// - tokenDuration - defaults to 1 hour.
// - claimUsername - defaults to "email"
// - claimGroups - defaults to "groups"
// - customScopes - defaults to "openid","offline_access","email","groups"
func NewOIDCConfigFromSecret(secret corev1.Secret) OIDCConfig {
	cfg := OIDCConfig{
		IssuerURL:      string(secret.Data["issuerURL"]),
		ClientID:       string(secret.Data["clientID"]),
		ClientSecret:   string(secret.Data["clientSecret"]),
		RedirectURL:    string(secret.Data["redirectURL"]),
		UsernamePrefix: string(secret.Data["oidcUsernamePrefix"]),
		GroupsPrefix:   string(secret.Data["oidcGroupsPrefix"]),
	}
	cfg.ClaimsConfig = claimsConfigFromSecret(secret)

	tokenDuration, err := time.ParseDuration(string(secret.Data["tokenDuration"]))
	if err != nil {
		tokenDuration = time.Hour
	}

	cfg.TokenDuration = tokenDuration

	scopes := splitAndTrim(string(secret.Data["customScopes"]))
	if len(scopes) == 0 {
		scopes = DefaultScopes
	}

	cfg.Scopes = scopes

	return cfg
}

func splitAndTrim(s string) []string {
	result := []string{}
	splits := strings.Split(s, ",")

	for _, s := range splits {
		if v := strings.TrimSpace(s); v != "" {
			result = append(result, v)
		}
	}

	return result
}

func claimsConfigFromSecret(secret corev1.Secret) *ClaimsConfig {
	claimUsername, ok := secret.Data["claimUsername"]
	if !ok {
		claimUsername = []byte(ClaimUsername)
	}

	claimGroups, ok := secret.Data["claimGroups"]
	if !ok {
		claimGroups = []byte(ClaimGroups)
	}

	if len(claimUsername) > 0 && len(claimGroups) > 0 {
		return &ClaimsConfig{
			Username: string(claimUsername),
			Groups:   string(claimGroups),
		}
	}

	return nil
}

// NewAuthServerConfig creates and returns a new AuthServerConfig.
//
// The oidcCfg.IssuerURL and oidcCfg.RedirectURL are given a light validation to
// ensure they are valid URLs.
func NewAuthServerConfig(log logr.Logger, oidcCfg OIDCConfig, kubernetesClient ctrlclient.Client, tsv TokenSignerVerifier, namespace string, authMethods map[AuthMethod]bool, noAuthUser string, sm SessionManager) (*AuthServerConfig, error) {
	return &AuthServerConfig{
		Log:                 log.WithName("auth-server"),
		client:              http.DefaultClient,
		kubernetesClient:    kubernetesClient,
		tokenSignerVerifier: tsv,
		OIDCConfig:          oidcCfg,
		namespace:           namespace,
		noAuthUser:          noAuthUser,
		authMethods:         authMethods,
		SessionManager:      sm,
	}, nil
}

// NewAuthServer creates a new AuthServer object.
func NewAuthServer(ctx context.Context, cfg *AuthServerConfig) (*AuthServer, error) {
	if cfg.authMethods[UserAccount] {
		var secret corev1.Secret
		err := cfg.kubernetesClient.Get(ctx, ctrlclient.ObjectKey{
			Namespace: cfg.namespace,
			Name:      ClusterUserAuthSecretName,
		}, &secret)

		if err != nil {
			return nil, fmt.Errorf("could not get secret for cluster user, %w", err)
		} else {
			featureflags.SetBoolean(FeatureFlagClusterUser, true)
		}
	} else {
		featureflags.SetBoolean(FeatureFlagClusterUser, false)
	}

	var provider *oidc.Provider

	if cfg.OIDCConfig.IssuerURL == "" {
		featureflags.SetBoolean(FeatureFlagOIDCAuth, false)
	} else if cfg.authMethods[OIDC] {
		var err error

		provider, err = oidc.NewProvider(ctx, cfg.OIDCConfig.IssuerURL)
		if err != nil {
			return nil, fmt.Errorf("could not create provider: %w", err)
		}
		featureflags.SetBoolean(FeatureFlagOIDCAuth, true)
	}

	if cfg.authMethods[Anonymous] {
		featureflags.SetBoolean(FeatureFlagAnonymousAuth, true)
	}

	if !featureflags.IsSet(FeatureFlagOIDCAuth) &&
		!featureflags.IsSet(FeatureFlagClusterUser) &&
		!featureflags.IsSet(FeatureFlagAnonymousAuth) {
		return nil, fmt.Errorf("OIDC auth, local auth or anonymous mode must be enabled, can't start")
	}

	return &AuthServer{*cfg, provider, cfg.SessionManager}, nil
}

// SetRedirectURL is used to set the redirect URL. This is meant to be used
// in unit tests only.
func (s *AuthServer) SetRedirectURL(url string) {
	s.OIDCConfig.RedirectURL = url
}

func (s *AuthServer) oidcEnabled() bool {
	return featureflags.IsSet(FeatureFlagOIDCAuth)
}

func (s *AuthServer) oidcPassthroughEnabled() bool {
	return featureflags.IsSet(FeatureFlagOIDCPassthrough)
}

func (s *AuthServer) verifier() *oidc.IDTokenVerifier {
	return s.provider.Verifier(&oidc.Config{ClientID: s.OIDCConfig.ClientID})
}

func (s *AuthServer) oauth2Config(scopes []string) *oauth2.Config {
	requestScopes := []string{}
	// Ensure "openid" scope is always present.
	if !contains(scopes, oidc.ScopeOpenID) {
		requestScopes = append(requestScopes, oidc.ScopeOpenID)
	}

	requestScopes = append(requestScopes, scopes...)

	return &oauth2.Config{
		ClientID:     s.OIDCConfig.ClientID,
		ClientSecret: s.OIDCConfig.ClientSecret,
		RedirectURL:  s.OIDCConfig.RedirectURL,
		Endpoint:     s.provider.Endpoint(),
		Scopes:       requestScopes,
	}
}

func (s *AuthServer) OAuth2Flow() http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if !s.oidcEnabled() {
			JSONError(s.Log, rw, "oidc provider not configured", http.StatusBadRequest)
			return
		}

		s.startAuthFlow(rw, r)
	}
}

func (s *AuthServer) Callback(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		rw.Header().Add("Allow", "GET")
		rw.WriteHeader(http.StatusMethodNotAllowed)

		return
	}

	ctx := oidc.ClientContext(r.Context(), s.client)

	// Authorization redirect callback from OAuth2 auth flow.
	if errorCode := r.FormValue("error"); errorCode != "" {
		s.Log.Info("authz redirect callback failed", "error", errorCode, "error_description", r.FormValue("error_description"))
		rw.WriteHeader(http.StatusBadRequest)

		return
	}

	code := r.FormValue("code")
	if code == "" {
		s.Log.Info("code value was empty")
		rw.WriteHeader(http.StatusBadRequest)

		return
	}

	stateCookie := s.SessionManager.GetString(r.Context(), StateCookieName)
	if stateCookie == "" {
		s.Log.Info("cookie was not found in the request", "cookie", StateCookieName)
		rw.WriteHeader(http.StatusBadRequest)

		return
	}

	if state := r.FormValue("state"); state != stateCookie {
		s.Log.Info("cookie value does not match state form value")
		rw.WriteHeader(http.StatusBadRequest)

		return
	}

	b, err := base64.StdEncoding.DecodeString(stateCookie)
	if err != nil {
		s.Log.Error(err, "cannot base64 decode cookie", "cookie", StateCookieName, "cookie_value", stateCookie)
		rw.WriteHeader(http.StatusBadRequest)

		return
	}

	var state SessionState
	if err := json.Unmarshal(b, &state); err != nil {
		s.Log.Error(err, "failed to unmarshal state to JSON", "state", string(b))
		rw.WriteHeader(http.StatusBadRequest)

		return
	}

	token, err := s.oauth2Config(nil).Exchange(ctx, code)
	if err != nil {
		s.Log.Error(err, "failed to exchange auth code for token", "code", code)
		rw.WriteHeader(http.StatusInternalServerError)

		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		JSONError(s.Log, rw, "no id_token in token response", http.StatusInternalServerError)
		return
	}

	_, err = s.verifier().Verify(r.Context(), rawIDToken)
	if err != nil {
		JSONError(s.Log, rw, fmt.Sprintf("failed to verify ID token: %v", err), http.StatusInternalServerError)
		return
	}

	s.setCookies(r.Context(), rawIDToken, token.AccessToken, token.RefreshToken)
	// Clear state cookie
	s.SessionManager.Remove(r.Context(), StateCookieName)

	http.Redirect(rw, r, state.ReturnURL, http.StatusSeeOther)
}

func (s *AuthServer) setCookies(ctx context.Context, idToken, accessToken, refreshToken string) {
	s.Log.V(logger.LogLevelDebug).Info("setting ID token cookie", "size", len(idToken))
	s.sm.Put(ctx, IDTokenCookieName, idToken)

	s.Log.V(logger.LogLevelDebug).Info("setting access token cookie", "size", len(accessToken))
	s.sm.Put(ctx, AccessTokenCookieName, accessToken)

	s.Log.V(logger.LogLevelDebug).Info("setting refresh token cookie", "size", len(refreshToken))
	s.sm.Put(ctx, RefreshTokenCookieName, refreshToken)
}

func (s *AuthServer) SignIn() http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			rw.Header().Add("Allow", "POST")
			rw.WriteHeader(http.StatusMethodNotAllowed)

			return
		}

		var loginRequest LoginRequest

		err := json.NewDecoder(r.Body).Decode(&loginRequest)
		if err != nil {
			s.Log.Error(err, "Failed to decode from JSON")
			JSONError(s.Log, rw, "Failed to read request body.", http.StatusBadRequest)

			return
		}

		var hashedSecret corev1.Secret

		if err := s.kubernetesClient.Get(r.Context(), ctrlclient.ObjectKey{
			Name:      ClusterUserAuthSecretName,
			Namespace: s.namespace,
		}, &hashedSecret); err != nil {
			s.Log.Error(err, "Failed to query for the secret")
			JSONError(s.Log, rw, "Please ensure that a password has been set.", http.StatusBadRequest)

			return
		}

		if loginRequest.Username != string(hashedSecret.Data["username"]) {
			s.Log.Info("Wrong username")
			rw.WriteHeader(http.StatusUnauthorized)

			return
		}

		if err := bcrypt.CompareHashAndPassword(hashedSecret.Data["password"], []byte(loginRequest.Password)); err != nil {
			s.Log.Error(err, "Failed to compare hash with password")
			rw.WriteHeader(http.StatusUnauthorized)

			return
		}

		signed, err := s.tokenSignerVerifier.Sign(loginRequest.Username)
		if err != nil {
			s.Log.Error(err, "Failed to create and sign token")
			rw.WriteHeader(http.StatusInternalServerError)

			return
		}

		s.SessionManager.Put(r.Context(), IDTokenCookieName, signed)
		rw.WriteHeader(http.StatusOK)
	}
}

// UserInfo inspects the cookie and attempts to verify it as an admin token. If successful,
// it returns a UserInfo object with the email set to the admin token subject. Otherwise it
// uses the token to query the OIDC provider's user info endpoint and return a UserInfo object
// back or a 401 status in any other case.
func (s *AuthServer) UserInfo(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		rw.Header().Add("Allow", "GET")
		rw.WriteHeader(http.StatusMethodNotAllowed)

		return
	}

	if s.noAuthUser != "" {
		ui := UserInfo{
			ID: s.noAuthUser,
		}
		toJSON(rw, ui, s.Log)
		return
	}

	idCookie := s.SessionManager.GetString(r.Context(), IDTokenCookieName)
	if idCookie == "" {
		s.Log.Info("failed to get ID Token from request")
		rw.WriteHeader(http.StatusBadRequest)

		return
	}

	claims, err := s.tokenSignerVerifier.Verify(idCookie)
	if err == nil {
		ui := UserInfo{
			ID:    claims.Subject,
			Email: claims.Subject,
		}
		toJSON(rw, ui, s.Log)

		return
	}

	if !s.oidcEnabled() {
		ui := UserInfo{}
		toJSON(rw, ui, s.Log)

		return
	}

	info, err := s.verifier().Verify(r.Context(), idCookie)
	if err != nil {
		s.Log.Error(err, "failed to parse user ID token")
		JSONError(s.Log, rw, fmt.Sprintf("failed to parse id token: %v", err), http.StatusUnauthorized)

		return
	}

	userPrincipal, err := s.OIDCConfig.ClaimsConfig.PrincipalFromClaims(info)
	if err != nil {
		s.Log.Error(err, "failed to parse user info")
		JSONError(s.Log, rw, fmt.Sprintf("failed to query user info endpoint: %v", err), http.StatusUnauthorized)

		return
	}

	ui := UserInfo{
		ID:     userPrincipal.ID,
		Email:  userPrincipal.ID,
		Groups: userPrincipal.Groups,
	}

	toJSON(rw, ui, s.Log)
}

func (s *AuthServer) RefreshHandler(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.Log.Info("Only POST requests allowed")
		rw.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	_, err := s.Refresh(rw, r)
	if err != nil {
		s.Log.V(logger.LogLevelWarn).Info("refreshing token failed", "err", err)
		JSONError(s.Log, rw, "failed to refresh", http.StatusUnauthorized)
		return
	}

	rw.WriteHeader(http.StatusOK)
}

// Refresh is used to refresh the access token and id token. It updates the cookies on the response
// with the new tokens. It returns the new user principal.
func (s *AuthServer) Refresh(rw http.ResponseWriter, r *http.Request) (*UserPrincipal, error) {
	ctx := oidc.ClientContext(r.Context(), s.client)

	refreshTokenCookie := s.SessionManager.GetString(r.Context(), RefreshTokenCookieName)
	if refreshTokenCookie == "" {
		return nil, errors.New("couldn't fetch refresh token from cookie")
	}

	token, err := s.oauth2Config(nil).TokenSource(
		ctx,
		&oauth2.Token{
			RefreshToken: refreshTokenCookie,
		}).Token()
	if err != nil {
		return nil, fmt.Errorf("failed to refresh token: %w", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("no id_token in token response")
	}

	s.setCookies(r.Context(), rawIDToken, token.AccessToken, token.RefreshToken)

	return parseJWTToken(ctx, s.verifier(), rawIDToken, s.OIDCConfig.ClaimsConfig, s.Log)
}

func toJSON(rw http.ResponseWriter, ui UserInfo, log logr.Logger) {
	b, err := json.Marshal(ui)
	if err != nil {
		JSONError(log, rw, fmt.Sprintf("failed to marshal to JSON: %v", err), http.StatusInternalServerError)
		return
	}

	_, err = rw.Write(b)
	if err != nil {
		log.Error(err, "Failing to write response")
	}
}

func (s *AuthServer) startAuthFlow(rw http.ResponseWriter, r *http.Request) {
	nonce, err := generateNonce()
	if err != nil {
		JSONError(s.Log, rw, fmt.Sprintf("failed to generate nonce: %v", err), http.StatusInternalServerError)
		return
	}

	returnURL := r.URL.Query().Get("return_url")

	if returnURL == "" {
		returnURL = r.URL.String()
	}

	b, err := json.Marshal(SessionState{
		Nonce:     nonce,
		ReturnURL: returnURL,
	})
	if err != nil {
		JSONError(s.Log, rw, fmt.Sprintf("failed to marshal state to JSON: %v", err), http.StatusInternalServerError)
		return
	}

	state := base64.StdEncoding.EncodeToString(b)

	authCodeURL := s.oauth2Config(s.OIDCConfig.Scopes).AuthCodeURL(state)

	// Issue state cookie
	s.SessionManager.Put(r.Context(), StateCookieName, state)

	http.Redirect(rw, r, authCodeURL, http.StatusSeeOther)
}

func (s *AuthServer) Logout(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.Log.Info("Only POST requests allowed")
		rw.WriteHeader(http.StatusMethodNotAllowed)

		return
	}

	if err := s.SessionManager.Destroy(r.Context()); err != nil {
		s.Log.Error(err, "failed to destroy session")
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}

	rw.WriteHeader(http.StatusOK)
}

// SessionState represents the state that needs to be persisted between
// the AuthN request from the Relying Party (RP) to the authorization
// endpoint of the OpenID Provider (OP) and the AuthN response back from
// the OP to the RP's callback URL. This state could be persisted server-side
// in a data store such as Redis but we prefer to operate stateless so we
// store this in a cookie instead. The cookie value and the value of the
// "state" parameter passed in the AuthN request are identical and set to
// the base64-encoded, JSON serialised state.
//
// https://openid.net/specs/openid-connect-core-1_0.html#Overview
// https://auth0.com/docs/configure/attack-protection/state-parameters#alternate-redirect-method
// https://community.auth0.com/t/state-parameter-and-user-redirection/8387/2
type SessionState struct {
	Nonce     string `json:"n"`
	ReturnURL string `json:"return_url"`
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}

	return false
}

func JSONError(log logr.Logger, w http.ResponseWriter, errStr string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)

	response := struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	}{Message: errStr, Code: code}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Error(err, "failed encoding error message", "message", errStr)
	}
}
