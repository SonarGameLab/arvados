// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package localdb

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"text/template"
	"time"

	"git.arvados.org/arvados.git/lib/controller/rpc"
	"git.arvados.org/arvados.git/sdk/go/arvados"
	"git.arvados.org/arvados.git/sdk/go/auth"
	"git.arvados.org/arvados.git/sdk/go/ctxlog"
	"git.arvados.org/arvados.git/sdk/go/httpserver"
	"github.com/coreos/go-oidc"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/api/people/v1"
)

type googleLoginController struct {
	Cluster    *arvados.Cluster
	RailsProxy *railsProxy

	issuer            string // override OIDC issuer URL (normally https://accounts.google.com) for testing
	peopleAPIBasePath string // override Google People API base URL (normally set by google pkg to https://people.googleapis.com/)
	provider          *oidc.Provider
	mu                sync.Mutex
}

func (ctrl *googleLoginController) getProvider() (*oidc.Provider, error) {
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	if ctrl.provider == nil {
		issuer := ctrl.issuer
		if issuer == "" {
			issuer = "https://accounts.google.com"
		}
		provider, err := oidc.NewProvider(context.Background(), issuer)
		if err != nil {
			return nil, err
		}
		ctrl.provider = provider
	}
	return ctrl.provider, nil
}

func (ctrl *googleLoginController) Logout(ctx context.Context, opts arvados.LogoutOptions) (arvados.LogoutResponse, error) {
	return noopLogout(ctrl.Cluster, opts)
}

func (ctrl *googleLoginController) Login(ctx context.Context, opts arvados.LoginOptions) (arvados.LoginResponse, error) {
	provider, err := ctrl.getProvider()
	if err != nil {
		return loginError(fmt.Errorf("error setting up OpenID Connect provider: %s", err))
	}
	redirURL, err := (*url.URL)(&ctrl.Cluster.Services.Controller.ExternalURL).Parse("/login")
	if err != nil {
		return loginError(fmt.Errorf("error making redirect URL: %s", err))
	}
	conf := &oauth2.Config{
		ClientID:     ctrl.Cluster.Login.Google.ClientID,
		ClientSecret: ctrl.Cluster.Login.Google.ClientSecret,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		RedirectURL:  redirURL.String(),
	}
	verifier := provider.Verifier(&oidc.Config{
		ClientID: conf.ClientID,
	})
	if opts.State == "" {
		// Initiate Google sign-in.
		if opts.ReturnTo == "" {
			return loginError(errors.New("missing return_to parameter"))
		}
		me := url.URL(ctrl.Cluster.Services.Controller.ExternalURL)
		callback, err := me.Parse("/" + arvados.EndpointLogin.Path)
		if err != nil {
			return loginError(err)
		}
		conf.RedirectURL = callback.String()
		state := ctrl.newOAuth2State([]byte(ctrl.Cluster.SystemRootToken), opts.Remote, opts.ReturnTo)
		return arvados.LoginResponse{
			RedirectLocation: conf.AuthCodeURL(state.String(),
				// prompt=select_account tells Google
				// to show the "choose which Google
				// account" page, even if the client
				// is currently logged in to exactly
				// one Google account.
				oauth2.SetAuthURLParam("prompt", "select_account")),
		}, nil
	} else {
		// Callback after Google sign-in.
		state := ctrl.parseOAuth2State(opts.State)
		if !state.verify([]byte(ctrl.Cluster.SystemRootToken)) {
			return loginError(errors.New("invalid OAuth2 state"))
		}
		oauth2Token, err := conf.Exchange(ctx, opts.Code)
		if err != nil {
			return loginError(fmt.Errorf("error in OAuth2 exchange: %s", err))
		}
		rawIDToken, ok := oauth2Token.Extra("id_token").(string)
		if !ok {
			return loginError(errors.New("error in OAuth2 exchange: no ID token in OAuth2 token"))
		}
		idToken, err := verifier.Verify(ctx, rawIDToken)
		if err != nil {
			return loginError(fmt.Errorf("error verifying ID token: %s", err))
		}
		authinfo, err := ctrl.getAuthInfo(ctx, ctrl.Cluster, conf, oauth2Token, idToken)
		if err != nil {
			return loginError(err)
		}
		ctxRoot := auth.NewContext(ctx, &auth.Credentials{Tokens: []string{ctrl.Cluster.SystemRootToken}})
		return ctrl.RailsProxy.UserSessionCreate(ctxRoot, rpc.UserSessionCreateOptions{
			ReturnTo: state.Remote + "," + state.ReturnTo,
			AuthInfo: *authinfo,
		})
	}
}

func (ctrl *googleLoginController) UserAuthenticate(ctx context.Context, opts arvados.UserAuthenticateOptions) (arvados.APIClientAuthorization, error) {
	return arvados.APIClientAuthorization{}, httpserver.ErrorWithStatus(errors.New("username/password authentication is not available"), http.StatusBadRequest)
}

// Use a person's token to get all of their email addresses, with the
// primary address at index 0. The provided defaultAddr is always
// included in the returned slice, and is used as the primary if the
// Google API does not indicate one.
func (ctrl *googleLoginController) getAuthInfo(ctx context.Context, cluster *arvados.Cluster, conf *oauth2.Config, token *oauth2.Token, idToken *oidc.IDToken) (*rpc.UserSessionAuthInfo, error) {
	var ret rpc.UserSessionAuthInfo
	defer ctxlog.FromContext(ctx).WithField("ret", &ret).Debug("getAuthInfo returned")

	var claims struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("error extracting claims from ID token: %s", err)
	} else if claims.Verified {
		// Fall back to this info if the People API call
		// (below) doesn't return a primary && verified email.
		if names := strings.Fields(strings.TrimSpace(claims.Name)); len(names) > 1 {
			ret.FirstName = strings.Join(names[0:len(names)-1], " ")
			ret.LastName = names[len(names)-1]
		} else {
			ret.FirstName = names[0]
		}
		ret.Email = claims.Email
	}

	if !ctrl.Cluster.Login.Google.AlternateEmailAddresses {
		if ret.Email == "" {
			return nil, fmt.Errorf("cannot log in with unverified email address %q", claims.Email)
		}
		return &ret, nil
	}

	svc, err := people.NewService(ctx, option.WithTokenSource(conf.TokenSource(ctx, token)), option.WithScopes(people.UserEmailsReadScope))
	if err != nil {
		return nil, fmt.Errorf("error setting up People API: %s", err)
	}
	if p := ctrl.peopleAPIBasePath; p != "" {
		// Override normal API endpoint (for testing)
		svc.BasePath = p
	}
	person, err := people.NewPeopleService(svc).Get("people/me").PersonFields("emailAddresses,names").Do()
	if err != nil {
		if strings.Contains(err.Error(), "Error 403") && strings.Contains(err.Error(), "accessNotConfigured") {
			// Log the original API error, but display
			// only the "fix config" advice to the user.
			ctxlog.FromContext(ctx).WithError(err).WithField("email", ret.Email).Error("People API is not enabled")
			return nil, errors.New("configuration error: Login.GoogleAlternateEmailAddresses is true, but Google People API is not enabled")
		} else {
			return nil, fmt.Errorf("error getting profile info from People API: %s", err)
		}
	}

	// The given/family names returned by the People API and
	// flagged as "primary" (if any) take precedence over the
	// split-by-whitespace result from above.
	for _, name := range person.Names {
		if name.Metadata != nil && name.Metadata.Primary {
			ret.FirstName = name.GivenName
			ret.LastName = name.FamilyName
			break
		}
	}

	altEmails := map[string]bool{}
	if ret.Email != "" {
		altEmails[ret.Email] = true
	}
	for _, ea := range person.EmailAddresses {
		if ea.Metadata == nil || !ea.Metadata.Verified {
			ctxlog.FromContext(ctx).WithField("address", ea.Value).Info("skipping unverified email address")
			continue
		}
		altEmails[ea.Value] = true
		if ea.Metadata.Primary || ret.Email == "" {
			ret.Email = ea.Value
		}
	}
	if len(altEmails) == 0 {
		return nil, errors.New("cannot log in without a verified email address")
	}
	for ae := range altEmails {
		if ae != ret.Email {
			ret.AlternateEmails = append(ret.AlternateEmails, ae)
			if i := strings.Index(ae, "@"); i > 0 && strings.ToLower(ae[i+1:]) == strings.ToLower(ctrl.Cluster.Users.PreferDomainForUsername) {
				ret.Username = strings.SplitN(ae[:i], "+", 2)[0]
			}
		}
	}
	return &ret, nil
}

func loginError(sendError error) (resp arvados.LoginResponse, err error) {
	tmpl, err := template.New("error").Parse(`<h2>Login error:</h2><p>{{.}}</p>`)
	if err != nil {
		return
	}
	err = tmpl.Execute(&resp.HTML, sendError.Error())
	return
}

func (ctrl *googleLoginController) newOAuth2State(key []byte, remote, returnTo string) oauth2State {
	s := oauth2State{
		Time:     time.Now().Unix(),
		Remote:   remote,
		ReturnTo: returnTo,
	}
	s.HMAC = s.computeHMAC(key)
	return s
}

type oauth2State struct {
	HMAC     []byte // hash of other fields; see computeHMAC()
	Time     int64  // creation time (unix timestamp)
	Remote   string // remote cluster if requesting a salted token, otherwise blank
	ReturnTo string // redirect target
}

func (ctrl *googleLoginController) parseOAuth2State(encoded string) (s oauth2State) {
	// Errors are not checked. If decoding/parsing fails, the
	// token will be rejected by verify().
	decoded, _ := base64.RawURLEncoding.DecodeString(encoded)
	f := strings.Split(string(decoded), "\n")
	if len(f) != 4 {
		return
	}
	fmt.Sscanf(f[0], "%x", &s.HMAC)
	fmt.Sscanf(f[1], "%x", &s.Time)
	fmt.Sscanf(f[2], "%s", &s.Remote)
	fmt.Sscanf(f[3], "%s", &s.ReturnTo)
	return
}

func (s oauth2State) verify(key []byte) bool {
	if delta := time.Now().Unix() - s.Time; delta < 0 || delta > 300 {
		return false
	}
	return hmac.Equal(s.computeHMAC(key), s.HMAC)
}

func (s oauth2State) String() string {
	var buf bytes.Buffer
	enc := base64.NewEncoder(base64.RawURLEncoding, &buf)
	fmt.Fprintf(enc, "%x\n%x\n%s\n%s", s.HMAC, s.Time, s.Remote, s.ReturnTo)
	enc.Close()
	return buf.String()
}

func (s oauth2State) computeHMAC(key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%x %s %s", s.Time, s.Remote, s.ReturnTo)
	return mac.Sum(nil)
}
