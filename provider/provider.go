package provider

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc"

	"golang.org/x/net/context"
	"golang.org/x/oauth2"
)

type ProviderConfig struct {
	ClientID               string
	ClientSecret           string
	ProviderURL            string
	PKCE                   bool
	Nonce                  bool
	AgentCommand           []string
	ProviderReturnHTML     string
	AgentUseDefaultBrowser bool
}

type Result struct {
	JWT    string
	Token  *oidc.IDToken
	Claims *TokenClaims
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

type OAuth2Token struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
}

func refresh(config oauth2.Config, t *OAuth2Token) error {
	ctx := context.Background()

	tokenSourceToken := oauth2.Token{
		AccessToken:  t.AccessToken,
		TokenType:    t.TokenType,
		RefreshToken: t.RefreshToken,
		Expiry:       t.Expiry,
	}
	ts := config.TokenSource(ctx, tokenSourceToken.WithExtra(map[string]interface{}{
		"id_token": t.IDToken,
	}))

	res, err := ts.Token()
	if err != nil {
		return err
	}
	idtoken, ok := res.Extra("id_token").(string)
	if !ok {
		return errors.New("Can't extract id_token")
	}
	t.AccessToken = res.AccessToken
	t.RefreshToken = res.RefreshToken
	t.Expiry = res.Expiry
	t.TokenType = res.TokenType
	t.IDToken = idtoken

	return nil
}

func (p ProviderConfig) Authenticate(t *OAuth2Token) error {
	ctx := context.Background()
	resultChannel := make(chan *oauth2.Token, 0)
	errorChannel := make(chan error, 0)

	provider, err := oidc.NewProvider(ctx, p.ProviderURL)
	if err != nil {
		return err
	}

	//determine a free port to use
	port, err := GetFreePort()
	if err != nil {
		return err
	}

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port) // + listener.Addr().String()
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
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}

	if t != nil {
		if err := refresh(config, t); err == nil {
			return nil
		}
		log.Println(err)
	}

	stateData := make([]byte, 32)
	if _, err = rand.Read(stateData); err != nil {
		return err
	}
	state := base64.URLEncoding.EncodeToString(stateData)

	codeData := make([]byte, 32)
	if _, err = rand.Read(codeData); err != nil {
		return err
	}
	codeVerifier := base64.StdEncoding.EncodeToString(codeData)
	codeDigest := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.URLEncoding.EncodeToString(codeDigest[:])
	codeChallengeEncoded := strings.Replace(codeChallenge, "=", "", -1)

	nonceData := make([]byte, 32)
	_, err = rand.Read(nonceData)
	nonce := base64.URLEncoding.EncodeToString(nonceData)

	var authCodeOptions []oauth2.AuthCodeOption
	var tokenCodeOptions []oauth2.AuthCodeOption

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

	myMux := http.NewServeMux()

	myMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		url := config.AuthCodeURL(state, authCodeOptions...)
		http.Redirect(w, r, url, http.StatusFound)
	})

	myMux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state did not match", http.StatusBadRequest)
			errorChannel <- errors.New("State did not match")
			return
		}
		//detecting an error parameter returned
		if r.URL.Query().Get("error") != "" {
			w.Write([]byte("<h1>Error:" + r.URL.Query().Get("error") +
				"</h1><p>" + r.URL.Query().Get("error_description") +
				"<br/><em>" + r.URL.Query().Get("error_uri") + "</em></p>"))
			errorChannel <- errors.New("Error:" + r.URL.Query().Get("error") + " " + r.URL.Query().Get("error_description") + " " + r.URL.Query().Get("error_uri"))
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

		if p.ProviderReturnHTML != "" {
			w.Write([]byte(p.ProviderReturnHTML))
		} else {
			w.Write([]byte("Signed in successfully, return to cli app"))
		}

		resultChannel <- oauth2Token
	})

	if !p.AgentUseDefaultBrowser {
		// Filter the commands, and replace "{}" with our callback url
		c := p.AgentCommand[:0]
		replacedURL := false
		for _, arg := range p.AgentCommand {
			if arg == "{}" {
				c = append(c, baseURL)
				replacedURL = true
			} else {
				c = append(c, arg)
			}
		}
		if !replacedURL {
			c = append(c, baseURL)
		}

		//TODO Drop privileges
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Start()
	} else {
		openbrowser(baseURL)
	}

	// Server and Listen
	server := &http.Server{
		Addr:    ":" + strconv.Itoa(port),
		Handler: myMux,
	}
	go server.ListenAndServe()

	select {
	case err := <-errorChannel:
		server.Shutdown(ctx)
		return err
	case res := <-resultChannel:
		server.Shutdown(ctx)
		IDToken, ok := res.Extra("id_token").(string)
		if !ok {
			return errors.New("Can't extract id_token")
		}
		t.AccessToken = res.AccessToken
		t.RefreshToken = res.RefreshToken
		t.Expiry = res.Expiry
		t.TokenType = res.TokenType
		t.IDToken = IDToken
		return nil
	case <-time.After(2 * time.Minute):
		server.Shutdown(ctx)
		return errors.New("no oauth2 flow callback received within last 2 minutes, exiting")
	}
}

// A function that can open url in the default browser on Windows, Linux or Mac
// taken from https://gist.github.com/hyg/9c4afcd91fe24316cbf0
func openbrowser(url string) {
	var err error

	switch runtime.GOOS {
	case "linux":
		// check if running WSL
		// taken from https://github.com/microsoft/WSL/issues/423#issuecomment-328526847
		cmd := "uname -r | grep -o Microsoft"
		out, _ := exec.Command("bash", "-c", cmd).Output()
		if string(out) == "Microsoft\n" {
			//if Microsoft is detected, then this is running WSL
			err = exec.Command("wslview", url).Start()
		} else {
			//otherwise standard linux
			err = exec.Command("xdg-open", url).Start()
		}
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("unsupported platform")
	}
	if err != nil {
		log.Fatal(err)
	}

}

// A function to determine any free ports to use
// taken from https://stackoverflow.com/a/48283226
func GetFreePort() (int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	err = ln.Close()
	if err != nil {
		return 0, err
	}
	return ln.Addr().(*net.TCPAddr).Port, nil
}
