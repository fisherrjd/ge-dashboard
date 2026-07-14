// Package orch is the typed client for the ge-orchestrator API — the
// dashboard's only data source (it holds no DB credentials).
package orch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func New(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("orchestrator %s: %d: %s", path, resp.StatusCode, truncate(body))
	}
	return json.Unmarshal(body, out)
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}

// Shapes mirror the orchestrator API responses (fields the UI needs).

type Health struct {
	OK          bool  `json:"ok"`
	ActiveRunID int64 `json:"active_run_id"`
}

type Run struct {
	RunID      int64      `json:"run_id"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Status     string     `json:"status"`
	BriefText  string     `json:"brief_text"`
	FailReason *string    `json:"fail_reason"`
	NStrats    int        `json:"n_strategies"`
}

type Item struct {
	Name     string `json:"name"`
	ID       int    `json:"id"`
	BuyLimit *int64 `json:"buy_limit"`
	Members  *bool  `json:"members"`
}

type Evaluation struct {
	At              time.Time       `json:"at"`
	CurHigh         *int64          `json:"cur_high"`
	CurLow          *int64          `json:"cur_low"`
	HighAgeS        *int            `json:"high_age_s"`
	LowAgeS         *int            `json:"low_age_s"`
	CurMargin       *int64          `json:"cur_margin"`
	Vol30m          *int64          `json:"vol_30m"`
	RealizedPer1hGp *int64          `json:"realized_per_1h_gp"`
	Checks          map[string]bool `json:"checks"`
	Verdict         string          `json:"verdict"`
}

type Strategy struct {
	StrategyID    int64           `json:"strategy_id"`
	RunID         int64           `json:"run_id"`
	Sid           string          `json:"sid"`
	Archetype     string          `json:"archetype"`
	Title         string          `json:"title"`
	Thesis        string          `json:"thesis"`
	Items         json.RawMessage `json:"items"`
	PrimaryItemID int             `json:"primary_item_id"`
	EntryText     string          `json:"entry"`
	ExitText      string          `json:"exit"`
	EntryPrice    int64           `json:"entry_price"`
	ExitPrice     int64           `json:"exit_price"`
	KillPrice     *int64          `json:"kill_price"`
	Capital       *int64          `json:"capital_required"`
	UnitsUsed     *int64          `json:"units_used"`
	Per1hGp       *int64          `json:"per_1h_gp"`
	PerDayGp      *int64          `json:"per_day_gp"`
	RoiPct        *float64        `json:"roi_pct"`
	Confidence    string          `json:"confidence"`
	Invalidation  string          `json:"invalidation"`
	State         string          `json:"state"`
	StateReason   *string         `json:"state_reason"`
	OpenedAt      time.Time       `json:"opened_at"`

	LiveChecks  map[string]bool `json:"live_checks"`
	LiveVerdict string          `json:"live_verdict"`
	Live        *Evaluation     `json:"live"`
}

// Pointer views of non-nullable prices so templates can share the gp helper.
func (s Strategy) EntryPriceP() *int64 { return &s.EntryPrice }
func (s Strategy) ExitPriceP() *int64  { return &s.ExitPrice }

func (s Strategy) ItemList() []Item {
	var items []Item
	json.Unmarshal(s.Items, &items)
	return items
}

type ScoreboardRow struct {
	Archetype           string   `json:"archetype"`
	N                   int      `json:"n"`
	Confirmed           int      `json:"confirmed"`
	Killed              int      `json:"killed"`
	Expired             int      `json:"expired"`
	Open                int      `json:"open"`
	RealizedVsProjected *float64 `json:"realized_vs_projected"`
}

func (c *Client) Health(ctx context.Context) (*Health, error) {
	var h Health
	return &h, c.get(ctx, "/api/health", &h)
}

func (c *Client) Runs(ctx context.Context) ([]Run, error) {
	var runs []Run
	return runs, c.get(ctx, "/api/runs?limit=50", &runs)
}

func (c *Client) Run(ctx context.Context, id int64) (*Run, []Strategy, error) {
	var out struct {
		Run        Run        `json:"run"`
		Strategies []Strategy `json:"strategies"`
	}
	err := c.get(ctx, fmt.Sprintf("/api/runs/%d", id), &out)
	return &out.Run, out.Strategies, err
}

func (c *Client) Report(ctx context.Context, id int64) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/runs/%d/report", c.BaseURL, id), nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func (c *Client) LatestStrategiesLive(ctx context.Context) ([]Strategy, error) {
	var out []Strategy
	return out, c.get(ctx, "/api/strategies?scope=latest_run&live=1", &out)
}

func (c *Client) Strategy(ctx context.Context, id int64) (*Strategy, []Evaluation, error) {
	var out struct {
		Strategy   Strategy     `json:"strategy"`
		Evaluations []Evaluation `json:"evaluations"`
	}
	err := c.get(ctx, fmt.Sprintf("/api/strategies/%d", id), &out)
	return &out.Strategy, out.Evaluations, err
}

func (c *Client) Scoreboard(ctx context.Context) ([]ScoreboardRow, error) {
	var out []ScoreboardRow
	return out, c.get(ctx, "/api/scoreboard", &out)
}

func (c *Client) BriefPreview(ctx context.Context, params string) (string, error) {
	var out struct {
		BriefText string `json:"brief_text"`
	}
	err := c.get(ctx, "/api/brief/preview?params="+params, &out)
	return out.BriefText, err
}
