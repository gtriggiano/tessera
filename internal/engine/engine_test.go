package engine

import (
	"testing"

	"github.com/gtriggiano/tessera/internal/request"
)

func TestDefaultAction(t *testing.T) {
	r := &request.Request{Method: "GET", Host: "www.example.com", Path: "/"}
	for _, tc := range []struct {
		defaultAction string
		want          Action
	}{
		{"allow", Allow},
		{"deny", Deny},
		// A value that is not allow never allows.
		{"", Deny},
		{"Allow", Deny},
	} {
		if got := New(tc.defaultAction).Decide(r); got.Action != tc.want {
			t.Errorf("default action %q: decision %q, want %q", tc.defaultAction, got.Action, tc.want)
		}
	}
}
