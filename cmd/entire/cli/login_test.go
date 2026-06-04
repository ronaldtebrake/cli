package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
)

// mockClient implements deviceAuthClient for unit tests.
type mockClient struct {
	responses []pollResponse
	calls     int
}

type pollResponse struct {
	result *auth.DeviceAuthPoll
	err    error
}

func (m *mockClient) StartDeviceAuth(_ context.Context) (*auth.DeviceAuthStart, error) {
	return nil, errors.New("not implemented in mock")
}

func (m *mockClient) BaseURL() string {
	return "http://test"
}

func (m *mockClient) PollDeviceAuth(_ context.Context, _ string) (*auth.DeviceAuthPoll, error) {
	if m.calls >= len(m.responses) {
		return nil, errors.New("unexpected poll call")
	}
	r := m.responses[m.calls]
	m.calls++
	return r.result, r.err
}

func TestWaitForApproval_ImmediateSuccess(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{AccessToken: "tok-123"}},
	}}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-123" {
		t.Fatalf("token = %q, want %q", token, "tok-123")
	}
	if poller.calls != 1 {
		t.Fatalf("calls = %d, want 1", poller.calls)
	}
}

func TestWaitForApproval_PendingThenSuccess(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{Error: "authorization_pending"}},
		{result: &auth.DeviceAuthPoll{Error: "authorization_pending"}},
		{result: &auth.DeviceAuthPoll{AccessToken: "tok-456"}},
	}}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-456" {
		t.Fatalf("token = %q, want %q", token, "tok-456")
	}
	if poller.calls != 3 {
		t.Fatalf("calls = %d, want 3", poller.calls)
	}
}

func TestWaitForApproval_AccessDenied(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{Error: "access_denied"}},
	}}

	_, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "device authorization denied") {
		t.Fatalf("err = %v, want 'device authorization denied'", err)
	}
}

func TestWaitForApproval_ExpiredToken(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{Error: "expired_token"}},
	}}

	_, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "device authorization expired") {
		t.Fatalf("err = %v, want 'device authorization expired'", err)
	}
}

func TestWaitForApproval_UnknownError(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{Error: "server_error"}},
	}}

	_, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "server_error") {
		t.Fatalf("err = %v, want to contain 'server_error'", err)
	}
}

func TestWaitForApproval_EmptyTokenOnSuccess(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{AccessToken: ""}},
	}}

	_, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "completed without a token") {
		t.Fatalf("err = %v, want 'completed without a token'", err)
	}
}

func TestWaitForApproval_SlowDown(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{Error: "slow_down"}},
		{result: &auth.DeviceAuthPoll{AccessToken: "tok-slow"}},
	}}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-slow" {
		t.Fatalf("token = %q, want %q", token, "tok-slow")
	}
}

func TestWaitForApproval_ExpiresInClamped(t *testing.T) {
	t.Parallel()

	// expiresIn=0 should use maxExpiresIn, not panic or return immediately.
	// We verify by checking the function still polls (doesn't error on first call).
	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{AccessToken: "tok-clamp"}},
	}}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", 0, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-clamp" {
		t.Fatalf("token = %q, want %q", token, "tok-clamp")
	}
}

func TestWaitForApproval_NegativeExpiresInClamped(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{AccessToken: "tok-neg"}},
	}}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", -1, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-neg" {
		t.Fatalf("token = %q, want %q", token, "tok-neg")
	}
}

func TestWaitForApproval_TransientErrorRetry(t *testing.T) {
	t.Parallel()

	poller := &mockClient{responses: []pollResponse{
		{err: errors.New("connection refused")},
		{err: errors.New("timeout")},
		{result: &auth.DeviceAuthPoll{AccessToken: "tok-retry"}},
	}}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-retry" {
		t.Fatalf("token = %q, want %q", token, "tok-retry")
	}
	if poller.calls != 3 {
		t.Fatalf("calls = %d, want 3", poller.calls)
	}
}

func TestWaitForApproval_TransientErrorExhausted(t *testing.T) {
	t.Parallel()

	var responses []pollResponse
	for range maxTransientErrors + 1 {
		responses = append(responses, pollResponse{err: errors.New("server error")})
	}
	poller := &mockClient{responses: responses}

	_, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "consecutive failures") {
		t.Fatalf("err = %v, want 'consecutive failures'", err)
	}
	if poller.calls != maxTransientErrors {
		t.Fatalf("calls = %d, want %d", poller.calls, maxTransientErrors)
	}
}

func TestWaitForApproval_TransientErrorCounterResets(t *testing.T) {
	t.Parallel()

	// 4 transient errors, then a pending response (resets counter), then 4 more, then success.
	var responses []pollResponse
	for range maxTransientErrors - 1 {
		responses = append(responses, pollResponse{err: errors.New("blip")})
	}
	responses = append(responses, pollResponse{result: &auth.DeviceAuthPoll{Error: "authorization_pending"}})
	for range maxTransientErrors - 1 {
		responses = append(responses, pollResponse{err: errors.New("blip")})
	}
	responses = append(responses, pollResponse{result: &auth.DeviceAuthPoll{AccessToken: "tok-reset"}})
	poller := &mockClient{responses: responses}

	token, _, err := waitForApproval(context.Background(), poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "tok-reset" {
		t.Fatalf("token = %q, want %q", token, "tok-reset")
	}
}

// TestChooseApprovalURL locks in that the CLI opens the URI with the
// user_code embedded (RFC 8628 §3.3.1) when the AS supplies one, falling
// back to the bare verification_uri otherwise. Most AS verification pages
// prefill the code input from the query param in the complete form; without
// this, the user has to type the code by hand even when the AS provided a
// click-through URL.
func TestChooseApprovalURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		start *auth.DeviceAuthStart
		want  string
	}{
		{
			name: "prefers complete URI when supplied",
			start: &auth.DeviceAuthStart{
				VerificationURI:         "http://test/cli/auth",
				VerificationURIComplete: "http://test/cli/auth?user_code=ABCD-1234",
			},
			want: "http://test/cli/auth?user_code=ABCD-1234",
		},
		{
			name: "falls back to bare verification_uri",
			start: &auth.DeviceAuthStart{
				VerificationURI: "http://test/cli/auth",
			},
			want: "http://test/cli/auth",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := chooseApprovalURL(tc.start); got != tc.want {
				t.Errorf("chooseApprovalURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWaitForApproval_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	poller := &mockClient{responses: []pollResponse{
		{result: &auth.DeviceAuthPoll{Error: "authorization_pending"}},
	}}

	_, _, err := waitForApproval(ctx, poller, "device-1", 60, time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err = %v, want context canceled", err)
	}
}

// fakeBrowserFlow implements auth.BrowserAuthFlow for unit tests.
type fakeBrowserFlow struct {
	authURL     string
	waitCode    string
	waitErr     error
	exchAccess  string
	exchRefresh string
	exchErr     error

	gotExchangeCode string
	closed          bool
}

func (f *fakeBrowserFlow) AuthorizationURL() string { return f.authURL }

func (f *fakeBrowserFlow) Wait(context.Context) (string, error) { return f.waitCode, f.waitErr }

func (f *fakeBrowserFlow) Exchange(_ context.Context, code string) (string, string, error) {
	f.gotExchangeCode = code
	return f.exchAccess, f.exchRefresh, f.exchErr
}

func (f *fakeBrowserFlow) Close() error {
	f.closed = true
	return nil
}

// fakeBrowserClient implements browserAuthClient for unit tests.
type fakeBrowserClient struct {
	flow     *fakeBrowserFlow
	startErr error
}

//nolint:ireturn // mirrors the browserAuthClient interface, which returns the auth.BrowserAuthFlow interface by design.
func (c *fakeBrowserClient) StartBrowserAuth(context.Context) (auth.BrowserAuthFlow, error) {
	if c.startErr != nil {
		return nil, c.startErr
	}
	return c.flow, nil
}

func (c *fakeBrowserClient) BaseURL() string { return "http://test" }

func TestShouldUseBrowserLogin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		useDevice bool
		canPrompt bool
		want      bool
	}{
		{useDevice: false, canPrompt: true, want: true},   // default interactive → browser
		{useDevice: false, canPrompt: false, want: false}, // headless → fall back to device
		{useDevice: true, canPrompt: true, want: false},   // --device forces device
		{useDevice: true, canPrompt: false, want: false},
	}
	for _, tc := range cases {
		if got := shouldUseBrowserLogin(tc.useDevice, tc.canPrompt); got != tc.want {
			t.Errorf("shouldUseBrowserLogin(%v, %v) = %v, want %v", tc.useDevice, tc.canPrompt, got, tc.want)
		}
	}
}

func TestRunBrowserLogin_StartError(t *testing.T) {
	t.Parallel()

	client := &fakeBrowserClient{startErr: errors.New("bind failed")}
	noopOpen := func(context.Context, string) error { return nil }

	err := runBrowserLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, client, noopOpen)
	if err == nil || !strings.Contains(err.Error(), "start login") {
		t.Fatalf("err = %v, want start login error", err)
	}
}

func TestRunBrowserLogin_OpensAuthorizationURL(t *testing.T) {
	t.Parallel()

	flow := &fakeBrowserFlow{authURL: "https://auth.test/authorize?x=1", waitErr: errors.New("stop")}
	client := &fakeBrowserClient{flow: flow}

	var openedURL string
	openURL := func(_ context.Context, u string) error {
		openedURL = u
		return nil
	}

	var out bytes.Buffer
	// The stubbed Wait returns an error, so runBrowserLogin stops before
	// persistLogin (which would hit the real keyring); we assert on the
	// side effects up to that point.
	if err := runBrowserLogin(context.Background(), &out, &bytes.Buffer{}, client, openURL); err == nil {
		t.Fatal("expected error from stubbed Wait")
	}

	if openedURL != flow.authURL {
		t.Errorf("opened URL = %q, want %q", openedURL, flow.authURL)
	}
	// Happy path shows the auth host, not the full authorize URL, and
	// doesn't print the URL at all (the browser opened fine).
	if !strings.Contains(out.String(), "Logging in to:") {
		t.Errorf("output missing 'Logging in to:' line:\n%s", out.String())
	}
	if strings.Contains(out.String(), flow.authURL) {
		t.Errorf("happy path should not print the full authorize URL:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Press Enter to open in browser...") {
		t.Errorf("output missing enter-to-open prompt:\n%s", out.String())
	}
	if !flow.closed {
		t.Error("flow was not closed")
	}
}

func TestRunBrowserLogin_OpenBrowserFallback(t *testing.T) {
	t.Parallel()

	flow := &fakeBrowserFlow{authURL: "https://auth.test/authorize", waitErr: errors.New("stop")}
	client := &fakeBrowserClient{flow: flow}
	failOpen := func(context.Context, string) error { return errors.New("no browser") }

	var out, errW bytes.Buffer
	if err := runBrowserLogin(context.Background(), &out, &errW, client, failOpen); err == nil {
		t.Fatal("expected error from stubbed Wait")
	}

	if !strings.Contains(errW.String(), "failed to open browser") {
		t.Errorf("stderr missing warning:\n%s", errW.String())
	}
	if !strings.Contains(out.String(), flow.authURL) {
		t.Errorf("stdout missing fallback URL:\n%s", out.String())
	}
}

func TestRunBrowserLogin_WaitError(t *testing.T) {
	t.Parallel()

	denied := errors.New("access_denied")
	flow := &fakeBrowserFlow{authURL: "https://auth.test/authorize", waitErr: denied}
	client := &fakeBrowserClient{flow: flow}
	noopOpen := func(context.Context, string) error { return nil }

	err := runBrowserLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, client, noopOpen)
	if !errors.Is(err, denied) {
		t.Fatalf("err = %v, want wrapped %v", err, denied)
	}
}

func TestRunBrowserLogin_ExchangeError(t *testing.T) {
	t.Parallel()

	flow := &fakeBrowserFlow{
		authURL:  "https://auth.test/authorize",
		waitCode: "the-code",
		exchErr:  errors.New("invalid_grant"),
	}
	client := &fakeBrowserClient{flow: flow}
	noopOpen := func(context.Context, string) error { return nil }

	err := runBrowserLogin(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, client, noopOpen)
	if err == nil || !strings.Contains(err.Error(), "complete login") {
		t.Fatalf("err = %v, want complete login error", err)
	}
	if flow.gotExchangeCode != "the-code" {
		t.Errorf("Exchange got code %q, want the-code", flow.gotExchangeCode)
	}
}
