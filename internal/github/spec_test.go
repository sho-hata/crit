package github

import (
	"testing"
)

func TestParsePRSpec(t *testing.T) {
	cases := []struct {
		in      string
		want    ChangeID
		wantErr bool
	}{
		{"295", ChangeID{Number: 295}, false},
		{
			"https://github.com/a/b/pull/295",
			ChangeID{Number: 295, Project: "a/b", Host: "github.com"},
			false,
		},
		{
			"https://github.com/a/b/pull/295/files",
			ChangeID{Number: 295, Project: "a/b", Host: "github.com"},
			false,
		},
		{
			"https://github.com/myorg/repo-b/pull/1?diff=split",
			ChangeID{Number: 1, Project: "myorg/repo-b", Host: "github.com"},
			false,
		},
		{
			"https://www.github.com/o/r/pull/2",
			ChangeID{Number: 2, Project: "o/r", Host: "github.com"},
			false,
		},
		{
			"https://github.example.com/acme/app/pull/9",
			ChangeID{Number: 9, Project: "acme/app", Host: "github.example.com"},
			false,
		},
		{"http://github.com/o/r/pull/7", ChangeID{Number: 7, Project: "o/r", Host: "github.com"}, false},
		{
			"https://github.com:443/o/r/pull/8",
			ChangeID{Number: 8, Project: "o/r", Host: "github.com"},
			false,
		},
		{
			"https://GITHUB.COM/O/R/pull/3",
			ChangeID{Number: 3, Project: "O/R", Host: "github.com"},
			false,
		},
		{"abc", ChangeID{}, true},
		{"-5", ChangeID{}, true},
		{"0", ChangeID{}, true},
		{"", ChangeID{}, true},
		{"https://github.com/a/b/issues/295", ChangeID{}, true},
		{"https://github.com/a/b/pull/0", ChangeID{}, true},
		{"https://github.com/only-one/pull/1", ChangeID{}, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParsePRSpec(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, c.wantErr)
			}
			if got != c.want {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
		})
	}
}
