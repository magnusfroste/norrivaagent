package main

import (
	"reflect"
	"testing"
)

// Go's flag package stops at the first bare word. People type
// `norriva link ./x --name y`, and that must work.
func TestFlagsMayFollowPositionals(t *testing.T) {
	cases := map[string][]string{
		"link ./x --name y":       {"--name", "y", "./x"},
		"link --name y ./x":       {"--name", "y", "./x"},
		"link ./x --name=y":       {"--name=y", "./x"},
		"login --manual --host h": {"--manual", "--host", "h"},
		"run do the thing":        {"do", "the", "thing"},
		"run --workspace w do it": {"--workspace", "w", "do", "it"},
	}
	for in, want := range cases {
		args := splitFields(in)[1:]
		if got := flagsFirst(args); !reflect.DeepEqual(got, want) {
			t.Errorf("%q → %v, want %v", in, got, want)
		}
	}
}

func splitFields(s string) []string {
	var out []string
	for _, f := range []byte(s) {
		_ = f
	}
	cur := ""
	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
