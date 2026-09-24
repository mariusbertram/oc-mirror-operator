package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/mariusbertram/oc-mirror-operator/pkg/oclog"
)

const (
	// StatusAttempts is how often a status report is sent before the
	// worker gives up on it.
	StatusAttempts = 3
	// StatusRetryDelay is the pause between two status report attempts.
	StatusRetryDelay = 2 * time.Second
	// StatusTimeout bounds one status report request.
	StatusTimeout = 10 * time.Second
	// ShouldMirrorTimeout bounds one /should-mirror request; on timeout the
	// worker mirrors the image anyway.
	ShouldMirrorTimeout = 5 * time.Second
)

// StatusRequest is the body of a worker's POST /status report.
type StatusRequest struct {
	PodName     string `json:"podName"`
	Destination string `json:"destination"`
	Digest      string `json:"digest"`
	Error       string `json:"error,omitempty"`
}

// StatusClient talks to the manager's worker status API.
type StatusClient struct {
	// ManagerURL is the manager's status API base URL. Empty disables
	// reporting, and ShouldMirror always answers true.
	ManagerURL string
	PodName    string
	Token      string
	// RetryDelay is the pause between report attempts.
	RetryDelay time.Duration
}

// StatusClientFromEnv returns a StatusClient configured from MANAGER_URL,
// POD_NAME and WORKER_TOKEN, as set on worker pods by the manager.
func StatusClientFromEnv() *StatusClient {
	return &StatusClient{
		ManagerURL: os.Getenv("MANAGER_URL"),
		PodName:    os.Getenv("POD_NAME"),
		Token:      os.Getenv("WORKER_TOKEN"),
		RetryDelay: StatusRetryDelay,
	}
}

// Report sends one image result to the manager, retrying up to
// StatusAttempts times so transient network blips don't lose it. The
// manager's handler is idempotent, so a duplicate delivery is harmless.
func (s *StatusClient) Report(ctx context.Context, dest, digest, errMsg string) {
	if s == nil || s.ManagerURL == "" || s.PodName == "" {
		return
	}
	body, err := json.Marshal(StatusRequest{
		PodName:     s.PodName,
		Destination: dest,
		Digest:      digest,
		Error:       errMsg,
	})
	if err != nil {
		oclog.Printf("Failed to marshal status request: %v\n", err)
		return
	}

	httpClient := &http.Client{Timeout: StatusTimeout}
	for attempt := 1; attempt <= StatusAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(s.RetryDelay)
		}
		httpReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, s.ManagerURL+"/status", bytes.NewReader(body))
		if reqErr != nil {
			oclog.Printf("Failed to build status request: %v\n", reqErr)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+s.Token)
		resp, doErr := httpClient.Do(httpReq)
		if doErr != nil {
			oclog.Printf("Status callback attempt %d/%d failed: %v\n", attempt, StatusAttempts, doErr)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return
		}
		oclog.Printf("Status callback attempt %d/%d: HTTP %d\n", attempt, StatusAttempts, resp.StatusCode)
	}
	oclog.Printf("Failed to report status to manager after %d attempts for %s\n", StatusAttempts, dest)
}

// ShouldMirror asks the manager whether dest is still required. It returns
// false only on 410 Gone. On any error (manager unreachable, no manager
// configured) it returns true: an unneeded mirror is preferable to skipping
// a still-required image.
func (s *StatusClient) ShouldMirror(ctx context.Context, dest string) bool {
	if s == nil || s.ManagerURL == "" {
		return true
	}
	reqCtx, cancel := context.WithTimeout(ctx, ShouldMirrorTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		s.ManagerURL+"/should-mirror?dest="+url.QueryEscape(dest), nil)
	if err != nil {
		return true
	}
	httpReq.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := (&http.Client{Timeout: ShouldMirrorTimeout}).Do(httpReq)
	if err != nil {
		oclog.Printf("Failed to query /should-mirror for %s: %v (proceeding)\n", dest, err)
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode != http.StatusGone
}
