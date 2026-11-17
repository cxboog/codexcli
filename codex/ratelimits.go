package codex

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Window is one rate-limit window of the subscription.
type Window struct {
	UsedPercent   float64   `json:"used_percent"`
	WindowMinutes int64     `json:"window_minutes,omitempty"`
	ResetsAt      time.Time `json:"resets_at,omitzero"`
}

// Credits is the account's pay-as-you-go credit state.
type Credits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

// RateLimits is the snapshot the backend attaches to model responses.
type RateLimits struct {
	LimitName string `json:"limit_name,omitempty"`
	PlanType  string `json:"plan_type,omitempty"`
	// Primary and Secondary are whatever windows the plan has; check
	// WindowMinutes rather than assuming which is the weekly one.
	Primary   *Window  `json:"primary,omitempty"`
	Secondary *Window  `json:"secondary,omitempty"`
	Credits   *Credits `json:"credits,omitempty"`
}

func parseRateLimitHeaders(h http.Header) *RateLimits {
	// The active limit may have its own header family (x-<id>-primary-...);
	// when it doesn't, the plain x-codex- family describes it.
	prefix := "x-codex-"
	active := h.Get("x-codex-active-limit")
	if p := "x-" + strings.ReplaceAll(active, "_", "-") + "-"; active != "" && h.Get(p+"primary-used-percent") != "" {
		prefix = p
	}
	rl := &RateLimits{LimitName: h.Get(prefix + "limit-name"), PlanType: h.Get("x-codex-plan-type")}
	if rl.LimitName == "" {
		rl.LimitName = active
	}
	rl.Primary = parseWindow(h, prefix+"primary-")
	rl.Secondary = parseWindow(h, prefix+"secondary-")
	if v := h.Get("x-codex-credits-has-credits"); v != "" {
		truthy := func(s string) bool { s = strings.ToLower(s); return s == "true" || s == "1" }
		rl.Credits = &Credits{
			HasCredits: truthy(v),
			Unlimited:  truthy(h.Get("x-codex-credits-unlimited")),
			Balance:    h.Get("x-codex-credits-balance"),
		}
	}
	if rl.Primary == nil && rl.Secondary == nil && rl.Credits == nil {
		return nil
	}
	return rl
}

func parseWindow(h http.Header, prefix string) *Window {
	used := h.Get(prefix + "used-percent")
	if used == "" {
		return nil
	}
	w := &Window{}
	w.UsedPercent, _ = strconv.ParseFloat(used, 64)
	w.WindowMinutes, _ = strconv.ParseInt(h.Get(prefix+"window-minutes"), 10, 64)
	if at, err := strconv.ParseInt(h.Get(prefix+"reset-at"), 10, 64); err == nil && at > 0 {
		w.ResetsAt = time.Unix(at, 0)
	}
	if w.UsedPercent == 0 && w.WindowMinutes == 0 && w.ResetsAt.IsZero() {
		return nil // the server sends zeroed headers for windows the plan lacks
	}
	return w
}
