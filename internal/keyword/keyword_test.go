package keyword

import (
	"reflect"
	"testing"
)

func TestFromIMAPBijective(t *testing.T) {
	got := FromIMAP([]string{`\Seen`, `\Flagged`, `Work`, `\Important`})
	want := map[string]bool{"$seen": true, "$flagged": true, "Work": true, `\Important`: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FromIMAP = %#v, want %#v", got, want)
	}
}

func TestToIMAPInverse(t *testing.T) {
	in := map[string]bool{"$seen": true, "$draft": true, "Work": true, "$skip": false}
	got := ToIMAP(in)
	want := []string{`\Seen`, `\Draft`, `Work`}
	if !equalSet(got, want) {
		t.Fatalf("ToIMAP = %q, want %q", got, want)
	}
	// Round trip: every system keyword maps back to itself.
	for _, sys := range []string{"$seen", "$answered", "$flagged", "$deleted", "$draft"} {
		flags := ToIMAP(map[string]bool{sys: true})
		back := FromIMAP(flags)
		if !back[sys] {
			t.Errorf("round trip of %s lost: flags=%q back=%#v", sys, flags, back)
		}
	}
}

func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
