package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"safuno/internal/config"
	wbp "safuno/internal/provider"
)

const resourcePath = "/v2/billing/meter/get-user-resource"

// Quota reports the account's credit packages via the billing resource API.
// Credits are floating-point balances, distinct from token counts, so
// RemainingTokens stays nil and each package becomes a quota bucket.
func (w WorkBuddy) Quota(ctx context.Context, client *http.Client, a config.Account) (wbp.Quota, error) {
	rc := realmOf(a)
	// A custom account origin (tests, mirrored deployments) is trusted for
	// billing too; otherwise the realm's billing upstream applies (CN billing
	// lives on www.codebuddy.cn while chat lives on copilot.tencent.com).
	billing := rc.billingUpstream
	if a.BaseURL != "" && a.BaseURL != rc.chatUpstream {
		billing = a.BaseURL
	}
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": time.Now().Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   "2036-01-01 00:00:00",
	})
	b, status, err := doJSON(ctx, client, http.MethodPost, billing+resourcePath, billingHeaders(a), body, 1<<20)
	if err != nil {
		return wbp.Quota{}, err
	}
	if status != 200 {
		return wbp.Quota{}, &wbp.HTTPError{Status: status, RetryAfter: ""}
	}
	var data struct {
		Data struct {
			Response struct {
				Data struct {
					Accounts []struct {
						PackageName string `json:"PackageName"`
						PackageCode string `json:"PackageCode"`
						// Prefer the precise cycle fields, fall back to the
						// coarse ones, exactly like the reference gateway.
						CycleCapacityRemainPrecise json.RawMessage `json:"CycleCapacityRemainPrecise"`
						CycleCapacityRemain        json.RawMessage `json:"CycleCapacityRemain"`
						CapacityRemainPrecise      json.RawMessage `json:"CapacityRemainPrecise"`
						CapacityRemain             json.RawMessage `json:"CapacityRemain"`
						CycleCapacitySizePrecise   json.RawMessage `json:"CycleCapacitySizePrecise"`
						CycleCapacitySize          json.RawMessage `json:"CycleCapacitySize"`
						CapacitySizePrecise        json.RawMessage `json:"CapacitySizePrecise"`
						CapacitySize               json.RawMessage `json:"CapacitySize"`
					} `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &data) != nil {
		return wbp.Quota{}, errors.New("invalid billing resource response")
	}
	now := time.Now().UTC()
	q := wbp.Quota{
		Source:     "workbuddy-credits",
		ObservedAt: now,
		ValidUntil: now.Add(10 * time.Minute),
	}
	for _, acc := range data.Data.Response.Data.Accounts {
		remain := firstNumber(acc.CycleCapacityRemainPrecise, acc.CycleCapacityRemain, acc.CapacityRemainPrecise, acc.CapacityRemain)
		size := firstNumber(acc.CycleCapacitySizePrecise, acc.CycleCapacitySize, acc.CapacitySizePrecise, acc.CapacitySize)
		name := acc.PackageName
		if name == "" {
			name = acc.PackageCode
		}
		if name == "" {
			name = "credits"
		}
		q.Buckets = append(q.Buckets, wbp.Bucket{
			Name:      name,
			Pool:      "credits",
			Unit:      "credits",
			Remaining: remain,
			Limit:     size,
		})
	}
	return q, nil
}

// firstNumber picks the first parseable non-zero value from a field set.
func firstNumber(fields ...json.RawMessage) float64 {
	for _, f := range fields {
		if len(f) == 0 || string(f) == "null" {
			continue
		}
		var v float64
		if json.Unmarshal(f, &v) == nil {
			return v
		}
	}
	return 0
}
