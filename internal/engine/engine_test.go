package engine

import (
	"reflect"
	"testing"
)

func TestReservedDomains(t *testing.T) {
	got := reservedDomains("http://coolix.example.com example.org:8443,www.example.org *.internal.example.com :80 Docs.Example.com.")
	want := []string{"coolix.example.com", "example.org", "www.example.org", "*.internal.example.com", "docs.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}
