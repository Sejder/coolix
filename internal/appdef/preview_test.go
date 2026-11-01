package appdef

import (
	"strings"
	"testing"

	"coolix/internal/units"
)

func TestPreviewName(t *testing.T) {
	for _, c := range []struct{ parent, ref, want string }{
		{"blog", "pr-12", "blog-pr12"},
		{"blog", "feature/New_UI", "blog-feature-new-ui"},
		{"a-very-long-app-nam", "pr-123", "a-very-long-ap-pr123"},
		{"blog", "---", ""},
	} {
		got := PreviewName(c.parent, c.ref)
		if c.want != "" && got != c.want {
			t.Errorf("%s %s: got %q, want %q", c.parent, c.ref, got, c.want)
		}
		if !units.ValidName(got) {
			t.Errorf("%s %s: %q is not a valid app name", c.parent, c.ref, got)
		}
	}
	a := PreviewName("blog", "a-really-long-branch-name-one")
	b := PreviewName("blog", "a-really-long-branch-name-two")
	if a == b || !units.ValidName(a) || !units.ValidName(b) {
		t.Errorf("long branches: %q %q", a, b)
	}
}

func TestPinSource(t *testing.T) {
	const rev = "0123456789abcdef0123456789abcdef01234567"
	for _, c := range []struct{ src, branch, rev, want string }{
		{"github:me/blog", "feat/x", rev, "github:me/blog?rev=" + rev},
		{"github:me/blog/main", "feat", "", "github:me/blog?ref=feat"},
		{"gitlab:me/blog?host=gitlab.example.com&ref=main", "f", rev, "gitlab:me/blog?host=gitlab.example.com&rev=" + rev},
		{"git+https://example.com/blog.git?ref=main", "f", rev, "git+https://example.com/blog.git?ref=refs/heads/f&rev=" + rev},
		{"git+file:///srv/blog", "f", "", "git+file:///srv/blog?ref=refs/heads/f"},
	} {
		got, err := PinSource(c.src, c.branch, c.rev)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.src, got, err, c.want)
		}
	}
	for _, c := range [][3]string{
		{"path:/srv/blog", "f", ""},
		{"github:me/blog", "a..b", ""},
		{"github:me/blog", "f", "nothex"},
		{"github:me", "f", ""},
	} {
		if _, err := PinSource(c[0], c[1], c[2]); err == nil {
			t.Errorf("%v: expected an error", c)
		}
	}
}

func TestValidatePreview(t *testing.T) {
	p := valid()
	p.Name, p.PreviewOf, p.PreviewRef = "blog-pr1", "blog", "pr-1"
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.Preview = &Preview{Enabled: true}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "can't have previews") {
		t.Fatalf("nested previews: %v", err)
	}
	q := valid()
	q.PreviewRef = "pr-1"
	if err := q.Validate(); err == nil {
		t.Fatal("preview_ref without preview_of accepted")
	}
	q.PreviewRef, q.Preview = "", &Preview{Enabled: true, Max: 99}
	if err := q.Validate(); err == nil {
		t.Fatal("max 99 accepted")
	}
}
