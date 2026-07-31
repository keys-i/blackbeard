package provider

import (
	"testing"

	"github.com/keys-i/blackbeard/src/internal/query"
)

func TestSearchRequestValidate(t *testing.T) {
	for _, limit := range []int{0, -1, query.MaxLimit + 1} {
		if err := (SearchRequest{Limit: limit}).Validate(); err == nil {
			t.Errorf("limit %d accepted", limit)
		}
	}
	if err := (SearchRequest{Limit: 50}).Validate(); err != nil {
		t.Fatal(err)
	}
}
