package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Event is one `runsc events` record (runc-compatible shape).
type Event struct {
	Type string          `json:"type"`
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

// EventStats is the data of a "stats" event (the fields this package uses).
type EventStats struct {
	CPU struct {
		Usage struct {
			Total  uint64 `json:"total"` // nanoseconds
			Kernel uint64 `json:"kernel"`
			User   uint64 `json:"user"`
		} `json:"usage"`
	} `json:"cpu"`
	Memory struct {
		Cache uint64 `json:"cache"`
		Usage struct {
			Usage uint64 `json:"usage"`
			Limit uint64 `json:"limit"`
			Max   uint64 `json:"max"`
		} `json:"usage"`
	} `json:"memory"`
	Pids struct {
		Current uint64 `json:"current"`
		Limit   uint64 `json:"limit"`
	} `json:"pids"`
}

// ParseStatsEvent reads the first "stats" event from `runsc events --stats`
// output (one JSON object per line; other event types are skipped).
func ParseStatsEvent(out []byte) (EventStats, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var ev Event
		err := dec.Decode(&ev)
		if errors.Is(err, io.EOF) {
			return EventStats{}, errors.New("bundle: no stats event in runsc output")
		}
		if err != nil {
			return EventStats{}, fmt.Errorf("bundle: parse runsc events: %w", err)
		}
		if ev.Type != "stats" {
			continue
		}
		var st EventStats
		if len(ev.Data) > 0 && string(ev.Data) != "null" {
			if err := json.Unmarshal(ev.Data, &st); err != nil {
				return EventStats{}, fmt.Errorf("bundle: parse runsc stats: %w", err)
			}
		}
		return st, nil
	}
}

// ParseDiskUsage reads the byte count of `du -s -B1` output.
func ParseDiskUsage(out []byte) (int64, error) {
	f := splitFields(string(out))
	if len(f) == 0 {
		return 0, errors.New("bundle: empty du output")
	}
	return strconv.ParseInt(f[0], 10, 64)
}

// ParsePid reads a pid file.
func ParsePid(out []byte) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bundle: bad pid %q", strings.TrimSpace(string(out)))
	}
	return n, nil
}

// DecodeWSLText decodes wsl.exe output, which is UTF-16LE (with or without
// BOM) for its own commands and UTF-8 for commands run inside a distro.
func DecodeWSLText(b []byte) string {
	if len(b) >= 2 && b[0] == 0xff && b[1] == 0xfe {
		b = b[2:]
	} else if !looksUTF16LE(b) {
		return string(b)
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(u))
}

func looksUTF16LE(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	zeros := 0
	for i := 1; i < len(b); i += 2 {
		if b[i] == 0 {
			zeros++
		}
	}
	return zeros*2 >= len(b)/2
}

func splitFields(s string) []string { return strings.Fields(s) }
