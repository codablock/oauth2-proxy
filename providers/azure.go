package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/bitly/go-simplejson"
	"github.com/dgrijalva/jwt-go"
	"github.com/oauth2-proxy/oauth2-proxy/v7/pkg/apis/sessions"
	"github.com/oauth2-proxy/oauth2-proxy/v7/pkg/logger"
	"github.com/oauth2-proxy/oauth2-proxy/v7/pkg/requests"
)

// AzureProvider represents an Azure based Identity Provider
type AzureProvider struct {
	*ProviderData
	Tenant string
}

var _ Provider = (*AzureProvider)(nil)

const (
	azureProviderName = "Azure"
	azureDefaultScope = "openid"
)

var (
	// Default Login URL for Azure.
	// Pre-parsed URL of https://login.microsoftonline.com/common/oauth2/authorize.
	azureDefaultLoginURL = &url.URL{
		Scheme: "https",
		Host:   "login.microsoftonline.com",
		Path:   "/common/oauth2/authorize",
	}

	// Default Redeem URL for Azure.
	// Pre-parsed URL of https://login.microsoftonline.com/common/oauth2/token.
	azureDefaultRedeemURL = &url.URL{
		Scheme: "https",
		Host:   "login.microsoftonline.com",
		Path:   "/common/oauth2/token",
	}

	// Default Profile URL for Azure.
	// Pre-parsed URL of https://graph.microsoft.com/v1.0/me.
	azureDefaultProfileURL = &url.URL{
		Scheme: "https",
		Host:   "graph.microsoft.com",
		Path:   "/v1.0/me",
	}

	// Default ProtectedResource URL for Azure.
	// Pre-parsed URL of https://graph.microsoft.com.
	azureDefaultProtectResourceURL = &url.URL{
		Scheme: "https",
		Host:   "graph.microsoft.com",
	}
)

// NewAzureProvider initiates a new AzureProvider
func NewAzureProvider(p *ProviderData) *AzureProvider {
	p.setProviderDefaults(providerDefaults{
		name:        azureProviderName,
		loginURL:    azureDefaultLoginURL,
		redeemURL:   azureDefaultRedeemURL,
		profileURL:  azureDefaultProfileURL,
		validateURL: nil,
		scope:       azureDefaultScope,
	})

	if p.ProtectedResource == nil || p.ProtectedResource.String() == "" {
		p.ProtectedResource = azureDefaultProtectResourceURL
	}
	if p.ValidateURL == nil || p.ValidateURL.String() == "" {
		p.ValidateURL = p.ProfileURL
	}

	return &AzureProvider{
		ProviderData: p,
		Tenant:       "common",
	}
}

// Configure defaults the AzureProvider configuration options
func (p *AzureProvider) Configure(tenant string) {
	if tenant == "" || tenant == "common" {
		// tenant is empty or default, remain on the default "common" tenant
		return
	}

	// Specific tennant specified, override the Login and RedeemURLs
	p.Tenant = tenant
	overrideTenantURL(p.LoginURL, azureDefaultLoginURL, tenant, "authorize")
	overrideTenantURL(p.RedeemURL, azureDefaultRedeemURL, tenant, "token")
}

func overrideTenantURL(current, defaultURL *url.URL, tenant, path string) {
	if current == nil || current.String() == "" || current.String() == defaultURL.String() {
		*current = url.URL{
			Scheme: "https",
			Host:   "login.microsoftonline.com",
			Path:   "/" + tenant + "/oauth2/" + path}
	}
}

// Redeem exchanges the OAuth2 authentication token for an ID token
func (p *AzureProvider) Redeem(ctx context.Context, redirectURL, code string) (*sessions.SessionState, error) {
	if code == "" {
		return nil, ErrMissingCode
	}
	clientSecret, err := p.GetClientSecret()
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Add("redirect_uri", redirectURL)
	params.Add("client_id", p.ClientID)
	params.Add("client_secret", clientSecret)
	params.Add("code", code)
	params.Add("grant_type", "authorization_code")
	if p.ProtectedResource != nil && p.ProtectedResource.String() != "" {
		params.Add("resource", p.ProtectedResource.String())
	}

	// blindly try json and x-www-form-urlencoded
	var jsonResponse struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresOn    int64  `json:"expires_on,string"`
		IDToken      string `json:"id_token"`
	}

	err = requests.New(p.RedeemURL.String()).
		WithContext(ctx).
		WithMethod("POST").
		WithBody(bytes.NewBufferString(params.Encode())).
		SetHeader("Content-Type", "application/x-www-form-urlencoded").
		Do().
		UnmarshalInto(&jsonResponse)
	if err != nil {
		return nil, err
	}

	created := time.Now()
	expires := time.Unix(jsonResponse.ExpiresOn, 0)

	return &sessions.SessionState{
		AccessToken:  jsonResponse.AccessToken,
		IDToken:      jsonResponse.IDToken,
		CreatedAt:    &created,
		ExpiresOn:    &expires,
		RefreshToken: jsonResponse.RefreshToken,
	}, nil
}

// RefreshSessionIfNeeded checks if the session has expired and uses the
// RefreshToken to fetch a new ID token if required
func (p *AzureProvider) RefreshSessionIfNeeded(ctx context.Context, s *sessions.SessionState) (bool, error) {
	if s == nil || s.ExpiresOn.After(time.Now()) || s.RefreshToken == "" {
		return false, nil
	}

	origExpiration := s.ExpiresOn

	err := p.redeemRefreshToken(ctx, s)
	if err != nil {
		return false, fmt.Errorf("unable to redeem refresh token: %v", err)
	}

	logger.Printf("refreshed id token %s (expired on %s)\n", s, origExpiration)
	return true, nil
}

func (p *AzureProvider) redeemRefreshToken(ctx context.Context, s *sessions.SessionState) (err error) {
	params := url.Values{}
	params.Add("client_id", p.ClientID)
	params.Add("client_secret", p.ClientSecret)
	params.Add("refresh_token", s.RefreshToken)
	params.Add("grant_type", "refresh_token")

	var jsonResponse struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresOn    int64  `json:"expires_on,string"`
		IDToken      string `json:"id_token"`
	}

	err = requests.New(p.RedeemURL.String()).
		WithContext(ctx).
		WithMethod("POST").
		WithBody(bytes.NewBufferString(params.Encode())).
		SetHeader("Content-Type", "application/x-www-form-urlencoded").
		Do().
		UnmarshalInto(&jsonResponse)

	if err != nil {
		return
	}

	now := time.Now()
	expires := time.Unix(jsonResponse.ExpiresOn, 0)
	s.AccessToken = jsonResponse.AccessToken
	s.IDToken = jsonResponse.IDToken
	s.RefreshToken = jsonResponse.RefreshToken
	s.CreatedAt = &now
	s.ExpiresOn = &expires
	return
}

func makeAzureHeader(accessToken string) http.Header {
	return makeAuthorizationHeader(tokenTypeBearer, accessToken, nil)
}

func getEmailFromJSON(json *simplejson.Json) (string, error) {
	var email string
	var err error

	email, err = json.Get("mail").String()

	if err != nil || email == "" {
		otherMails, otherMailsErr := json.Get("otherMails").Array()
		if len(otherMails) > 0 {
			email = otherMails[0].(string)
		}
		err = otherMailsErr
	}

	if err != nil || email == "" {
		email, err = json.Get("userPrincipalName").String()
		if err != nil {
			logger.Errorf("unable to find userPrincipalName: %s", err)
			return "", err
		}
	}

	return email, err
}

// full claims that can be emitted by AAD are
// https://docs.microsoft.com/en-us/azure/active-directory/develop/id-tokens
// only include email here since we only need email extraction
type aadIDTokenClaims struct {
	Email string `json:"email,omitempty"`
}

// Valid is the function validating the claims,
// required by jwt.Claims interface.
// returning nil as we only use it to extract claims instead of validating them
func (aadIDTokenClaims) Valid() error {
	return nil
}

func getEmailFromIDToken(idToken string) (string, error) {
	claims := &aadIDTokenClaims{}
	parser := jwt.Parser{}

	// ParseUnverified to extract claims without verifying signature
	if _, _, err := parser.ParseUnverified(idToken, claims); err != nil {
		return "", err
	}
	if claims.Email == "" {
		return "", errors.New("missing email claim from id_token")
	}
	return claims.Email, nil
}

func getEmailFromProfileAPI(ctx context.Context, accessToken, profileURL string) (string, error) {
	if accessToken == "" {
		return "", errors.New("missing access token")
	}

	json, err := requests.New(profileURL).
		WithContext(ctx).
		WithHeaders(makeAzureHeader(accessToken)).
		Do().
		UnmarshalJSON()
	if err != nil {
		return "", err
	}

	return getEmailFromJSON(json)
}

// EnrichSessionState finds the email to enrich the session state
func (p *AzureProvider) EnrichSessionState(ctx context.Context, s *sessions.SessionState) error {
	var email string
	var err error

	if s.IDToken != "" {
		email, err := getEmailFromIDToken(s.IDToken)
		if err != nil || email == "" {
			logger.Errorf("unable to find email from id_token: %s", err)
		} else {
			s.Email = email
			return nil
		}
	}

	email, err = getEmailFromProfileAPI(ctx, s.AccessToken, p.ProfileURL.String())
	if email == "" {
		logger.Errorf("failed to get email address: %s", err)
		return err
	}
	s.Email = email

	return err
}

func (p *AzureProvider) GetLoginURL(redirectURI, state string) string {
	extraParams := url.Values{}
	if p.ProtectedResource != nil && p.ProtectedResource.String() != "" {
		extraParams.Add("resource", p.ProtectedResource.String())
	}
	a := makeLoginURL(p.ProviderData, redirectURI, state, extraParams)
	return a.String()
}

// ValidateSession validates the AccessToken
func (p *AzureProvider) ValidateSession(ctx context.Context, s *sessions.SessionState) bool {
	return validateToken(ctx, p, s.AccessToken, makeAzureHeader(s.AccessToken))
}
