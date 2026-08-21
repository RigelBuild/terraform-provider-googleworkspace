package googleworkspace

import (
	"testing"
)

func TestSnakeToCamel(t *testing.T) {
	input := make([]string, 3)
	input[0] = "i_am_snake_case"
	input[1] = "IAmUpperCamelCase"
	input[2] = "iAmAlreadyCamelCase"

	expected := []string{"iAmSnakeCase", "iAmUpperCamelCase", "iAmAlreadyCamelCase"}

	for it, in := range input {
		result := SnakeToCamel(in)

		if result != expected[it] {
			t.Errorf("Failed [%s]: result (%s) did not match expected (%s)", in, result, expected[it])
		}
	}
}

func TestCamelToSnake(t *testing.T) {
	input := make([]string, 3)
	input[0] = "i_am_snake_case"
	input[1] = "IAmUpperCamelCase"
	input[2] = "iAmLowerCamelCase"

	expected := []string{"i_am_snake_case", "i_am_upper_camel_case", "i_am_lower_camel_case"}

	for it, in := range input {
		result := CameltoSnake(in)

		if result != expected[it] {
			t.Errorf("Failed [%s]: result (%s) did not match expected (%s)", in, result, expected[it])
		}
	}
}

func TestIsEmail(t *testing.T) {
	type testCase struct {
		input string
		want  bool
	}

	tests := []testCase{
		{
			input: "",
			want:  false,
		},
		{
			input: "1234567890987654321",
			want:  false,
		},
		{
			input: "example.com",
			want:  false,
		},
		{
			input: "user@example.com",
			want:  true,
		},
	}

	for _, tc := range tests {
		got := isEmail(tc.input)
		if tc.want != got {
			t.Fatalf("expected: %v, got: %v", tc.want, got)
		}
	}
}

func TestEmptyOrStringInSlice(t *testing.T) {
	// The email `type` enum is representative of every nested-object enum the
	// helper guards (organizations, phones, addresses, …).
	validate := emptyOrStringInSlice([]string{"custom", "home", "other", "work"})

	type testCase struct {
		name    string
		input   string
		wantErr bool
	}

	tests := []testCase{
		// The bug: an untyped nested entry from the Directory API materializes as
		// "" and must import cleanly rather than fail the enum.
		{name: "empty accepted (absent on import)", input: "", wantErr: false},
		{name: "valid enum value accepted", input: "home", wantErr: false},
		{name: "other valid enum value accepted", input: "work", wantErr: false},
		// Enum validation is preserved for genuinely wrong authored values.
		{name: "invalid value rejected", input: "bogus", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := validate(tc.input, "type")
			gotErr := len(errs) > 0
			if gotErr != tc.wantErr {
				t.Fatalf("input %q: wantErr=%v, got errs=%v", tc.input, tc.wantErr, errs)
			}
		})
	}

	// The helper is enum-shape-agnostic: it takes the allowed set as an argument,
	// so a structurally different enum (multi-word, underscore-bearing values,
	// like external_ids' set) behaves identically — "" accepted, a real value
	// accepted, a wrong value rejected.
	t.Run("enum-agnostic (underscore values)", func(t *testing.T) {
		validateExternalIDs := emptyOrStringInSlice([]string{"account", "custom", "login_id", "network"})
		for input, wantErr := range map[string]bool{
			"":         false, // absent on import
			"login_id": false, // valid multi-word value
			"account":  false,
			"bogus_id": true, // not in the set
		} {
			_, errs := validateExternalIDs(input, "type")
			if gotErr := len(errs) > 0; gotErr != wantErr {
				t.Fatalf("input %q: wantErr=%v, got errs=%v", input, wantErr, errs)
			}
		}
	})
}
