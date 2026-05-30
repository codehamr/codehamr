package cloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestReachableHitsV1Models pins the heartbeat path: /v1/models, the
// OpenAI-standard listing endpoint every backend serves. Probing GET /
// instead could block behind a backend's inference loop until timeout.
func TestReachableHitsV1Models(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Reachable(ctx, srv.URL); err != nil {
		t.Fatalf("Reachable returned %v, want nil", err)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("Reachable hit %q, want /v1/models", gotPath)
	}
}

// TestReachableTreatsNon2xxAsReachable: any HTTP response counts as
// reachable (even 404/401); only transport errors and timeouts mean down.
func TestReachableTreatsNon2xxAsReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Reachable(ctx, srv.URL); err != nil {
		t.Fatalf("404 must count as reachable, got %v", err)
	}
}

func TestFromHeadersMissing(t *testing.T) {
	if got := FromHeaders(http.Header{}); got != (BudgetStatus{}) {
		t.Fatalf("missing X-Budget-Remaining must produce zero BudgetStatus, got %+v", got)
	}
}

func TestFromHeadersClampsAndParses(t *testing.T) {
	cases := []struct {
		name string
		val  string
		set  bool
		want float64
	}{
		{"fresh", "1.0", true, 1.0},
		{"mid", "0.73", true, 0.73},
		{"depleted", "0", true, 0.0},
		{"clamped above 1", "1.05", true, 1.0},
		{"clamped below 0", "-0.1", true, 0.0},
		{"garbage", "abc", false, 0.0},
		{"empty", "", false, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.val != "" {
				h.Set("X-Budget-Remaining", tc.val)
			}
			got := FromHeaders(h)
			if got.Set != tc.set {
				t.Fatalf("Set: got %v want %v", got.Set, tc.set)
			}
			if got.Remaining != tc.want {
				t.Fatalf("Remaining: got %v want %v", got.Remaining, tc.want)
			}
		})
	}
}

// TestFromHeadersRejectsNaNAndInf: ParseFloat accepts NaN/±Inf without
// error and the [0,1] clamp is a no-op against NaN, so they'd render as
// int(NaN*100+0.5) → MinInt64. Both must collapse to the missing-header zero.
func TestFromHeadersRejectsNaNAndInf(t *testing.T) {
	cases := []string{"NaN", "+Inf", "-Inf", "Infinity"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			h := http.Header{}
			h.Set("X-Budget-Remaining", raw)
			got := FromHeaders(h)
			if got.Set {
				t.Fatalf("X-Budget-Remaining=%q must be treated as missing, got %+v", raw, got)
			}
		})
	}
}

func TestStatusSuffix(t *testing.T) {
	if got := (BudgetStatus{}).StatusSuffix(); got != "" {
		t.Fatalf("zero value must render empty, got %q", got)
	}
	cases := []struct {
		remaining float64
		want      string
	}{
		{1.0, " · 100% pass"},
		{0.73, " · 73% pass"},
		{0.005, " · 1% pass"},
		{0.0, " · 0% pass"},
	}
	for _, tc := range cases {
		got := BudgetStatus{Set: true, Remaining: tc.remaining}.StatusSuffix()
		if got != tc.want {
			t.Fatalf("Remaining=%v: got %q want %q", tc.remaining, got, tc.want)
		}
	}
}

func TestContextWindowFromHeaders(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want int
	}{
		{"missing", "", 0},
		{"valid", "262144", 262144},
		{"min boundary", "1024", 1024},
		{"too small", "1023", 0},
		{"too large", "9999999999", 0},
		{"negative", "-1", 0},
		{"garbage", "abc", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.val != "" {
				h.Set("X-Context-Window", tc.val)
			}
			if got := ContextWindowFromHeaders(h); got != tc.want {
				t.Fatalf("want %d, got %d", tc.want, got)
			}
		})
	}
}

func TestAuthHeader(t *testing.T) {
	if AuthHeader("hp_abc") != "Bearer hp_abc" {
		t.Fatal("bad auth header")
	}
}
