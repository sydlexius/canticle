package config

import "testing"

func TestResolveBool(t *testing.T) {
	bp := func(v bool) *bool { return &v }

	cases := []struct {
		name          string
		cli           *bool
		lib           *bool
		globalDefault bool
		want          bool
	}{
		{"cli wins over lib and global", bp(true), bp(false), false, true},
		{"cli false wins over lib and global true", bp(false), bp(true), true, false},
		{"lib wins over global when cli nil", nil, bp(true), false, true},
		{"lib false wins over global true when cli nil", nil, bp(false), true, false},
		{"global used when cli and lib nil (true)", nil, nil, true, true},
		{"global used when cli and lib nil (false)", nil, nil, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveBool(tc.cli, tc.lib, tc.globalDefault); got != tc.want {
				t.Fatalf("ResolveBool(%v, %v, %v) = %v; want %v", tc.cli, tc.lib, tc.globalDefault, got, tc.want)
			}
		})
	}
}
