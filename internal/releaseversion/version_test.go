package releaseversion

import "testing"

func TestOrderingAndChannels(t *testing.T) {
	for _, test := range []struct {
		left, right string
		comparison  int
	}{
		{"1.2.3-rc.2", "1.2.3-rc.1", 1},
		{"1.2.3-rc.10", "1.2.3-rc.2", 1},
		{"1.2.3", "1.2.3-rc.99", 1},
		{"1.2.3-rc.1", "1.2.4-rc.0", -1},
		{"v1.2.3", "1.2.3", 0},
		{"1.2.3-rc.1", "1.2.3-rc.1", 0},
	} {
		left, err := Parse(test.left)
		if err != nil {
			t.Fatal(err)
		}
		right, err := Parse(test.right)
		if err != nil {
			t.Fatal(err)
		}
		if got := left.Compare(right); got != test.comparison {
			t.Errorf("%s compared to %s: %d", test.left, test.right, got)
		}
		floor, err := Parse(left.Floor())
		if err != nil || floor.Channel() != left.Channel() || !floor.IsZero() || left.Compare(floor) <= 0 {
			t.Fatalf("invalid same-channel baseline for %s: %v", test.left, err)
		}
	}
}

func TestMalformedVersionsFailClosed(t *testing.T) {
	for _, value := range []string{"dev", "", "1.2", "+1.2.3", "01.2.3", "1.2.3-rc.", "1.2.3-rc.01", "1.2.3-beta.1", "1.2.3+build", "1.2.3-rc.1.extra", "1.2.3-rc.-1", "18446744073709551616.1.1", "1.2.3-rc.18446744073709551616"} {
		if _, err := Parse(value); err == nil {
			t.Errorf("accepted malformed version %q", value)
		}
	}
}
