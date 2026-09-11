package store

import (
	"context"
	"github.com/tenapato/kasane-mcp/internal/core"
)

type UsageTotals struct {
	Retrievals     int64 `json:"retrievals"`
	BaselineTokens int64 `json:"baseline_tokens"`
	ReturnedTokens int64 `json:"returned_tokens"`
	SavedTokens    int64 `json:"saved_tokens"`
}
type UsageDay struct {
	Date string `json:"date"`
	UsageTotals
}
type UsageReport struct {
	Days   int         `json:"days"`
	Totals UsageTotals `json:"totals"`
	Daily  []UsageDay  `json:"daily"`
}

// RecordContextUsage stores aggregate estimates only, never memory text or queries.
func (s *Store) RecordContextUsage(ctx context.Context, ws string, fullCharacters, returnedCharacters int64) error {
	if !validUUID(ws) || fullCharacters < 0 || returnedCharacters < 0 || returnedCharacters > fullCharacters {
		return core.ErrInvalid
	}
	baseline, returned := (fullCharacters+3)/4, (returnedCharacters+3)/4
	_, err := s.DB.Exec(ctx, `INSERT INTO workspace_usage_daily(workspace_id,day,retrievals,baseline_tokens,returned_tokens,saved_tokens)
 VALUES($1::uuid,(now() AT TIME ZONE 'UTC')::date,1,$2,$3,$4)
 ON CONFLICT(workspace_id,day) DO UPDATE SET retrievals=workspace_usage_daily.retrievals+1,
 baseline_tokens=workspace_usage_daily.baseline_tokens+EXCLUDED.baseline_tokens,
 returned_tokens=workspace_usage_daily.returned_tokens+EXCLUDED.returned_tokens,
 saved_tokens=workspace_usage_daily.saved_tokens+EXCLUDED.saved_tokens`, ws, baseline, returned, baseline-returned)
	return err
}
func (s *Store) Usage(ctx context.Context, ws string) (UsageReport, error) {
	out := UsageReport{Days: 30, Daily: []UsageDay{}}
	rows, err := s.DB.Query(ctx, `SELECT to_char(d.day,'YYYY-MM-DD'),COALESCE(u.retrievals,0),COALESCE(u.baseline_tokens,0),COALESCE(u.returned_tokens,0),COALESCE(u.saved_tokens,0)
 FROM generate_series((now() AT TIME ZONE 'UTC')::date-29,(now() AT TIME ZONE 'UTC')::date,interval '1 day') AS d(day)
 LEFT JOIN workspace_usage_daily u ON u.day=d.day::date AND u.workspace_id=$1::uuid ORDER BY d.day`, ws)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var day UsageDay
		if err = rows.Scan(&day.Date, &day.Retrievals, &day.BaselineTokens, &day.ReturnedTokens, &day.SavedTokens); err != nil {
			return out, err
		}
		out.Daily = append(out.Daily, day)
		out.Totals.Retrievals += day.Retrievals
		out.Totals.BaselineTokens += day.BaselineTokens
		out.Totals.ReturnedTokens += day.ReturnedTokens
		out.Totals.SavedTokens += day.SavedTokens
	}
	return out, rows.Err()
}
