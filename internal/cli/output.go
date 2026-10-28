package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

var protoJSON = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}

// printProto writes a protobuf response as one line of JSON.
func (a *app) printProto(m proto.Message) error {
	b, err := protoJSON.Marshal(m)
	if err != nil {
		return err
	}
	// protojson output is valid JSON but its whitespace is unstable; compact it.
	var v json.RawMessage = b
	out, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.stdout, "%s\n", out)
	return err
}

// printJSONValue writes a local command's result as one line of JSON.
func (a *app) printJSONValue(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

func row(w io.Writer, cols ...string) {
	for i, c := range cols {
		cols[i] = cellText(c)
	}
	_, _ = fmt.Fprintln(w, strings.Join(cols, "\t"))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func capsString(c *mycelv1.Capabilities) string {
	if c == nil || len(c.GetNames()) == 0 {
		return "-"
	}
	return strings.Join(c.GetNames(), ",")
}

func roleString(r mycelv1.Role) string {
	switch r {
	case mycelv1.Role_ROLE_ADMIN:
		return "admin"
	case mycelv1.Role_ROLE_MEMBER:
		return "member"
	default:
		return "-"
	}
}

func parseRole(s string) (mycelv1.Role, error) {
	switch strings.ToLower(s) {
	case "admin":
		return mycelv1.Role_ROLE_ADMIN, nil
	case "member":
		return mycelv1.Role_ROLE_MEMBER, nil
	default:
		return mycelv1.Role_ROLE_UNSPECIFIED, usagef("invalid role %q: want admin or member", s)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// bytesString formats a byte count in binary units, e.g. "16.0 GiB".
func bytesString(n uint64) string {
	if n == 0 {
		return "-"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// ago renders an RFC 3339 timestamp relative to now ("12s ago"); unparseable
// values are shown as they are.
func ago(ts string, now time.Time) string {
	if ts == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	d := now.Sub(t)
	switch {
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}

func splitList(items []string) []string {
	var out []string
	for _, it := range items {
		for _, p := range strings.Split(it, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// isControl reports C0 and C1 control characters (including DEL).
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// termText strips C0/C1 controls except newline and tab from text that came
// from the network before it is printed for a person (--json stays raw).
func termText(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && isControl(r) {
			return -1
		}
		return r
	}, s)
}

// cellText makes a table cell one line: newline and tab become spaces and
// other controls are dropped.
func cellText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case isControl(r):
			return -1
		}
		return r
	}, s)
}
