package modules

import "testing"

// Expected values are ansible-core 2.21.4's mask_url on CPython 3.14.
func TestMaskURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://example.com/a":                      "http://example.com/a",
		"http://user:pw@example.com/a":              "http://****:****@example.com/a",
		"HTTP://user:pw@Example.com:8080/a;p?q=1#f": "http://****:****@Example.com:8080/a;p?q=1#f",
		"https://user@example.com/":                 "https://****@example.com/",
		"https://:pw@example.com/":                  "https://****:****@example.com/",
		"https://user:@example.com/":                "https://user:@example.com/",
		"https://:@example.com/":                    "https://:@example.com/",
		"https://@example.com/":                     "https://@example.com/",
		"http://u:p@h/x?":                           "http://****:****@h/x",
		"http://u:p@h/x#":                           "http://****:****@h/x",
		"http://u:p@h":                              "http://****:****@h",
		"http://a@b:c@h/x":                          "http://****:****@h/x",
		"redis://:password@host":                    "redis://****:****@host",
		"ftp://u:p@h/f.txt":                         "ftp://****:****@h/f.txt",
		"u:p@h/x":                                   "u:p@h/x",
		"//u:p@h/x":                                 "//****:****@h/x",
		"  http://u:p@h/x":                          "http://****:****@h/x",
		"http://u:p@[::1]:80/x":                     "http://****:****@[::1]:80/x",
	} {
		if got := MaskURL(in); got != want {
			t.Errorf("MaskURL(%q) = %q, want %q", in, got, want)
		}
	}
}
