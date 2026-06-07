package driveapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	drive "google.golang.org/api/drive/v3"
)

type Credentials struct {
	Installed *ClientCredentials `json:"installed"`
	Web       *ClientCredentials `json:"web"`
}

type ClientCredentials struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	RedirectURIs []string `json:"redirect_uris"`
}

func LoadOAuthConfig(credentialsPath string) (*oauth2.Config, error) {
	data, err := os.ReadFile(credentialsPath)
	if err != nil {
		return nil, err
	}
	cfg, err := google.ConfigFromJSON(data, drive.DriveScope, drive.DriveMetadataReadonlyScope)
	if err != nil {
		return nil, err
	}
	cfg.RedirectURL = "http://localhost:8080/callback"
	return cfg, nil
}

func RunAddAccountFlow(ctx context.Context, oauthConfig *oauth2.Config) (*oauth2.Token, error) {
	listener, err := net.Listen("tcp", "localhost:8080")
	if err != nil {
		return nil, fmt.Errorf("start OAuth callback server: %w", err)
	}
	defer listener.Close()

	state := fmt.Sprintf("omnidrive-%d", time.Now().UnixNano())
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	server := &http.Server{}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("state"); got != state {
			errCh <- fmt.Errorf("invalid OAuth state")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("OAuth callback did not include code")
			return
		}
		fmt.Fprintln(w, "OmniDrive login complete. You can close this browser tab.")
		codeCh <- code
	})

	go func() { _ = server.Serve(listener) }()
	defer server.Shutdown(context.Background())

	authURL := oauthConfig.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	_ = openBrowser(authURL)

	select {
	case code := <-codeCh:
		return oauthConfig.Exchange(ctx, code)
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func EmailFromIDToken(token *oauth2.Token) string {
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return ""
	}
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Email
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
