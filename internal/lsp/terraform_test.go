package lsp

import "testing"

func TestTFNoResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code int
		msg  string
		want bool
	}{
		// Answers measured against terraform-ls 0.39.0.
		{tfSystemErrorCode, "main.tf (3,14): position outside of any attribute name, value or block", true},
		{tfSystemErrorCode, `main.tf (14,13): position outside of "module" body`, true},
		{tfSystemErrorCode, `main.tf (24,12): unknown attribute "foo"`, true},
		{tfSystemErrorCode, "no reference origin found", true},
		{tfSystemErrorCode, "no reference target found", true},
		// Same generic code, but a real failure.
		{tfSystemErrorCode, "main.tf: file not found", false},
		{tfSystemErrorCode, "no schema available", false},
		// A known message under another code is not this server's no-result.
		{-32603, "no reference origin found", false},
	}
	for _, tc := range cases {
		if got := tfNoResult(&ResponseError{Code: tc.code, Message: tc.msg}); got != tc.want {
			t.Errorf("tfNoResult(%d, %q) = %v, want %v", tc.code, tc.msg, got, tc.want)
		}
	}
}
