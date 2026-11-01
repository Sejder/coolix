package appdef

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"coolix/internal/units"
)

// Preview configures preview deployments of an app's branches and pull
// requests. Previews get the app's settings and their own empty databases,
// but none of its secrets unless Secrets is set.
type Preview struct {
	Enabled bool `toml:"enabled,omitempty"`
	// Domain: previews are served at <preview name>.<Domain> (default:
	// services.coolix.previews.domain).
	Domain string `toml:"domain,omitempty"`
	// Secrets gives previews the app's secrets. Their code comes from
	// branches and pull requests, so this is off by default.
	Secrets bool `toml:"secrets,omitempty"`
	// Max previews at once (default 5).
	Max int `toml:"max,omitempty"`
}

const (
	DefaultPreviewMax = 5
	maxPreviewMax     = 50
)

// PreviewsEnabled reports whether the app takes previews.
func (a *App) PreviewsEnabled() bool { return a.Preview != nil && a.Preview.Enabled }

// PreviewMax is how many previews the app may have at once.
func (a *App) PreviewMax() int {
	if a.Preview == nil || a.Preview.Max == 0 {
		return DefaultPreviewMax
	}
	return a.Preview.Max
}

// IsPreview reports whether the app is a preview of another app.
func (a *App) IsPreview() bool { return a.PreviewOf != "" }

// PushRepo is the app whose bare repo a push app builds from: previews of
// push apps build branches of their parent's repo.
func (a *App) PushRepo() string {
	if a.PreviewOf != "" {
		return a.PreviewOf
	}
	return a.Name
}

var prRefRe = regexp.MustCompile(`^pr-[1-9][0-9]{0,8}$`)

func (a *App) validatePreview() []error {
	var errs []error
	if p := a.Preview; p != nil {
		if p.Domain != "" && !units.ValidDomain(p.Domain) {
			errs = append(errs, fmt.Errorf("preview domain %q: invalid hostname", p.Domain))
		}
		if p.Max < 0 || p.Max > maxPreviewMax {
			errs = append(errs, fmt.Errorf("preview max %d: want 1-%d", p.Max, maxPreviewMax))
		}
	}
	if a.PreviewOf != "" {
		switch {
		case !units.ValidName(a.PreviewOf) || a.PreviewOf == a.Name:
			errs = append(errs, fmt.Errorf("preview_of %q: invalid app name", a.PreviewOf))
		case !prRefRe.MatchString(a.PreviewRef) && !ValidBranch(a.PreviewRef):
			errs = append(errs, fmt.Errorf("preview_ref %q: want a branch or pr-<number>", a.PreviewRef))
		case a.PreviewsEnabled():
			errs = append(errs, errors.New("a preview can't have previews"))
		}
	} else if a.PreviewRef != "" || a.PreviewSecrets {
		errs = append(errs, errors.New("preview_ref and preview_secrets are only for previews (preview_of)"))
	}
	return errs
}

// PRRef is the preview_ref of pull request n.
func PRRef(n int) string { return "pr-" + strconv.Itoa(n) }

// PreviewName names the preview of parent for ref (a branch or PRRef):
// <parent>-pr12 or <parent>-<branch slug>, cut to fit app name limits.
func PreviewName(parent, ref string) string {
	suffix := slug(ref)
	if prRefRe.MatchString(ref) {
		suffix = "pr" + strings.TrimPrefix(ref, "pr-")
	}
	const max = 20
	if len(suffix) > max-3 {
		// Keep long branch names apart: a hash of the whole ref.
		sum := sha256.Sum256([]byte(ref))
		suffix = strings.TrimRight(suffix[:max-3-7], "-") + "-" + hex.EncodeToString(sum[:])[:6]
	}
	p := parent
	if len(p)+1+len(suffix) > max {
		p = strings.TrimRight(p[:max-1-len(suffix)], "-")
	}
	return p + "-" + suffix
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(branch string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(branch), "-"), "-")
	if s == "" {
		sum := sha256.Sum256([]byte(branch))
		return "b" + hex.EncodeToString(sum[:])[:6]
	}
	return s
}

// PinSource turns an app's flake reference into one of branch at rev, for a
// preview: github:o/r -> github:o/r?rev=<rev>, git+https://x/r.git ->
// git+https://x/r.git?ref=refs/heads/<branch>&rev=<rev>.
func PinSource(source, branch, rev string) (string, error) {
	if !ValidBranch(branch) {
		return "", fmt.Errorf("invalid branch %q", branch)
	}
	if rev != "" && !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(rev) {
		return "", fmt.Errorf("invalid commit %q", rev)
	}
	base, query, _ := strings.Cut(source, "?")
	q, err := url.ParseQuery(query)
	if err != nil {
		return "", fmt.Errorf("source %q: %w", source, err)
	}
	q.Del("ref")
	q.Del("rev")
	var params []string
	scheme, rest, _ := strings.Cut(base, ":")
	switch {
	case scheme == "github" || scheme == "gitlab" || scheme == "sourcehut":
		// owner/repo[/ref]: drop the ref, it's replaced below.
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) < 2 {
			return "", fmt.Errorf("source %q: want %s:owner/repo", source, scheme)
		}
		base = scheme + ":" + parts[0] + "/" + parts[1]
		if rev != "" {
			params = append(params, "rev="+rev)
		} else {
			params = append(params, "ref="+branch)
		}
	case strings.HasPrefix(scheme, "git+"):
		params = append(params, "ref=refs/heads/"+branch)
		if rev != "" {
			params = append(params, "rev="+rev)
		}
	default:
		return "", fmt.Errorf("previews need a git source (github:, gitlab:, sourcehut: or git+…), not %q", source)
	}
	if other := q.Encode(); other != "" {
		params = append([]string{other}, params...)
	}
	return base + "?" + strings.Join(params, "&"), nil
}
