package docker

import (
	"regexp"
	"strings"
)

const dockerHub = "docker.io"

// imageRef is a normalised image reference.
type imageRef struct {
	repo   string // registry/path, e.g. docker.io/library/alpine
	tag    string // "" when only a digest was given
	digest string // "sha256:…" or ""
}

func (r imageRef) String() string {
	s := r.repo
	if r.tag != "" {
		s += ":" + r.tag
	}
	if r.digest != "" {
		s += "@" + r.digest
	}
	return s
}

var (
	repoPathRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	domainRe   = regexp.MustCompile(`^(?:[a-zA-Z0-9-]+\.)*[a-zA-Z0-9-]+(?::[0-9]+)?$`)
	tagRe      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRe   = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	imageIDRe  = regexp.MustCompile(`^(?:sha256:)?[a-f0-9]{12,64}$`)
)

// isDomain reports whether the first path component of a reference names a
// registry, following the Docker reference grammar.
func isDomain(s string) bool {
	return s == "localhost" || strings.ContainsAny(s, ".:") || strings.ToLower(s) != s
}

// parseRef normalises a reference the way the Docker CLI does: Docker Hub
// is docker.io, official images live under library/, and a reference
// without tag or digest means :latest when defaultTag is set. ok is false
// for image IDs and malformed references.
func parseRef(s string, defaultTag bool) (ref imageRef, explicit bool, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" || imageIDRe.MatchString(s) {
		return imageRef{}, false, false
	}
	name, digest, hasDigest := strings.Cut(s, "@")
	if hasDigest && !digestRe.MatchString(digest) {
		return imageRef{}, false, false
	}
	var tag string
	if i, j := strings.LastIndex(name, ":"), strings.LastIndex(name, "/"); i > j {
		name, tag = name[:i], name[i+1:]
		if !tagRe.MatchString(tag) {
			return imageRef{}, false, false
		}
	}
	domain, path := dockerHub, name
	if first, rest, found := strings.Cut(name, "/"); found && isDomain(first) {
		domain, path = first, rest
	}
	if domain == "index.docker.io" {
		domain = dockerHub
	}
	if !domainRe.MatchString(domain) || !repoPathRe.MatchString(path) {
		return imageRef{}, false, false
	}
	if domain == dockerHub && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	explicit = tag != "" || hasDigest
	if !explicit && defaultTag {
		tag = "latest"
	}
	return imageRef{repo: domain + "/" + path, tag: tag, digest: digest}, explicit, true
}

// imageRule is one entry of a grant's image allowlist: a registry or
// namespace prefix ("ghcr.io/org/", "ghcr.io"), a repository (any tag), or
// an exact repository:tag or repository@digest.
type imageRule struct {
	prefix string
	repo   string
	tag    string
	digest string
}

func parseImageRules(entries []string) []imageRule {
	out := make([]imageRule, 0, len(entries))
	for _, e := range entries {
		if r, ok := parseImageRule(e); ok {
			out = append(out, r)
		}
	}
	return out
}

func parseImageRule(s string) (imageRule, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return imageRule{}, false
	}
	if !strings.Contains(s, "/") && (s == "localhost" || strings.Contains(s, ".")) && !strings.Contains(s, ":") {
		s += "/" // a bare registry name
	}
	if strings.HasSuffix(s, "/") {
		trimmed := strings.TrimSuffix(s, "/")
		first, _, _ := strings.Cut(trimmed, "/")
		if isDomain(first) {
			if first == "index.docker.io" {
				trimmed = dockerHub + strings.TrimPrefix(trimmed, "index.docker.io")
			}
		} else {
			trimmed = dockerHub + "/" + trimmed
		}
		if trimmed == "" || strings.Contains(trimmed, "//") {
			return imageRule{}, false
		}
		return imageRule{prefix: trimmed + "/"}, true
	}
	ref, explicit, ok := parseRef(s, false)
	if !ok {
		return imageRule{}, false
	}
	r := imageRule{repo: ref.repo}
	if explicit {
		r.tag, r.digest = ref.tag, ref.digest
	}
	return r, true
}

// allowed reports whether a normalised reference matches a rule.
func (r imageRule) allowed(ref imageRef) bool {
	if r.prefix != "" {
		return strings.HasPrefix(ref.repo, r.prefix)
	}
	if r.repo != ref.repo {
		return false
	}
	switch {
	case r.digest != "":
		return ref.digest == r.digest
	case r.tag != "":
		return ref.tag == r.tag
	}
	return true
}

func imageAllowed(rules []imageRule, ref imageRef) bool {
	for _, r := range rules {
		if r.allowed(ref) {
			return true
		}
	}
	return false
}

// refAllowed parses a reference (":latest" implied) and checks it.
func refAllowed(rules []imageRule, s string) (imageRef, bool) {
	ref, _, ok := parseRef(s, true)
	if !ok {
		return imageRef{}, false
	}
	return ref, imageAllowed(rules, ref)
}
