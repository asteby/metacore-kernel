package strings

import "testing"

func TestTitleCase(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"hello", "Hello"},
		{"HELLO WORLD", "Hello World"},
		{"hello world", "Hello World"},
		{"user_profile", "User_Profile"},
		{"order details", "Order Details"},
		{"álvaro pérez", "Álvaro Pérez"},
		{"foo-bar-baz", "Foo-Bar-Baz"},
	}
	for _, c := range cases {
		if got := TitleCase(c.in); got != c.want {
			t.Errorf("TitleCase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFold(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"Ñandú Pérez", "nandu perez"},
		{"  Ñandú   PÉREZ \t\n", "nandu perez"},
		{"ÁÉÍÓÚ üÜ", "aeiou uu"},
		{"café", "cafe"}, // already decomposed input
		{"Straße", "straße"},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := Fold(c.in); got != c.want {
			t.Errorf("Fold(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
