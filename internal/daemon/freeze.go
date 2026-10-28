package daemon

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
)

// FrozenFile records a network freeze (<NodeDir>/frozen.json), so a node
// restarted while frozen stays frozen until the hub sends Unfreeze.
const FrozenFile = "frozen.json"

const (
	scopeAll     = "all"
	frozenMarker = "frozen"
	// pauseFrozen and pauseLocalFirst are the reasons a job's sandbox is
	// paused; it runs again once no reason is left.
	pauseFrozen     = "freeze"
	pauseLocalFirst = "local_first"
)

// freezeState is GET /api/local/freeze and the content of frozen.json.
type freezeState struct {
	Frozen bool   `json:"frozen"`
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
	Since  string `json:"since,omitempty"`
}

func (n *node) isFrozen() bool {
	n.fzMu.Lock()
	defer n.fzMu.Unlock()
	return n.fz.Frozen
}

func (n *node) freezeSnapshot() freezeState {
	n.fzMu.Lock()
	defer n.fzMu.Unlock()
	return n.fz
}

// loadFrozen reads frozen.json at startup.
func (n *node) loadFrozen() {
	b, err := os.ReadFile(filepath.Join(n.dir, FrozenFile))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var f freezeState
	if err != nil || json.Unmarshal(b, &f) != nil {
		// Fail closed: a freeze record that cannot be read keeps the node frozen.
		n.log.Error("frozen.json unreadable; staying frozen until the hub unfreezes", "err", err)
		f = freezeState{Frozen: true, Scope: scopeAll, Reason: "unreadable freeze record"}
	}
	if f.Frozen {
		n.fz = f
		n.log.Warn("node starts frozen", "reason", f.Reason, "since", f.Since)
	}
}

// applyStartupFreeze makes the brokers deny when the node starts frozen.
func (n *node) applyStartupFreeze() {
	if f := n.freezeSnapshot(); f.Frozen {
		n.grants.b.freeze(f.Reason)
	}
}

// freeze handles the Freeze ctl. Scope "all" pauses every sandbox and
// worker, makes every broker deny and refuses new jobs, spawns and agent
// write RPCs until Unfreeze; the state survives a restart. Scope
// "channel:<name>" is enforced by the hub (it pauses the channel's agents
// with AgentCtl), so the node only logs it.
func (n *node) freeze(f *mycelv1.Freeze) string {
	scope := f.GetScope()
	switch {
	case scope == "" || scope == scopeAll:
	case strings.HasPrefix(scope, "channel:") && len(scope) > len("channel:"):
		n.log.Info("channel freeze from hub; the hub pauses the channel's agents", "scope", scope, "reason", f.GetReason())
		return ""
	default:
		return ctlError("invalid_argument", "unknown freeze scope %q", scope)
	}
	reason := f.GetReason()
	if reason == "" {
		reason = "network frozen"
	}
	st := freezeState{Frozen: true, Scope: scopeAll, Reason: reason, Since: time.Now().UTC().Format(time.RFC3339)}
	n.fzMu.Lock()
	already := n.fz.Frozen
	if already {
		st.Since = n.fz.Since
	}
	n.fz = st
	n.fzMu.Unlock()
	b, err := json.Marshal(st)
	if err == nil {
		err = writeFileAtomic(filepath.Join(n.dir, FrozenFile), b)
	}
	n.log.Warn("node frozen by hub", "reason", reason)
	n.grants.b.freeze(reason)
	n.jobs.setPause(pauseFrozen, true)
	n.local.freezeWorkers(true)
	if err != nil {
		// Frozen in memory all the same; a restart would forget it.
		n.log.Error("store freeze state", "err", err)
		return ctlError("internal", "could not store freeze")
	}
	return ""
}

// unfreeze handles the Unfreeze ctl.
func (n *node) unfreeze(u *mycelv1.Unfreeze) string {
	scope := u.GetScope()
	switch {
	case scope == "" || scope == scopeAll:
	case strings.HasPrefix(scope, "channel:"):
		n.log.Info("channel unfreeze from hub", "scope", scope)
		return ""
	default:
		return ctlError("invalid_argument", "unknown freeze scope %q", scope)
	}
	n.fzMu.Lock()
	was := n.fz.Frozen
	n.fz = freezeState{}
	n.fzMu.Unlock()
	if err := os.Remove(filepath.Join(n.dir, FrozenFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		n.log.Error("remove freeze state", "err", err)
	}
	if !was {
		return ""
	}
	n.log.Warn("node unfrozen by hub")
	n.grants.b.unfreeze()
	n.jobs.setPause(pauseFrozen, false)
	n.local.freezeWorkers(false)
	return ""
}

func (l *localAPI) handleFreeze(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, l.n.freezeSnapshot())
}
