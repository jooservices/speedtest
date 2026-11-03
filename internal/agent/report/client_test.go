package report

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("broken read") }
func (brokenBody) Close() error             { return nil }

func TestClientHTTPStatusesAndEnvelopes(t *testing.T) {
	cases := []struct {
		status int
		body   string
		retry  bool
	}{
		{401, `secret`, false}, {403, "", false}, {400, "", false}, {413, "", false}, {422, "", false}, {429, "", true}, {503, "", true}, {302, "", false}, {200, "", false},
		{207, `{`, true}, {207, `{"success":true,"code":207,"data":{}}`, true},
		{207, `{"success":false,"code":207,"data":[]}`, true},
		{207, `{"success":true,"code":207,"data":null}`, true},
		{207, strings.Repeat("x", maxResponseBody+1), true},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status)+tc.body[:min(len(tc.body), 10)], func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := Client{BaseURL: mustURL(t, srv.URL)}
			_, err := c.SendBatch(context.Background(), []Result{result(t)})
			if err == nil || Retryable(err) != tc.retry || strings.Contains(err.Error(), "secret") {
				t.Fatal(err)
			}
		})
	}
}

func TestClientConstructionTransportAndReadFailures(t *testing.T) {
	c := Client{}
	if _, err := c.SendBatch(context.Background(), []Result{result(t)}); err == nil {
		t.Fatal("missing URL")
	}
	c.BaseURL = mustURL(t, "http://localhost")
	if _, err := c.SendBatch(context.Background(), nil); err == nil {
		t.Fatal("empty batch")
	}
	r := result(t)
	r.Raw = []byte("bad")
	if _, err := c.SendBatch(context.Background(), []Result{r}); err == nil {
		t.Fatal("encoding")
	}
	c.HTTP = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("transport secret") })}
	_, err := c.SendBatch(context.Background(), []Result{result(t)})
	if !Retryable(err) || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
	c.HTTP = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 207, Body: brokenBody{}, Header: make(http.Header)}, nil
	})}
	if _, err := c.SendBatch(context.Background(), []Result{result(t)}); !Retryable(err) {
		t.Fatal(err)
	}
	c.BaseURL = mustURL(t, "http://localhost")
	c.BaseURL.Host = "bad\nheader"
	if _, err := c.SendBatch(context.Background(), []Result{result(t)}); err == nil {
		t.Fatal("invalid host")
	}
	if Retryable(errors.New("plain")) {
		t.Fatal("plain error retryable")
	}
	se := &SendError{Status: 401, Err: io.EOF}
	if !errors.Is(se, io.EOF) || !strings.Contains(se.Error(), "401") {
		t.Fatal(se)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c := Client{BaseURL: mustURL(t, source.URL), Credential: "fixture"}
	if _, err := c.SendBatch(context.Background(), []Result{result(t)}); err == nil {
		t.Fatal("redirect followed")
	}
	if called {
		t.Fatal("credential forwarded")
	}
}
