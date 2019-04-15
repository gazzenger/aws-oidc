package provider

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"os/exec"
	"strings"

	oidc "github.com/coreos/go-oidc"

	"golang.org/x/net/context"
	"golang.org/x/oauth2"
)

type ProviderConfig struct {
	ClientID     string
	ClientSecret string
	ProviderURL  string
	PKCE         bool
	Nonce        bool
	ReAuth       bool
	AgentCommand []string
}

type Result struct {
	JWT   string
	Token *oidc.IDToken
}

type TokenClaims struct {
	Issuer        string   `json:"iss"`
	Audience      string   `json:"aud"`
	Subject       string   `json:"sub"`
	Picture       string   `json:"picture"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Groups        []string `json:"groups"`
}

func Authenticate(p *ProviderConfig) (Result, error) {
	ctx := context.Background()
	resultChannel := make(chan Result, 0)
	errorChannel := make(chan error, 0)

	provider, err := oidc.NewProvider(ctx, p.ProviderURL)
	if err != nil {
		return Result{"", nil}, err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Result{"", nil}, err
	}
	baseURL := "http://" + listener.Addr().String()
	redirectURL := baseURL + "/auth/callback"

	oidcConfig := &oidc.Config{
		ClientID:             p.ClientID,
		SupportedSigningAlgs: []string{"RS256"},
	}
	verifier := provider.Verifier(oidcConfig)

	config := oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email", "groups"},
	}

	stateData := make([]byte, 32)
	if _, err = rand.Read(stateData); err != nil {
		return Result{"", nil}, err
	}
	state := base64.URLEncoding.EncodeToString(stateData)

	codeData := make([]byte, 32)
	if _, err = rand.Read(codeData); err != nil {
		return Result{"", nil}, err
	}
	codeVerifier := base64.StdEncoding.EncodeToString(codeData)
	codeDigest := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.URLEncoding.EncodeToString(codeDigest[:])
	codeChallengeEncoded := strings.Replace(codeChallenge, "=", "", -1)

	nonceData := make([]byte, 32)
	_, err = rand.Read(nonceData)
	nonce := base64.URLEncoding.EncodeToString(nonceData)

	authCodeOptions := []oauth2.AuthCodeOption{}
	tokenCodeOptions := []oauth2.AuthCodeOption{}

	if p.PKCE {
		authCodeOptions = append(authCodeOptions,
			oauth2.SetAuthURLParam("code_challenge", codeChallengeEncoded),
			oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		)
		tokenCodeOptions = append(tokenCodeOptions,
			oauth2.SetAuthURLParam("code_verifier", codeVerifier),
		)
	}

	if p.Nonce {
		authCodeOptions = append(authCodeOptions, oauth2.SetAuthURLParam("nonce", nonce))
	}

	if p.ReAuth {
		authCodeOptions = append(authCodeOptions, oauth2.SetAuthURLParam("acr_values", "onelogin:nist:level:1:re-auth"))
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		url := config.AuthCodeURL(state, authCodeOptions...)
		http.Redirect(w, r, url, http.StatusFound)
	})

	http.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state did not match", http.StatusBadRequest)
			errorChannel <- errors.New("State did not match")
			return
		}

		oauth2Token, err := config.Exchange(ctx, r.URL.Query().Get("code"), tokenCodeOptions...)
		if err != nil {
			http.Error(w, "Failed to exchange token: "+err.Error(), http.StatusInternalServerError)
			errorChannel <- errors.New("Failed to exchange token: " + err.Error())
			return
		}
		rawIDToken, ok := oauth2Token.Extra("id_token").(string)
		if !ok {
			http.Error(w, "No id_token field in oauth2 token.", http.StatusInternalServerError)
			errorChannel <- errors.New("No id_token field in oauth2 token")
			return
		}
		idToken, err := verifier.Verify(ctx, rawIDToken)
		if err != nil {
			http.Error(w, "Failed to verify ID Token: "+err.Error(), http.StatusInternalServerError)
			errorChannel <- errors.New("Failed to verify ID Token: " + err.Error())
			return
		}
		if p.Nonce && idToken.Nonce != nonce {
			http.Error(w, "Failed to verify Nonce", http.StatusInternalServerError)
			errorChannel <- errors.New("Failed to verify Nonce")
			return
		}

		var claims = new(TokenClaims)
		if err := idToken.Claims(&claims); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			errorChannel <- errors.New("Failed to verify Claims: " + err.Error())
			return
		}
		w.Write([]byte("Signed in successfully, return to cli app"))
		resultChannel <- Result{rawIDToken, idToken}
	})

	// Filter the commands, and replace "{}" with our callback url
	c := p.AgentCommand[:0]
	for _, arg := range p.AgentCommand {
		if arg == "{}" {
			c = append(c, baseURL)
		} else {
			c = append(c, arg)
		}
	}
	cmd := exec.Command(c[0], c[1:]...)
	cmd.Run()

	server := &http.Server{}
	go func() {
		server.Serve(listener)
	}()

	select {
	case err := <-errorChannel:
		server.Shutdown(ctx)
		return Result{}, err
	case res := <-resultChannel:
		server.Shutdown(ctx)
		return res, nil
	}
}
