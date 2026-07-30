package app

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestHelpAndVersion(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{nil, "Usage:"},
		{[]string{"version"}, "test-version"},
	} {
		var out bytes.Buffer
		if err := Run(context.Background(), test.args, strings.NewReader(""), &out, &out, "test-version"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), test.want) {
			t.Fatalf("output %q does not contain %q", out.String(), test.want)
		}
	}
}
