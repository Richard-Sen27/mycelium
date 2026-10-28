package hub

import (
	"context"
	"strconv"
	"time"

	"connectrpc.com/connect"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

const (
	defaultAuditLimit = 100
	maxAuditLimit     = 1000
	maxAuditFilter    = 512
)

// ListAudit replays the audit log (the SQLite table the AUDIT stream
// mirrors), newest first. Members only see entries whose actor is
// themselves, one of their nodes or one of their agents; admins see all.
func (e *eventService) ListAudit(ctx context.Context, req *connect.Request[mycelv1.ListAuditRequest]) (*connect.Response[mycelv1.ListAuditResponse], error) {
	s := e.s
	u, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	for _, f := range []string{m.GetTraceId(), m.GetActor(), m.GetResource()} {
		if len(f) > maxAuditFilter {
			return nil, invalidArg("filters are at most %d bytes", maxAuditFilter)
		}
	}
	q := auditQuery{traceID: m.GetTraceId(), actor: m.GetActor(), resource: m.GetResource(),
		limit: clampLimit(m.GetLimit(), defaultAuditLimit, maxAuditLimit)}
	if q.since, err = auditTime("since", m.GetSince()); err != nil {
		return nil, err
	}
	if q.until, err = auditTime("until", m.GetUntil()); err != nil {
		return nil, err
	}
	if tok := m.GetPageToken(); tok != "" {
		n, err := strconv.ParseInt(tok, 10, 64)
		if err != nil || n <= 0 {
			return nil, invalidArg("invalid page_token")
		}
		q.before = n
	}
	if u.Role != roleAdmin {
		q.member = u.ID
	}
	want := q.limit
	q.limit++
	rows, err := s.st.queryAudit(ctx, q)
	if err != nil {
		return nil, s.internalError("list audit", err)
	}
	out := &mycelv1.ListAuditResponse{}
	for i, r := range rows {
		if i == want {
			out.NextPageToken = strconv.FormatInt(rows[i-1].Seq, 10)
			break
		}
		out.Entries = append(out.Entries, r.proto())
	}
	return connect.NewResponse(out), nil
}

// auditTime normalises an RFC 3339 filter to the stored format.
func auditTime(name, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return "", invalidArg("%s must be an RFC 3339 time", name)
	}
	return formatTime(t), nil
}
