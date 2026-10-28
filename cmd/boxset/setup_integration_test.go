//go:build integration && unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
)

func TestSetupTokenRequiresTTY(t *testing.T) {
	dir := t.TempDir()
	const sentinel = "synthetic-setup-token-must-stay-private"
	if err := os.WriteFile(filepath.Join(dir, auth.TokenFileName), []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "setup-token")
	cmd.Env = []string{"BOXSET_DATA_DIR=" + dir}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("non-terminal setup-token exit: %v", err)
	}
	if !bytes.Contains(out, []byte("interactive terminal")) || bytes.Contains(out, []byte(sentinel)) {
		t.Fatal("non-terminal output must explain the refusal without the token")
	}
}

func TestSetupTokenPTY(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	s.waitReady(t)
	token, err := os.ReadFile(filepath.Join(dir, auth.TokenFileName))
	if err != nil {
		t.Fatal("read synthetic setup credential")
	}
	defer clear(token)
	utility, err := exec.LookPath("script")
	if err != nil {
		t.Fatal("the dev image's script utility is required for positive TTY proof")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, utility, "-q", "-e", "-c", binary+" setup-token", "/dev/null")
	cmd.Env = []string{"BOXSET_DATA_DIR=" + dir, "PATH=/usr/bin:/bin", "SHELL=/bin/sh", "TERM=dumb"}
	out, err := cmd.CombinedOutput()
	defer clear(out)
	if err != nil || !bytes.Equal(bytes.TrimSpace(out), token) {
		t.Fatalf("TTY setup-token proof failed: exit=%v output_bytes=%d", err, len(out))
	}
	t.Log("PTY-SETUP-TOKEN-PASS stdin=true stdout=true credential_not_logged=true")
}

func TestSetupOverHTTP(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	s.waitReady(t)
	tokenPath := filepath.Join(dir, auth.TokenFileName)
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(tokenPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file permissions: %v", err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	post := func(bearer string, want int, wantCode string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+s.port+"/api/v1/setup",
			strings.NewReader(`{"username":"e\u0301","password":"synthetic-long-password"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want || resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("setup status=%d, want %d with no-referrer", resp.StatusCode, want)
		}
		if bytes.Contains(body, token) || bytes.Contains(body, []byte("synthetic-long-password")) {
			t.Fatal("setup response disclosed a credential")
		}
		if bytes.Contains(body, []byte("username_key")) || bytes.Contains(body, []byte("usernameKey")) {
			t.Fatal("setup response disclosed the private username key")
		}
		if wantCode != "" {
			var problem struct{ Code string }
			if err := json.Unmarshal(body, &problem); err != nil || problem.Code != wantCode {
				t.Fatalf("problem code=%q, want %q: %v", problem.Code, wantCode, err)
			}
		} else {
			var created struct {
				User struct{ Username, Role string }
			}
			if err := json.Unmarshal(body, &created); err != nil || created.User.Username != "é" || created.User.Role != "admin" {
				t.Fatalf("created admin: %+v, %v", created, err)
			}
		}
	}
	post("wrong", 401, "setup_credential_invalid")
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+s.port+"/api/v1/setup",
		strings.NewReader(`{"username":"invalid\u202eusername","password":"synthetic-long-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil || resp.StatusCode != 422 || bytes.Contains(body, []byte("invalid"+"\u202e"+"username")) || bytes.Contains(body, []byte(`"value"`)) {
		t.Fatalf("invalid username response status=%d read error=%v", resp.StatusCode, readErr)
	}
	post(string(token), 201, "")
	post(string(token), 409, "setup_completed")
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if got := s.exited(t); got != 0 {
		t.Fatalf("shutdown=%d", got)
	}
	if strings.Contains(s.stderr.String(), string(token)) || strings.Contains(s.stderr.String(), "synthetic-long-password") {
		t.Fatal("setup log disclosed a credential")
	}
	if !strings.Contains(s.stderr.String(), "setup-token") {
		t.Fatal("setup log lacks the local token retrieval command")
	}
}
