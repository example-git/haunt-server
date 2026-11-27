package ai

import (
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/example-git/haunt-server/internal/ai/provider"
)

func zone(name string) (*time.Location, error) {
	if len(name) > 128 || name == "" || name == "Local" || strings.Contains(name, "..") {
		return nil, errors.New("provide an IANA timezone such as America/Toronto or UTC")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, errors.New("unknown IANA timezone")
	}
	return loc, nil
}

func runtimeContext(now time.Time, name string) string {
	if now.IsZero() {
		now = time.Now()
	}
	local := "unknown"
	if loc, err := zone(name); err == nil {
		local = name + " " + now.In(loc).Format(time.RFC3339)
	}
	return fmt.Sprintf("Authoritative server time at turn start: %s. Channel timezone and local time: %s. User timezone: unknown. Locale: unknown. Use this clock for relative dates. Never infer the user's location or timezone.\n\n", now.UTC().Format(time.RFC3339), local)
}

func clockTool(call provider.ToolCall) (provider.ToolResult, error) {
	var args struct {
		At      string `json:"at"`
		Zone    string `json:"timezone"`
		Days    int    `json:"calendar_days"`
		Seconds int64  `json:"elapsed_seconds"`
	}
	if !arguments(call, &args) || args.Days < -36525 || args.Days > 36525 || args.Seconds < -3155760000 || args.Seconds > 3155760000 {
		return toolFailure(call, "Invalid clock arguments."), nil
	}
	loc, err := zone(args.Zone)
	if err != nil {
		return toolFailure(call, err.Error()), nil
	}
	at := time.Now()
	if args.At != "" {
		at, err = time.Parse(time.RFC3339, args.At)
		if err != nil {
			return toolFailure(call, "Supply RFC3339 with a UTC offset to disambiguate local time."), nil
		}
	}
	at = at.In(loc).AddDate(0, 0, args.Days).Add(time.Duration(args.Seconds) * time.Second)
	abbr, offset := at.Zone()
	return jsonResult(call, map[string]any{"utc": at.UTC().Format(time.RFC3339), "local": at.Format(time.RFC3339), "timezone": loc.String(), "abbreviation": abbr, "offset_seconds": offset, "daylight_saving": at.IsDST()})
}
