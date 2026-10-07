package testutil

import "testing"

func TestLiveEnabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"true", false},
		{"1", true},
	}
	for _, c := range cases {
		t.Run("value="+c.val, func(t *testing.T) {
			t.Setenv(LiveEnvVar, c.val)
			if got := LiveEnabled(); got != c.want {
				t.Fatalf("LiveEnabled() with %s=%q = %v, want %v", LiveEnvVar, c.val, got, c.want)
			}
		})
	}
}

func TestRequireLive(t *testing.T) {
	run := func(env string) (ranBody, skipped bool) {
		t.Setenv(LiveEnvVar, env)
		t.Run("inner", func(t *testing.T) {
			defer func() { skipped = t.Skipped() }()
			RequireLive(t)
			ranBody = true
		})
		return ranBody, skipped
	}

	if ran, skipped := run(""); ran || !skipped {
		t.Errorf("RequireLive must skip when NOFX_LIVE_TESTS is unset (ran=%v skipped=%v)", ran, skipped)
	}
	if ran, skipped := run("1"); !ran || skipped {
		t.Errorf("RequireLive must not skip when NOFX_LIVE_TESTS=1 (ran=%v skipped=%v)", ran, skipped)
	}
}
